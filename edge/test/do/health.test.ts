import { exports } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { api, login } from "./api-helpers";
import { FakeAgent, visit } from "./fake-agent";

describe("health & diagnose", () => {
  it("reports version and protocol range", async () => {
    const r = await api("/health");
    expect(r.status).toBe(200);
    expect(r.json).toMatchObject({ ok: true, min_proto: 1, max_proto: 1 });
    expect(typeof r.json.version).toBe("string");
  });

  it("echoes over a plain-Worker WebSocket and refuses non-upgrades", async () => {
    expect((await api("/diagnose/ws")).status).toBe(426);
    const res = await exports.default.fetch("https://tuzy.dev/api/v1/diagnose/ws", {
      headers: { host: "tuzy.dev", upgrade: "websocket", "cf-connecting-ip": "192.0.2.77" },
    });
    expect(res.status).toBe(101);
    const ws = res.webSocket!;
    ws.accept();
    const got = new Promise<string>((resolve) => ws.addEventListener("message", (e) => resolve(String(e.data))));
    ws.send("ping-123");
    expect(await got).toBe("ping-123");
    ws.close(1000, "bye");
  });
});

describe("interstitial opt-out header", () => {
  it("lets automation skip the browser warning, and never forwards the header", async () => {
    const { token } = await login(`skip${Date.now()}@example.com`);
    const agent = await FakeAgent.connect("skip-bot", { token });
    const nav = { "sec-fetch-dest": "document", "sec-fetch-mode": "navigate", accept: "text/html" };
    expect(await (await visit("skip-bot", "/", { headers: nav })).text()).toContain("You are about to visit a tunnel");
    const r = visit("skip-bot", "/", { headers: { ...nav, "tuzy-skip-warning": "true", "x-tuzy-skip-warning": "1" } });
    const req = await agent.request("/");
    const names = req.head.headers.map(([k]) => k);
    expect(names).not.toContain("tuzy-skip-warning");
    expect(names).not.toContain("x-tuzy-skip-warning");
    await agent.respond(req.id, 200, "app");
    expect(await (await r).text()).toBe("app");
    // A forged edge meta header alone doesn't skip it.
    expect(await (await visit("skip-bot", "/", { headers: { ...nav, "x-tuzy-skip-warning": "1" } })).text()).toContain("You are about to visit");
    agent.close();
  });
});

describe("install.sh", () => {
  it("serves the installer from the apex", async () => {
    const res = await exports.default.fetch("https://tuzy.dev/install.sh", { headers: { host: "tuzy.dev" } });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("shellscript");
    const body = await res.text();
    expect(body.startsWith("#!/bin/sh")).toBe(true);
    expect(body).toContain('REPO="https://github.com/nhtera/tuzy/releases"');
  });

  it("serves the Windows installer from the apex", async () => {
    const res = await exports.default.fetch("https://tuzy.dev/install.ps1", { headers: { host: "tuzy.dev" } });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    const body = await res.text();
    expect(body.startsWith("# tuzy installer for Windows")).toBe(true);
    expect(body).toContain("$repo = 'https://github.com/nhtera/tuzy/releases'");
  });
});
