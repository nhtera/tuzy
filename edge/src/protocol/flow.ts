/**
 * Send-side credit (PROTOCOL.md §4.1). A sender may transmit body bytes only while holding both
 * stream and connection credit; `acquire` waits for both with an idle timeout (§4.4).
 */
import { MAX_WINDOW, ProtocolError } from "./frames";

export class CreditTimeoutError extends Error {
  constructor() {
    super("credit timeout");
    this.name = "CreditTimeoutError";
  }
}

export class StreamClosedError extends Error {
  constructor() {
    super("stream closed");
    this.name = "StreamClosedError";
  }
}

/** A credit counter that wakes waiters whenever credit is added. */
export class Credit {
  private wakers: (() => void)[] = [];

  constructor(public available: number) {}

  /** Adds peer-granted credit. Exceeding MAX_WINDOW is a connection error (§4.1). */
  add(n: number): void {
    if (this.available + n > MAX_WINDOW) throw new ProtocolError("malformed", "window exceeds MAX_WINDOW");
    this.available += n;
    this.wake();
  }

  take(n: number): void {
    this.available -= n;
  }

  /** Resolves on the next `add` (or `wake`). */
  changed(): Promise<void> {
    return new Promise((resolve) => this.wakers.push(resolve));
  }

  /** Wakes all waiters without adding credit (used on close so they can observe it). */
  wake(): void {
    const w = this.wakers;
    this.wakers = [];
    for (const f of w) f();
  }
}

/**
 * Waits until both credits are positive, then takes and returns up to `want` bytes
 * (bounded by both credits). Throws CreditTimeoutError after `timeoutMs` without progress,
 * StreamClosedError when `isClosed()` turns true.
 */
export async function acquireCredit(
  stream: Credit,
  conn: Credit,
  want: number,
  timeoutMs: number,
  isClosed: () => boolean,
): Promise<number> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (isClosed()) throw new StreamClosedError();
    const n = Math.min(want, stream.available, conn.available);
    if (n > 0) {
      stream.take(n);
      conn.take(n);
      return n;
    }
    const remaining = deadline - Date.now();
    if (remaining <= 0) throw new CreditTimeoutError();
    let timer: ReturnType<typeof setTimeout> | undefined;
    await Promise.race([
      stream.changed(),
      conn.changed(),
      new Promise<void>((resolve) => {
        timer = setTimeout(resolve, remaining);
      }),
    ]);
    if (timer !== undefined) clearTimeout(timer);
  }
}

/**
 * Receive-side window accounting (§4.1/§4.2): bytes received and not yet granted back may never
 * exceed the window; a peer that exceeds it is violating the protocol.
 */
export class ReceiveWindow {
  private held = 0;

  constructor(readonly size: number) {}

  received(n: number): void {
    this.held += n;
    if (this.held > this.size) throw new ProtocolError("malformed", "peer exceeded the receive window");
  }

  granted(n: number): void {
    this.held = Math.max(0, this.held - n);
  }

  get outstanding(): number {
    return this.held;
  }
}
