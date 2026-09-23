import { describe, expect, it } from "vitest";
import { isValidTunnelLabel, normalizeHost, tunnelLabel } from "../../src/lib/host";

describe("normalizeHost", () => {
  it.each([
    ["Shop.Tuzy.Dev", "shop.tuzy.dev"],
    ["shop.localhost:8787", "shop.localhost"],
    ["shop.tuzy.dev.", "shop.tuzy.dev"],
    ["shop.tuzy.dev.:443", "shop.tuzy.dev"],
    ["[::1]:8787", "[::1]"],
    [" tuzy.dev ", "tuzy.dev"],
  ])("%s → %s", (input, want) => {
    expect(normalizeHost(input)).toBe(want);
  });

  it.each(["shop.tuzy.dev:abc", "shop.tuzy.dev:", "shop.tuzy.dev:123456", "[::1", "", ":80", "."])(
    "rejects malformed %j",
    (input) => {
      expect(normalizeHost(input)).toBeNull();
    },
  );
});

describe("isValidTunnelLabel", () => {
  it.each(["abc", "shop", "my-app-2", "a1b", "a".repeat(32)])("accepts %s", (l) => {
    expect(isValidTunnelLabel(l)).toBe(true);
  });

  it.each(["ab", "a".repeat(33), "-shop", "shop-", "sh_op", "sh op", "a/b", "user@shop", "xn--p1ai", "a--b", "SHOP", ""])(
    "rejects %j",
    (l) => {
      expect(isValidTunnelLabel(l)).toBe(false);
    },
  );
});

describe("tunnelLabel", () => {
  it.each([
    ["shop.tuzy.dev", "tuzy.dev", "shop"],
    ["SHOP.tuzy.dev:443", "tuzy.dev", "shop"],
    ["shop.tuzy.dev.", "tuzy.dev", "shop"],
    ["tuzy.dev", "tuzy.dev", ""],
    ["tuzy.dev.", "tuzy.dev", ""],
    ["shop.localhost:8787", "localhost", "shop"],
    ["a.b.tuzy.dev", "tuzy.dev", null],
    ["eviltuzy.dev", "tuzy.dev", null],
    ["tuzy.dev.evil.com", "tuzy.dev", null],
    ["example.com", "tuzy.dev", null],
    [".tuzy.dev", "tuzy.dev", null],
    ["sh_op.tuzy.dev", "tuzy.dev", null],
    ["-shop-.tuzy.dev", "tuzy.dev", null],
    ["xn--p1ai.tuzy.dev", "tuzy.dev", null],
    [`${"a".repeat(80)}.tuzy.dev`, "tuzy.dev", null],
    ["shop.tuzy.dev:abc", "tuzy.dev", null],
    ["[::1]:8787", "localhost", null],
  ])("%s under %s → %s", (host, base, want) => {
    expect(tunnelLabel(host, base)).toBe(want);
  });

  it("returns null for a missing host", () => {
    expect(tunnelLabel(null, "tuzy.dev")).toBeNull();
    expect(tunnelLabel("", "tuzy.dev")).toBeNull();
  });
});
