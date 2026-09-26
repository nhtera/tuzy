import { describe, expect, it } from "vitest";
import { STATUS_PAGES, statusPage, type StatusPage } from "../../src/pages/status-pages";
import { resetPage } from "../../src/protocol/http-stream";

const keys = Object.keys(STATUS_PAGES) as StatusPage[];
const TRANSIENT: StatusPage[] = ["offline", "draining", "busy", "rate_limited"];
const WITH_PATH: StatusPage[] = ["offline", "draining", "bad_gateway", "timeout"];

describe("status pages", () => {
  it.each(keys)("%s: status, code header, lock-down headers", async (key) => {
    const { status, code } = STATUS_PAGES[key];
    const res = statusPage(key);
    expect(res.status).toBe(status);
    expect(code).toMatch(new RegExp(`^TUZY-${status}-[A-Z-]+$`));
    expect(res.headers.get("tuzy-error")).toBe(code);
    expect(res.headers.get("x-tuzy-edge")).toBe("1");
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(res.headers.get("content-security-policy")).toContain("default-src 'none'");
    expect(res.headers.get("x-content-type-options")).toBe("nosniff");
    expect(res.headers.get("retry-after") !== null).toBe(TRANSIENT.includes(key));

    const html = await res.text();
    expect(html).toContain(`${status} · ${code}`);
    expect(html).toContain(`limits-and-faq.md#${code.toLowerCase()}`);
    expect(html.includes('class="path"')).toBe(WITH_PATH.includes(key));
    expect(html).not.toContain("<script");
    expect(new TextEncoder().encode(html).byteLength).toBeLessThan(8 * 1024);
  });

  it("codes are unique", () => {
    const codes = keys.map((k) => STATUS_PAGES[k].code);
    expect(new Set(codes).size).toBe(codes.length);
  });

  it("breaks the path at the agent when it is offline, at the service on a timeout", async () => {
    const offline = await statusPage("offline").text();
    expect(offline).toMatch(/<li class="down"><span class="x"[^>]*>.*?tuzy agent/);
    const timeout = await statusPage("timeout").text();
    expect(timeout).not.toMatch(/<li class="down">.{0,400}tuzy agent/);
    expect(timeout).toMatch(/<li class="down"><span class="x"[^>]*>.*?Your service/);
  });

  it("escapes the detail", async () => {
    const html = await statusPage("bad_gateway", "<script>alert(1)</script>").text();
    expect(html).toContain("&lt;script&gt;");
    expect(html).not.toContain("<script>alert");
  });

  it("maps early resets to the timeout page, everything else to bad gateway", () => {
    for (const code of ["head_timeout", "credit_timeout", "stream_timeout", "long_stream_budget"] as const) {
      expect(resetPage(code)).toBe("timeout");
    }
    expect(resetPage("protocol_error")).toBe("bad_gateway");
    expect(resetPage("cancelled")).toBe("bad_gateway");
  });
});
