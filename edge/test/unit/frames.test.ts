import { describe, expect, it } from "vitest";
import vectors from "../../../protocol/testdata/frames.json";
import {
  decodeFrame,
  encodeFrame,
  encodeU32Frame,
  FrameType,
  payloadText,
  ProtocolError,
} from "../../src/protocol/frames";

type Payload =
  | { kind: "empty" }
  | { kind: "json"; value: Record<string, unknown> }
  | { kind: "bytes"; hex: string }
  | { kind: "text"; value: string }
  | { kind: "u32"; value: number };

interface ValidVector {
  name: string;
  hex: string;
  decoded: { type: keyof typeof FrameType; type_code: number; stream_id: number; payload: Payload };
}
interface InvalidVector {
  name: string;
  hex: string;
  error: "malformed" | "stream_error";
}

const fromHex = (hex: string) => new Uint8Array((hex.match(/../g) ?? []).map((b) => parseInt(b, 16)));
const toHex = (bytes: Uint8Array) => Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");

describe("golden vectors: valid", () => {
  it.each((vectors.valid as ValidVector[]).map((v) => [v.name, v] as const))("%s", (_, v) => {
    const frame = decodeFrame(fromHex(v.hex));
    expect(frame.type).toBe(v.decoded.type_code);
    expect(frame.type).toBe(FrameType[v.decoded.type]);
    expect(frame.streamId).toBe(v.decoded.stream_id);

    const p = v.decoded.payload;
    switch (p.kind) {
      case "empty":
        expect(frame.payload.byteLength).toBe(0);
        expect(toHex(encodeFrame(frame.type, frame.streamId))).toBe(v.hex);
        break;
      case "json":
        expect(frame.json).toEqual(p.value);
        break;
      case "bytes":
        expect(toHex(frame.payload)).toBe(p.hex);
        expect(toHex(encodeFrame(frame.type, frame.streamId, fromHex(p.hex)))).toBe(v.hex);
        break;
      case "text":
        expect(payloadText(frame.payload)).toBe(p.value);
        break;
      case "u32":
        expect(frame.value).toBe(p.value);
        expect(toHex(encodeU32Frame(frame.type as typeof FrameType.WINDOW, frame.streamId, p.value))).toBe(v.hex);
        break;
    }
  });
});

describe("golden vectors: invalid", () => {
  it.each((vectors.invalid as InvalidVector[]).map((v) => [v.name, v] as const))("%s", (_, v) => {
    let err: unknown;
    try {
      decodeFrame(fromHex(v.hex));
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(ProtocolError);
    expect((err as ProtocolError).kind).toBe(v.error);
  });
});

describe("limits not covered by vectors", () => {
  it("rejects an oversized WS_BINARY as a stream error", () => {
    const f = encodeFrame(FrameType.WS_BINARY, 9, new Uint8Array(1024 * 1024 + 1));
    expect(() => decodeFrame(f)).toThrowError(expect.objectContaining({ kind: "stream_error", streamId: 9 }));
  });

  it("accepts a 1 MiB WS_BINARY", () => {
    const f = encodeFrame(FrameType.WS_BINARY, 9, new Uint8Array(1024 * 1024));
    expect(decodeFrame(f).payload.byteLength).toBe(1024 * 1024);
  });

  it("rejects an oversized RES_BODY", () => {
    const f = encodeFrame(FrameType.RES_BODY, 1, new Uint8Array(65 * 1024));
    expect(() => decodeFrame(f)).toThrowError(expect.objectContaining({ kind: "malformed" }));
  });
});
