/**
 * Wire protocol v1 frame codec (protocol/PROTOCOL.md §2).
 *
 *   frame := type:u8 | stream_id:u32 (big-endian) | payload:bytes
 *
 * `decodeFrame` enforces every connection-level "malformed" rule of §2.2 and throws
 * `ProtocolError` ("malformed" → close 1002, "stream_error" → per-stream handling).
 * Field-level schema checks of JSON payloads live with the consumers (tunnel-object).
 */

export const FrameType = {
  HELLO: 0x01,
  READY: 0x02,
  GOAWAY: 0x03,
  DRAIN: 0x04,
  REQ_HEAD: 0x10,
  REQ_BODY: 0x11,
  REQ_END: 0x12,
  RES_HEAD: 0x20,
  RES_BODY: 0x21,
  RES_END: 0x22,
  WS_TEXT: 0x30,
  WS_BINARY: 0x31,
  WS_CLOSE: 0x32,
  WINDOW: 0x40,
  RESET: 0x41,
  ACK: 0x42,
} as const;
export type FrameType = (typeof FrameType)[keyof typeof FrameType];

/** Normative wire constants (§2.2). Edge *policy* timeouts live in lib/config.ts. */
export const HEADER_SIZE = 5;
export const MAX_BODY_CHUNK = 64 * 1024;
export const MAX_WS_MESSAGE = 1024 * 1024;
export const MAX_JSON_PAYLOAD = 128 * 1024;
export const STREAM_WINDOW = 1024 * 1024;
export const CONN_WINDOW = 8 * 1024 * 1024;
export const MAX_WINDOW = 2 ** 31 - 1;
export const WS_MSG_CREDIT = 8;
export const WS_MSG_TOLERANCE = 16;
export const MAX_STREAMS = 128;
export const CREDIT_TIMEOUT_MS = 30_000;
export const MAX_STREAM_ID = 2 ** 32 - 1;

/** Heartbeat text messages (§7): the only text allowed on the agent socket. */
export const PING = "tuzy-ping";
export const PONG = "tuzy-pong";

const STREAM0_TYPES = new Set<number>([FrameType.HELLO, FrameType.READY, FrameType.GOAWAY, FrameType.DRAIN]);
const JSON_TYPES = new Set<number>([
  FrameType.HELLO,
  FrameType.READY,
  FrameType.GOAWAY,
  FrameType.REQ_HEAD,
  FrameType.RES_HEAD,
  FrameType.WS_CLOSE,
  FrameType.RESET,
]);
const EMPTY_TYPES = new Set<number>([FrameType.DRAIN, FrameType.REQ_END, FrameType.RES_END]);
const U32_TYPES = new Set<number>([FrameType.WINDOW, FrameType.ACK]);
const KNOWN_TYPES = new Set<number>(Object.values(FrameType));

export type ProtocolErrorKind = "malformed" | "stream_error";

export class ProtocolError extends Error {
  constructor(
    readonly kind: ProtocolErrorKind,
    message: string,
    /** Stream the error applies to (stream_error only). */
    readonly streamId = 0,
    /** Visitor-WS close code for stream errors: 1007 invalid UTF-8, 1009 too big. */
    readonly closeCode = 1002,
  ) {
    super(message);
    this.name = "ProtocolError";
  }
}

/** A decoded frame. Exactly one payload view is meaningful, depending on `type`. */
export interface Frame {
  type: FrameType;
  streamId: number;
  /** Raw payload bytes: a view into the (per-message, never reused) WebSocket buffer. */
  payload: Uint8Array;
  /** Parsed JSON object for JSON frame types. */
  json?: Record<string, unknown>;
  /** Credit value for WINDOW / ACK. */
  value?: number;
}

const utf8Strict = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const utf8 = new TextEncoder();

/** Decodes one binary WebSocket message into a frame, validating §2.2. */
export function decodeFrame(message: ArrayBuffer | ArrayBufferView): Frame {
  // ArrayBuffer.isView (not `instanceof ArrayBuffer`): buffers may come from another realm.
  const bytes = ArrayBuffer.isView(message)
    ? new Uint8Array(message.buffer, message.byteOffset, message.byteLength)
    : new Uint8Array(message);
  if (bytes.byteLength < HEADER_SIZE) throw new ProtocolError("malformed", "frame shorter than header");

  const type = bytes[0]!;
  if (!KNOWN_TYPES.has(type)) throw new ProtocolError("malformed", `unknown frame type 0x${type.toString(16)}`);
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const streamId = view.getUint32(1);
  const payload = bytes.subarray(HEADER_SIZE);

  if (STREAM0_TYPES.has(type)) {
    if (streamId !== 0) throw new ProtocolError("malformed", "control frame on non-zero stream");
  } else if (type !== FrameType.WINDOW && streamId === 0) {
    throw new ProtocolError("malformed", "stream frame on stream 0");
  }

  const frame: Frame = { type: type as FrameType, streamId, payload };

  if (EMPTY_TYPES.has(type) && payload.byteLength !== 0) {
    throw new ProtocolError("malformed", "payload must be empty");
  }
  if (U32_TYPES.has(type)) {
    if (payload.byteLength !== 4) throw new ProtocolError("malformed", "credit payload must be 4 bytes");
    const value = view.getUint32(HEADER_SIZE);
    if (value === 0) throw new ProtocolError("malformed", "credit value must be > 0");
    frame.value = value;
  }
  if (JSON_TYPES.has(type)) {
    if (payload.byteLength > MAX_JSON_PAYLOAD) throw new ProtocolError("malformed", "JSON payload too large");
    let parsed: unknown;
    try {
      parsed = JSON.parse(utf8Strict.decode(payload));
    } catch {
      throw new ProtocolError("malformed", "invalid JSON payload");
    }
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      throw new ProtocolError("malformed", "JSON payload must be an object");
    }
    frame.json = parsed as Record<string, unknown>;
  }
  if ((type === FrameType.REQ_BODY || type === FrameType.RES_BODY) && payload.byteLength > MAX_BODY_CHUNK) {
    throw new ProtocolError("malformed", "body chunk larger than 64 KiB");
  }
  if (type === FrameType.WS_TEXT || type === FrameType.WS_BINARY) {
    if (payload.byteLength > MAX_WS_MESSAGE) throw new ProtocolError("stream_error", "ws message too big", streamId, 1009);
    if (type === FrameType.WS_TEXT) {
      try {
        utf8Strict.decode(payload);
      } catch {
        throw new ProtocolError("stream_error", "ws text is not valid UTF-8", streamId, 1007);
      }
    }
  }
  return frame;
}

/** Encodes a frame with a raw payload. */
export function encodeFrame(type: FrameType, streamId: number, payload: Uint8Array = new Uint8Array(0)): Uint8Array {
  const out = new Uint8Array(HEADER_SIZE + payload.byteLength);
  out[0] = type;
  new DataView(out.buffer).setUint32(1, streamId);
  out.set(payload, HEADER_SIZE);
  return out;
}

/** Encodes a JSON frame (compact JSON). */
export function encodeJsonFrame(type: FrameType, streamId: number, value: Record<string, unknown>): Uint8Array {
  return encodeFrame(type, streamId, utf8.encode(JSON.stringify(value)));
}

/** Encodes a WINDOW / ACK frame. */
export function encodeU32Frame(type: typeof FrameType.WINDOW | typeof FrameType.ACK, streamId: number, value: number): Uint8Array {
  const payload = new Uint8Array(4);
  new DataView(payload.buffer).setUint32(0, value);
  return encodeFrame(type, streamId, payload);
}

/** Encodes a WS_TEXT frame from a string. */
export function encodeTextFrame(streamId: number, text: string): Uint8Array {
  return encodeFrame(FrameType.WS_TEXT, streamId, utf8.encode(text));
}

/** Decodes a WS_TEXT payload (already validated by decodeFrame). */
export function payloadText(payload: Uint8Array): string {
  return utf8Strict.decode(payload);
}
