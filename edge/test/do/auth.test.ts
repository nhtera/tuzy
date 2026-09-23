import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { reserveCodeSlot } from "../../src/api/login-codes";
import { canonicalMailbox } from "../../src/lib/email-validation";
import { sentEmails } from "../../src/lib/email";
import { api, lastCode, login } from "./api-helpers";

let n = 0;
const email = (tag = "u") => `${tag}${n++}.${Date.now()}@example.com`;

describe("email login", () => {
  it("first login creates the account and returns a working token", async () => {
    const e = email();
    const { token, userId } = await login(e);
    expect(token).toMatch(/^tzy_[A-Za-z0-9_-]{43}$/);
    const me = await api("/me", { token });
    expect(me.status).toBe(200);
    expect(me.json.user).toMatchObject({ id: userId, email: e, max_names: 10 });
    expect(me.json.token).toMatchObject({ scope: "full", label: "test" });
    const row = await env.DB.prepare("SELECT token_hash FROM tokens WHERE user_id = ?1").bind(userId).first<{ token_hash: string }>();
    expect(row!.token_hash).not.toContain(token.slice(4, 20)); // only the hash is stored
  });

  it("second login reuses the account", async () => {
    const e = email();
    const a = await login(e);
    const b = await login(e);
    expect(b.userId).toBe(a.userId);
    expect(b.token).not.toBe(a.token);
  });

  it("normalizes the email and accepts spaced codes", async () => {
    const e = email();
    const start = await api("/auth/email/start", { body: { email: `  ${e.toUpperCase()} ` } });
    const code = lastCode(e);
    const v = await api("/auth/email/verify", { body: { login_id: start.json.login_id, code: `${code.slice(0, 3)} ${code.slice(3)}` } });
    expect(v.status).toBe(200);
    expect(v.json.user.email).toBe(e);
  });

  it("locks a code after 5 wrong attempts", async () => {
    const e = email();
    const start = await api("/auth/email/start", { body: { email: e } });
    const code = lastCode(e);
    const wrong = code === "000000" ? "111111" : "000000";
    for (let i = 0; i < 5; i++) {
      expect((await api("/auth/email/verify", { body: { login_id: start.json.login_id, code: wrong } })).status).toBe(400);
    }
    const sixth = await api("/auth/email/verify", { body: { login_id: start.json.login_id, code } });
    expect(sixth.status).toBe(400);
    expect(sixth.json.error.code).toBe("invalid_code");
  });

  it("accepts a greylisted first code after a resend (last-3 rule)", async () => {
    const e = email();
    await api("/auth/email/start", { body: { email: e } });
    const firstCode = lastCode(e);
    const resend = await api("/auth/email/start", { body: { email: e } });
    const v = await api("/auth/email/verify", { body: { login_id: resend.json.login_id, code: firstCode } });
    expect(v.status, JSON.stringify(v.json)).toBe(200);
  });

  it("concurrent verifies of one code mint exactly one token", async () => {
    const e = email("race");
    const start = await api("/auth/email/start", { body: { email: e } });
    const code = lastCode(e);
    const results = await Promise.all(Array.from({ length: 5 }, () => api("/auth/email/verify", { body: { login_id: start.json.login_id, code } })));
    expect(results.filter((r) => r.status === 200)).toHaveLength(1);
    const tokens = await env.DB.prepare("SELECT COUNT(*) AS n FROM tokens t JOIN users u ON u.id = t.user_id WHERE u.email = ?1").bind(e).first<{ n: number }>();
    expect(tokens!.n).toBe(1);
  });

  it("rejects expired and reused codes", async () => {
    const e = email();
    const s1 = await api("/auth/email/start", { body: { email: e } });
    await env.DB.prepare("UPDATE login_codes SET expires_at = 1 WHERE id = ?1").bind(s1.json.login_id).run();
    expect((await api("/auth/email/verify", { body: { login_id: s1.json.login_id, code: lastCode(e) } })).status).toBe(400);

    const s2 = await api("/auth/email/start", { body: { email: e } });
    const code = lastCode(e);
    expect((await api("/auth/email/verify", { body: { login_id: s2.json.login_id, code } })).status).toBe(200);
    expect((await api("/auth/email/verify", { body: { login_id: s2.json.login_id, code } })).status).toBe(400);
  });

  it("rejects invalid and disposable emails", async () => {
    expect((await api("/auth/email/start", { body: { email: "not-an-email" } })).json.error.code).toBe("invalid_email");
    expect((await api("/auth/email/start", { body: { email: "x@mailinator.com" } })).json.error.code).toBe("disposable_email");
    expect((await api("/auth/email/start", { body: { email: "x@sub.mailinator.com" } })).json.error.code).toBe("disposable_email");
  });

  it("answers known and unknown emails identically (no enumeration)", async () => {
    const known = email("known");
    await login(known);
    const a = await api("/auth/email/start", { body: { email: known } });
    const b = await api("/auth/email/start", { body: { email: email("unknown") } });
    expect(a.status).toBe(b.status);
    expect(Object.keys(a.json).sort()).toEqual(Object.keys(b.json).sort());
  });
});

describe("caps and budget", () => {
  it("the D1 cap + budget batch is atomic under 20 concurrent callers", async () => {
    const e = email("atomic");
    const day = "2099-01-01"; // isolated budget row
    const now = Math.floor(Date.now() / 1000);
    const results = await Promise.all(
      Array.from({ length: 20 }, (_, i) =>
        reserveCodeSlot(env, { loginId: `${i}`.padStart(32, "a"), email: e, purpose: "login", codeHash: "h", ipPrefix: `10.0.0.${i}/32`, now, day, budget: 900 }),
      ),
    );
    const ok = results.filter((r) => r !== null).length;
    const rows = await env.DB.prepare("SELECT COUNT(*) AS n FROM login_codes WHERE email = ?1").bind(e).first<{ n: number }>();
    const budget = await env.DB.prepare("SELECT sent FROM email_budget WHERE day = ?1").bind(day).first<{ sent: number }>();
    expect(ok).toBe(5);
    expect(rows!.n).toBe(5);
    expect(budget!.sent).toBe(5);
  });

  it("caps count the canonical mailbox (+tags and gmail dots share one cap)", async () => {
    expect(canonicalMailbox("Jo.Hn+x@gmail.com".toLowerCase())).toBe("john@gmail.com");
    expect(canonicalMailbox("a+b@googlemail.com")).toBe("a@gmail.com");
    expect(canonicalMailbox("a.b+c@example.com")).toBe("a.b@example.com");
    const base = `cap${Date.now()}`;
    const variants = [`${base}@gmail.com`, `${base}+1@gmail.com`, `${base.slice(0, 2)}.${base.slice(2)}@gmail.com`, `${base}+2@gmail.com`, `${base}+3@gmail.com`, `${base}+4@gmail.com`];
    const statuses: number[] = [];
    for (const v of variants) statuses.push((await api("/auth/email/start", { body: { email: v } })).status);
    expect(statuses.slice(0, 5).every((s) => s === 200)).toBe(true);
    expect(statuses[5]).toBe(429); // 6th in the hour for the same mailbox
  });

  it("a parallel burst cannot exceed the per-email hourly cap", async () => {
    const e = email("burst");
    const results = await Promise.all(Array.from({ length: 20 }, (_, i) => api("/auth/email/start", { body: { email: e }, ip: `203.0.113.${i}` })));
    const rows = await env.DB.prepare("SELECT COUNT(*) AS n FROM login_codes WHERE email = ?1").bind(e).first<{ n: number }>();
    expect(rows!.n).toBeLessThanOrEqual(5);
    expect(results.filter((r) => r.status === 200).length).toBe(rows!.n);
    expect(results.every((r) => r.status === 200 || r.status === 429)).toBe(true);
  });

  it("caps codes per IP prefix per day (IPv6 /64)", async () => {
    const now = Math.floor(Date.now() / 1000);
    const stmts = Array.from({ length: 30 }, (_, i) =>
      env.DB.prepare("INSERT INTO login_codes (id, email, mailbox, code_hash, ip_prefix, created_at, expires_at, last_sent_at) VALUES (?1, ?2, ?2, 'x', '2001:db8:1:2::/64', ?3, ?3, ?3)").bind(
        `seed${String(i).padStart(2, "0")}`.padEnd(32, "0"),
        `seed${i}@example.com`,
        now,
      ),
    );
    await env.DB.batch(stmts);
    const r = await api("/auth/email/start", { body: { email: email() }, ip: "2001:db8:1:2:aaaa::1" });
    expect(r.status).toBe(429);
  });

  it("80% budget pauses signups but not existing users; admin is told once", async ({ onTestFinished }) => {
    onTestFinished(() => env.DB.prepare("DELETE FROM email_budget").run().then(() => {}));
    const existing = email("existing");
    await login(existing);
    const day = new Date().toISOString().slice(0, 10);
    await env.DB.prepare("INSERT INTO email_budget (day, sent) VALUES (?1, 719) ON CONFLICT(day) DO UPDATE SET sent = 719").bind(day).run();
    const adminBefore = sentEmails.filter((m) => m.to === "admin@example.test").length;
    expect((await api("/auth/email/start", { body: { email: existing } })).status).toBe(200); // 719 → 720 crosses 80 %
    expect(sentEmails.filter((m) => m.to === "admin@example.test").length).toBe(adminBefore + 1);
    const fresh = await api("/auth/email/start", { body: { email: email("new") } });
    expect(fresh.status, JSON.stringify(fresh.json)).toBe(503);
    expect(fresh.json.error.code).toBe("signups_paused");
    expect((await api("/auth/email/start", { body: { email: existing } })).status).toBe(200);

    await env.DB.prepare("UPDATE email_budget SET sent = 900 WHERE day = ?1").bind(day).run();
    // 100 %: nobody gets a code (a fresh address: `existing` already used its 3/min burst).
    const full = await api("/auth/email/start", { body: { email: email("full") } });
    expect(full.status).toBe(503);
    expect(full.json.error.code).toBe("email_unavailable");
    await env.DB.prepare("DELETE FROM email_budget WHERE day = ?1").bind(day).run();
  });

  it("a failed send answers 503 and still counts", async () => {
    const e = `victim+sendfail@example.com`;
    const r = await api("/auth/email/start", { body: { email: e } });
    expect(r.status).toBe(503);
    const row = await env.DB.prepare("SELECT send_failed_at FROM login_codes WHERE email = ?1").bind(e).first<{ send_failed_at: number }>();
    expect(row!.send_failed_at).toBeGreaterThan(0);
  });
});
