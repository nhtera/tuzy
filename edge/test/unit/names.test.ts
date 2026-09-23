import { describe, expect, it } from "vitest";
import { autoName, checkName } from "../../src/lib/names";

describe("checkName", () => {
  it.each(["abc", "shop", "my-app-2", "a".repeat(32)])("accepts %s", (n) => expect(checkName(n)).toEqual({ ok: true }));

  it.each([
    ["ab", "invalid_name"],
    ["a".repeat(33), "invalid_name"],
    ["xn--p1ai", "invalid_name"],
    ["a--b", "invalid_name"],
    ["-abc", "invalid_name"],
    ["Shop", "invalid_name"],
    ["www", "reserved_name"],
    ["api", "reserved_name"],
    ["tuzy", "reserved_name"],
    ["paypal-login", "blocked_name"],
    ["my-bank", "blocked_name"],
    ["secure-signin", "blocked_name"],
  ])("rejects %s (%s)", (n, code) => {
    const r = checkName(n);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe(code);
  });
});

describe("autoName", () => {
  it("produces valid adjective-noun-NN names", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 500; i++) {
      const n = autoName();
      expect(n).toMatch(/^[a-z]+-[a-z]+-\d{2}$/);
      expect(checkName(n)).toEqual({ ok: true });
      seen.add(n);
    }
    expect(seen.size).toBeGreaterThan(490);
  });
});
