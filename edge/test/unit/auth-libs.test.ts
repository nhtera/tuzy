import { describe, expect, it } from "vitest";
import { isDisposable, isValidEmail, normalizeEmail } from "../../src/lib/email-validation";
import { ipPrefix, ipv4Slash24 } from "../../src/lib/ip-prefix";
import { codeMatches, generateCode, hashCode, normalizeCode } from "../../src/lib/otp";
import { generateToken, looksLikeToken } from "../../src/lib/tokens";

describe("otp", () => {
  it("generates 6-digit codes", () => {
    for (let i = 0; i < 200; i++) expect(generateCode()).toMatch(/^\d{6}$/);
  });
  it("normalizes user input", () => {
    expect(normalizeCode("482 913")).toBe("482913");
    expect(normalizeCode("482-913")).toBe("482913");
    expect(normalizeCode("48291")).toBeNull();
    expect(normalizeCode(482913)).toBeNull();
  });
  it("binds the hash to the login id", async () => {
    const h = await hashCode("a".repeat(32), "123456");
    expect(await codeMatches("a".repeat(32), "123456", h)).toBe(true);
    expect(await codeMatches("b".repeat(32), "123456", h)).toBe(false);
    expect(await codeMatches("a".repeat(32), "123457", h)).toBe(false);
  });
});

describe("ipPrefix", () => {
  it.each([
    ["203.0.113.9", "203.0.113.9/32"],
    ["2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"],
    ["2001:0db8:0001:0002:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"],
    ["::1", "0:0:0:0::/64"],
    ["::ffff:198.51.100.1", "198.51.100.1/32"],
    ["fe80::1%en0", "fe80:0:0:0::/64"],
    ["", "unknown"],
    ["999.1.1.1", "unknown"],
    ["gibberish", "unknown"],
  ])("%s → %s", (ip, want) => expect(ipPrefix(ip)).toBe(want));
  it("computes IPv4 /24", () => {
    expect(ipv4Slash24("203.0.113.9")).toBe("203.0.113.0/24");
    expect(ipv4Slash24("2001:db8::1")).toBeNull();
  });
});

describe("email validation", () => {
  it("normalizes and validates", () => {
    expect(normalizeEmail("  Foo@Example.COM ")).toBe("foo@example.com");
    expect(isValidEmail("a.b+tag@mail.example.co.uk")).toBe(true);
    for (const bad of ["a@b", "a@@b.com", "a b@c.com", "@c.com", "a@-c.com", "a@c..com"]) expect(isValidEmail(bad), bad).toBe(false);
  });
  it("blocks disposable domains and their subdomains", () => {
    expect(isDisposable("x@mailinator.com")).toBe(true);
    expect(isDisposable("x@eu.mailinator.com")).toBe(true);
    expect(isDisposable("x@gmail.com")).toBe(false);
  });
});

describe("tokens", () => {
  it("formats tokens", () => {
    const t = generateToken();
    expect(t).toMatch(/^tzy_[A-Za-z0-9_-]{43}$/);
    expect(looksLikeToken(t)).toBe(true);
    expect(looksLikeToken("tzy_short")).toBe(false);
    expect(looksLikeToken("Bearer x")).toBe(false);
  });
});
