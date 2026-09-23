import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { HOLD_SECONDS, NameRepoError, getReservation, heldFor, listNames, remove, rename, reserve, setDefault } from "../../src/lib/name-repo";

let seq = 0;
async function user(maxNames = 10): Promise<string> {
  const id = `usr_names${String(seq++).padStart(11, "0")}`;
  await env.DB.prepare("INSERT INTO users (id, email, created_at, max_names) VALUES (?1, ?2, 0, ?3)").bind(id, `${id}@example.com`, maxNames).run();
  return id;
}
const now = () => Math.floor(Date.now() / 1000);
const code = async (p: Promise<unknown>) => {
  try {
    await p;
    return "ok";
  } catch (e) {
    return e instanceof NameRepoError ? e.code : String(e);
  }
};

describe("reserve", () => {
  it("20 concurrent reserves at limit 10 → exactly 10", async () => {
    const u = await user(10);
    const results = await Promise.all(Array.from({ length: 20 }, (_, i) => code(reserve(env.DB, u, `burst-${u.slice(-4)}-${i}`, now()))));
    expect(results.filter((r) => r === "ok").length).toBe(10);
    expect(results.filter((r) => r === "quota_exceeded").length).toBe(10);
    const names = await listNames(env.DB, u);
    expect(names.filter((n) => n.is_default === 1)).toHaveLength(1);
  });

  it("two users race for one name → exactly one wins", async () => {
    const [a, b] = [await user(), await user()];
    const results = await Promise.all([code(reserve(env.DB, a, "race-name", now())), code(reserve(env.DB, b, "race-name", now()))]);
    expect(results.sort()).toEqual(["name_taken", "ok"]);
  });

  it("the first name becomes the default", async () => {
    const u = await user();
    await reserve(env.DB, u, "first-one", now());
    await reserve(env.DB, u, "second-one", now());
    expect((await getReservation(env.DB, "first-one"))!.is_default).toBe(1);
    expect((await getReservation(env.DB, "second-one"))!.is_default).toBe(0);
  });
});

describe("rename", () => {
  it("renames the default in place at 10/10, holding the old name with a hint", async () => {
    const u = await user(2);
    const r0 = await reserve(env.DB, u, "old-default", now());
    await reserve(env.DB, u, "other-name", now());
    const { reservation: r, outboxIds } = await rename(env.DB, u, "old-default", "new-default", now());
    expect(outboxIds).toHaveLength(1);
    expect(r).toMatchObject({ name: "new-default", is_default: 1 });
    expect(r.gen).not.toBe(r0.gen);
    expect(await heldFor(env.DB, "old-default", u, now())).toEqual({ renamed_to: "new-default" });
    const outbox = await env.DB.prepare("SELECT payload FROM do_outbox WHERE name = 'old-default'").first<{ payload: string }>();
    expect(JSON.parse(outbox!.payload)).toEqual({ reason: "renamed", newName: "new-default", gen: r0.gen });
  });

  it("rejects renaming to the same name and keeps rename hints pointing at the latest name", async () => {
    const u = await user();
    await reserve(env.DB, u, "chain-a", now());
    expect(await code(rename(env.DB, u, "chain-a", "chain-a", now()))).toBe("same_name");
    await rename(env.DB, u, "chain-a", "chain-b", now());
    await rename(env.DB, u, "chain-b", "chain-c", now());
    expect(await heldFor(env.DB, "chain-a", u, now())).toEqual({ renamed_to: "chain-c" });
    expect(await heldFor(env.DB, "chain-b", u, now())).toEqual({ renamed_to: "chain-c" });
  });

  it("refuses a target held for another user (409 on hold)", async () => {
    const [a, b] = [await user(), await user()];
    await reserve(env.DB, a, "held-target", now());
    await remove(env.DB, a, "held-target", now());
    await reserve(env.DB, b, "b-name", now());
    expect(await code(rename(env.DB, b, "b-name", "held-target", now()))).toBe("name_on_hold");
  });

  it("refuses to rename a suspended name (403) and others' names (404)", async () => {
    const [a, b] = [await user(), await user()];
    await reserve(env.DB, a, "susp-name", now());
    await env.DB.prepare("UPDATE reservations SET status = 'suspended' WHERE name = 'susp-name'").run();
    expect(await code(rename(env.DB, a, "susp-name", "susp-new", now()))).toBe("suspended");
    await reserve(env.DB, a, "a-owned", now());
    expect(await code(rename(env.DB, b, "a-owned", "stolen", now()))).toBe("not_found");
    expect(await code(remove(env.DB, b, "a-owned", now()))).toBe("not_found");
    expect(await code(setDefault(env.DB, b, "a-owned"))).toBe("not_found");
  });

  it("refuses a taken target", async () => {
    const [a, b] = [await user(), await user()];
    await reserve(env.DB, a, "taken-target", now());
    await reserve(env.DB, b, "b-rename", now());
    expect(await code(rename(env.DB, b, "b-rename", "taken-target", now()))).toBe("name_taken");
  });
});

describe("remove, holds and defaults", () => {
  it("holds a removed name 12 months for its owner only, and reclaim is explicit", async () => {
    const [a, b] = [await user(), await user()];
    await reserve(env.DB, a, "hold-me", now());
    await remove(env.DB, a, "hold-me", now());
    const hold = await env.DB.prepare("SELECT held_for, until FROM released_names WHERE name = 'hold-me'").first<{ held_for: string; until: number }>();
    expect(hold!.held_for).toBe(a);
    expect(hold!.until - now()).toBeGreaterThanOrEqual(HOLD_SECONDS - 5);
    expect(await code(reserve(env.DB, b, "hold-me", now()))).toBe("name_on_hold");
    expect(await code(reserve(env.DB, a, "hold-me", now()))).toBe("ok"); // explicit reclaim by the owner
    expect(await heldFor(env.DB, "hold-me", a, now())).toBeNull();
  });

  it("promotes the oldest remaining name when the default is removed", async () => {
    const u = await user();
    await reserve(env.DB, u, "def-a", now() - 10);
    await reserve(env.DB, u, "def-b", now() - 5);
    await reserve(env.DB, u, "def-c", now());
    await remove(env.DB, u, "def-a", now());
    const names = await listNames(env.DB, u);
    expect(names.filter((n) => n.is_default === 1).map((n) => n.name)).toEqual(["def-b"]);
    await setDefault(env.DB, u, "def-c");
    expect((await listNames(env.DB, u)).filter((n) => n.is_default === 1).map((n) => n.name)).toEqual(["def-c"]);
  });

  it("rm then add works within the quota", async () => {
    const u = await user(1);
    await reserve(env.DB, u, "only-one", now());
    expect(await code(reserve(env.DB, u, "second-try", now()))).toBe("quota_exceeded");
    await remove(env.DB, u, "only-one", now());
    expect(await code(reserve(env.DB, u, "second-try", now()))).toBe("ok");
  });
});
