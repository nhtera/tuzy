import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { markerKey } from "../../src/lib/connected-marker";
import { setNameSuspended } from "../../src/lib/moderation";
import { pushOutbox } from "../../src/lib/outbox";
import { monthKey } from "../../src/lib/usage";
import { FrameType } from "../../src/protocol/frames";
import { dailyCron } from "../../src/scheduled";
import { api, login } from "./api-helpers";
import { FakeAgent, sleep, visit } from "./fake-agent";

let n = 0;
const email = () => `ab${n++}.${Date.now()}@example.com`;
const now = () => Math.floor(Date.now() / 1000);
const DOC = { "sec-fetch-dest": "document", "sec-fetch-mode": "navigate", accept: "text/html" };
/** A user's click on the interstitial's "Visit site" link. */
const CLICK = { "sec-fetch-dest": "document", "sec-fetch-mode": "navigate", "sec-fetch-site": "same-origin", "sec-fetch-user": "?1" };

async function admin(): Promise<string> {
  const a = await login(email());
  await env.DB.prepare("UPDATE users SET role = 'admin' WHERE id = ?1").bind(a.userId).run();
  return a.token;
}

async function serveOnce(agent: FakeAgent, path: string, body = "app"): Promise<void> {
  const req = await agent.request(path);
  await agent.respond(req.id, 200, body, [["content-type", "text/html"]]);
}

async function waitFor(pred: () => Promise<boolean>, ms = 5000): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await pred()) return;
    await sleep(50);
  }
  throw new Error("condition not met in time");
}

describe("interstitial", () => {
  it("shows once for browser navigations on untrusted tunnels; webhooks pass straight through", async () => {
    const { token } = await login(email());
    const agent = await FakeAgent.connect("inter-new", { token });

    const page = await visit("inter-new", "/dash?x=1", { headers: DOC });
    expect(page.status).toBe(200);
    expect(page.headers.get("x-tuzy-edge")).toBe("1");
    const html = await page.text();
    expect(html).toContain("You are about to visit a tunnel");
    expect(html).toContain('href="/__tuzy/continue?to=%2Fdash%3Fx%3D1"');
    expect(html).toContain("tuzy.dev/abuse?name=inter-new");
    expect(html).toContain("accounts younger than 7 days"); // the policy is stated on the page itself
    expect(html).toContain("/aup#browser-warning");

    // A webhook (POST JSON) and an API fetch reach the app, marked noindex.
    const hook = visit("inter-new", "/webhook", { method: "POST", headers: { "content-type": "application/json" }, body: "{}" });
    await serveOnce(agent, "/webhook");
    const hookRes = await hook;
    expect(hookRes.status).toBe(200);
    expect(hookRes.headers.get("x-robots-tag")).toBe("noindex");
    const xhr = visit("inter-new", "/api", { headers: { "sec-fetch-dest": "empty", "sec-fetch-mode": "cors" } });
    await serveOnce(agent, "/api");
    expect((await xhr).status).toBe(200);

    // A cross-site link (or a sibling tunnel's <img>) to continue mints nothing.
    for (const h of [
      { ...CLICK, "sec-fetch-site": "cross-site" } as Record<string, string>,
      { ...CLICK, "sec-fetch-site": "same-site", "sec-fetch-dest": "image" },
      {},
      { referer: "https://evil.tuzy.dev/" },
    ]) {
      const r = await visit("inter-new", "/__tuzy/continue?to=%2Fdash", { headers: h });
      expect(r.status).toBe(302);
      expect(r.headers.get("set-cookie")).toBeNull();
    }
    // An old browser without fetch metadata: a same-origin Referer counts as the click.
    const legacy = await visit("inter-new", "/__tuzy/continue?to=%2F", { headers: { referer: "https://inter-new.tuzy.dev/dash" } });
    expect(legacy.headers.get("set-cookie")).toMatch(/^tuzy_ok=/);

    // Continue: host-bound cookie, safe redirect only.
    const cont = await visit("inter-new", "/__tuzy/continue?to=%2Fdash%3Fx%3D1", { headers: CLICK });
    expect(cont.status).toBe(302);
    expect(cont.headers.get("location")).toBe("/dash?x=1");
    const setCookie = cont.headers.get("set-cookie")!;
    expect(setCookie).toMatch(/^tuzy_ok=\d+\.[0-9a-f]{64}; Max-Age=604800; Path=\/; Secure; HttpOnly; SameSite=Lax$/);
    expect((await visit("inter-new", "/__tuzy/continue?to=//evil.com", { headers: CLICK })).headers.get("location")).toBe("/");
    expect((await visit("inter-new", "/__tuzy/other")).status).toBe(404);

    // Auto-submitted cross-site forms and frames are navigations too.
    const post = await visit("inter-new", "/login", { method: "POST", headers: { ...DOC, "sec-fetch-site": "cross-site" }, body: "u=x" });
    expect(await post.text()).toContain("You are about to visit a tunnel");
    const framed = await visit("inter-new", "/", { headers: { "sec-fetch-dest": "iframe" } });
    expect(framed.headers.get("x-frame-options")).toBe("DENY");

    const cookie = setCookie.split(";")[0]!;
    // A shadowing cookie planted by a sibling tunnel (Domain=tuzy.dev) doesn't hide ours.
    const shadowed = visit("inter-new", "/", { headers: { ...DOC, cookie: `tuzy_ok=planted; ${cookie}` } });
    await serveOnce(agent, "/");
    expect(await (await shadowed).text()).toBe("app");
    const through = visit("inter-new", "/dash?x=1", { headers: { ...DOC, cookie } });
    const req = await agent.request("/dash");
    expect(req.head.headers.some(([k, v]: [string, string]) => k === "cookie" && v.includes("tuzy_ok"))).toBe(false); // never reaches the app
    await agent.respond(req.id, 200, "app");
    expect(await (await through).text()).toBe("app");

    // The cookie of one tunnel doesn't work on another.
    const other = await FakeAgent.connect("inter-other", { token });
    const blocked = await visit("inter-other", "/", { headers: { ...DOC, cookie } });
    expect(await blocked.text()).toContain("You are about to visit a tunnel");
    agent.close();
    other.close();
  });

  it("is still shown for a 6-day-old account", async () => {
    const u = await login(email());
    await env.DB.prepare("UPDATE users SET created_at = ?2 WHERE id = ?1").bind(u.userId, now() - 6 * 86400).run();
    const agent = await FakeAgent.connect("inter-six", { token: u.token });
    expect(await (await visit("inter-six", "/", { headers: DOC })).text()).toContain("You are about to visit a tunnel");
    agent.close();
  });

  it("is skipped for accounts 7+ days old and comes back live when an admin untrusts them", async () => {
    const adminToken = await admin();
    const u = await login(email());
    await env.DB.prepare("UPDATE users SET created_at = ?2 WHERE id = ?1").bind(u.userId, now() - 7 * 86400 - 60).run();
    const agent = await FakeAgent.connect("inter-old", { token: u.token });
    const r = visit("inter-old", "/", { headers: DOC });
    await serveOnce(agent, "/");
    const res = await r;
    expect(await res.text()).toBe("app");
    expect(res.headers.get("x-robots-tag")).toBeNull();

    expect((await api(`/admin/users/${u.userId}/untrust`, { token: adminToken, body: {} })).json).toMatchObject({ changed: true });
    await waitFor(async () => (await (await visit("inter-old", "/", { headers: DOC })).text()).includes("You are about to visit"));
    agent.close();
  });
});

describe("admin", () => {
  it("is admin-only, and never with a connect-scope token", async () => {
    const { token } = await login(email());
    expect((await api("/admin/reports", { token })).status).toBe(403);
    expect((await api("/admin/names/x/suspend", { token, body: {} })).status).toBe(403);
    const adminToken = await admin();
    const ci = await api("/tokens", { token: adminToken, body: { scope: "connect", label: "ci" } });
    expect((await api("/admin/reports", { token: ci.json.token })).json.error.code).toBe("insufficient_scope");
  });

  it("sets the quota for new accounts and reads the audit log", async () => {
    const adminToken = await admin();
    expect((await api("/admin/settings/new-account-max-names", { token: adminToken, body: { max: 2 } })).status).toBe(200);
    try {
      const u = await login(email());
      expect((await api("/names", { token: u.token })).json.limit).toBe(2);
    } finally {
      await api("/admin/settings/new-account-max-names", { token: adminToken, body: { max: 10 } });
    }
    const log = await api("/admin/audit?action=settings.&limit=5", { token: adminToken });
    expect(log.json.entries[0]).toMatchObject({ action: "settings.new_account_max_names", target: "10" });
  });

  it("suspend-name drops the tunnel and serves 451 within seconds; unsuspend restores it", async () => {
    const adminToken = await admin();
    const { token } = await login(email());
    const agent = await FakeAgent.connect("susp-live", { token });
    const t0 = Date.now();
    expect((await api("/admin/names/susp-live/suspend", { token: adminToken, body: { reason: "phishing" } })).json).toMatchObject({ changed: true });
    await agent.waitClosed(5000);
    expect(agent.frames.find((f) => f.type === FrameType.GOAWAY)?.json).toMatchObject({ reason: "suspended" });
    expect((await visit("susp-live", "/")).status).toBe(451);
    expect(Date.now() - t0).toBeLessThan(5000);
    const refused = await FakeAgent.open("susp-live", { token, reserve: false });
    expect(refused.res.status).toBe(403);

    expect((await api("/admin/names/susp-live/unsuspend", { token: adminToken, body: {} })).json).toMatchObject({ changed: true });
    const again = await FakeAgent.connect("susp-live", { token, reserve: false });
    const r = visit("susp-live", "/");
    await serveOnce(again, "/");
    expect((await r).status).toBe(200);
    again.close();

    const audit = await env.DB.prepare("SELECT action FROM audit_log WHERE target = 'susp-live' ORDER BY created_at").all<{ action: string }>();
    expect(audit.results.map((a) => a.action)).toEqual(expect.arrayContaining(["name.suspend", "name.unsuspend"]));
  });

  it("converges through the outbox when the immediate push didn't happen", async () => {
    const { token } = await login(email());
    const agent = await FakeAgent.connect("susp-late", { token });
    const out = await setNameSuspended(env, "susp-late", true, { userId: null }); // no push (as if the DO call failed)
    expect(out.outboxIds).toHaveLength(1);
    const r = visit("susp-late", "/");
    await serveOnce(agent, "/");
    expect((await r).status).toBe(200); // DO doesn't know yet
    await pushOutbox(env); // the */5 cron
    expect((await visit("susp-late", "/")).status).toBe(451);
    // Unsuspend without a push, then the cron converges again.
    await setNameSuspended(env, "susp-late", false, { userId: null });
    await pushOutbox(env);
    expect((await visit("susp-late", "/")).status).toBe(502); // offline, no longer suspended
  });

  it("block-name removes the reservation and keeps everyone out; release lifts it", async () => {
    const adminToken = await admin();
    const u = await login(email());
    expect((await api("/names", { token: u.token, body: { name: "brandy-shop" } })).status).toBe(201);
    expect((await api("/admin/names/brandy-shop/block", { token: adminToken, body: {} })).status).toBe(200);
    expect((await api("/names", { token: u.token })).json.names).toHaveLength(0);
    const other = await login(email());
    expect((await api("/names", { token: other.token, body: { name: "brandy-shop" } })).json.error.code).toBe("name_on_hold");
    expect((await api("/admin/names/brandy-shop/release", { token: adminToken, body: {} })).json).toMatchObject({ changed: true });
    expect((await api("/names", { token: other.token, body: { name: "brandy-shop" } })).status).toBe(201);
  });

  it("reserve-for-user, set-max-names, suspend-user by email", async () => {
    const adminToken = await admin();
    const e = email();
    const u = await login(e);
    expect((await api("/admin/users/" + encodeURIComponent(e) + "/max-names", { token: adminToken, body: { max: 0 } })).json).toMatchObject({ max_names: 0 });
    expect((await api("/names", { token: u.token, body: { name: "quota-zero" } })).json.error.code).toBe("quota_exceeded");
    expect((await api("/admin/names/vip-name/reserve", { token: adminToken, body: { user: e } })).status).toBe(201);
    expect((await api("/names", { token: u.token })).json.names.map((x: { name: string }) => x.name)).toEqual(["vip-name"]);
    const agent = await FakeAgent.connect("vip-name", { token: u.token, reserve: false });
    expect((await api(`/admin/users/${u.userId}/suspend`, { token: adminToken, body: {} })).json).toMatchObject({ changed: true });
    await agent.waitClosed(5000);
    expect((await api("/me", { token: u.token })).json.error.code).toBe("account_suspended");
    expect((await api(`/admin/users/${u.userId}/unsuspend`, { token: adminToken, body: {} })).json).toMatchObject({ changed: true });
    expect((await api("/me", { token: u.token })).status).toBe(200);
  });

  it("outbox status", async () => {
    const adminToken = await admin();
    const r = await api("/admin/outbox", { token: adminToken });
    expect(r.status).toBe(200);
    expect(r.json).toHaveProperty("pending");
  });
});

describe("abuse reports", () => {
  const report = (name: string, ip: string, extra: Record<string, unknown> = {}, token?: string) =>
    api("/abuse", { ip, token, body: { name, category: "phishing", details: "fake bank login", ...extra } });

  it("validates input, needs JSON, and silently accepts honeypot hits", async () => {
    const { token } = await login(email());
    await api("/names", { token, body: { name: "valid-target" } });
    expect((await report("nope-nothing-here", "203.0.100.1")).status).toBe(404);
    expect((await report("valid-target", "203.0.100.2", { category: "meh" })).status).toBe(400);
    expect((await report("https://valid-target.tuzy.dev/login", "203.0.100.3")).status).toBe(202);
    const hp = await report("valid-target", "203.0.100.4", { website: "spam" });
    expect(hp.status).toBe(202);
    expect(hp.json.id).toBeUndefined();
    const { exports } = await import("cloudflare:workers");
    const form = await exports.default.fetch("https://tuzy.dev/api/v1/abuse", {
      method: "POST",
      headers: { host: "tuzy.dev", "content-type": "application/x-www-form-urlencoded", "cf-connecting-ip": "203.0.100.5" },
      body: "name=valid-target&category=phishing",
    });
    expect(form.status).toBe(415);
  });

  it("quarantines a new account's name after 3 distinct networks; one /24 isn't enough", async () => {
    const { token, userId } = await login(email());
    const agent = await FakeAgent.connect("phish-one", { token });
    for (const ip of ["198.18.1.1", "198.18.1.2", "198.18.1.3"]) expect((await report("phish-one", ip)).status).toBe(202);
    expect((await env.DB.prepare("SELECT status FROM reservations WHERE name = 'phish-one'").first<{ status: string }>())!.status).toBe("active");
    // Different /64s inside one /48 count once.
    await report("phish-one", "2001:db8:aa:1::1");
    await report("phish-one", "2001:db8:aa:2::1");
    expect((await env.DB.prepare("SELECT status FROM reservations WHERE name = 'phish-one'").first<{ status: string }>())!.status).toBe("active");
    await report("phish-one", "198.18.2.1");
    await agent.waitClosed(5000);
    expect((await visit("phish-one", "/")).status).toBe(451);
    const u = await env.DB.prepare("SELECT trust_revoked, status FROM users WHERE id = ?1").bind(userId).first<{ trust_revoked: number; status: string }>();
    expect(u).toMatchObject({ trust_revoked: 1, status: "active" });

    // A second quarantined name suspends the whole account.
    await api("/names", { token, body: { name: "phish-two" } });
    for (const ip of ["198.19.1.1", "198.19.2.1", "198.19.3.1"]) await report("phish-two", ip);
    expect((await env.DB.prepare("SELECT status FROM users WHERE id = ?1").bind(userId).first<{ status: string }>())!.status).toBe("suspended");
  });

  it("weighs logged-in reporters double and spares accounts older than 7 days", async () => {
    const owner = await login(email());
    await api("/names", { token: owner.token, body: { name: "weighed-one" } });
    const fresh = await login(email());
    await report("weighed-one", "198.20.9.1", {}, fresh.token); // a brand-new account weighs 1
    const reporter = await login(email());
    await env.DB.prepare("UPDATE users SET created_at = ?2 WHERE id = ?1").bind(reporter.userId, now() - 8 * 86400).run();
    await report("weighed-one", "198.20.1.1", {}, reporter.token); // weight 2
    // 1 + 2 = 3 → quarantined
    expect((await env.DB.prepare("SELECT status FROM reservations WHERE name = 'weighed-one'").first<{ status: string }>())!.status).toBe("suspended");

    const veteran = await login(email());
    await env.DB.prepare("UPDATE users SET created_at = ?2 WHERE id = ?1").bind(veteran.userId, now() - 8 * 86400).run();
    await api("/names", { token: veteran.token, body: { name: "veteran-app" } });
    for (const ip of ["198.21.1.1", "198.21.2.1", "198.21.3.1", "198.21.4.1"]) await report("veteran-app", ip);
    expect((await env.DB.prepare("SELECT status FROM reservations WHERE name = 'veteran-app'").first<{ status: string }>())!.status).toBe("active");
  });

  it("an upheld report revokes the owner's auto-trust", async () => {
    const adminToken = await admin();
    const owner = await login(email());
    await env.DB.prepare("UPDATE users SET created_at = ?2 WHERE id = ?1").bind(owner.userId, now() - 60 * 86400).run();
    await api("/names", { token: owner.token, body: { name: "upheld-app" } });
    const r = await report("upheld-app", "198.22.1.1");
    const list = await api("/admin/reports", { token: adminToken });
    expect(list.json.reports.some((x: { id: string }) => x.id === r.json.id)).toBe(true);
    expect((await api(`/admin/reports/${r.json.id}`, { token: adminToken, body: { status: "actioned" } })).json).toMatchObject({ trust_revoked: true });
    expect((await api("/me", { token: owner.token })).json.user.trusted).toBe(false);
  });
});

describe("unsuspend", () => {
  it("clears the reports that caused a quarantine, so one more report doesn't re-quarantine", async () => {
    const adminToken = await admin();
    const { token } = await login(email());
    await api("/names", { token, body: { name: "griefed-app" } });
    const rep = (ip: string) => api("/abuse", { ip, body: { name: "griefed-app", category: "spam" } });
    for (const ip of ["198.30.1.1", "198.30.2.1", "198.30.3.1"]) await rep(ip);
    const status = async () => (await env.DB.prepare("SELECT status FROM reservations WHERE name = 'griefed-app'").first<{ status: string }>())!.status;
    expect(await status()).toBe("suspended");
    await api("/admin/names/griefed-app/unsuspend", { token: adminToken, body: {} });
    await rep("198.30.4.1");
    expect(await status()).toBe("active");
    const open = await env.DB.prepare("SELECT COUNT(*) AS n FROM abuse_reports WHERE name = 'griefed-app' AND status = 'open'").first<{ n: number }>();
    expect(open!.n).toBe(1);
  });
});

describe("long-stream budget", () => {
  it("cuts long streams once the account's monthly budget is spent (across names)", async () => {
    const u = await login(email());
    await env.DB.prepare("INSERT INTO usage_monthly (user_id, month, long_stream_seconds) VALUES (?1, ?2, 180000)").bind(u.userId, monthKey()).run();
    const agent = await FakeAgent.connect("budget-out", { token: u.token });
    const v = visit("budget-out", "/sse");
    const req = await agent.request("/sse");
    agent.head(req.id, 200, [["content-type", "text/event-stream"]]);
    await v;
    const reset = await agent.next((f) => f.type === FrameType.RESET && f.streamId === req.id, 8000);
    expect(reset.json).toMatchObject({ code: "long_stream_budget" });
    agent.close();
  });

  it("accrues time past the first long-stream window and reports it in /me", async () => {
    const u = await login(email());
    const agent = await FakeAgent.connect("budget-in", { token: u.token });
    const v = visit("budget-in", "/sse");
    const req = await agent.request("/sse");
    agent.head(req.id, 200, [["content-type", "text/event-stream"]]);
    await v;
    await sleep(4800); // LONG_STREAM_AFTER is 3 s in tests → ~1.8 s accrued
    agent.end(req.id);
    await waitFor(async () => (((await api("/me", { token: u.token })).json.usage?.long_stream_seconds as number) ?? 0) >= 1);
    const me = await api("/me", { token: u.token });
    expect(me.json.usage).toMatchObject({ month: monthKey(), long_stream_budget_seconds: 180000 });
    expect(agent.frames.some((f) => f.type === FrameType.RESET && f.streamId === req.id)).toBe(false);
    agent.close();
  });
});

describe("holds and audit", () => {
  it("caps active holds per user (rename refused, remove releases without a hold)", async () => {
    const u = await login(email());
    const values = Array.from({ length: 20 }, (_, i) => `('held-cap-${i}', '${u.userId}', ${now() + 86400}, NULL)`).join(",");
    await env.DB.prepare(`INSERT INTO released_names (name, held_for, until, renamed_to) VALUES ${values}`).run();
    await api("/names", { token: u.token, body: { name: "cap-live" } });
    expect((await api("/names/cap-live", { method: "PATCH", token: u.token, body: { rename: "cap-new" } })).json.error.code).toBe("hold_quota");
    // Removing still works at the cap, just without a hold.
    expect((await api("/names/cap-live", { method: "DELETE", token: u.token })).status).toBe(204);
    expect(await env.DB.prepare("SELECT 1 FROM released_names WHERE name = 'cap-live'").first()).toBeNull();
  });

  it("writes an audit row for every account mutation", async () => {
    const u = await login(email());
    await api("/names", { token: u.token, body: { name: "audit-a" } });
    await api("/names/audit-a", { method: "PATCH", token: u.token, body: { rename: "audit-b" } });
    await api("/names/audit-b", { method: "PATCH", token: u.token, body: { default: true } });
    const t = await api("/tokens", { token: u.token, body: { scope: "connect", label: "ci" } });
    await api(`/tokens/${t.json.id}`, { method: "DELETE", token: u.token });
    await api("/names/audit-b", { method: "DELETE", token: u.token });
    const rows = await env.DB.prepare("SELECT action FROM audit_log WHERE actor_user_id = ?1 ORDER BY created_at, rowid").bind(u.userId).all<{ action: string }>();
    expect(rows.results.map((r) => r.action)).toEqual(["auth.login", "name.reserve", "name.rename", "name.default", "token.create", "token.revoke", "name.remove"]);
  });
});

describe("daily cron", () => {
  it("cleans up, erases deleted accounts' emails after 30 days and re-pushes suspensions", async () => {
    const t = now();
    const old = t - 40 * 86400;
    await env.DB.batch([
      env.DB.prepare("INSERT INTO users (id, email, status, created_at, deleted_at) VALUES ('usr_gone0000000000001', 'gone@example.com', 'deleted', ?1, ?1)").bind(old),
      env.DB.prepare("INSERT INTO users (id, email, status, created_at, deleted_at) VALUES ('usr_gone0000000000002', 'recent@example.com', 'deleted', ?1, ?2)").bind(old, t - 86400),
      env.DB.prepare(
        "INSERT INTO login_codes (id, email, mailbox, code_hash, ip_prefix, created_at, expires_at, last_sent_at) VALUES ('lc_old', 'x@example.com', 'x@example.com', 'h', 'p', ?1, ?1, ?1)",
      ).bind(t - 2 * 86400),
      env.DB.prepare("INSERT INTO released_names (name, held_for, until) VALUES ('expired-hold', 'usr_x', ?1), ('blocked-forever', '__blocked__', ?1)").bind(t - 10),
      env.DB.prepare("INSERT INTO audit_log (id, action, created_at) VALUES ('aud_ancient', 'x', ?1)").bind(t - 200 * 86400),
      env.DB.prepare("INSERT INTO usage_monthly (user_id, month, long_stream_seconds) VALUES ('usr_x', '2020-01', 5)"),
    ]);
    // A suspended, connected name whose DO lost the flag.
    const { token } = await login(email());
    const agent = await FakeAgent.connect("recon-me", { token });
    await env.DB.prepare("UPDATE reservations SET status = 'suspended' WHERE name = 'recon-me'").run();
    expect(await env.NAMES_KV.get(markerKey("recon-me"))).not.toBeNull();

    // The same address deleted twice (erased once already) must not break the cron.
    await env.DB.batch([
      env.DB.prepare("INSERT INTO users (id, email, status, created_at, deleted_at) VALUES ('usr_twice000000000001', 'twice@example.com', 'deleted', ?1, ?1)").bind(old),
    ]);
    await dailyCron(env);
    await env.DB.prepare("INSERT INTO users (id, email, status, created_at, deleted_at) VALUES ('usr_twice000000000002', 'twice@example.com', 'deleted', ?1, ?1)").bind(old).run();
    const out = await dailyCron(env);
    expect(out.erased).toBeGreaterThanOrEqual(1);
    const twice = await env.DB.prepare("SELECT email FROM users WHERE id LIKE 'usr_twice%' ORDER BY id").all<{ email: string }>();
    expect(twice.results.every((u) => u.email.startsWith("deleted:usr_twice"))).toBe(true);
    expect(out.repushed).toBeGreaterThanOrEqual(1);
    const emails = await env.DB.prepare("SELECT id, email FROM users WHERE id LIKE 'usr_gone%' ORDER BY id").all<{ email: string }>();
    expect(emails.results[0]!.email).toMatch(/^deleted:usr_gone0000000000001:[0-9a-f]{64}$/);
    expect(emails.results[1]!.email).toBe("recent@example.com");
    expect(await env.DB.prepare("SELECT 1 FROM login_codes WHERE id = 'lc_old'").first()).toBeNull();
    expect(await env.DB.prepare("SELECT 1 FROM released_names WHERE name = 'expired-hold'").first()).toBeNull();
    expect(await env.DB.prepare("SELECT 1 FROM released_names WHERE name = 'blocked-forever'").first()).not.toBeNull();
    expect(await env.DB.prepare("SELECT 1 FROM audit_log WHERE id = 'aud_ancient'").first()).toBeNull();
    expect(await env.DB.prepare("SELECT 1 FROM usage_monthly WHERE month = '2020-01'").first()).toBeNull();
    await agent.waitClosed(5000);
    expect((await visit("recon-me", "/")).status).toBe(451);
  });
});

describe("site pages", () => {
  it("serves landing, legal pages, the abuse form and security.txt", async () => {
    const { exports } = await import("cloudflare:workers");
    const get = (p: string) => exports.default.fetch(`https://tuzy.dev${p}`, { headers: { host: "tuzy.dev" } });
    for (const p of ["/", "/terms", "/privacy", "/aup", "/abuse?name=<x>"]) {
      const r = await get(p);
      expect(r.status, p).toBe(200);
      expect(r.headers.get("content-security-policy")).toContain("script-src 'self'");
      const html = await r.text();
      expect(html).not.toContain("<x>");
    }
    expect(await (await get("/privacy")).text()).toContain("tuzy account delete");
    const sec = await (await get("/.well-known/security.txt")).text();
    expect(sec).toMatch(/^Contact: mailto:security@tuzy\.dev$/m);
    expect(sec).toMatch(/^Expires: \d{4}-\d\d-\d\dT/m);
    expect((await get("/abuse.js")).headers.get("content-type")).toContain("javascript");
    const site = await get("/site.js");
    expect(site.headers.get("content-type")).toContain("javascript");
    expect(await site.text()).toContain("clipboard");
    const landing = await (await get("/")).text();
    expect(landing).toContain('src="/site.js"');
    expect(landing).toContain("accounts younger than 7 days");
    expect(landing).not.toMatch(/[\u2013\u2014]/); // no en/em dashes in site copy
  });
});
