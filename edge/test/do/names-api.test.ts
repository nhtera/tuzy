import { env } from "cloudflare:workers";
import { listDurableObjectIds } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { FrameType } from "../../src/protocol/frames";
import type { ConnectMeta } from "../../src/tunnel-object";
import { api, lastCode, login } from "./api-helpers";
import { FakeAgent } from "./fake-agent";

let n = 0;
const email = () => `nm${n++}.${Date.now()}@example.com`;

describe("names API", () => {
  it("add → list (default first) → set default → remove, with quota and holds", async () => {
    const { token } = await login(email());
    expect((await api("/names", { token, body: { name: "my-shop" } })).status).toBe(201);
    const auto = await api("/names", { token, body: { auto: true } });
    expect(auto.status).toBe(201);
    expect(auto.json.name).toMatch(/^[a-z]+-[a-z]+-\d{2}$/);
    let list = await api("/names", { token });
    expect(list.json).toMatchObject({ used: 2, limit: 10 });
    expect(list.json.names.find((x: { default: boolean }) => x.default).name).toBe("my-shop");
    expect(list.json.names.find((x: { name: string }) => x.name === "my-shop").url).toBe("https://my-shop.tuzy.dev");
    expect((await api(`/names/${auto.json.name}`, { method: "PATCH", token, body: { default: true } })).json.default).toBe(true);
    expect((await api("/names/my-shop", { method: "DELETE", token })).status).toBe(204);
    list = await api("/names", { token });
    expect(list.json.used).toBe(1);
    expect(list.json.held.map((h: { name: string }) => h.name)).toEqual(["my-shop"]);
  });

  it("validates names and maps repo errors", async () => {
    const a = await login(email());
    const b = await login(email());
    expect((await api("/names", { token: a.token, body: { name: "ab" } })).json.error.code).toBe("invalid_name");
    expect((await api("/names", { token: a.token, body: { name: "www" } })).json.error.code).toBe("reserved_name");
    expect((await api("/names", { token: a.token, body: { name: "paypal-help" } })).json.error.code).toBe("blocked_name");
    await api("/names", { token: a.token, body: { name: "owned-by-a" } });
    const taken = await api("/names", { token: b.token, body: { name: "owned-by-a" } });
    expect(taken.status).toBe(409);
    expect(taken.json.error.code).toBe("name_taken");
    expect((await api("/names/owned-by-a", { method: "DELETE", token: b.token })).status).toBe(404);
    expect((await api("/names/owned-by-a", { method: "PATCH", token: b.token, body: { rename: "mine-now" } })).status).toBe(404);
  });

  it("suggests a free name and lists without waking any Durable Object", async () => {
    const { token } = await login(email());
    const s = await api("/names/suggest", { token });
    expect(s.json.name).toMatch(/^[a-z]+-[a-z]+-\d{2}$/);
    await api("/names", { token, body: { name: "quiet-name" } });
    const before = (await listDurableObjectIds(env.TUNNEL)).length;
    await api("/names", { token });
    expect((await listDurableObjectIds(env.TUNNEL)).length).toBe(before);
  });

  it("live state reports online / never connected", async () => {
    const { token } = await login(email());
    await api("/names", { token, body: { name: "live-one" } });
    await api("/names", { token, body: { name: "live-two" } });
    const agent = await FakeAgent.connect("live-one", { token });
    await new Promise((r) => setTimeout(r, 100));
    const list = await api("/names?live=1", { token });
    const state = Object.fromEntries(list.json.names.map((x: { name: string; state: string }) => [x.name, x.state]));
    expect(state).toEqual({ "live-one": "online", "live-two": "never_connected" });
    agent.close();
  });
});

describe("connect authorization", () => {
  it("404 for unreserved and others' names, 403 when suspended", async () => {
    const a = await login(email());
    const b = await login(email());
    expect((await FakeAgent.open("nobody-owns", { token: a.token, reserve: false })).res.status).toBe(404);
    await api("/names", { token: b.token, body: { name: "b-private" } });
    const other = await FakeAgent.open("b-private", { token: a.token, reserve: false });
    expect(other.res.status).toBe(404);
    expect(((await other.res.json()) as { error: { code: string } }).error.code).toBe("name_not_reserved");
    await api("/names", { token: a.token, body: { name: "a-suspended" } });
    await env.DB.prepare("UPDATE reservations SET status = 'suspended' WHERE name = 'a-suspended'").run();
    expect((await FakeAgent.open("a-suspended", { token: a.token, reserve: false })).res.status).toBe(403);
  });

  it("renaming a live tunnel moves the agent; the old name then answers 410 with the new name", async () => {
    const { token } = await login(email());
    await api("/names", { token, body: { name: "before-name" } });
    const agent = await FakeAgent.connect("before-name", { token, reserve: false });
    const r = await api("/names/before-name", { method: "PATCH", token, body: { rename: "after-name" } });
    expect(r.status).toBe(200);
    expect((await agent.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "renamed", new_name: "after-name" });
    const moved = await FakeAgent.connect("after-name", { token, reserve: false });
    moved.close();
    const old = await FakeAgent.open("before-name", { token, reserve: false });
    expect(old.res.status).toBe(410);
    expect(((await old.res.json()) as { error: unknown }).error).toMatchObject({ code: "name_released", new_name: "after-name" });
  });

  it("the DO re-validates ownership at connect (stale Worker check is refused)", async () => {
    const a = await login(email());
    await api("/names", { token: a.token, body: { name: "raced-name" } });
    await api("/names/raced-name", { method: "DELETE", token: a.token }); // deleted after the Worker checked
    const stub = env.TUNNEL.get(env.TUNNEL.idFromName("raced-name"));
    const meta: ConnectMeta = { name: "raced-name", url: "https://raced-name.tuzy.dev", instanceId: "i".repeat(16), force: false, userId: a.userId, tokenId: "tok_x", gen: "stale" };
    const res = await stub.fetch(
      new Request("https://connect.internal/", { headers: { upgrade: "websocket", "x-tuzy-meta": JSON.stringify(meta) } }),
    );
    expect(res.status).toBe(410);
  });

  it("removing a live name closes the tunnel (GOAWAY deleted) and holds it for its owner", async () => {
    const a = await login(email());
    const b = await login(email());
    await api("/names", { token: a.token, body: { name: "doomed-name" } });
    const agent = await FakeAgent.connect("doomed-name", { token: a.token, reserve: false });
    expect((await api("/names/doomed-name", { method: "DELETE", token: a.token })).status).toBe(204);
    expect((await agent.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "deleted" });
    expect((await api("/names", { token: b.token, body: { name: "doomed-name" } })).json.error.code).toBe("name_on_hold");
    expect((await api("/names", { token: a.token, body: { name: "doomed-name" } })).status).toBe(201);
  });

  it("account deletion holds the user's names for 12 months", async () => {
    const e = email();
    const a = await login(e);
    const b = await login(email());
    await api("/names", { token: a.token, body: { name: "legacy-name" } });
    const start = await api("/account/delete/start", { token: a.token, body: {} });
    expect((await api("/account/delete/confirm", { token: a.token, body: { login_id: start.json.login_id, code: lastCode(e) } })).status).toBe(200);
    expect((await api("/names", { token: b.token, body: { name: "legacy-name" } })).json.error.code).toBe("name_on_hold");
  });

  it("a new reservation incarnation drops sockets admitted under the old gen", async () => {
    const { token } = await login(email());
    expect((await api("/names", { token, body: { name: "gen-swap" } })).status).toBe(201);
    const first = await FakeAgent.connect("gen-swap", { token, reserve: false });
    // Simulate a re-created reservation whose GOAWAY never arrived.
    await env.DB.prepare("UPDATE reservations SET gen = 'feedfacefeedface' WHERE name = 'gen-swap'").run();
    const second = await FakeAgent.connect("gen-swap", { token, reserve: false }); // no --force needed
    await first.waitClosed(5000);
    expect(first.frames.some((f) => f.type === FrameType.GOAWAY)).toBe(true);
    expect(second.closed).toBeNull();
    second.close();
  });
});
