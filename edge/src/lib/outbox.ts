/**
 * D1 → Durable Object outbox. A state change and its outbox rows are written in ONE D1 batch;
 * rows are then pushed right away (waitUntil) and retried by the 5-minute cron until they converge.
 */
import type { TunnelObject, GoawayReason } from "../tunnel-object";
import { newId, nowSec } from "./ids";

export type OutboxAction =
  | { action: "revokeToken"; payload: { tokenId: string } }
  | { action: "revokeUser"; payload: { userId: string } }
  | { action: "goaway"; payload: { reason: GoawayReason; newName?: string; message?: string; gen?: string } }
  | { action: "setSuspended"; payload: { suspended: boolean; gen?: string } }
  | { action: "setTrusted"; payload: { trusted: boolean } };

export const MAX_OUTBOX_ATTEMPTS = 20;
/** One DO RPC may not hold up the whole push. */
const RPC_TIMEOUT_MS = 10_000;

/** Prepared INSERT for one outbox row (add it to the same batch as the state change). */
export function outboxRow(db: D1Database, name: string, item: OutboxAction, id = newId("obx")): { id: string; stmt: D1PreparedStatement } {
  const now = nowSec();
  return {
    id,
    stmt: db
      .prepare("INSERT INTO do_outbox (id, name, action, payload, next_at, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?5)")
      .bind(id, name, item.action, JSON.stringify(item.payload), now),
  };
}

function stub(env: Env, name: string): DurableObjectStub<TunnelObject> {
  return env.TUNNEL.get(env.TUNNEL.idFromName(name)) as unknown as DurableObjectStub<TunnelObject>;
}

async function apply(env: Env, name: string, action: string, payload: Record<string, unknown>): Promise<void> {
  const s = stub(env, name);
  switch (action) {
    case "revokeToken":
      await s.revokeToken(String(payload.tokenId));
      return;
    case "revokeUser":
      await s.revokeUser(String(payload.userId));
      return;
    case "goaway":
      await s.goaway(payload.reason as GoawayReason, {
        gen: payload.gen as string | undefined,
        newName: payload.newName as string | undefined,
        message: payload.message as string | undefined,
      });
      return;
    case "setSuspended":
      await s.setSuspended(Boolean(payload.suspended), payload.gen as string | undefined);
      return;
    case "setTrusted":
      await s.setTrusted(Boolean(payload.trusted));
      return;
    default:
      throw new Error(`unknown outbox action ${action}`);
  }
}

interface OutboxRow {
  id: string;
  name: string;
  action: string;
  payload: string;
  attempts: number;
}

/** IDs returned by an outbox `INSERT … RETURNING id` in a batch result. */
export function returnedIds(result: D1Result | undefined): string[] {
  return ((result?.results ?? []) as { id: string }[]).map((r) => r.id);
}

/** Pushes pending rows (all due rows, or only `ids`). Failures back off exponentially. */
export async function pushOutbox(env: Env, ids?: string[]): Promise<{ done: number; failed: number }> {
  const now = nowSec();
  if (ids && ids.length > 100) {
    // D1 allows 100 bound parameters per statement: push in chunks.
    let done = 0;
    let failed = 0;
    for (let i = 0; i < ids.length; i += 100) {
      const r = await pushOutbox(env, ids.slice(i, i + 100));
      done += r.done;
      failed += r.failed;
    }
    return { done, failed };
  }
  const rows = ids?.length
    ? (
        await env.DB.prepare(
          `SELECT id, name, action, payload, attempts FROM do_outbox WHERE done_at IS NULL AND id IN (${ids.map(() => "?").join(",")})`,
        )
          .bind(...ids)
          .all<OutboxRow>()
      ).results
    : (
        await env.DB.prepare(
          "SELECT id, name, action, payload, attempts FROM do_outbox WHERE done_at IS NULL AND next_at <= ?1 AND attempts < ?2 ORDER BY next_at LIMIT 100",
        )
          .bind(now, MAX_OUTBOX_ATTEMPTS)
          .all<OutboxRow>()
      ).results;
  let done = 0;
  let failed = 0;
  for (const r of rows) {
    try {
      let timer: ReturnType<typeof setTimeout> | undefined;
      await Promise.race([
        apply(env, r.name, r.action, JSON.parse(r.payload) as Record<string, unknown>),
        new Promise((_, reject) => {
          timer = setTimeout(() => reject(new Error("DO RPC timeout")), RPC_TIMEOUT_MS);
        }),
      ]).finally(() => timer !== undefined && clearTimeout(timer));
      await env.DB.prepare("UPDATE do_outbox SET done_at = ?1, attempts = attempts + 1 WHERE id = ?2").bind(nowSec(), r.id).run();
      done++;
    } catch (e) {
      console.error("outbox push failed", r.id, r.action, e);
      const backoff = Math.min(3600, 30 * 2 ** r.attempts);
      await env.DB.prepare("UPDATE do_outbox SET attempts = attempts + 1, next_at = ?1 WHERE id = ?2").bind(nowSec() + backoff, r.id).run();
      failed++;
    }
  }
  return { done, failed };
}

/** Rows still retrying after 5 minutes (the cron alerts the admin about them, once a day). */
export async function staleOutboxCount(env: Env): Promise<number> {
  const r = await env.DB.prepare("SELECT COUNT(*) AS n FROM do_outbox WHERE done_at IS NULL AND created_at < ?1 AND attempts < ?2")
    .bind(nowSec() - 300, MAX_OUTBOX_ATTEMPTS)
    .first<{ n: number }>();
  return r?.n ?? 0;
}
