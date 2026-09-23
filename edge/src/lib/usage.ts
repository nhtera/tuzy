/**
 * Monthly long-stream budget (cost guardrail). Only time past a stream's first LONG_STREAM_AFTER
 * counts. The DO accrues in memory and flushes deltas every 15 min and at stream end; D1 returns the
 * account-wide total; a name re-reads it when older than 5 min, so drift stays small.
 */

export const DEFAULT_LONG_STREAM_BUDGET = 180_000; // 50 h / UTC month

export const monthKey = (ms = Date.now()) => new Date(ms).toISOString().slice(0, 7);

export function longStreamBudget(env: Env): number {
  const n = Number(env.LONG_STREAM_BUDGET_SECONDS);
  return Number.isFinite(n) && n >= 0 ? n : DEFAULT_LONG_STREAM_BUDGET;
}

export async function readUsage(db: D1Database, userId: string, month = monthKey()): Promise<number> {
  const r = await db
    .prepare("SELECT long_stream_seconds AS s FROM usage_monthly WHERE user_id = ?1 AND month = ?2")
    .bind(userId, month)
    .first<{ s: number }>();
  return r?.s ?? 0;
}

/** Adds `seconds` and returns the new account total for the month. */
export async function addUsage(db: D1Database, userId: string, seconds: number, month = monthKey()): Promise<number> {
  const r = await db
    .prepare(
      `INSERT INTO usage_monthly (user_id, month, long_stream_seconds) VALUES (?1, ?2, ?3)
       ON CONFLICT(user_id, month) DO UPDATE SET long_stream_seconds = long_stream_seconds + ?3
       RETURNING long_stream_seconds AS s`,
    )
    .bind(userId, month, Math.max(0, Math.round(seconds)))
    .first<{ s: number }>();
  return r?.s ?? 0;
}
