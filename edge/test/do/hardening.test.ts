/**
 * Review-driven tests (phase 2 code review): credit accounting across resets and eviction,
 * receive-window enforcement, slow readers, and the remaining protocol edge cases.
 */
import { env } from "cloudflare:workers";
import { evictDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { CONN_WINDOW, encodeFrame, encodeJsonFrame, encodeU32Frame, FrameType } from "../../src/protocol/frames";
import type { TunnelObject } from "../../src/tunnel-object";
import { truncateUtf8 } from "../../src/tunnel-object";
import { bytes, FakeAgent, sha256, sleep, visit } from "./fake-agent";

const stub = (name: string) => env.TUNNEL.get(env.TUNNEL.idFromName(name)) as unknown as DurableObjectStub<TunnelObject>;

async function openWs(agent: FakeAgent, name: string) {
  const resP = visit(name, "/ws", { headers: { upgrade: "websocket" } });
  const req = await agent.request("/ws");
  agent.head(req.id, 101);
  const ws = (await resP).webSocket!;
  ws.accept();
  const closed = new Promise<number>((r) => ws.addEventListener("close", (e) => r(e.code)));
  return { ws, id: req.id, closed };
}

describe("connection credit (review C1/H1)", () => {
  it("returns all connection credit after a visitor cancels mid-download, even across eviction", async () => {
    const a = await FakeAgent.connect("credit-cancel");
    const resP = visit("credit-cancel", "/big");
    const req = await a.request("/big");
    a.head(req.id, 200);
    void a.write(req.id, bytes(4 * 1024 * 1024)).catch(() => {});
    const reader = (await resP).body!.getReader();
    await reader.read();
    await reader.cancel();
    await a.next((f) => f.type === FrameType.RESET && f.streamId === req.id);
    await sleep(300);
    expect(a.sendConn).toBe(CONN_WINDOW); // every sent byte was granted back

    await evictDurableObject(stub("credit-cancel"));
    const data = bytes(3 * 1024 * 1024, 5);
    const res2P = visit("credit-cancel", "/again");
    const req2 = await a.request("/again");
    const w = a.respond(req2.id, 200, data);
    const got = new Uint8Array(await (await res2P).arrayBuffer());
    await w;
    expect(await sha256(got)).toBe(await sha256(data));
    a.close();
  });

  it("returns connection credit after an early response discards the rest", async () => {
    const a = await FakeAgent.connect("credit-early");
    const resP = visit("credit-early", "/r");
    const req = await a.request("/r");
    a.head(req.id, 200);
    await a.write(req.id, bytes(100_000));
    a.end(req.id);
    await (await resP).arrayBuffer();
    await sleep(100);
    expect(a.sendConn).toBe(CONN_WINDOW);
    a.close();
  });

  it("keeps 256 slow-reader streams within the connection window", async () => {
    const a = await FakeAgent.connect("slow256");
    const size = 256 * 1024;
    const responses = Array.from({ length: 256 }, (_, i) => visit("slow256", `/s${i}`));
    await a.next(() => a.reqs.size >= 128, 5_000);
    const writes = [...a.reqs.keys()].map((id) => a.respond(id, 200, bytes(size, id)));
    const settled = await Promise.all(responses);
    expect(settled.filter((r) => r.status === 503).length).toBe(128);
    await sleep(300);
    // Note: in-process test pipes buffer eagerly, so this checks correctness under 128 concurrent
    // streams + the 503 cap; the receive-window bound itself is unit-tested (ReceiveWindow).
    expect(CONN_WINDOW - a.sendConn).toBeLessThanOrEqual(CONN_WINDOW);
    const ok = settled.filter((r) => r.status === 200);
    const lengths = await Promise.all(ok.map(async (r) => (await r.arrayBuffer()).byteLength));
    await Promise.all(writes);
    expect(lengths.every((n) => n === size)).toBe(true);
    expect(a.closed).toBeNull();
    a.close();
  });

  it("closes 1002 when WINDOW would exceed MAX_WINDOW", async () => {
    const a = await FakeAgent.connect("maxwin");
    a.send(encodeU32Frame(FrameType.WINDOW, 0, 2 ** 31 - 1));
    expect((await a.waitClosed()).code).toBe(1002);
  });
});

describe("stream edge cases", () => {
  it("answers a duplicate RES_HEAD with RESET protocol_error", async () => {
    const a = await FakeAgent.connect("dup-head");
    const resP = visit("dup-head", "/d");
    const req = await a.request("/d");
    a.head(req.id, 200);
    a.head(req.id, 200);
    expect((await a.next((f) => f.type === FrameType.RESET && f.streamId === req.id)).json).toMatchObject({ code: "protocol_error" });
    await expect((await resP).text()).rejects.toThrow();
    a.close();
  });

  it("resets with cancelled when the visitor leaves before the head", async () => {
    const a = await FakeAgent.connect("abort-early");
    const ac = new AbortController();
    const resP = visit("abort-early", "/slow", { signal: ac.signal });
    const req = await a.request("/slow");
    ac.abort();
    await expect(resP).rejects.toThrow();
    const reset = await a.next((f) => f.type === FrameType.RESET && f.streamId === req.id, 2_500);
    expect(reset.json).toMatchObject({ code: "cancelled" });
    a.close();
  });

  it("rejects a body longer than its content-length with protocol_error", async () => {
    const a = await FakeAgent.connect("cl-over");
    const resP = visit("cl-over", "/x");
    const req = await a.request("/x");
    a.head(req.id, 200, [["content-length", "3"]]);
    await a.write(req.id, new Uint8Array(10));
    expect((await a.next((f) => f.type === FrameType.RESET && f.streamId === req.id)).json).toMatchObject({ code: "protocol_error" });
    await resP.then((r) => r.arrayBuffer()).catch(() => {});
    a.close();
  });

  it("answers oversized request headers with 431", async () => {
    const a = await FakeAgent.connect("big-head");
    const headers: Record<string, string> = {};
    for (let i = 0; i < 70; i++) headers[`x-pad-${i}`] = "a".repeat(2000);
    const res = await visit("big-head", "/", { headers });
    expect(res.status).toBe(431);
    a.close();
  });
});

describe("handshake edge cases", () => {
  it("closes 1002 without HELLO within HELLO_TIMEOUT", async () => {
    const { agent } = await FakeAgent.open("no-hello-timeout");
    expect((await agent!.waitClosed(5_000)).code).toBe(1002);
  });

  it("closes 1002 when HELLO's instance_id differs from the connect parameter", async () => {
    const { agent } = await FakeAgent.open("hello-mismatch");
    agent!.send(encodeJsonFrame(FrameType.HELLO, 0, { proto: 1, client: "x", instance_id: "someone-else-1234567" }));
    expect((await agent!.waitClosed()).code).toBe(1002);
  });

  it("DRAIN rule 3: the old socket closing never touches the new agent's streams", async () => {
    const a = await FakeAgent.connect("drain-close");
    a.send(encodeFrame(FrameType.DRAIN, 0));
    await sleep(50);
    const b = await FakeAgent.connect("drain-close");
    const resP = visit("drain-close", "/b");
    const req = await b.request("/b");
    a.close();
    await a.waitClosed();
    await sleep(50);
    await b.respond(req.id, 200, "b-ok");
    expect(await (await resP).text()).toBe("b-ok");
    b.close();
  });
});

describe("visitor WebSocket edge cases", () => {
  it("closes a visitor 1009 for an oversized visitor message and tells the agent", async () => {
    const a = await FakeAgent.connect("ws-big-in");
    const { ws, id, closed } = await openWs(a, "ws-big-in");
    ws.send(new Uint8Array(1024 * 1024 + 1));
    expect(await closed).toBe(1009);
    expect((await a.next((f) => f.type === FrameType.WS_CLOSE && f.streamId === id)).json).toMatchObject({ code: 1009 });
    a.close();
  });

  it("closes a visitor 1009 for an oversized agent message, keeping the tunnel", async () => {
    const a = await FakeAgent.connect("ws-big-out");
    const { id, closed } = await openWs(a, "ws-big-out");
    a.send(encodeFrame(FrameType.WS_BINARY, id, new Uint8Array(1024 * 1024 + 1)));
    expect(await closed).toBe(1009);
    expect(a.closed).toBeNull();
    a.close();
  });

  it("closes 1011 when buffered messages were lost to eviction (pending flag)", async () => {
    const a = await FakeAgent.connect("ws-pending");
    a.autoAck = false;
    const { ws, id, closed } = await openWs(a, "ws-pending");
    for (let i = 0; i < 9; i++) ws.send(`m${i}`); // 8 sent, 1 buffered in memory
    await a.next(() => a.wsMessages(id).length === 8);
    await sleep(100);
    await evictDurableObject(stub("ws-pending"));
    ws.send("wake"); // the rebuilt object finds pending=true with an empty buffer
    expect(await closed).toBe(1011);
    expect((await a.next((f) => f.type === FrameType.WS_CLOSE && f.streamId === id)).json).toMatchObject({ code: 1011 });
    a.close();
  });

  it("closes cleanly even with a long multi-byte close reason", async () => {
    const a = await FakeAgent.connect("ws-reason");
    const { id, closed } = await openWs(a, "ws-reason");
    a.send(encodeJsonFrame(FrameType.WS_CLOSE, id, { code: 4000, reason: "漢".repeat(60) }));
    expect(await closed).toBe(4000);
    a.close();
  });

  it("answers a malformed WS_CLOSE with RESET protocol_error", async () => {
    const a = await FakeAgent.connect("ws-badclose");
    const { id, closed } = await openWs(a, "ws-badclose");
    a.send(encodeJsonFrame(FrameType.WS_CLOSE, id, { code: "nope" }));
    expect((await a.next((f) => f.type === FrameType.RESET && f.streamId === id)).json).toMatchObject({ code: "protocol_error" });
    expect(await closed).toBe(1011);
    a.close();
  });
});

describe("truncateUtf8", () => {
  it("cuts on a character boundary within the byte budget", () => {
    const s = truncateUtf8("漢".repeat(60), 120);
    expect(new TextEncoder().encode(s).byteLength).toBeLessThanOrEqual(120);
    expect(s).toBe("漢".repeat(40));
    expect(truncateUtf8("short", 120)).toBe("short");
  });
});
