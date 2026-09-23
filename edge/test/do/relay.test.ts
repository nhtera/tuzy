import { env } from "cloudflare:workers";
import { listDurableObjectIds } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { FrameType } from "../../src/protocol/frames";
import { bytes, FakeAgent, sha256, visit } from "./fake-agent";

describe("connect", () => {
  it("HELLO → READY with the public URL and epoch", async () => {
    const { agent } = await FakeAgent.open("ready1");
    const ready = await agent!.hello();
    expect(ready.type).toBe(FrameType.READY);
    expect(ready.json).toEqual({ name: "ready1", url: "https://ready1.tuzy.dev", epoch: 1 });
    agent!.close();
  });

  it("closes 1002 when the first frame is not HELLO", async () => {
    const { agent } = await FakeAgent.open("nohello");
    agent!.send(new Uint8Array([0x04, 0, 0, 0, 0])); // DRAIN before HELLO
    expect((await agent!.waitClosed()).code).toBe(1002);
  });

  it("answers an unknown proto with GOAWAY upgrade_required", async () => {
    const { agent } = await FakeAgent.open("oldproto");
    agent!.send(
      new Uint8Array([0x01, 0, 0, 0, 0, ...new TextEncoder().encode(JSON.stringify({ proto: 9, client: "x", instance_id: agent!.instance }))]),
    );
    const f = await agent!.next((f) => f.type === FrameType.GOAWAY);
    expect(f.json).toMatchObject({ reason: "upgrade_required" });
  });

  it("closes 1003 on a text message other than the heartbeat", async () => {
    const agent = await FakeAgent.connect("texty");
    agent.ws.send("hello");
    expect((await agent.waitClosed()).code).toBe(1003);
  });

  it("answers the heartbeat via auto-response", async () => {
    const agent = await FakeAgent.connect("pinger");
    const pong = new Promise<string>((r) => agent.ws.addEventListener("message", (e) => typeof e.data === "string" && r(e.data)));
    agent.ping();
    expect(await pong).toBe("tuzy-pong");
    agent.close();
  });

  it("does not create a Durable Object for a never-connected name", async () => {
    const before = (await listDurableObjectIds(env.TUNNEL)).length;
    expect((await visit("neverseen", "/")).status).toBe(502);
    expect((await listDurableObjectIds(env.TUNNEL)).length).toBe(before);
  });
});

describe("HTTP relay", () => {
  it("relays a GET with filtered headers and meta", async () => {
    const agent = await FakeAgent.connect("get1");
    const resP = visit("get1", "/hello?x=1", {
      headers: { "cf-connecting-ip": "198.51.100.7", "x-tuzy-remote-ip": "6.6.6.6", cookie: "a=1; tuzy_warn=1" },
    });
    const req = await agent.request("/hello");
    expect(req.head).toMatchObject({ method: "GET", path: "/hello?x=1", kind: "http", remote_ip: "198.51.100.7" });
    const h = Object.fromEntries(req.head.headers);
    expect(h.host).toBe("get1.tuzy.dev");
    expect(h.cookie).toBe("a=1");
    expect(h["x-forwarded-for"]).toBe("198.51.100.7");
    expect(Object.keys(h).some((k) => k.startsWith("cf-") || k.startsWith("x-tuzy-"))).toBe(false);

    await agent.respond(req.id, 201, "hi there", [
      ["content-type", "text/plain"],
      ["set-cookie", "tuzy_evil=1"],
      ["set-cookie", "ok=1"],
    ]);
    const res = await resP;
    expect(res.status).toBe(201);
    expect(await res.text()).toBe("hi there");
    expect(res.headers.getSetCookie()).toEqual(["ok=1"]);
    agent.close();
  });

  it("sends every path (including control-looking ones) to the app", async () => {
    const agent = await FakeAgent.connect("paths");
    for (const path of ["/connect", "/goaway", "/suspend", "/status", "/api/v1/connect"]) {
      const resP = visit("paths", path, { method: "POST", body: "x" });
      const req = await agent.request(path);
      await agent.body(req.id);
      await agent.respond(req.id, 200, `app:${path}`);
      expect(await (await resP).text()).toBe(`app:${path}`);
    }
    agent.close();
  });

  it("streams a 20 MB upload with windowing and checksum match", async () => {
    const agent = await FakeAgent.connect("upload");
    const data = bytes(20 * 1024 * 1024, 7);
    const resP = visit("upload", "/up", { method: "POST", body: data });
    const req = await agent.request("/up");
    agent.head(req.id, 200); // head first: the body takes longer than the test head timeout
    const got = await agent.body(req.id);
    expect(got.byteLength).toBe(data.byteLength);
    await agent.write(req.id, new TextEncoder().encode(await sha256(got)));
    agent.end(req.id);
    expect(await (await resP).text()).toBe(await sha256(data));
    agent.close();
  });

  it("streams a 50 MB download with checksum match and coalesced WINDOW", async () => {
    const agent = await FakeAgent.connect("download");
    const data = bytes(50 * 1024 * 1024, 11);
    const resP = visit("download", "/big");
    const req = await agent.request("/big");
    const writeP = agent.respond(req.id, 200, data, [["content-length", String(data.byteLength)]]);
    const res = await resP;
    const got = new Uint8Array(await res.arrayBuffer());
    await writeP;
    expect(got.byteLength).toBe(data.byteLength);
    expect(await sha256(got)).toBe(await sha256(data));
    const windows = agent.frames.filter((f) => f.type === FrameType.WINDOW && f.streamId === req.id).length;
    expect(windows).toBeLessThanOrEqual(Math.ceil(data.byteLength / (256 * 1024)) + 1);
    agent.close();
  });

  it("returns an early 413 promptly while the upload is still running", async () => {
    const agent = await FakeAgent.connect("early");
    agent.pauseRequestCredit = true; // the "app" reads nothing
    let pushed = 0;
    const body = new ReadableStream<Uint8Array>({
      async pull(ctrl) {
        if (pushed >= 64 * 1024 * 1024) return ctrl.close();
        pushed += 256 * 1024;
        ctrl.enqueue(new Uint8Array(256 * 1024));
      },
    });
    const resP = visit("early", "/upload", { method: "POST", body });
    const req = await agent.request("/upload");
    await agent.respond(req.id, 413, "too large");
    const res = await resP;
    expect(res.status).toBe(413);
    expect(await res.text()).toBe("too large");
    await agent.next((f) => f.type === FrameType.REQ_END && f.streamId === req.id);
    expect(pushed).toBeLessThan(64 * 1024 * 1024);
    agent.close();
  });

  it("delivers SSE chunks incrementally", async () => {
    const agent = await FakeAgent.connect("sse");
    const resP = visit("sse", "/events");
    const req = await agent.request("/events");
    agent.head(req.id, 200, [["content-type", "text/event-stream"]]);
    await agent.write(req.id, new TextEncoder().encode("data: one\n\n"));
    const res = await resP;
    const reader = res.body!.getReader();
    const first = await reader.read();
    expect(new TextDecoder().decode(first.value)).toBe("data: one\n\n");
    // Latency from the agent sending a chunk to the visitor receiving it (target < 200 ms).
    const t0 = Date.now();
    const secondP = reader.read();
    await agent.write(req.id, new TextEncoder().encode("data: two\n\n"));
    const second = await secondP;
    expect(Date.now() - t0).toBeLessThan(200);
    expect(new TextDecoder().decode(second.value)).toBe("data: two\n\n");
    agent.end(req.id);
    expect((await reader.read()).done).toBe(true);
    agent.close();
  });

  it("handles HEAD and 204 without a body", async () => {
    const agent = await FakeAgent.connect("nobody");
    const r1 = visit("nobody", "/h", { method: "HEAD" });
    const q1 = await agent.request("/h");
    await agent.respond(q1.id, 200, "", [["content-length", "42"]]);
    expect((await r1).status).toBe(200);
    const r2 = visit("nobody", "/n");
    const q2 = await agent.request("/n");
    await agent.respond(q2.id, 204);
    const res = await r2;
    expect(res.status).toBe(204);
    expect(await res.text()).toBe("");
    agent.close();
  });

  it("returns 502 when the agent resets before the head", async () => {
    const agent = await FakeAgent.connect("reset1");
    const resP = visit("reset1", "/x");
    const req = await agent.request("/x");
    agent.send(new Uint8Array([0x41, 0, 0, 0, req.id, ...new TextEncoder().encode('{"code":"local_error","message":"refused"}')]));
    expect((await resP).status).toBe(502);
    agent.close();
  });

  it("serves the offline page after the agent disconnects", async () => {
    const agent = await FakeAgent.connect("gone");
    agent.close();
    await agent.waitClosed();
    await new Promise((r) => setTimeout(r, 50));
    const res = await visit("gone", "/");
    expect(res.status).toBe(502);
    expect(await res.text()).toContain("offline");
  });
});
