/**
 * TunnelObject: one Durable Object per tunnel name (phase 2).
 *
 * - `fetch` carries ONLY visitor traffic and the internal agent connect (hostname
 *   `connect.internal`, unreachable from tunnel hosts). Control operations are RPC methods.
 * - Hibernation-safe: everything needed while idle lives in socket tags + attachments.
 *     agent socket   tags ["agent", "c:<epoch>"]                    attachment AgentAttachment
 *     visitor socket tags ["visitor", "v:<epoch>", "s:<epoch>:<id>"] attachment VisitorAttachment
 *   In-memory state (HTTP streams, coalesced credit, WS send buffers) only exists while a request
 *   is in flight, which keeps the DO awake.
 */
import { DurableObject } from "cloudflare:workers";
import { edgePolicy, type EdgePolicy } from "./lib/config";
import { writeConnectedMarker } from "./lib/connected-marker";
import { META_CONNECT, META_CONTINENT, META_PROTO, META_REMOTE_IP, visitorToAgentHeaders } from "./lib/headers";
import { apiError, statusPage } from "./pages/status-pages";
import { Credit, ReceiveWindow } from "./protocol/flow";
import {
  CONN_WINDOW,
  decodeFrame,
  encodeFrame,
  encodeJsonFrame,
  encodeTextFrame,
  encodeU32Frame,
  FrameType,
  MAX_JSON_PAYLOAD,
  MAX_STREAM_ID,
  MAX_STREAMS,
  MAX_WS_MESSAGE,
  payloadText,
  PING,
  PONG,
  ProtocolError,
  WS_MSG_CREDIT,
  type Frame,
} from "./protocol/frames";
import { HttpStream, type StreamHost } from "./protocol/http-stream";

/** Meta the Worker passes on the internal connect request (JSON in x-tuzy-meta). */
export interface ConnectMeta {
  name: string;
  /** Public tunnel URL, e.g. https://shop.tuzy.dev */
  url: string;
  instanceId: string;
  force: boolean;
  tokenId?: string;
  userId?: string;
  scope?: string;
  trusted?: boolean;
  gen?: number;
}

export interface AgentAttachment {
  role: "agent";
  epoch: number;
  name: string;
  url: string;
  instanceId: string;
  client: string;
  connectedAt: number;
  /** Next stream id to allocate; persisted on every allocation so ids are never reused. */
  nextStreamId: number;
  hello: boolean;
  draining: boolean;
  /** Edge→agent connection send credit, persisted whenever the connection goes idle. */
  sendCredit?: number;
  tokenId?: string;
  userId?: string;
}

export interface VisitorAttachment {
  role: "visitor";
  epoch: number;
  streamId: number;
  /** Edge→agent WS messages not yet ACKed (persisted so hibernation never resets it, §5.3). */
  outstanding: number;
  /** Messages are buffered in memory waiting for credit. */
  pending: boolean;
  /** WS_CLOSE already exchanged; ignore further close events. */
  closed: boolean;
}

export type GoawayReason = "renamed" | "deleted" | "suspended" | "replaced" | "revoked" | "upgrade_required" | "restart";

export interface TunnelStatus {
  state: "online" | "offline";
  since: number | null;
  epoch: number | null;
  client: string | null;
}

const OPEN = 1; // WebSocket.READY_STATE_OPEN
/** Total bytes of visitor WS messages buffered in memory per tunnel (M3 budget). */
const WS_BUFFER_BUDGET = 8 * 1024 * 1024;
const utf8 = new TextEncoder();

/** Close codes that may not be sent on the wire map to 1000. */
function sendableCode(code: unknown): number {
  const c = typeof code === "number" && Number.isInteger(code) ? code : 1000;
  if (c < 1000 || c > 4999 || c === 1004 || c === 1005 || c === 1006 || c === 1015) return 1000;
  return c;
}

/** Truncates to at most `max` UTF-8 bytes on a character boundary (close reasons ≤ 123 bytes). */
export function truncateUtf8(s: string, max: number): string {
  if (utf8.encode(s).byteLength <= max) return s;
  let out = "";
  let bytes = 0;
  for (const ch of s) {
    const n = utf8.encode(ch).byteLength;
    if (bytes + n > max) break;
    out += ch;
    bytes += n;
  }
  return out;
}

function closeSocket(ws: WebSocket, code: number, reason: string): void {
  try {
    ws.close(sendableCode(code), truncateUtf8(reason, 120));
  } catch {
    try {
      ws.close(sendableCode(code));
    } catch {
      // already closed
    }
  }
}

/** Callbacks an AgentConn needs from its TunnelObject (kept off the public RPC surface). */
interface ConnHooks {
  acceptVisitorSocket(epoch: number, id: number, headers: Headers): Response;
  admitLong(key: string): boolean;
  releaseLong(key: string): void;
  /** The connection became idle: persist state that must survive hibernation. */
  persistIdle(conn: AgentConn): void;
}

/** In-memory state for one agent connection (epoch). */
class AgentConn implements StreamHost {
  readonly connCredit: Credit;
  readonly streams = new Map<number, HttpStream>();
  /** Response bytes received and not yet granted back (connection receive window, §4.1). */
  private readonly recvWindow = new ReceiveWindow(CONN_WINDOW);
  private connUngranted = 0;

  constructor(
    readonly ws: WebSocket,
    readonly epoch: number,
    readonly policy: EdgePolicy,
    private readonly hooks: ConnHooks,
    sendCredit: number,
  ) {
    this.connCredit = new Credit(sendCredit);
  }

  send(frame: Uint8Array): void {
    this.ws.send(frame);
  }

  received(n: number): void {
    this.recvWindow.received(n);
  }

  grantConn(n: number): void {
    this.connUngranted += n;
    // Coalesce while streams are active; flush at once when idle so credit never sits in memory
    // across hibernation (§4.3).
    if (this.connUngranted >= CONN_WINDOW / 4 || this.streams.size === 0) this.flushConn();
  }

  flushConn(): void {
    if (this.connUngranted <= 0) return;
    const n = this.connUngranted;
    this.connUngranted = 0;
    this.recvWindow.granted(n);
    try {
      this.send(encodeU32Frame(FrameType.WINDOW, 0, n));
    } catch {
      // agent gone
    }
  }

  admitLong(id: number): boolean {
    return this.hooks.admitLong(`${this.epoch}:${id}`);
  }

  acceptVisitorSocket(id: number, headers: Headers): Response {
    return this.hooks.acceptVisitorSocket(this.epoch, id, headers);
  }

  onDone(id: number): void {
    this.streams.delete(id);
    this.hooks.releaseLong(`${this.epoch}:${id}`);
    if (this.streams.size === 0) {
      this.flushConn();
      this.hooks.persistIdle(this);
    }
  }
}

export class TunnelObject extends DurableObject<Env> {
  private readonly policy: EdgePolicy;
  private epoch = 0;
  private name = "";
  private suspended = false;
  private trusted = false;
  private gen: number | undefined;
  private readonly conns = new Map<number, AgentConn>();
  /** Visitor WS messages waiting for agent credit, keyed "epoch:streamId". */
  private readonly wsBuffers = new Map<string, { msg: string | ArrayBuffer; size: number }[]>();
  private wsBufferedBytes = 0;
  /** Streams past LONG_STREAM_AFTER, per name (all epochs), keyed "epoch:id". */
  private readonly longStreams = new Set<string>();
  private markerWritten = false;
  private readonly hooks: ConnHooks = {
    acceptVisitorSocket: (epoch, id, headers) => this.acceptVisitorSocket(epoch, id, headers),
    admitLong: (key) => {
      if (this.longStreams.size >= this.policy.maxLongStreams) return false;
      this.longStreams.add(key);
      return true;
    },
    releaseLong: (key) => void this.longStreams.delete(key),
    persistIdle: (conn) => {
      const att = conn.ws.deserializeAttachment() as AgentAttachment | null;
      if (!att || conn.ws.readyState !== OPEN) return;
      att.sendCredit = conn.connCredit.available;
      conn.ws.serializeAttachment(att);
    },
  };

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.policy = edgePolicy(env);
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair(PING, PONG));
    void ctx.blockConcurrencyWhile(async () => {
      const s = await ctx.storage.get<number | string | boolean>(["epoch", "name", "suspended", "trusted", "gen", "marker"]);
      this.markerWritten = (s.get("marker") as boolean | undefined) ?? false;
      this.epoch = (s.get("epoch") as number | undefined) ?? 0;
      this.name = (s.get("name") as string | undefined) ?? "";
      this.suspended = (s.get("suspended") as boolean | undefined) ?? false;
      this.trusted = (s.get("trusted") as boolean | undefined) ?? false;
      this.gen = s.get("gen") as number | undefined;
    });
    // Woken from hibernation: buffered visitor messages were lost with the old isolate (§5.3).
    for (const ws of ctx.getWebSockets("visitor")) {
      const att = ws.deserializeAttachment() as VisitorAttachment | null;
      if (att?.pending && !att.closed) this.failVisitor(ws, att, 1011, "tunnel state lost");
    }
  }

  // ───────────────────────────── fetch: connect + visitor traffic ─────────────────────────────

  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    if (url.hostname === "connect.internal") return this.handleConnect(request);
    return this.handleVisitor(request, url);
  }

  private handleConnect(request: Request): Response {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return apiError(400, "websocket_required", "connect requires a WebSocket upgrade");
    }
    let meta: ConnectMeta;
    try {
      meta = JSON.parse(request.headers.get(META_CONNECT) ?? "") as ConnectMeta;
    } catch {
      return apiError(400, "bad_meta", "missing connect meta");
    }
    if (this.suspended) return apiError(403, "suspended", "this tunnel is suspended; contact abuse@tuzy.dev");

    const now = Date.now();
    const current = this.currentAgent();
    if (current) {
      const { ws, att } = current;
      if (att.instanceId === meta.instanceId) {
        this.dropAgent(ws, 1000, "replaced by the same agent instance"); // §3.2 rule 1
      } else if (this.isDead(ws, att, now)) {
        this.dropAgent(ws, 1001, "agent unresponsive"); // rule 2
      } else if (att.draining) {
        // rule 3: the draining socket keeps finishing its in-flight streams under its own epoch
      } else if (meta.force) {
        this.sendGoaway(ws, "replaced", { message: "another agent took over this tunnel" }); // rule 4
        this.dropAgent(ws, 1000, "replaced");
      } else {
        return apiError(409, "name_in_use", "this name is live on another device; rerun with --force"); // rule 5
      }
    }

    this.epoch += 1;
    if (this.name !== meta.name) {
      this.name = meta.name;
      void this.ctx.storage.put("name", meta.name);
    }
    void this.ctx.storage.put("epoch", this.epoch);

    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    const att: AgentAttachment = {
      role: "agent",
      epoch: this.epoch,
      name: meta.name,
      url: meta.url,
      instanceId: meta.instanceId,
      client: "",
      connectedAt: now,
      nextStreamId: 1,
      hello: false,
      draining: false,
      tokenId: meta.tokenId,
      userId: meta.userId,
    };
    this.ctx.acceptWebSocket(server, ["agent", `c:${this.epoch}`]);
    server.serializeAttachment(att);
    // Best effort HELLO deadline; if the DO hibernates first, isDead() treats the socket as dead.
    setTimeout(() => {
      const a = server.deserializeAttachment() as AgentAttachment | null;
      if (server.readyState === OPEN && a && !a.hello) this.dropAgent(server, 1002, "no HELLO");
    }, this.policy.helloTimeoutMs);
    return new Response(null, { status: 101, webSocket: client });
  }

  private handleVisitor(request: Request, url: URL): Response | Promise<Response> {
    if (this.suspended) return statusPage("suspended");
    const current = this.currentAgent();
    if (!current || !current.att.hello) return statusPage("offline");
    if (current.att.draining) return statusPage("draining");
    const { ws, att } = current;
    const conn = this.conn(ws, att.epoch);
    if (conn.streams.size + this.ctx.getWebSockets(`v:${att.epoch}`).length >= MAX_STREAMS) return statusPage("busy");

    const id = att.nextStreamId;
    if (id > MAX_STREAM_ID) {
      this.sendGoaway(ws, "restart");
      this.dropAgent(ws, 1000, "stream ids exhausted");
      return statusPage("offline");
    }
    att.nextStreamId = id + 1;
    ws.serializeAttachment(att);

    const kind = request.headers.get("upgrade")?.toLowerCase() === "websocket" ? "ws" : "http";
    const remoteIp = request.headers.get(META_REMOTE_IP) ?? "";
    const headers = visitorToAgentHeaders(request.headers, {
      remoteIp,
      continent: request.headers.get(META_CONTINENT) ?? "",
      proto: request.headers.get(META_PROTO) ?? "https",
    });
    const head = encodeJsonFrame(FrameType.REQ_HEAD, id, {
      method: request.method,
      path: url.pathname + url.search,
      headers,
      remote_ip: remoteIp,
      kind,
    });
    if (head.byteLength - 5 > MAX_JSON_PAYLOAD) return statusPage("header_too_large");

    const stream = new HttpStream(id, conn, kind, request.method);
    conn.streams.set(id, stream);
    return stream.start(head, kind === "http" ? request.body : null, request.signal);
  }

  /** Called (via ConnHooks) when the agent accepted a visitor WebSocket (RES_HEAD 101). */
  private acceptVisitorSocket(epoch: number, streamId: number, headers: Headers): Response {
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    this.ctx.acceptWebSocket(server, ["visitor", `v:${epoch}`, `s:${epoch}:${streamId}`]);
    const att: VisitorAttachment = { role: "visitor", epoch, streamId, outstanding: 0, pending: false, closed: false };
    server.serializeAttachment(att);
    return new Response(null, { status: 101, webSocket: client, headers });
  }

  // ───────────────────────────── WebSocket event handlers ─────────────────────────────

  override async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const att = ws.deserializeAttachment() as AgentAttachment | VisitorAttachment | null;
    if (att?.role === "agent") return this.onAgentMessage(ws, att, message);
    if (att?.role === "visitor") return this.onVisitorMessage(ws, att, message);
  }

  override async webSocketClose(ws: WebSocket, code: number, reason: string): Promise<void> {
    this.onSocketGone(ws, code, reason);
  }

  override async webSocketError(ws: WebSocket): Promise<void> {
    this.onSocketGone(ws, 1011, "socket error");
  }

  private onSocketGone(ws: WebSocket, code: number, reason: string): void {
    const att = ws.deserializeAttachment() as AgentAttachment | VisitorAttachment | null;
    if (att?.role === "agent") {
      closeSocket(ws, code, reason); // complete the close handshake
      this.agentGone(ws, att.epoch);
    } else if (att?.role === "visitor" && !att.closed) {
      // Visitor closed first: tell the agent (the agent answers with WS_CLOSE, which we ignore).
      att.closed = true;
      ws.serializeAttachment(att);
      this.dropWsBuffer(`${att.epoch}:${att.streamId}`);
      this.sendToAgent(att.epoch, encodeJsonFrame(FrameType.WS_CLOSE, att.streamId, { code: sendableCode(code), reason }));
      closeSocket(ws, code, reason);
    }
  }

  // ───────────────────────────── agent frames ─────────────────────────────

  private onAgentMessage(ws: WebSocket, att: AgentAttachment, message: string | ArrayBuffer): void {
    if (typeof message === "string") {
      // Only `tuzy-ping` is allowed and it is answered by the runtime auto-response (§7).
      this.dropAgent(ws, 1003, "unexpected text message");
      return;
    }
    let frame: Frame;
    try {
      frame = decodeFrame(message);
    } catch (e) {
      if (e instanceof ProtocolError && e.kind === "stream_error" && att.hello) {
        // Bad WS message from the agent (§2.2): close only that visitor socket (1007 / 1009).
        this.closeVisitorStream(att.epoch, e.streamId, e.closeCode, e.message, true);
        return;
      }
      this.dropAgent(ws, 1002, e instanceof Error ? e.message : "protocol error");
      return;
    }

    try {
      if (!att.hello) {
        if (frame.type !== FrameType.HELLO) throw new ProtocolError("malformed", "first frame must be HELLO");
        this.onHello(ws, att, frame);
        return;
      }
      this.dispatch(ws, att, frame);
    } catch (e) {
      this.dropAgent(ws, 1002, e instanceof Error ? e.message : "protocol error");
    }
  }

  private onHello(ws: WebSocket, att: AgentAttachment, f: Frame): void {
    const { proto, client, instance_id } = f.json ?? {};
    if (typeof proto !== "number" || typeof client !== "string" || typeof instance_id !== "string") {
      throw new ProtocolError("malformed", "bad HELLO");
    }
    if (proto !== 1) {
      this.sendGoaway(ws, "upgrade_required", { message: "this tuzy version is no longer supported; please upgrade" });
      this.dropAgent(ws, 1000, "upgrade required");
      return;
    }
    if (instance_id !== att.instanceId) throw new ProtocolError("malformed", "HELLO instance_id mismatch");
    att.hello = true;
    att.client = client.slice(0, 200);
    ws.serializeAttachment(att);
    ws.send(encodeJsonFrame(FrameType.READY, 0, { name: att.name, url: att.url, epoch: att.epoch }));
    if (!this.markerWritten) {
      this.markerWritten = true;
      void this.ctx.storage.put("marker", true);
      this.ctx.waitUntil(writeConnectedMarker(this.env.NAMES_KV, att.name).catch(() => {}));
    }
  }

  private dispatch(ws: WebSocket, att: AgentAttachment, f: Frame): void {
    const conn = this.conn(ws, att.epoch);
    switch (f.type) {
      case FrameType.DRAIN:
        att.draining = true;
        ws.serializeAttachment(att);
        return;
      case FrameType.WINDOW:
        if (f.streamId === 0) conn.connCredit.add(f.value!);
        else conn.streams.get(f.streamId)?.onFrame(f);
        return;
      case FrameType.RES_HEAD:
      case FrameType.RES_END:
        conn.streams.get(f.streamId)?.onFrame(f);
        return;
      case FrameType.RES_BODY: {
        const s = conn.streams.get(f.streamId);
        if (s) s.onFrame(f);
        else {
          conn.received(f.payload.byteLength);
          conn.grantConn(f.payload.byteLength); // unknown/closed stream: return credit (§4.3)
        }
        return;
      }
      case FrameType.RESET: {
        const s = conn.streams.get(f.streamId);
        if (s) s.onFrame(f);
        else this.closeVisitorStream(att.epoch, f.streamId, 1011, "tunnel stream reset", false);
        return;
      }
      case FrameType.WS_TEXT:
      case FrameType.WS_BINARY:
        return this.agentWsMessage(att.epoch, f);
      case FrameType.WS_CLOSE: {
        const { code, reason } = f.json ?? {};
        if (typeof code !== "number" || !Number.isInteger(code) || (reason !== undefined && typeof reason !== "string")) {
          // Schema error: stream-level (§2.2) → RESET protocol_error, drop the visitor socket.
          ws.send(encodeJsonFrame(FrameType.RESET, f.streamId, { code: "protocol_error", message: "bad WS_CLOSE" }));
          this.closeVisitorStream(att.epoch, f.streamId, 1011, "tunnel protocol error", false);
          return;
        }
        this.closeVisitorStream(att.epoch, f.streamId, code, reason ?? "", true);
        return;
      }
      case FrameType.ACK:
        return this.onAck(att.epoch, f.streamId, f.value!);
      default:
        // HELLO twice, or an edge→agent frame type sent by the agent.
        throw new ProtocolError("malformed", `unexpected frame 0x${f.type.toString(16)} from agent`);
    }
  }

  // ───────────────────────────── visitor WebSocket relay (§5) ─────────────────────────────

  private onVisitorMessage(ws: WebSocket, att: VisitorAttachment, message: string | ArrayBuffer): void {
    if (att.closed) return;
    const bytes = typeof message === "string" ? utf8.encode(message) : new Uint8Array(message);
    if (bytes.byteLength > MAX_WS_MESSAGE) {
      this.failVisitor(ws, att, 1009, "message too big");
      return;
    }
    if (!this.agentSocket(att.epoch)) {
      this.failVisitor(ws, att, 1011, "tunnel agent disconnected");
      return;
    }
    const key = `${att.epoch}:${att.streamId}`;
    const buf = this.wsBuffers.get(key);
    if (att.outstanding < WS_MSG_CREDIT && !buf?.length) {
      att.outstanding += 1; // persist BEFORE send so hibernation can't lose the increment
      ws.serializeAttachment(att);
      this.sendToAgent(att.epoch, this.wsFrame(att.streamId, message, bytes));
      return;
    }
    if ((buf?.length ?? 0) >= WS_MSG_CREDIT || this.wsBufferedBytes + bytes.byteLength > WS_BUFFER_BUDGET) {
      this.failVisitor(ws, att, 1008, "tunnel agent is not keeping up");
      return;
    }
    this.wsBuffers.set(key, [...(buf ?? []), { msg: message, size: bytes.byteLength }]);
    this.wsBufferedBytes += bytes.byteLength;
    if (!att.pending) {
      att.pending = true;
      ws.serializeAttachment(att);
    }
  }

  private onAck(epoch: number, streamId: number, n: number): void {
    const ws = this.visitorSocket(epoch, streamId);
    if (!ws) return;
    const att = ws.deserializeAttachment() as VisitorAttachment;
    att.outstanding = Math.max(0, att.outstanding - n); // excess ACKs are ignored (§5.2)
    const key = `${epoch}:${streamId}`;
    const buf = this.wsBuffers.get(key) ?? [];
    while (buf.length > 0 && att.outstanding < WS_MSG_CREDIT) {
      const { msg, size } = buf.shift()!;
      this.wsBufferedBytes -= size;
      att.outstanding += 1;
      ws.serializeAttachment(att);
      this.sendToAgent(epoch, this.wsFrame(streamId, msg));
    }
    if (buf.length === 0) this.wsBuffers.delete(key);
    att.pending = buf.length > 0;
    ws.serializeAttachment(att);
  }

  private agentWsMessage(epoch: number, f: Frame): void {
    const ws = this.visitorSocket(epoch, f.streamId);
    if (!ws) return;
    try {
      if (f.type === FrameType.WS_TEXT) ws.send(payloadText(f.payload));
      else ws.send(f.payload.slice().buffer);
    } catch {
      return; // visitor gone; its close handler notifies the agent
    }
    // ACK in the same handler that delivered the message (§5.2).
    this.sendToAgent(epoch, encodeU32Frame(FrameType.ACK, f.streamId, 1));
  }

  /** Closes a visitor stream's socket (agent-initiated or error); optionally answers WS_CLOSE. */
  private closeVisitorStream(epoch: number, streamId: number, code: unknown, reason: string, answer: boolean): void {
    const ws = this.visitorSocket(epoch, streamId);
    if (!ws) return;
    const att = ws.deserializeAttachment() as VisitorAttachment;
    if (att.closed) return;
    att.closed = true;
    att.pending = false;
    ws.serializeAttachment(att);
    this.dropWsBuffer(`${epoch}:${streamId}`);
    if (answer) this.sendToAgent(epoch, encodeJsonFrame(FrameType.WS_CLOSE, streamId, { code: sendableCode(code), reason }));
    closeSocket(ws, sendableCode(code), reason);
  }

  /** Edge-side visitor failure: close the visitor socket and tell the agent. */
  private failVisitor(ws: WebSocket, att: VisitorAttachment, code: number, reason: string): void {
    att.closed = true;
    att.pending = false;
    ws.serializeAttachment(att);
    this.dropWsBuffer(`${att.epoch}:${att.streamId}`);
    this.sendToAgent(att.epoch, encodeJsonFrame(FrameType.WS_CLOSE, att.streamId, { code, reason }));
    closeSocket(ws, code, reason);
  }

  private dropWsBuffer(key: string): void {
    for (const { size } of this.wsBuffers.get(key) ?? []) this.wsBufferedBytes -= size;
    this.wsBuffers.delete(key);
  }

  private wsFrame(streamId: number, message: string | ArrayBuffer, bytes?: Uint8Array): Uint8Array {
    if (typeof message === "string") return encodeTextFrame(streamId, message);
    return encodeFrame(FrameType.WS_BINARY, streamId, bytes ?? new Uint8Array(message));
  }

  // ───────────────────────────── agent lifecycle helpers ─────────────────────────────

  /** Current agent = the OPEN agent socket with the latest epoch. */
  private currentAgent(): { ws: WebSocket; att: AgentAttachment } | null {
    let best: { ws: WebSocket; att: AgentAttachment } | null = null;
    for (const ws of this.ctx.getWebSockets("agent")) {
      if (ws.readyState !== OPEN) continue;
      const att = ws.deserializeAttachment() as AgentAttachment | null;
      if (att && (!best || att.epoch > best.att.epoch)) best = { ws, att };
    }
    return best;
  }

  private agentSocket(epoch: number): WebSocket | null {
    const ws = this.ctx.getWebSockets(`c:${epoch}`)[0];
    return ws && ws.readyState === OPEN ? ws : null;
  }

  private visitorSocket(epoch: number, streamId: number): WebSocket | null {
    return this.ctx.getWebSockets(`s:${epoch}:${streamId}`)[0] ?? null;
  }

  private conn(ws: WebSocket, epoch: number): AgentConn {
    let c = this.conns.get(epoch);
    if (!c) {
      const att = ws.deserializeAttachment() as AgentAttachment | null;
      c = new AgentConn(ws, epoch, this.policy, this.hooks, att?.sendCredit ?? CONN_WINDOW);
      this.conns.set(epoch, c);
    }
    return c;
  }

  /** §3.2 rule 2: no pong for DEAD_SOCKET_AFTER (or none yet and connected that long), or no HELLO in time. */
  private isDead(ws: WebSocket, att: AgentAttachment, now: number): boolean {
    if (!att.hello && now - att.connectedAt > this.policy.helloTimeoutMs) return true;
    const pong = this.ctx.getWebSocketAutoResponseTimestamp(ws);
    const last = pong ? pong.getTime() : att.connectedAt;
    return now - last > this.policy.deadSocketMs;
  }

  private sendToAgent(epoch: number, frame: Uint8Array): void {
    try {
      this.agentSocket(epoch)?.send(frame);
    } catch {
      // agent gone
    }
  }

  private sendGoaway(ws: WebSocket, reason: GoawayReason, extra: { new_name?: string; message?: string } = {}): void {
    try {
      ws.send(encodeJsonFrame(FrameType.GOAWAY, 0, { reason, ...extra }));
    } catch {
      // already closed
    }
  }

  /** Closes an agent socket and fails everything that belonged to its epoch. */
  private dropAgent(ws: WebSocket, code: number, reason: string): void {
    const att = ws.deserializeAttachment() as AgentAttachment | null;
    closeSocket(ws, code, reason);
    if (att) this.agentGone(ws, att.epoch);
  }

  /** Fails the epoch's HTTP streams and visitor sockets. Idempotent; stale epochs only touch their own. */
  private agentGone(_ws: WebSocket, epoch: number): void {
    const conn = this.conns.get(epoch);
    this.conns.delete(epoch);
    if (conn) for (const s of [...conn.streams.values()]) s.agentGone();
    for (const v of this.ctx.getWebSockets(`v:${epoch}`)) {
      const att = v.deserializeAttachment() as VisitorAttachment;
      if (att.closed) continue;
      att.closed = true;
      v.serializeAttachment(att);
      this.dropWsBuffer(`${epoch}:${att.streamId}`);
      closeSocket(v, 1012, "tunnel agent disconnected");
    }
  }

  // ───────────────────────────── RPC control plane ─────────────────────────────

  private genMatches(gen: number | undefined): boolean {
    return gen === undefined || this.gen === undefined || gen === this.gen;
  }

  /** Sends GOAWAY to every agent socket and closes them. No-op on a stale `gen`. */
  async goaway(reason: GoawayReason, opts: { gen?: number; newName?: string; message?: string } = {}): Promise<boolean> {
    if (!this.genMatches(opts.gen)) return false;
    for (const ws of this.ctx.getWebSockets("agent")) {
      this.sendGoaway(ws, reason, { new_name: opts.newName, message: opts.message });
      this.dropAgent(ws, 1000, reason);
    }
    return true;
  }

  async setSuspended(suspended: boolean, gen?: number): Promise<boolean> {
    if (!this.genMatches(gen)) return false;
    this.suspended = suspended;
    await this.ctx.storage.put("suspended", suspended);
    if (suspended) await this.goaway("suspended", { message: "this tunnel was suspended; contact abuse@tuzy.dev" });
    return true;
  }

  async setTrusted(trusted: boolean): Promise<void> {
    this.trusted = trusted;
    await this.ctx.storage.put("trusted", trusted);
  }

  /** Closes live sessions authenticated with `tokenId` (logout / tokens rm). */
  async revokeToken(tokenId: string): Promise<number> {
    let n = 0;
    for (const ws of this.ctx.getWebSockets("agent")) {
      const att = ws.deserializeAttachment() as AgentAttachment | null;
      if (att?.tokenId !== tokenId) continue;
      this.sendGoaway(ws, "revoked", { message: "this session was logged out" });
      this.dropAgent(ws, 1000, "revoked");
      n += 1;
    }
    return n;
  }

  /** Closes live sessions of a deleted account (only that user's sockets). */
  async revokeUser(userId: string): Promise<number> {
    let n = 0;
    for (const ws of this.ctx.getWebSockets("agent")) {
      const att = ws.deserializeAttachment() as AgentAttachment | null;
      if (att?.userId !== userId) continue;
      this.sendGoaway(ws, "deleted", { message: "this account was deleted" });
      this.dropAgent(ws, 1000, "account deleted");
      n += 1;
    }
    return n;
  }

  async status(): Promise<TunnelStatus> {
    const cur = this.currentAgent();
    if (!cur || !cur.att.hello) return { state: "offline", since: null, epoch: null, client: null };
    return { state: "online", since: cur.att.connectedAt, epoch: cur.att.epoch, client: cur.att.client };
  }
}
