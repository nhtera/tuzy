/**
 * Abuse reports and auto-quarantine (phase 7).
 *
 * Auto-quarantine: ≥ QUARANTINE_THRESHOLD weighted reports within 24 h from DISTINCT reporter
 * prefixes (IPv4 /24, IPv6 /64; a logged-in reporter weighs 2) on a name whose owner account is
 * younger than 7 days → the name is suspended, the owner's trust revoked, the admin emailed. An
 * owner with 2 suspended names is suspended as a whole. Admins undo it with one command each.
 */
import { sendEmail } from "./email";
import { ipPrefix, ipv4Slash24 } from "./ip-prefix";
import { setNameSuspended, setUserSuspended, setUserTrust, type Outcome } from "./moderation";

export const CATEGORIES = ["phishing", "malware", "spam", "illegal", "other"] as const;
export type Category = (typeof CATEGORIES)[number];

const NEW_ACCOUNT_SECONDS = 7 * 24 * 3600;
const WINDOW_SECONDS = 24 * 3600;
/**
 * Daily caps on admin alert emails (the account-wide send quota is shared with logins). Report
 * notices and auto-action notices have separate budgets so report spam can't mute auto-actions.
 */
const ALERT_CAPS = { report: 50, action: 200 } as const;

/**
 * Reporter network for distinctness: IPv4 /24, IPv6 /48 (free tunnel brokers hand out /48s, so a
 * /64 per report would let one host look like thousands). Rate limiting still keys on /64.
 */
export function reporterPrefix(ip: string | null | undefined): string {
  const p = ipPrefix(ip);
  if (p.endsWith("/32")) return ipv4Slash24(p.slice(0, -3)) ?? p;
  const m = /^([0-9a-f]+:[0-9a-f]+:[0-9a-f]+):[0-9a-f]+::\/64$/.exec(p);
  return m ? `${m[1]}::/48` : p;
}

/** Only logged-in reporters with an established account (≥ 7 days) weigh double. */
export const VERIFIED_REPORTER_SECONDS = 7 * 24 * 3600;

export function quarantineThreshold(env: Env): number {
  const n = Number(env.QUARANTINE_THRESHOLD);
  return Number.isInteger(n) && n > 0 ? n : 3;
}

/** Weighted distinct-prefix score of open/actioned reports on `name` in the last 24 h. */
export async function reportScore(db: D1Database, name: string, now: number): Promise<number> {
  const r = await db
    .prepare(
      `SELECT COALESCE(SUM(w), 0) AS score FROM (
         SELECT MAX(CASE WHEN reporter_user_id IS NOT NULL THEN 2 ELSE 1 END) AS w
           FROM abuse_reports WHERE name = ?1 AND created_at > ?2 AND status <> 'dismissed'
          GROUP BY reporter_prefix)`,
    )
    .bind(name, now - WINDOW_SECONDS)
    .first<{ score: number }>();
  return r?.score ?? 0;
}

/** Best-effort admin alert (never throws), capped per day and kind. */
export async function alertAdmin(env: Env, kind: keyof typeof ALERT_CAPS, subject: string, text: string): Promise<boolean> {
  if (!env.ADMIN_EMAIL) return false;
  const key = `alert:${kind}:${new Date().toISOString().slice(0, 10)}`;
  try {
    const n = Number((await env.NAMES_KV.get(key)) ?? "0");
    if (n >= ALERT_CAPS[kind]) return false;
    await sendEmail(env, { to: env.ADMIN_EMAIL, subject: `[tuzy] ${subject}`, text });
    await env.NAMES_KV.put(key, String(n + 1), { expirationTtl: 2 * 86400 });
    return true;
  } catch (e) {
    console.error("admin alert failed", e);
    return false;
  }
}

export interface QuarantineResult {
  quarantined: boolean;
  userSuspended: boolean;
  alertSent: boolean;
  outboxIds: string[];
}

/** Applies the auto-quarantine rules after a new report on `name`. */
export async function maybeQuarantine(env: Env, name: string, now: number): Promise<QuarantineResult> {
  const none = { quarantined: false, userSuspended: false, alertSent: false, outboxIds: [] };
  const owner = await env.DB.prepare(
    `SELECT r.user_id, r.status, u.created_at FROM reservations r JOIN users u ON u.id = r.user_id WHERE r.name = ?1`,
  )
    .bind(name)
    .first<{ user_id: string; status: string; created_at: number }>();
  if (!owner || owner.status !== "active" || now - owner.created_at >= NEW_ACCOUNT_SECONDS) return none;
  const score = await reportScore(env.DB, name, now);
  if (score < quarantineThreshold(env)) return none;

  const system = { userId: null };
  const reason = `auto-quarantine: ${score} weighted reports in 24 h`;
  const suspended = await setNameSuspended(env, name, true, system, reason);
  if (!suspended.changed) return none; // a concurrent report already did it
  const outs: Outcome[] = [suspended];
  let userSuspended = false;
  let followUpError = "";
  // The name is suspended (committed). Follow-ups must not turn the report into a 500; a failure
  // is reported to the admin instead (the suspension itself converges through the outbox).
  try {
    outs.push(await setUserTrust(env, owner.user_id, "untrust", system, reason));
    const n = await env.DB.prepare("SELECT COUNT(*) AS n FROM reservations WHERE user_id = ?1 AND status = 'suspended'")
      .bind(owner.user_id)
      .first<{ n: number }>();
    if ((n?.n ?? 0) >= 2) {
      const u = await setUserSuspended(env, owner.user_id, true, system, "auto: 2 quarantined names");
      outs.push(u);
      userSuspended = u.changed;
    }
  } catch (e) {
    console.error("auto-quarantine follow-up failed", e);
    followUpError = `\nWARNING: follow-up failed (${String(e)}); revoke trust / check the owner manually.\n`;
  }
  const alertSent = await alertAdmin(
    env,
    "action",
    `auto-quarantined ${name}${userSuspended ? " + suspended its owner" : ""}`,
    `${reason}.\nOwner: ${owner.user_id}${userSuspended ? " (account suspended: 2 quarantined names)" : ""}\n${followUpError}\nReview: tuzy admin reports\nUndo:   tuzy admin unsuspend-name ${name}${userSuspended ? `\n        tuzy admin unsuspend-user ${owner.user_id}` : ""}\n`,
  );
  return { quarantined: true, userSuspended, alertSent, outboxIds: outs.flatMap((o) => o.outboxIds) };
}
