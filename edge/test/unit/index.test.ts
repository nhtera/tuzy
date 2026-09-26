import { exports } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { TEST_TOKEN } from "../setup";

const get = (url: string, host: string, init: RequestInit = {}) =>
  exports.default.fetch(url, { ...init, headers: { host, ...(init.headers as Record<string, string>) }, redirect: "manual" });

describe("worker routing", () => {
  it("serves the apex app", async () => {
    const res = await get("https://tuzy.dev/", "tuzy.dev");
    expect(res.status).toBe(200);
    expect(await res.text()).toContain("tuzy");
  });

  it("serves the health endpoint", async () => {
    const res = await get("https://tuzy.dev/api/v1/health", "tuzy.dev");
    expect(await res.json()).toMatchObject({ ok: true, min_proto: 1, max_proto: 1 });
  });

  it("returns JSON 404 for unknown API paths", async () => {
    const res = await get("https://tuzy.dev/api/v1/nope", "tuzy.dev");
    expect(res.status).toBe(404);
    expect(await res.json()).toMatchObject({ error: { code: "not_found" } });
  });

  it("redirects www to the apex", async () => {
    const res = await get("https://www.tuzy.dev/x?y=1", "www.tuzy.dev");
    expect(res.status).toBe(301);
    expect(res.headers.get("location")).toBe("https://tuzy.dev/x?y=1");
  });

  it("404s foreign and nested hosts", async () => {
    for (const host of ["example.com", "a.b.tuzy.dev", "sh_op.tuzy.dev"]) {
      const res = await get("https://x/", host);
      expect(res.status, host).toBe(404);
      expect(res.headers.get("x-tuzy-edge")).toBe("1");
    }
  });

  it("serves the offline page for a never-connected tunnel", async () => {
    const res = await get("https://ghost.tuzy.dev/", "ghost.tuzy.dev");
    expect(res.status).toBe(502);
    expect(res.headers.get("x-tuzy-edge")).toBe("1");
  });

  it("answers non-browsers with one plain-text line, browsers with the page", async () => {
    const BROWSER = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8";
    for (const accept of [undefined, "*/*", "application/json"]) {
      const res = await get("https://ghost.tuzy.dev/", "ghost.tuzy.dev", accept ? { headers: { accept } } : {});
      expect(res.status, accept).toBe(502);
      expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
      expect(res.headers.get("tuzy-error")).toBe("TUZY-502-AGENT-OFFLINE");
      expect(res.headers.get("retry-after")).toBe("5");
      expect(res.headers.get("x-tuzy-edge")).toBe("1");
      expect(res.headers.get("content-security-policy")).toBeNull();
      expect(await res.text()).toBe("tuzy: tunnel offline (TUZY-502-AGENT-OFFLINE)\n");
    }
    const page = await get("https://ghost.tuzy.dev/", "ghost.tuzy.dev", { headers: { accept: BROWSER } });
    expect(page.status).toBe(502);
    expect(page.headers.get("content-type")).toBe("text/html; charset=utf-8");
    expect(await page.text()).toContain("TUZY-502-AGENT-OFFLINE");

    const notFound = await get("https://x/", "example.com");
    expect(await notFound.text()).toBe("tuzy: tunnel not found (TUZY-404-TUNNEL-NOT-FOUND)\n");
  });

  it("leaves API errors alone", async () => {
    const res = await get("https://tuzy.dev/api/v1/nope", "tuzy.dev");
    expect(res.headers.get("tuzy-error")).toBeNull();
    expect(await res.json()).toMatchObject({ error: { code: "not_found" } });
  });

  it("reserves /__tuzy/ on tunnel hosts", async () => {
    const res = await get("https://ghost.tuzy.dev/__tuzy/x", "ghost.tuzy.dev");
    expect(res.status).toBe(404);
  });
});

describe("connect endpoint validation", () => {
  const connect = (qs: string, headers: Record<string, string> = {}) =>
    get(`https://tuzy.dev/api/v1/connect?${qs}`, "tuzy.dev", {
      headers: { upgrade: "websocket", authorization: `Bearer ${TEST_TOKEN}`, ...headers },
    });

  it("requires a WebSocket upgrade", async () => {
    const res = await get("https://tuzy.dev/api/v1/connect?name=shop&instance=aaaaaaaaaaaaaaaa", "tuzy.dev");
    expect(res.status).toBe(400);
    expect(await res.json()).toMatchObject({ error: { code: "websocket_required" } });
  });

  it("rejects a missing or wrong token with 401", async () => {
    expect((await connect("name=shop&instance=aaaaaaaaaaaaaaaa", { authorization: "" })).status).toBe(401);
    expect((await connect("name=shop&instance=aaaaaaaaaaaaaaaa", { authorization: "Bearer nope" })).status).toBe(401);
  });

  it("rejects invalid names and instances with 400", async () => {
    expect(await (await connect("name=a--b&instance=aaaaaaaaaaaaaaaa")).json()).toMatchObject({ error: { code: "invalid_name" } });
    expect(await (await connect("name=shop&instance=short")).json()).toMatchObject({ error: { code: "invalid_instance" } });
    expect(await (await connect(`name=shop&instance=${"a".repeat(65)}`)).json()).toMatchObject({ error: { code: "invalid_instance" } });
  });
});
