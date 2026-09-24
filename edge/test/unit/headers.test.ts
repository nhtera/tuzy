import { describe, expect, it } from "vitest";
import { agentToVisitorHeaders, restoreAcceptEncoding, stripEdgeInternal, stripTuzyCookies, visitorToAgentHeaders } from "../../src/lib/headers";

const meta = { remoteIp: "203.0.113.9", continent: "EU", proto: "https" };

describe("visitorToAgentHeaders", () => {
  it("drops hop-by-hop, edge-internal, forwarded and ws handshake headers; sets x-forwarded-*", () => {
    const h = new Headers({
      host: "shop.tuzy.dev",
      connection: "keep-alive, x-custom-hop",
      "x-custom-hop": "1",
      "keep-alive": "timeout=5",
      "transfer-encoding": "chunked",
      upgrade: "websocket",
      "cf-connecting-ip": "1.1.1.1",
      "cf-ray": "abc",
      "cdn-loop": "cloudflare",
      "x-tuzy-remote-ip": "6.6.6.6",
      "x-forwarded-for": "6.6.6.6",
      forwarded: "for=6.6.6.6",
      "tuzy-skip-warning": "1",
      "sec-websocket-key": "k",
      "sec-websocket-protocol": "chat",
      accept: "text/html",
      "x-real-ip": "6.6.6.6",
      "true-client-ip": "6.6.6.6",
      "x-client-ip": "6.6.6.6",
    });
    const out = Object.fromEntries(visitorToAgentHeaders(h, meta));
    expect(out).toEqual({
      host: "shop.tuzy.dev",
      "sec-websocket-protocol": "chat",
      accept: "text/html",
      "x-forwarded-for": "203.0.113.9",
      "x-real-ip": "203.0.113.9",
      "x-forwarded-proto": "https",
      "x-forwarded-host": "shop.tuzy.dev",
    });
  });

  it("strips tuzy_* cookies but keeps the rest", () => {
    const out = visitorToAgentHeaders(new Headers({ cookie: "a=1; tuzy_warn=x; TUZY_s=y; b=2" }), meta);
    expect(out).toContainEqual(["cookie", "a=1; b=2"]);
  });

  it("drops the cookie header entirely when only tuzy_* cookies remain", () => {
    const out = visitorToAgentHeaders(new Headers({ cookie: "tuzy_warn=x" }), meta);
    expect(out.find(([k]) => k === "cookie")).toBeUndefined();
  });
});

describe("stripTuzyCookies", () => {
  it.each([
    ["a=1", "a=1"],
    ["tuzy_a=1", null],
    [" a=1 ;tuzy_x=2; ", "a=1"],
  ])("%j → %j", (input, want) => expect(stripTuzyCookies(input)).toBe(want));
});

describe("stripEdgeInternal", () => {
  it("removes cf-*, x-tuzy-*, cdn-loop and tuzy-skip-warning only", () => {
    const h = new Headers({ "cf-ipcountry": "VN", "x-tuzy-meta": "{}", "cdn-loop": "x", "tuzy-skip-warning": "1", accept: "*/*" });
    stripEdgeInternal(h);
    expect([...h.keys()]).toEqual(["accept"]);
  });
});

describe("agentToVisitorHeaders", () => {
  it("keeps duplicate set-cookie, drops tuzy_* cookies and hop-by-hop", () => {
    const h = agentToVisitorHeaders(
      [
        ["Set-Cookie", "a=1; Path=/"],
        ["set-cookie", "tuzy_warn=1"],
        ["set-cookie", "b=2"],
        ["transfer-encoding", "chunked"],
        ["connection", "close, x-hop"],
        ["x-hop", "1"],
        ["content-encoding", "gzip"],
      ],
      200,
    );
    expect(h.getSetCookie()).toEqual(["a=1; Path=/", "b=2"]);
    expect(h.get("transfer-encoding")).toBeNull();
    expect(h.get("x-hop")).toBeNull();
    expect(h.get("content-encoding")).toBe("gzip");
  });

  it("drops x-tuzy-* so the local app cannot spoof edge pages", () => {
    const h = agentToVisitorHeaders([["X-Tuzy-Edge", "1"], ["x-ok", "1"]], 502);
    expect([...h.keys()]).toEqual(["x-ok"]);
  });

  it("forwards only the subprotocol on 101", () => {
    const h = agentToVisitorHeaders(
      [
        ["sec-websocket-protocol", "chat"],
        ["x-other", "1"],
      ],
      101,
    );
    expect([...h.keys()]).toEqual(["sec-websocket-protocol"]);
  });

  it("drops invalid header values instead of throwing", () => {
    const h = agentToVisitorHeaders([["x-bad", "a\nb"], ["x-ok", "1"]], 200);
    expect(h.get("x-ok")).toBe("1");
  });
});

describe("restoreAcceptEncoding", () => {
  const rewritten = () => new Headers({ "accept-encoding": "gzip, br" });
  it("puts back the visitor's original value (SigV4 signs it)", () => {
    const h = rewritten();
    restoreAcceptEncoding(h, { clientAcceptEncoding: "identity" });
    expect(h.get("accept-encoding")).toBe("identity");
  });
  it("drops the header when the visitor sent an empty one", () => {
    const h = rewritten();
    restoreAcceptEncoding(h, { clientAcceptEncoding: "" });
    expect(h.has("accept-encoding")).toBe(false);
  });
  it("leaves it alone without cf or without a recorded original", () => {
    for (const cf of [undefined, {}]) {
      const h = rewritten();
      restoreAcceptEncoding(h, cf);
      expect(h.get("accept-encoding")).toBe("gzip, br");
    }
  });
});
