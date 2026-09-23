import { describe, expect, it } from "vitest";
import { acquireCredit, Credit, CreditTimeoutError, ReceiveWindow, StreamClosedError } from "../../src/protocol/flow";
import { MAX_WINDOW, ProtocolError } from "../../src/protocol/frames";

describe("ReceiveWindow", () => {
  it("allows up to the window and rejects one byte more", () => {
    const w = new ReceiveWindow(100);
    w.received(60);
    w.received(40);
    expect(() => w.received(1)).toThrow(ProtocolError);
  });

  it("frees space as credit is granted back", () => {
    const w = new ReceiveWindow(100);
    w.received(100);
    w.granted(30);
    w.received(30);
    expect(w.outstanding).toBe(100);
    w.granted(1000);
    expect(w.outstanding).toBe(0);
  });
});

describe("Credit / acquireCredit", () => {
  it("takes the minimum of both credits and the request", async () => {
    const s = new Credit(10);
    const c = new Credit(4);
    expect(await acquireCredit(s, c, 8, 100, () => false)).toBe(4);
    expect(s.available).toBe(6);
    expect(c.available).toBe(0);
  });

  it("waits for credit and resumes on add", async () => {
    const s = new Credit(0);
    const c = new Credit(100);
    const p = acquireCredit(s, c, 50, 1000, () => false);
    setTimeout(() => s.add(20), 10);
    expect(await p).toBe(20);
  });

  it("times out without credit", async () => {
    await expect(acquireCredit(new Credit(0), new Credit(10), 5, 30, () => false)).rejects.toBeInstanceOf(CreditTimeoutError);
  });

  it("stops when the stream closes", async () => {
    let closed = false;
    const s = new Credit(0);
    const p = acquireCredit(s, new Credit(10), 5, 1000, () => closed);
    closed = true;
    s.wake();
    await expect(p).rejects.toBeInstanceOf(StreamClosedError);
  });

  it("rejects a window above MAX_WINDOW", () => {
    const c = new Credit(MAX_WINDOW);
    expect(() => c.add(1)).toThrow(ProtocolError);
  });
});
