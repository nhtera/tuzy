import { describe, expect, it } from "vitest";
import { hasValidCookie, sanitizeTo, signCookie, wantsInterstitial, COOKIE_MAX_AGE } from "../../src/lib/interstitial";
import { reportedName } from "../../src/api/abuse-routes";
import { reporterPrefix } from "../../src/lib/abuse";

const h = (o: Record<string, string>) => new Headers(o);

describe("interstitial decision", () => {
  it.each([
    ["document navigation", "GET", { "sec-fetch-dest": "document", "sec-fetch-mode": "navigate" }, true],
    ["HEAD document", "HEAD", { "sec-fetch-dest": "document" }, true],
    ["fetch/XHR", "GET", { "sec-fetch-dest": "empty", "sec-fetch-mode": "cors" }, false],
    ["image", "GET", { "sec-fetch-dest": "image" }, false],
    ["iframe", "GET", { "sec-fetch-dest": "iframe" }, true],
    ["form POST navigation", "POST", { "sec-fetch-dest": "document" }, true],
    ["old browser GET html", "GET", { accept: "text/html,application/xhtml+xml" }, true],
    ["curl", "GET", { accept: "*/*" }, false],
    ["webhook POST", "POST", { "content-type": "application/json" }, false],
    ["html POST without fetch metadata", "POST", { accept: "text/html" }, false],
    ["fetch metadata without dest", "GET", { "sec-fetch-mode": "cors", accept: "text/html" }, false],
  ])("%s", (_n, method, headers, want) => {
    expect(wantsInterstitial(method as string, h(headers as Record<string, string>))).toBe(want);
  });
});

describe("continue target", () => {
  it.each([
    [null, "/"],
    ["", "/"],
    ["/", "/"],
    ["/app?x=1#y", "/app?x=1#y"],
    ["//evil.com", "/"],
    ["/\\evil.com", "/"],
    ["https://evil.com", "/"],
    ["javascript:alert(1)", "/"],
    ["evil.com", "/"],
    ["/ok\r\nset-cookie: x", "/"],
  ])("%s → %s", (to, want) => expect(sanitizeTo(to)).toBe(want));
});

describe("interstitial cookie", () => {
  const secret = "s3cret";
  const now = 1_800_000_000;
  it("is valid only for its own host and before expiry", async () => {
    const v = await signCookie(secret, "shop.tuzy.dev", now);
    const cookie = `a=b; tuzy_ok=${v}; c=d`;
    expect(await hasValidCookie(secret, "shop.tuzy.dev", cookie, now + 10)).toBe(true);
    expect(await hasValidCookie(secret, "SHOP.tuzy.dev", cookie, now + 10)).toBe(true);
    expect(await hasValidCookie(secret, "evil.tuzy.dev", cookie, now + 10)).toBe(false);
    expect(await hasValidCookie(secret, "shop.tuzy.dev", cookie, now + COOKIE_MAX_AGE + 1)).toBe(false);
    expect(await hasValidCookie("other", "shop.tuzy.dev", cookie, now + 10)).toBe(false);
  });
  it("rejects forged, malformed and far-future cookies", async () => {
    const v = await signCookie(secret, "shop.tuzy.dev", now);
    const [exp, mac] = v.split(".");
    expect(await hasValidCookie(secret, "shop.tuzy.dev", `tuzy_ok=${Number(exp) + 1}.${mac}`, now)).toBe(false);
    expect(await hasValidCookie(secret, "shop.tuzy.dev", `tuzy_ok=${exp}.${"0".repeat(64)}`, now)).toBe(false);
    expect(await hasValidCookie(secret, "shop.tuzy.dev", "tuzy_ok=garbage", now)).toBe(false);
    expect(await hasValidCookie(secret, "shop.tuzy.dev", null, now)).toBe(false);
    const far = await signCookie(secret, "shop.tuzy.dev", now + 10 * COOKIE_MAX_AGE);
    expect(await hasValidCookie(secret, "shop.tuzy.dev", `tuzy_ok=${far}`, now)).toBe(false);
  });
});

describe("abuse helpers", () => {
  it.each([
    ["shop", "shop"],
    ["SHOP.tuzy.dev", "shop"],
    ["https://shop.tuzy.dev/login?x=1", "shop"],
    ["shop.tuzy.dev/path", "shop"],
    ["https://evil.com", null],
    ["", null],
    ["a b", null],
  ])("reportedName(%s)", (input, want) => expect(reportedName(input, "tuzy.dev")).toBe(want));

  it("groups reporters by /24 and /48", () => {
    expect(reporterPrefix("203.0.113.7")).toBe("203.0.113.0/24");
    expect(reporterPrefix("::ffff:203.0.113.9")).toBe("203.0.113.0/24");
    expect(reporterPrefix("2001:db8:1:2:3:4:5:6")).toBe("2001:db8:1::/48");
  });
});
