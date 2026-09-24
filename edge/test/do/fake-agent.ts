/**
 * Test double for the Go agent: speaks wire protocol v1 over a real WebSocket to the Worker.
 * Tracks send credit (stream + connection) so response bodies respect the edge's windows, and
 * grants request-body credit as the "local app" consumes (immediately, unless paused).
 */
import { env, exports } from "cloudflare:workers";
import { hashToken } from "../../src/lib/tokens";
import {
  CONN_WINDOW,
  decodeFrame,
  encodeFrame,
  encodeJsonFrame,
  encodeTextFrame,
  encodeU32Frame,
  FrameType,
  MAX_BODY_CHUNK,
  payloadText,
  STREAM_WINDOW,
  type Frame,
} from "../../src/protocol/frames";

import { TEST_TOKEN as TOKEN } from "../setup";

export { TOKEN };
export const BASE = "tuzy.dev";

let instanceSeq = 0;
export const newInstance = () => `inst-${Date.now().toString(36)}-${(instanceSeq++).toString().padStart(6, "0")}`;

export interface ReqState {
  id: number;
  head: { method: string; path: string; headers: [string, string][]; remote_ip: string; kind: "http" | "ws" };
  chunks: Uint8Array[];
  ended: boolean;
  reset?: { code: string };
}

type FrameWaiter = { pred: (f: Frame) => boolean; resolve: (f: Frame) => void };

export class FakeAgent {
  readonly frames: Frame[] = [];
  readonly reqs = new Map<number, ReqState>();
  readonly sendStream = new Map<number, number>();
  sendConn = CONN_WINDOW;
  closed: { code: number; reason: string } | null = null;
  /** When true, REQ_BODY is not acknowledged with WINDOW (simulates a slow local app). */
  pauseRequestCredit = false;
  /** Auto-ACK visitor WS messages (off to test edge buffering). */
  autoAck = true;
  private waiters: FrameWaiter[] = [];
  private creditWakers: (() => void)[] = [];
  private closeWaiters: (() => void)[] = [];
  private heartbeat?: ReturnType<typeof setInterval>;
  onRequest?: (req: ReqState) => void;

  private constructor(
    readonly ws: WebSocket,
    readonly instance: string,
  ) {
    ws.addEventListener("message", (e) => {
      if (typeof e.data === "string") return; // tuzy-pong
      const f = decodeFrame(e.data as ArrayBuffer);
      // Copy payloads: the message buffer is only valid for this event.
      f.payload = f.payload.slice();
      this.frames.push(f);
      this.handle(f);
      this.waiters = this.waiters.filter((w) => (w.pred(f) ? (w.resolve(f), false) : true));
    });
    ws.addEventListener("close", (e) => {
      this.stopHeartbeat();
      this.closed = { code: e.code, reason: e.reason };
      this.closeWaiters.forEach((f) => f());
      this.wakeCredit();
    });
  }

  /** Opens the WebSocket (no HELLO yet). Returns the raw Response when not upgraded. */
  static async open(
    name: string,
    opts: { instance?: string; force?: boolean; token?: string; reserve?: boolean } = {},
  ): Promise<{ agent?: FakeAgent; res: Response }> {
    if (opts.reserve !== false) await ensureReservation(name, opts.token ?? TOKEN);
    const instance = opts.instance ?? newInstance();
    const qs = new URLSearchParams({ name, instance, ...(opts.force ? { force: "1" } : {}) });
    const res = await exports.default.fetch(`https://${BASE}/api/v1/connect?${qs}`, {
      headers: { host: BASE, upgrade: "websocket", authorization: `Bearer ${opts.token ?? TOKEN}` },
    });
    if (res.status !== 101 || !res.webSocket) return { res };
    res.webSocket.accept();
    // This compat date defaults client sockets to Blob messages; frames need ArrayBuffers.
    (res.webSocket as unknown as { binaryType: string }).binaryType = "arraybuffer";
    return { agent: new FakeAgent(res.webSocket, instance), res };
  }

  /**
   * Opens and completes HELLO → READY, then pings like the real agent (the edge treats a silent
   * agent as dead after DEAD_SOCKET_SECONDS). `heartbeat: false` keeps it silent.
   */
  static async connect(
    name: string,
    opts: { instance?: string; force?: boolean; token?: string; reserve?: boolean; heartbeat?: boolean } = {},
  ): Promise<FakeAgent> {
    const { agent, res } = await FakeAgent.open(name, opts);
    if (!agent) throw new Error(`connect failed: ${res.status} ${await res.text()}`);
    await agent.hello();
    if (opts.heartbeat !== false) agent.heartbeat = setInterval(() => agent.ping(), 300);
    return agent;
  }

  /** Stops the automatic pings (the agent goes silent while its socket stays open). */
  stopHeartbeat(): void {
    if (this.heartbeat !== undefined) clearInterval(this.heartbeat);
  }

  async hello(): Promise<Frame> {
    this.send(encodeJsonFrame(FrameType.HELLO, 0, { proto: 1, client: "tuzy/test", instance_id: this.instance }));
    return this.next((f) => f.type === FrameType.READY || f.type === FrameType.GOAWAY);
  }

  send(frame: Uint8Array): void {
    this.ws.send(frame);
  }

  next(pred: (f: Frame) => boolean, timeoutMs = 10_000): Promise<Frame> {
    const hit = this.frames.find(pred);
    if (hit) return Promise.resolve(hit);
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error("timed out waiting for frame")), timeoutMs);
      this.waiters.push({ pred, resolve: (f) => (clearTimeout(t), resolve(f)) });
    });
  }

  waitClosed(timeoutMs = 10_000): Promise<{ code: number; reason: string }> {
    if (this.closed) return Promise.resolve(this.closed);
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error("timed out waiting for close")), timeoutMs);
      this.closeWaiters.push(() => (clearTimeout(t), resolve(this.closed!)));
    });
  }

  /** Waits until a REQ_HEAD with a matching path arrives. */
  async request(pathPrefix = "/"): Promise<ReqState> {
    const f = await this.next((f) => f.type === FrameType.REQ_HEAD && String(f.json!.path).startsWith(pathPrefix) && !this.taken.has(f.streamId));
    this.taken.add(f.streamId);
    return this.reqs.get(f.streamId)!;
  }
  private taken = new Set<number>();

  /** Resolves once REQ_END arrived for the stream; returns the concatenated body. */
  async body(id: number): Promise<Uint8Array> {
    await this.next((f) => f.type === FrameType.REQ_END && f.streamId === id, 60_000);
    return concat(this.reqs.get(id)!.chunks);
  }

  head(id: number, status: number, headers: [string, string][] = []): void {
    this.send(encodeJsonFrame(FrameType.RES_HEAD, id, { status, headers }));
  }

  /** Sends a response body chunk-by-chunk, waiting for edge credit. */
  async write(id: number, data: Uint8Array): Promise<void> {
    let off = 0;
    while (off < data.byteLength) {
      if (this.closed) throw new Error("agent socket closed");
      const n = Math.min(MAX_BODY_CHUNK, data.byteLength - off, this.sendStream.get(id) ?? STREAM_WINDOW, this.sendConn);
      if (n <= 0) {
        await new Promise<void>((r) => this.creditWakers.push(r));
        continue;
      }
      this.sendStream.set(id, (this.sendStream.get(id) ?? STREAM_WINDOW) - n);
      this.sendConn -= n;
      this.send(encodeFrame(FrameType.RES_BODY, id, data.subarray(off, off + n)));
      off += n;
    }
  }

  end(id: number): void {
    this.send(encodeFrame(FrameType.RES_END, id));
  }

  async respond(id: number, status: number, body: Uint8Array | string = "", headers: [string, string][] = []): Promise<void> {
    this.head(id, status, headers);
    await this.write(id, typeof body === "string" ? new TextEncoder().encode(body) : body);
    this.end(id);
  }

  /** Grants request-body credit that was withheld while paused. */
  grantRequestCredit(id: number, n: number): void {
    this.send(encodeU32Frame(FrameType.WINDOW, id, n));
    this.send(encodeU32Frame(FrameType.WINDOW, 0, n));
  }

  wsText(id: number, text: string): void {
    this.send(encodeTextFrame(id, text));
  }

  wsAck(id: number, n: number): void {
    this.send(encodeU32Frame(FrameType.ACK, id, n));
  }

  ping(): void {
    try {
      this.ws.send("tuzy-ping");
    } catch {
      // closed
    }
  }

  close(code = 1000): void {
    try {
      this.ws.close(code, "bye");
    } catch {
      // already closed
    }
  }

  wsMessages(id: number): string[] {
    return this.frames.filter((f) => f.type === FrameType.WS_TEXT && f.streamId === id).map((f) => payloadText(f.payload));
  }

  private handle(f: Frame): void {
    switch (f.type) {
      case FrameType.REQ_HEAD: {
        const req: ReqState = { id: f.streamId, head: f.json as unknown as ReqState["head"], chunks: [], ended: false };
        this.reqs.set(f.streamId, req);
        this.sendStream.set(f.streamId, STREAM_WINDOW);
        this.onRequest?.(req);
        break;
      }
      case FrameType.REQ_BODY: {
        const req = this.reqs.get(f.streamId);
        req?.chunks.push(f.payload);
        if (!this.pauseRequestCredit && f.payload.byteLength > 0) this.grantRequestCredit(f.streamId, f.payload.byteLength);
        break;
      }
      case FrameType.REQ_END: {
        const req = this.reqs.get(f.streamId);
        if (req) req.ended = true;
        break;
      }
      case FrameType.WINDOW:
        if (f.streamId === 0) this.sendConn += f.value!;
        else this.sendStream.set(f.streamId, (this.sendStream.get(f.streamId) ?? 0) + f.value!);
        this.wakeCredit();
        break;
      case FrameType.RESET: {
        const req = this.reqs.get(f.streamId);
        if (req) req.reset = f.json as { code: string };
        break;
      }
      case FrameType.WS_TEXT:
      case FrameType.WS_BINARY:
        if (this.autoAck) this.wsAck(f.streamId, 1);
        break;
    }
  }

  private wakeCredit(): void {
    const w = this.creditWakers;
    this.creditWakers = [];
    w.forEach((f) => f());
  }
}

/**
 * Connects now require the name to be reserved by the token's user: reserve it directly in D1
 * (INSERT OR IGNORE — an existing reservation by someone else is left alone).
 */
export async function ensureReservation(name: string, token: string): Promise<void> {
  const row = await env.DB.prepare("SELECT user_id FROM tokens WHERE token_hash = ?1").bind(await hashToken(token)).first<{ user_id: string }>();
  if (!row) return;
  await env.DB.prepare(
    `INSERT OR IGNORE INTO reservations (name, user_id, gen, is_default, created_at)
     VALUES (?1, ?2, lower(hex(randomblob(8))), NOT EXISTS (SELECT 1 FROM reservations WHERE user_id = ?2), 0)`,
  )
    .bind(name, row.user_id)
    .run();
}

export function concat(chunks: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(chunks.reduce((n, c) => n + c.byteLength, 0));
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.byteLength;
  }
  return out;
}

/** Visitor request to a tunnel host. */
export function visit(name: string, path = "/", init: RequestInit = {}): Promise<Response> {
  return exports.default.fetch(`https://${name}.${BASE}${path}`, {
    ...init,
    headers: { host: `${name}.${BASE}`, ...(init.headers as Record<string, string>) },
    redirect: "manual",
  });
}

export async function sha256(data: Uint8Array): Promise<string> {
  const d = await crypto.subtle.digest("SHA-256", data);
  return Array.from(new Uint8Array(d), (b) => b.toString(16).padStart(2, "0")).join("");
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Deterministic pseudo-random bytes. */
export function bytes(n: number, seed = 1): Uint8Array {
  const out = new Uint8Array(n);
  let x = seed >>> 0 || 1;
  for (let i = 0; i < n; i++) {
    x ^= x << 13;
    x ^= x >>> 17;
    x ^= x << 5;
    out[i] = x & 0xff;
  }
  return out;
}
