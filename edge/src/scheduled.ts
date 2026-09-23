/**
 * Cron handlers.
 *   every 5 min  — retry the D1 → DO outbox; alert the admin (once a day) about rows stuck > 5 min.
 *   03:00 UTC    — housekeeping (retention), email erasure for deleted accounts, and re-pushing the
 *                  suspended flag to every connected suspended name (converges a lost DO state).
 */
import { markerKey } from "./lib/connected-marker";
import { sendEmail } from "./lib/email";
import { nowSec, sha256Hex } from "./lib/ids";
import { BLOCKED } from "./lib/moderation";
import { pushOutbox, staleOutboxCount } from "./lib/outbox";
import { monthKey } from "./lib/usage";

const DAY = 86400;

export async function scheduled(controller: ScheduledController, env: Env): Promise<void> {
  if (controller.cron === "*/5 * * * *") return outboxCron(env);
  if (controller.cron === "0 3 * * *") await dailyCron(env);
}

async function outboxCron(env: Env): Promise<void> {
  await pushOutbox(env);
  const stale = await staleOutboxCount(env);
  if (stale > 0 && env.ADMIN_EMAIL) {
    const key = `alert:outbox:${new Date().toISOString().slice(0, 10)}`;
    if ((await env.NAMES_KV.get(key)) === null) {
      try {
        await sendEmail(env, {
          to: env.ADMIN_EMAIL,
          subject: `[tuzy] ${stale} outbox row(s) stuck > 5 min`,
          text: `The D1 → Durable Object outbox has ${stale} row(s) pending for more than 5 minutes.\nCheck \`wrangler tail\` for "outbox push failed".\n`,
        });
        await env.NAMES_KV.put(key, "1", { expirationTtl: 86400 }); // only once it was actually sent
      } catch (e) {
        console.error("admin alert failed", e);
      }
    }
  }
}

/** Oldest month to keep in usage_monthly (13 months back, yyyy-mm). */
function monthsAgo(n: number, now = new Date()): string {
  return monthKey(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - n, 1));
}

/** Runs one daily section; a failure is logged and never stops the others. */
async function section<T>(name: string, fn: () => Promise<T>, fallback: T): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    console.error(`daily cron: ${name} failed`, e);
    return fallback;
  }
}

export async function dailyCron(env: Env): Promise<{ erased: number; repushed: number }> {
  const db = env.DB;
  const now = nowSec();
  await section(
    "retention",
    () =>
      db.batch([
        db.prepare("DELETE FROM login_codes WHERE created_at < ?1").bind(now - DAY),
        db.prepare("DELETE FROM released_names WHERE until <= ?1 AND held_for <> ?2").bind(now, BLOCKED),
        db.prepare("DELETE FROM audit_log WHERE created_at < ?1").bind(now - 180 * DAY),
        db.prepare("DELETE FROM abuse_reports WHERE status <> 'open' AND created_at < ?1").bind(now - 365 * DAY),
        // Open reports nobody acted on within 90 days are noise (and bury real ones in `admin reports`).
        db.prepare("UPDATE abuse_reports SET status = 'dismissed' WHERE status = 'open' AND created_at < ?1").bind(now - 90 * DAY),
        db.prepare("DELETE FROM do_outbox WHERE done_at IS NOT NULL AND done_at < ?1").bind(now - 7 * DAY),
        db.prepare("DELETE FROM email_budget WHERE day < ?1").bind(new Date((now - 60 * DAY) * 1000).toISOString().slice(0, 10)),
        db.prepare("DELETE FROM usage_monthly WHERE month < ?1").bind(monthsAgo(13)),
        // Sessions of revoked tokens, or not seen for 180 days, can no longer need a revoke fan-out.
        db
          .prepare(
            `DELETE FROM tunnel_sessions WHERE connected_at < ?1
                OR token_id IN (SELECT id FROM tokens WHERE revoked_at IS NOT NULL AND revoked_at < ?2)`,
          )
          .bind(now - 180 * DAY, now - DAY),
      ]),
    [],
  );

  // Email erasure 30 days after account deletion (the row stays for audit/foreign keys). The value
  // includes the user id: the same address deleted twice must not collide on users.email UNIQUE.
  const erased = await section(
    "erasure",
    async () => {
      const doomed = await db
        .prepare("SELECT id, email FROM users WHERE status = 'deleted' AND deleted_at < ?1 AND email NOT LIKE 'deleted:%' LIMIT 100")
        .bind(now - 30 * DAY)
        .all<{ id: string; email: string }>();
      if (!doomed.results.length) return 0;
      await db.batch(
        await Promise.all(
          doomed.results.map(async (u) =>
            db.prepare("UPDATE users SET email = ?2 WHERE id = ?1").bind(u.id, `deleted:${u.id}:${await sha256Hex(u.email)}`),
          ),
        ),
      );
      // Reports they filed keep no address either.
      await db.batch(
        doomed.results.map((u) => db.prepare("UPDATE abuse_reports SET reporter_email = NULL WHERE reporter_email = ?1").bind(u.email)),
      );
      return doomed.results.length;
    },
    0,
  );

  // Reconciliation: suspended names whose DO may have lost the flag (connected marker present).
  // A random sample per day bounds the work; the outbox cron does the pushing.
  const repushed = await section(
    "reconcile",
    async () => {
      const suspended = await db
        .prepare("SELECT name, gen FROM reservations WHERE status = 'suspended' ORDER BY random() LIMIT 200")
        .all<{ name: string; gen: string }>();
      const connected = [];
      for (const r of suspended.results) if ((await env.NAMES_KV.get(markerKey(r.name))) !== null) connected.push(r);
      if (!connected.length) return 0;
      const res = await db.batch(
        connected.map((r) =>
          db
            .prepare(
              `INSERT INTO do_outbox (id, name, action, payload, next_at, created_at)
               VALUES ('obx_' || lower(hex(randomblob(8))), ?1, 'setSuspended', json_object('suspended', json('true'), 'gen', ?2), ?3, ?3) RETURNING id`,
            )
            .bind(r.name, r.gen, now),
        ),
      );
      const ids = res.flatMap((x) => ((x.results ?? []) as { id: string }[]).map((r) => r.id));
      await pushOutbox(env, ids);
      return ids.length;
    },
    0,
  );
  return { erased, repushed };
}
