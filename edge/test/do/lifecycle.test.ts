import { env } from "cloudflare:workers";
import { evictDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { FrameType } from "../../src/protocol/frames";
import type { TunnelObject } from "../../src/tunnel-object";
import { FakeAgent, newInstance, sleep, visit } from "./fake-agent";

const stub = (name: string) =>
  env.TUNNEL.get(env.TUNNEL.idFromName(name)) as unknown as DurableObjectStub<TunnelObject>;

async function roundTrip(agent: FakeAgent, name: string, path = "/rt"): Promise<number> {
  const resP = visit(name, path);
  const req = await agent.request(path);
  await agent.respond(req.id, 200, "ok");
  const res = await resP;
  expect(await res.text()).toBe("ok");
  return req.id;
}

describe("replace rules (PROTOCOL §3.2)", () => {
  it("same instance replaces silently", async () => {
    const instance = newInstance();
    const a = await FakeAgent.connect("same1", { instance });
    const b = await FakeAgent.connect("same1", { instance });
    expect((await a.waitClosed()).code).toBe(1000);
    expect(a.frames.some((f) => f.type === FrameType.GOAWAY)).toBe(false);
    expect((await b.next((f) => f.type === FrameType.READY)).json).toMatchObject({ epoch: 2 });
    await roundTrip(b, "same1");
    b.close();
  });

  it("409 when another live instance holds the name", async () => {
    const a = await FakeAgent.connect("busy1");
    a.ping();
    const { agent, res } = await FakeAgent.open("busy1");
    expect(agent).toBeUndefined();
    expect(res.status).toBe(409);
    expect(await res.json()).toMatchObject({ error: { code: "name_in_use" } });
    await roundTrip(a, "busy1"); // the holder is untouched
    a.close();
  });

  it("force replaces with GOAWAY replaced", async () => {
    const a = await FakeAgent.connect("force1");
    const b = await FakeAgent.connect("force1", { force: true });
    expect((await a.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "replaced" });
    await a.waitClosed();
    await roundTrip(b, "force1");
    b.close();
  });

  it("replaces a dead socket (no pong for DEAD_SOCKET_SECONDS)", async () => {
    const a = await FakeAgent.connect("dead1"); // never pings
    await sleep(2_200);
    const b = await FakeAgent.connect("dead1");
    await a.waitClosed();
    await roundTrip(b, "dead1");
    b.close();
  });

  it("a stale-epoch close does not touch the new agent's streams", async () => {
    const instance = newInstance();
    const a = await FakeAgent.connect("stale1", { instance });
    const aResP = visit("stale1", "/old");
    await a.request("/old");
    const b = await FakeAgent.connect("stale1", { instance });
    expect((await aResP).status).toBe(502); // A's in-flight stream failed with A's epoch
    const bResP = visit("stale1", "/new");
    const req = await b.request("/new");
    await a.waitClosed();
    await sleep(50); // let A's close event run in the DO
    await b.respond(req.id, 200, "still fine");
    expect(await (await bResP).text()).toBe("still fine");
    b.close();
  });
});

describe("DRAIN", () => {
  it("stops new streams, finishes in-flight ones, and lets a new agent take over", async () => {
    const a = await FakeAgent.connect("drain1");
    const inflight = visit("drain1", "/slow");
    const req = await a.request("/slow");
    a.send(new Uint8Array([0x04, 0, 0, 0, 0]));
    await sleep(50);
    const blocked = await visit("drain1", "/new");
    expect(blocked.status).toBe(502);
    expect(await blocked.text()).toContain("restarting");
    await a.respond(req.id, 200, "finished");
    expect(await (await inflight).text()).toBe("finished");
    const b = await FakeAgent.connect("drain1"); // different instance, no --force needed
    await roundTrip(b, "drain1");
    a.close();
    b.close();
  });
});

describe("timeouts and caps", () => {
  it("head timeout → 504 and RESET head_timeout", async () => {
    const a = await FakeAgent.connect("headto");
    const t0 = Date.now();
    const res = await visit("headto", "/hang");
    expect(res.status).toBe(504);
    expect(Date.now() - t0).toBeGreaterThanOrEqual(2_900);
    const reset = await a.next((f) => f.type === FrameType.RESET);
    expect(reset.json).toMatchObject({ code: "head_timeout" });
    a.close();
  });

  it("credit timeout → 504 and RESET credit_timeout", async () => {
    const a = await FakeAgent.connect("creditto");
    a.pauseRequestCredit = true;
    const res = await visit("creditto", "/up", { method: "POST", body: new Uint8Array(3 * 1024 * 1024) });
    expect(res.status).toBe(504);
    expect((await a.next((f) => f.type === FrameType.RESET)).json).toMatchObject({ code: "credit_timeout" });
    const req = [...a.reqs.values()][0]!;
    const sent = req.chunks.reduce((n, c) => n + c.byteLength, 0);
    expect(sent).toBe(1024 * 1024); // exactly one stream window, never more
    a.close();
  });

  it("caps concurrent streams at 128 → 503", async () => {
    const a = await FakeAgent.connect("cap128");
    const pending = Array.from({ length: 128 }, (_, i) => visit("cap128", `/p${i}`));
    await a.next(() => a.reqs.size >= 128, 5_000);
    const over = await visit("cap128", "/over");
    expect(over.status).toBe(503);
    for (const id of a.reqs.keys()) await a.respond(id, 200, "x");
    expect((await Promise.all(pending)).every((r) => r.status === 200)).toBe(true);
    a.close();
  });

  it("allows at most 8 long streams per name", async () => {
    const a = await FakeAgent.connect("long9");
    const streams = Array.from({ length: 9 }, (_, i) => visit("long9", `/sse${i}`));
    for (let i = 0; i < 9; i++) {
      const req = await a.request(`/sse${i}`);
      a.head(req.id, 200, [["content-type", "text/event-stream"]]);
    }
    await Promise.all(streams);
    await sleep(1_500);
    const resets = a.frames.filter((f) => f.type === FrameType.RESET);
    expect(resets.length).toBe(1);
    expect(resets[0]!.json).toMatchObject({ code: "stream_timeout" });
    a.close();
  });
});

describe("protocol violations", () => {
  it("drops frames for unknown streams without closing", async () => {
    const a = await FakeAgent.connect("unknown1");
    a.send(new Uint8Array([0x21, 0, 0, 0, 99, 1, 2, 3])); // RES_BODY on stream 99
    a.send(new Uint8Array([0x22, 0, 0, 0, 98])); // RES_END on stream 98
    await roundTrip(a, "unknown1");
    expect(a.closed).toBeNull();
    a.close();
  });

  it("closes 1002 on a malformed frame", async () => {
    const a = await FakeAgent.connect("garbage1");
    a.send(new Uint8Array([0xff, 0, 0, 0, 1]));
    expect((await a.waitClosed()).code).toBe(1002);
  });

  it("closes 1002 when the agent sends an edge-only frame type", async () => {
    const a = await FakeAgent.connect("wrongdir");
    a.send(new Uint8Array([0x12, 0, 0, 0, 1])); // REQ_END is edge → agent only
    expect((await a.waitClosed()).code).toBe(1002);
  });
});

describe("RPC control plane", () => {
  it("status reports online/offline", async () => {
    expect(await stub("stat1").status()).toMatchObject({ state: "offline" });
    const a = await FakeAgent.connect("stat1");
    expect(await stub("stat1").status()).toMatchObject({ state: "online", epoch: 1, client: "tuzy/test" });
    a.close();
  });

  it("goaway sends the reason and new name, then closes", async () => {
    const a = await FakeAgent.connect("rename1");
    expect(await stub("rename1").goaway("renamed", { newName: "rename2" })).toBe(true);
    expect((await a.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "renamed", new_name: "rename2" });
    await a.waitClosed();
  });

  it("suspension blocks visitors and connects", async () => {
    const a = await FakeAgent.connect("susp1");
    await stub("susp1").setSuspended(true);
    expect((await a.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "suspended" });
    expect((await visit("susp1", "/")).status).toBe(451);
    const { res } = await FakeAgent.open("susp1");
    expect(res.status).toBe(403);
    await stub("susp1").setSuspended(false);
    const b = await FakeAgent.connect("susp1");
    await roundTrip(b, "susp1");
    b.close();
  });
});

describe("hibernation safety", () => {
  it("rebuilds from tags + attachments after eviction; stream ids keep increasing", async () => {
    const a = await FakeAgent.connect("evict1");
    const first = await roundTrip(a, "evict1", "/one");
    await evictDurableObject(stub("evict1"));
    const second = await roundTrip(a, "evict1", "/two");
    expect(second).toBeGreaterThan(first);
    a.close();
  });
});
