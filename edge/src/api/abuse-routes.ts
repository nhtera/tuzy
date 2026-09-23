/**
 * POST /api/v1/abuse — public abuse reports (phase 7). JSON only (a cross-site HTML form can't send
 * it without a CORS preflight), rate-limited per reporter network, honeypot field `website`.
 * A logged-in reporter (Bearer token) weighs double in auto-quarantine.
 */
import { Hono } from "hono";
import { CATEGORIES, VERIFIED_REPORTER_SECONDS, alertAdmin, maybeQuarantine, reporterPrefix, type Category } from "../lib/abuse";
import { ipPrefix } from "../lib/ip-prefix";
import { audit } from "../lib/audit";
import { isValidEmail, normalizeEmail } from "../lib/email-validation";
import { newId, nowSec } from "../lib/ids";
import { pushLater } from "../lib/moderation";
import { authenticate, type AppEnv } from "./auth-middleware";
import { ApiError, jsonBody } from "./errors";

export const abuseRoutes = new Hono<AppEnv>();

/** Accepts `shop`, `shop.tuzy.dev` or `https://shop.tuzy.dev/login`. */
export function reportedName(input: unknown, baseDomain: string): string | null {
  let s = String(input ?? "").trim().toLowerCase();
  if (!s) return null;
  try {
    if (/^https?:\/\//.test(s)) s = new URL(s).hostname;
  } catch {
    return null;
  }
  s = s.replace(/\/.*$/, "");
  const suffix = `.${baseDomain}`;
  if (s.endsWith(suffix)) s = s.slice(0, -suffix.length);
  return /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(s) ? s : null;
}

abuseRoutes.post("/", async (c) => {
  if (!/^application\/json\b/i.test(c.req.header("content-type") ?? "")) {
    throw new ApiError(415, "json_required", "send the report as application/json");
  }
  const ip = c.req.header("cf-connecting-ip");
  const prefix = reporterPrefix(ip);
  const rl = await c.env.RL_ABUSE.limit({ key: ipPrefix(ip) });
  if (!rl.success) throw new ApiError(429, "rate_limited", "too many reports; try again in a minute", {}, { "retry-after": "60" });

  const body = await jsonBody(c.req.raw);
  if (typeof body.website === "string" && body.website !== "") return c.json({ received: true }, 202); // honeypot

  const name = reportedName(body.name ?? body.url, c.env.BASE_DOMAIN);
  if (!name) throw new ApiError(400, "invalid_name", `give the tunnel address, e.g. shop.${c.env.BASE_DOMAIN}`);
  const category = String(body.category ?? "") as Category;
  if (!CATEGORIES.includes(category)) throw new ApiError(400, "invalid_category", `category must be one of ${CATEGORIES.join(", ")}`);
  const details = typeof body.details === "string" ? body.details.slice(0, 4000) : null;
  let email: string | null = null;
  if (typeof body.email === "string" && body.email.trim() !== "") {
    email = normalizeEmail(body.email);
    if (!isValidEmail(email)) throw new ApiError(400, "invalid_email", "that email address doesn't look right");
  }
  const owner = await c.env.DB.prepare("SELECT user_id FROM reservations WHERE name = ?1").bind(name).first<{ user_id: string }>();
  if (!owner) throw new ApiError(404, "unknown_tunnel", `there is no tunnel named ${name}`);

  // Logged-in reporters with an established account weigh double (fresh throwaways don't).
  let reporterUserId: string | null = null;
  if (c.req.header("authorization")) {
    reporterUserId = await authenticate(c.env, c.executionCtx, c.req.header("authorization")).then(
      async (a) => {
        const u = await c.env.DB.prepare("SELECT created_at FROM users WHERE id = ?1").bind(a.userId).first<{ created_at: number }>();
        return u && nowSec() - u.created_at >= VERIFIED_REPORTER_SECONDS ? a.userId : null;
      },
      () => null,
    );
  }

  const now = nowSec();
  const id = newId("rpt");
  const db = c.env.DB;
  await db.batch([
    db
      .prepare(
        `INSERT INTO abuse_reports (id, name, category, details, reporter_email, reporter_user_id, reporter_prefix, owner_user_id, created_at)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)`,
      )
      .bind(id, name, category, details, email, reporterUserId, prefix, owner.user_id, now),
    audit(db, { actor: reporterUserId, action: "abuse.report", target: name, meta: { report: id, category }, ipPrefix: prefix }),
  ]);

  const q = await maybeQuarantine(c.env, name, now);
  pushLater(c.env, c.executionCtx, q.outboxIds);
  // First report on a name within 24 h: tell the admin (auto-actions alert on their own).
  if (!q.quarantined) {
    c.executionCtx.waitUntil(
      (async () => {
        const first = await db
          .prepare("SELECT COUNT(*) AS n FROM abuse_reports WHERE name = ?1 AND created_at > ?2")
          .bind(name, now - 86400)
          .first<{ n: number }>();
        if ((first?.n ?? 0) !== 1) return;
        const sent = await alertAdmin(c.env, "report", `abuse report: ${name} (${category})`, `${details ?? "(no details)"}\n\nReporter: ${email ?? "anonymous"} from ${prefix}\nReview: tuzy admin reports\n`);
        if (sent) await db.prepare("UPDATE abuse_reports SET alert_sent = 1 WHERE id = ?1").bind(id).run();
      })().catch((e) => console.error("abuse alert failed", e)),
    );
  } else if (q.alertSent) {
    await db.prepare("UPDATE abuse_reports SET alert_sent = 1 WHERE id = ?1").bind(id).run();
  }
  return c.json({ received: true, id }, 202);
});
