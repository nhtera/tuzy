/**
 * Issuing and verifying emailed 6-digit codes (login and account deletion).
 *
 * Caps and the global daily send budget are enforced atomically in one D1 batch; a failed send
 * still counts. Verify accepts any of the last 3 unconsumed codes for the email, so a greylisted
 * first email that arrives after a resend still works, and no reversible code is ever stored.
 */
import { EmailUnavailableError, loginCodeEmail, sendEmail } from "../lib/email";
import { canonicalMailbox } from "../lib/email-validation";
import { nowSec, randomHex } from "../lib/ids";
import { CODE_TTL_SECONDS, codeMatches, generateCode, hashCode, MAX_CODE_ATTEMPTS } from "../lib/otp";
import { ApiError } from "./errors";

export type CodePurpose = "login" | "delete";

/**
 * Per-mailbox and per-prefix caps (hour/day). Caps count the canonical mailbox, so "+tag" and
 * Gmail-dot variants can't multiply email bombing. Accepted v1 trade-off: someone who knows your
 * address can use up its 10 codes/day and delay your next login until the window rolls over.
 */
const EMAIL_PER_HOUR = 5;
const EMAIL_PER_DAY = 10;
const PREFIX_PER_DAY = 30;

const utcDay = (sec: number) => new Date(sec * 1000).toISOString().slice(0, 10);

export function dailyBudget(env: Env): number {
  const n = Number(env.DAILY_SEND_BUDGET);
  return Number.isFinite(n) && n > 0 ? n : 900;
}

async function sentToday(env: Env, day: string): Promise<number> {
  const r = await env.DB.prepare("SELECT sent FROM email_budget WHERE day = ?1").bind(day).first<{ sent: number }>();
  return r?.sent ?? 0;
}

/**
 * Atomically inserts a code row if every cap and the daily budget allow it, and counts it against
 * the budget (one D1 batch = one transaction). Returns the day's new send count, or null when a
 * cap/budget refused it.
 */
export async function reserveCodeSlot(
  env: Env,
  r: { loginId: string; email: string; purpose: CodePurpose; codeHash: string; ipPrefix: string; now: number; day: string; budget: number },
): Promise<number | null> {
  const [ins, bud] = await env.DB.batch([
    env.DB.prepare(
      `INSERT INTO login_codes (id, email, mailbox, purpose, code_hash, ip_prefix, created_at, expires_at, last_sent_at)
       SELECT ?1, ?2, ?13, ?3, ?4, ?5, ?6, ?6 + ?7, ?6
        WHERE (SELECT COUNT(*) FROM login_codes WHERE mailbox = ?13 AND created_at > ?6 - 3600) < ?8
          AND (SELECT COUNT(*) FROM login_codes WHERE mailbox = ?13 AND created_at > ?6 - 86400) < ?9
          AND (SELECT COUNT(*) FROM login_codes WHERE ip_prefix = ?5 AND created_at > ?6 - 86400) < ?10
          AND (SELECT COALESCE(SUM(sent), 0) FROM email_budget WHERE day = ?11) < ?12`,
    ).bind(r.loginId, r.email, r.purpose, r.codeHash, r.ipPrefix, r.now, CODE_TTL_SECONDS, EMAIL_PER_HOUR, EMAIL_PER_DAY, PREFIX_PER_DAY, r.day, r.budget, canonicalMailbox(r.email)),
    env.DB.prepare(
      `INSERT INTO email_budget (day, sent) SELECT ?1, 1 WHERE changes() = 1
       ON CONFLICT(day) DO UPDATE SET sent = sent + 1
       RETURNING sent`,
    ).bind(r.day),
  ]);
  if (!ins || ins.meta.changes === 0) return null;
  return (bud?.results[0] as { sent: number } | undefined)?.sent ?? 0;
}

/**
 * Issues a code for `email` and emails it. Returns the login id.
 * Throws 429 rate_limited, 503 signups_paused / email_unavailable.
 */
export async function issueCode(
  env: Env,
  ctx: { waitUntil(p: Promise<unknown>): void },
  email: string,
  purpose: CodePurpose,
  ipPrefix: string,
): Promise<string> {
  // Burst shedding (per location, eventually consistent). D1 below is authoritative.
  const [byIp, byEmail] = await Promise.all([env.RL_OTP_IP.limit({ key: ipPrefix }), env.RL_OTP_EMAIL.limit({ key: email })]);
  if (!byIp.success || !byEmail.success) throw new ApiError(429, "rate_limited", "too many code requests; wait a minute", {}, { "retry-after": "60" });

  const now = nowSec();
  const day = utcDay(now);
  const budget = dailyBudget(env);
  const sent = await sentToday(env, day);
  if (sent >= budget) throw new ApiError(503, "email_unavailable", "login emails are paused for today; try again later");
  if (sent >= 0.8 * budget && purpose === "login") {
    // Degraded mode (documented enumeration trade-off): only existing accounts get codes.
    const known = await env.DB.prepare("SELECT 1 AS ok FROM users WHERE email = ?1 AND status = 'active'").bind(email).first();
    if (!known) throw new ApiError(503, "signups_paused", "new signups are paused for today; try again tomorrow");
  }

  const loginId = randomHex(16);
  const code = generateCode();
  const slot = await reserveCodeSlot(env, { loginId, email, purpose, codeHash: await hashCode(loginId, code), ipPrefix, now, day, budget });
  if (slot === null) {
    if ((await sentToday(env, day)) >= budget) throw new ApiError(503, "email_unavailable", "login emails are paused for today; try again later");
    throw new ApiError(429, "rate_limited", "too many code requests for this email or network; try again later", {}, { "retry-after": "3600" });
  }

  // Crossing 80 % of the budget: tell the admin once (verified destination: free, uncounted).
  if (slot === Math.ceil(0.8 * budget) && env.ADMIN_EMAIL) {
    ctx.waitUntil(
      sendEmail(env, {
        to: env.ADMIN_EMAIL,
        subject: `[tuzy] email budget at 80% (${slot}/${budget}) — signups paused`,
        text: `Login email budget for ${day} reached 80%. New signups are paused until 00:00 UTC; existing users can still log in.\n`,
      }).catch((e) => console.error("budget alert failed", e)),
    );
  }

  try {
    await sendEmail(env, loginCodeEmail(email, code, purpose));
  } catch (e) {
    await env.DB.prepare("UPDATE login_codes SET send_failed_at = ?1 WHERE id = ?2").bind(nowSec(), loginId).run();
    if (e instanceof EmailUnavailableError) throw new ApiError(503, "email_unavailable", "could not send the email; try again in a few minutes");
    throw e;
  }
  return loginId;
}

interface CodeRow {
  id: string;
  email: string;
  code_hash: string;
}

/**
 * Checks `code` against login `loginId` and, failing that, the last 3 unconsumed codes for the same
 * email and purpose (each attempt counts against that code's own budget). Returns the email.
 * When `expectedEmail` is set the code must belong to that address.
 */
export async function verifyCode(env: Env, loginId: string, code: string, purpose: CodePurpose, expectedEmail?: string): Promise<{ email: string; id: string }> {
  const now = nowSec();
  const invalid = new ApiError(400, "invalid_code", "wrong or expired code; check the latest email or request a new code");
  if (!/^[0-9a-f]{32}$/.test(loginId)) throw invalid;

  const first = await env.DB.prepare(
    `UPDATE login_codes SET attempts = attempts + 1
      WHERE id = ?1 AND purpose = ?2 AND consumed_at IS NULL AND expires_at > ?3 AND attempts < ?4
      RETURNING id, email, code_hash`,
  )
    .bind(loginId, purpose, now, MAX_CODE_ATTEMPTS)
    .first<CodeRow>();
  if (!first || (expectedEmail !== undefined && first.email !== expectedEmail)) throw invalid;
  if (await codeMatches(first.id, code, first.code_hash)) return { email: first.email, id: first.id };

  const others = await env.DB.prepare(
    `UPDATE login_codes SET attempts = attempts + 1
      WHERE id IN (SELECT id FROM login_codes
                    WHERE email = ?1 AND purpose = ?2 AND consumed_at IS NULL AND expires_at > ?3
                    ORDER BY created_at DESC LIMIT 3)
        AND id <> ?4 AND attempts < ?5
      RETURNING id, email, code_hash`,
  )
    .bind(first.email, purpose, now, first.id, MAX_CODE_ATTEMPTS)
    .all<CodeRow>();
  for (const row of others.results) {
    if (await codeMatches(row.id, code, row.code_hash)) return { email: row.email, id: row.id };
  }
  throw invalid;
}

/**
 * Consumes the matched code; MUST be the first statement of the success batch so the following
 * statements can gate on `changes() = 1` (a concurrent verify of the same code gets 0 and loses).
 */
export function consumeMatched(db: D1Database, id: string): D1PreparedStatement {
  return db.prepare("UPDATE login_codes SET consumed_at = ?1 WHERE id = ?2 AND consumed_at IS NULL").bind(nowSec(), id);
}

/** Consumes every other outstanding code of that purpose for the email. */
export function consumeCodes(db: D1Database, email: string, purpose: CodePurpose): D1PreparedStatement {
  return db.prepare("UPDATE login_codes SET consumed_at = ?1 WHERE email = ?2 AND purpose = ?3 AND consumed_at IS NULL").bind(nowSec(), email, purpose);
}
