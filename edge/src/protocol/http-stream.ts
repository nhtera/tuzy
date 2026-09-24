/**
 * Edge side of one tunnel stream (PROTOCOL.md §3.1, §4): relays a visitor request to the agent
 * and the agent's response back to the visitor.
 *
 * - REQ_HEAD is sent first; the request body is pumped **concurrently** with waiting for RES_HEAD,
 *   so early responses (e.g. 413 mid-upload) return promptly (§4.3).
 * - Request body chunks need both stream and connection send credit (§4.1).
 * - Response body chunks are written to an IdentityTransformStream; WINDOW is granted only after
 *   `write()` resolves, i.e. when the visitor has consumed the bytes (coalesced at ¼ window).
 * - `kind:"ws"` streams hand a 101 over to the host, which turns them into hibernatable visitor
 *   sockets (see tunnel-object.ts); the HTTP stream then ends.
 */
import type { EdgePolicy } from "../lib/config";
import { agentToVisitorHeaders, isHeaderPairs } from "../lib/headers";
import { statusPage, type StatusPage } from "../pages/status-pages";
import { acquireCredit, Credit, CreditTimeoutError } from "./flow";
import {
  encodeFrame,
  encodeJsonFrame,
  encodeU32Frame,
  FrameType,
  MAX_BODY_CHUNK,
  STREAM_WINDOW,
  type Frame,
} from "./frames";

export type ResetCode =
  | "protocol_error"
  | "flow_control"
  | "credit_timeout"
  | "head_timeout"
  | "stream_timeout"
  | "cancelled"
  | "local_error"
  | "long_stream_budget";

/** What an HttpStream needs from its agent connection. */
export interface StreamHost {
  readonly policy: EdgePolicy;
  /** Connection-level send credit (edge → agent request bodies). */
  readonly connCredit: Credit;
  /** Sends a frame on the agent socket; throws if the socket is gone. */
  send(frame: Uint8Array): void;
  /**
   * Accounts `n` response bytes received on the connection (§4.1 receive window). Throws a
   * "malformed" ProtocolError when the agent exceeds the connection window.
   */
  received(n: number): void;
  /** Returns connection receive credit for `n` consumed/discarded response bytes (coalesced). */
  grantConn(n: number): void;
  /** Flushes any coalesced connection credit now. */
  flushConn(): void;
  /** Long-stream gate at `longStreamMs`: true to continue, else the RESET code to cut it with. */
  admitLong(id: number): true | "stream_timeout" | "long_stream_budget";
  /** Turns an accepted ws stream (RES_HEAD 101) into a visitor WebSocket response. */
  acceptVisitorSocket(id: number, headers: Headers): Response;
  /** Called exactly once when the stream is finished or reset. */
  onDone(id: number): void;
  /**
   * False once the agent socket is gone or silent for DEAD_SOCKET_SECONDS. After a code update the
   * runtime can terminate the socket without a close event; a stream must not wait out its head
   * timeout on it (that keeps the old object instance alive and new agent sockets stall).
   */
  agentAlive(): boolean;
}

const NULL_BODY_STATUS = new Set([101, 204, 205, 304]);

export class HttpStream {
  private readonly sendCredit = new Credit(STREAM_WINDOW);
  /** Response bytes received but not yet granted back (stream receive window accounting). */
  private recvOutstanding = 0;
  private streamUngranted = 0;
  private headDone = false;
  private resEnded = false;
  private reqEndSent = false;
  private done = false;
  private nullBody = false;
  private expectedLength: number | null = null;
  private bodyBytes = 0;
  private lengthMismatch = false;
  private writer?: WritableStreamDefaultWriter<Uint8Array>;
  private writeChain: Promise<void> = Promise.resolve();
  private reader?: ReadableStreamDefaultReader<Uint8Array>;
  private resolveHead!: (r: Response) => void;
  private readonly head = new Promise<Response>((r) => (this.resolveHead = r));
  private readonly timers: ReturnType<typeof setTimeout>[] = [];
  private liveness?: ReturnType<typeof setInterval>;

  constructor(
    readonly id: number,
    private readonly host: StreamHost,
    private readonly kind: "http" | "ws",
    private readonly method: string,
  ) {}

  /** Sends REQ_HEAD, starts the body pump and resolves with the visitor response. */
  start(reqHead: Uint8Array, body: ReadableStream<Uint8Array> | null, signal?: AbortSignal): Promise<Response> {
    const p = this.host.policy;
    try {
      this.host.send(reqHead);
    } catch {
      this.finish();
      return Promise.resolve(statusPage("offline"));
    }
    this.timers.push(
      setTimeout(() => {
        if (!this.headDone) this.reset("head_timeout");
      }, p.headTimeoutMs),
    );
    this.timers.push(setTimeout(() => this.reset("stream_timeout"), p.maxStreamMs));
    this.timers.push(
      setTimeout(() => {
        if (this.done) return;
        const admitted = this.host.admitLong(this.id);
        if (admitted !== true) this.reset(admitted);
      }, p.longStreamMs),
    );
    // Fail fast (502) when the agent dies silently instead of waiting for the head/credit timeouts.
    this.liveness = setInterval(() => {
      if (!this.host.agentAlive()) this.reset("cancelled", false, "bad_gateway");
    }, Math.max(250, Math.min(5_000, p.deadSocketMs / 4)));
    if (signal?.aborted) {
      this.reset("cancelled");
      return this.head;
    }
    signal?.addEventListener("abort", () => this.reset("cancelled"));

    if (this.kind === "http") void this.pump(body);
    else this.reqEndSent = true; // ws streams carry no request body (§3.1)
    return this.head;
  }

  /** Handles a stream frame from the agent. */
  onFrame(f: Frame): void {
    switch (f.type) {
      case FrameType.RES_HEAD:
        return this.onResHead(f);
      case FrameType.RES_BODY:
        return this.onResBody(f.payload);
      case FrameType.RES_END:
        return this.onResEnd();
      case FrameType.WINDOW:
        this.sendCredit.add(f.value!);
        return;
      case FrameType.RESET:
        return this.reset("cancelled", false, "bad_gateway");
    }
  }

  /** The agent connection went away. */
  agentGone(): void {
    this.reset("cancelled", false, "bad_gateway");
  }

  private onResHead(f: Frame): void {
    if (this.headDone || this.done) return this.protocolError();
    const status = f.json?.status;
    const pairs = f.json?.headers;
    if (typeof status !== "number" || !Number.isInteger(status) || !isHeaderPairs(pairs)) return this.protocolError();
    const validHttp = status >= 200 && status <= 599;
    const valid = this.kind === "ws" ? status === 101 || validHttp : validHttp;
    if (!valid) return this.protocolError();

    this.headDone = true;
    const headers = agentToVisitorHeaders(pairs, status);
    if (status === 101) {
      // WebSocket accepted: the host owns it from now on; this HTTP stream is complete.
      this.resolveHead(this.host.acceptVisitorSocket(this.id, headers));
      this.resEnded = true;
      this.finish();
      return;
    }

    this.nullBody = this.method === "HEAD" || NULL_BODY_STATUS.has(status);
    if (this.nullBody) {
      this.resolveHead(new Response(null, { status, headers, encodeBody: "manual" }));
      return;
    }
    const length = Number(headers.get("content-length"));
    const fixed = headers.has("content-length") && Number.isSafeInteger(length) && length >= 0;
    this.expectedLength = fixed ? length : null;
    const ts = fixed ? new FixedLengthStream(length) : new IdentityTransformStream();
    this.writer = ts.writable.getWriter() as WritableStreamDefaultWriter<Uint8Array>;
    this.resolveHead(new Response(ts.readable, { status, headers, encodeBody: "manual" }));
  }

  private onResBody(payload: Uint8Array): void {
    const len = payload.byteLength;
    this.host.received(len);
    if (!this.headDone || this.resEnded || this.done) {
      // Late/unexpected data: discard but always return connection credit (§4.3).
      this.host.grantConn(len);
      if (!this.headDone && !this.done) this.protocolError();
      return;
    }
    this.recvOutstanding += len;
    // Stream receive window: bytes received and not yet granted back (queued + consumed-ungranted).
    if (this.recvOutstanding + this.streamUngranted > STREAM_WINDOW) {
      this.reset("flow_control", true, "bad_gateway");
      this.consumed(len);
      return;
    }
    if (this.nullBody || !this.writer) {
      this.consumed(len);
      return;
    }
    const writer = this.writer;
    if (this.expectedLength !== null) {
      this.bodyBytes += len;
      if (this.bodyBytes > this.expectedLength) {
        this.lengthMismatch = true;
        this.reset("protocol_error", true, "bad_gateway");
        this.consumed(len);
        return;
      }
    }
    this.writeChain = this.writeChain.then(async () => {
      try {
        if (!this.done) await writer.write(payload);
      } catch {
        this.reset("cancelled"); // visitor went away
      } finally {
        this.consumed(len);
      }
    });
  }

  /** Response bytes consumed by the visitor (or discarded): return credit to the agent. */
  private consumed(len: number): void {
    this.recvOutstanding -= len;
    this.host.grantConn(len);
    if (this.done || this.resEnded) return;
    this.streamUngranted += len;
    if (this.streamUngranted >= STREAM_WINDOW / 4) {
      this.sendSafe(encodeU32Frame(FrameType.WINDOW, this.id, this.streamUngranted));
      this.streamUngranted = 0;
    }
  }

  private onResEnd(): void {
    if (!this.headDone || this.resEnded || this.done) return this.protocolError();
    this.resEnded = true;
    if (!this.nullBody && this.expectedLength !== null && this.bodyBytes !== this.expectedLength) this.lengthMismatch = true;
    this.stopPump();
    const writer = this.writer;
    this.writeChain = this.writeChain.then(async () => {
      try {
        if (writer && !this.done) await writer.close();
      } catch {
        // visitor gone or body length != content-length
        return this.reset(this.lengthMismatch ? "protocol_error" : "cancelled");
      }
      this.finish();
    });
  }

  /** Pumps the visitor request body as REQ_BODY frames, then REQ_END. */
  private async pump(body: ReadableStream<Uint8Array> | null): Promise<void> {
    const p = this.host.policy;
    try {
      if (body) {
        this.reader = body.getReader();
        for (;;) {
          const { done, value } = await this.reader.read();
          if (done || this.resEnded || this.done) break;
          let off = 0;
          while (off < value.byteLength) {
            const want = Math.min(MAX_BODY_CHUNK, value.byteLength - off);
            const n = await acquireCredit(this.sendCredit, this.host.connCredit, want, p.creditTimeoutMs, () => this.done || this.resEnded);
            this.host.send(encodeFrame(FrameType.REQ_BODY, this.id, value.subarray(off, off + n)));
            off += n;
          }
        }
      }
    } catch (e) {
      if (e instanceof CreditTimeoutError) return this.reset("credit_timeout");
      if (!this.done && !this.resEnded) return this.reset("cancelled");
    }
    this.sendReqEnd();
  }

  private sendReqEnd(): void {
    if (this.reqEndSent || this.done) return;
    this.reqEndSent = true;
    this.sendSafe(encodeFrame(FrameType.REQ_END, this.id));
  }

  /** After RES_END: stop reading the visitor body and tell the agent the request is over. */
  private stopPump(): void {
    this.sendReqEnd();
    this.sendCredit.wake();
    void this.reader?.cancel().catch(() => {});
  }

  private protocolError(): void {
    this.reset("protocol_error", true, "bad_gateway");
  }

  /**
   * Aborts the stream: RESET to the agent (unless it initiated), then the visitor sees an edge
   * status page if no head was sent yet, else an aborted body.
   */
  private reset(code: ResetCode, notifyAgent = true, page?: StatusPage): void {
    if (this.done) return;
    if (notifyAgent) this.sendSafe(encodeJsonFrame(FrameType.RESET, this.id, { code, message: code }));
    if (!this.headDone) {
      this.headDone = true;
      const fallback: StatusPage =
        code === "head_timeout" || code === "credit_timeout" || code === "stream_timeout" || code === "long_stream_budget" ? "timeout" : "bad_gateway";
      this.resolveHead(statusPage(page ?? fallback));
    } else {
      void this.writer?.abort(code).catch(() => {});
    }
    this.sendCredit.wake();
    void this.reader?.cancel().catch(() => {});
    this.finish();
  }

  /** Marks the stream done. The host flushes coalesced connection credit once it is idle. */
  private finish(): void {
    if (this.done) return;
    this.done = true;
    for (const t of this.timers) clearTimeout(t);
    if (this.liveness !== undefined) clearInterval(this.liveness);
    this.sendCredit.wake();
    this.host.onDone(this.id);
  }

  private sendSafe(frame: Uint8Array): void {
    try {
      this.host.send(frame);
    } catch {
      // Agent socket gone; its close handler fails the stream.
    }
  }
}
