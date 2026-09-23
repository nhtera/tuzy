import { env } from "cloudflare:workers";
import { evictDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { encodeFrame, encodeJsonFrame, FrameType } from "../../src/protocol/frames";
import type { TunnelObject } from "../../src/tunnel-object";
import { FakeAgent, sleep, visit } from "./fake-agent";

interface Visitor {
  ws: WebSocket;
  messages: (string | ArrayBuffer)[];
  closed: Promise<{ code: number; reason: string }>;
  next(n?: number): Promise<void>;
}

/** Opens a visitor WebSocket and completes the agent side of the upgrade (RES_HEAD 101). */
async function openVisitor(agent: FakeAgent, name: string, path = "/ws"): Promise<{ v: Visitor; id: number }> {
  const resP = visit(name, path, { headers: { upgrade: "websocket", "sec-websocket-protocol": "chat" } });
  const req = await agent.request(path);
  expect(req.head.kind).toBe("ws");
  agent.head(req.id, 101, [["sec-websocket-protocol", "chat"]]);
  const res = await resP;
  expect(res.status).toBe(101);
  expect(res.headers.get("sec-websocket-protocol")).toBe("chat");
  const ws = res.webSocket!;
  ws.accept();
  (ws as unknown as { binaryType: string }).binaryType = "arraybuffer";
  const messages: (string | ArrayBuffer)[] = [];
  let wake: (() => void) | null = null;
  ws.addEventListener("message", (e) => {
    messages.push(e.data as string | ArrayBuffer);
    wake?.();
  });
  const closed = new Promise<{ code: number; reason: string }>((r) => ws.addEventListener("close", (e) => r({ code: e.code, reason: e.reason })));
  const v: Visitor = {
    ws,
    messages,
    closed,
    async next(n = 1) {
      while (messages.length < n) await new Promise<void>((r) => (wake = r));
    },
  };
  return { v, id: req.id };
}

describe("visitor WebSocket relay", () => {
  it("echoes text and binary both ways with per-message ACKs", async () => {
    const agent = await FakeAgent.connect("echo1");
    const { v, id } = await openVisitor(agent, "echo1");
    v.ws.send("hello");
    const inbound = await agent.next((f) => f.type === FrameType.WS_TEXT && f.streamId === id);
    expect(new TextDecoder().decode(inbound.payload)).toBe("hello");
    agent.wsText(id, "hello back");
    agent.send(encodeFrame(FrameType.WS_BINARY, id, new Uint8Array([1, 2, 3])));
    await v.next(2);
    expect(v.messages[0]).toBe("hello back");
    expect(new Uint8Array(v.messages[1] as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    const acked = () => agent.frames.filter((f) => f.type === FrameType.ACK && f.streamId === id).reduce((n, f) => n + f.value!, 0);
    await agent.next(() => acked() >= 2);
    await sleep(50);
    expect(acked()).toBe(2); // exactly one ACK per delivered message
    agent.close();
  });

  it("holds visitor messages beyond 8 unacked, flushes on ACK, and closes 1008 on overflow", async () => {
    const agent = await FakeAgent.connect("credit1");
    agent.autoAck = false;
    const { v, id } = await openVisitor(agent, "credit1");
    for (let i = 0; i < 12; i++) v.ws.send(`m${i}`);
    await sleep(200);
    expect(agent.wsMessages(id)).toHaveLength(8);
    agent.wsAck(id, 4);
    await agent.next(() => agent.wsMessages(id).length === 12);
    expect(agent.wsMessages(id)).toEqual(Array.from({ length: 12 }, (_, i) => `m${i}`));

    // Now 8 outstanding again; 8 more buffer, the 9th overflows.
    for (let i = 12; i < 12 + 9; i++) v.ws.send(`m${i}`);
    expect((await v.closed).code).toBe(1008);
    expect((await agent.next((f) => f.type === FrameType.WS_CLOSE && f.streamId === id)).json).toMatchObject({ code: 1008 });
    agent.close();
  });

  it("propagates a visitor close to the agent", async () => {
    const agent = await FakeAgent.connect("vclose1");
    const { v, id } = await openVisitor(agent, "vclose1");
    v.ws.close(4000, "bye from visitor");
    const f = await agent.next((f) => f.type === FrameType.WS_CLOSE && f.streamId === id);
    expect(f.json).toMatchObject({ code: 4000, reason: "bye from visitor" });
    agent.close();
  });

  it("propagates an agent WS_CLOSE to the visitor and answers it", async () => {
    const agent = await FakeAgent.connect("aclose1");
    const { v, id } = await openVisitor(agent, "aclose1");
    agent.send(encodeJsonFrame(FrameType.WS_CLOSE, id, { code: 4001, reason: "app closed" }));
    expect(await v.closed).toMatchObject({ code: 4001, reason: "app closed" });
    expect((await agent.next((f) => f.type === FrameType.WS_CLOSE && f.streamId === id)).json).toMatchObject({ code: 4001 });
    agent.close();
  });

  it("closes the visitor with 1007 on invalid UTF-8 from the agent, keeping the tunnel", async () => {
    const agent = await FakeAgent.connect("utf1");
    const { v, id } = await openVisitor(agent, "utf1");
    agent.send(encodeFrame(FrameType.WS_TEXT, id, new Uint8Array([0xc3, 0x28])));
    expect((await v.closed).code).toBe(1007);
    expect(agent.closed).toBeNull();
    agent.close();
  });

  it("closes visitors with 1012 when the agent disconnects", async () => {
    const agent = await FakeAgent.connect("agone1");
    const { v } = await openVisitor(agent, "agone1");
    agent.close();
    expect((await v.closed).code).toBe(1012);
  });

  it("relays a rejected upgrade as a normal response", async () => {
    const agent = await FakeAgent.connect("reject1");
    const resP = visit("reject1", "/ws", { headers: { upgrade: "websocket" } });
    const req = await agent.request("/ws");
    await agent.respond(req.id, 403, "no sockets for you");
    const res = await resP;
    expect(res.status).toBe(403);
    expect(await res.text()).toBe("no sockets for you");
    agent.close();
  });

  it("keeps relaying after the DO is evicted (state in tags + attachments)", async () => {
    const agent = await FakeAgent.connect("wsevict1");
    const { v, id } = await openVisitor(agent, "wsevict1");
    v.ws.send("before");
    await agent.next(() => agent.wsMessages(id).length === 1);
    await evictDurableObject(env.TUNNEL.get(env.TUNNEL.idFromName("wsevict1")) as unknown as DurableObjectStub<TunnelObject>);
    v.ws.send("after");
    await agent.next(() => agent.wsMessages(id).length === 2);
    agent.wsText(id, "reply");
    await v.next(1);
    expect(v.messages[0]).toBe("reply");
    agent.close();
  });
});
