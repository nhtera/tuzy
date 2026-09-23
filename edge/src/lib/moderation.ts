/**
 * Moderation operations shared by the admin API and auto-quarantine (phase 7). Each is ONE D1 batch:
 * {state change, audit row, outbox rows}. Follow-up statements are gated on a per-call `change_ref`
 * nonce so they fire only when THIS call made the change (a concurrent duplicate is a no-op).
 */
import { markerKey } from "./connected-marker";
import { auditWhen, type AuditEntry } from "./audit";
import { nowSec, randomHex } from "./ids";
import { HOLD_SECONDS } from "./name-repo";
import { pushOutbox, returnedIds } from "./outbox";

export const BLOCKED = "__blocked__";

/** Name quota given to NEW accounts (admin-tunable without a deploy, e.g. during an abuse wave). */
export const NEW_ACCOUNT_MAX_NAMES_KEY = "config:new_account_max_names";
export const DEFAULT_MAX_NAMES = 10;

export async function newAccountMaxNames(env: Env): Promise<number> {
  const raw = await env.NAMES_KV.get(NEW_ACCOUNT_MAX_NAMES_KEY).catch(() => null);
  const v = raw === null ? NaN : Number(raw);
  return Number.isInteger(v) && v >= 0 && v <= 1000 ? v : DEFAULT_MAX_NAMES;
}
const BLOCK_SECONDS = 100 * 365 * 24 * 3600;

export interface Actor {
  userId: string | null; // null = system (auto-quarantine)
  ipPrefix?: string | null;
}

export interface Outcome {
  changed: boolean;
  outboxIds: string[];
}

const entry = (actor: Actor, action: string, target: string, meta?: Record<string, unknown>): AuditEntry => ({
  actor: actor.userId,
  action,
  target,
  meta,
  ipPrefix: actor.ipPrefix ?? null,
});

const outboxSelect = (action: string, payloadSql: string) =>
  `INSERT INTO do_outbox (id, name, action, payload, next_at, created_at)
   SELECT 'obx_' || lower(hex(randomblob(8))), r.name, '${action}', ${payloadSql}, ?9, ?9`;

/** Push the rows now (best effort; the 5-minute cron retries). */
export function pushLater(env: Env, ctx: { waitUntil(p: Promise<unknown>): void }, ids: string[]): void {
  if (ids.length) ctx.waitUntil(pushOutbox(env, ids).catch((e) => console.error("outbox push failed", e)));
}

/** Suspends (or reinstates) one name: visitors get 451 and the agent GOAWAY suspended. */
export async function setNameSuspended(env: Env, name: string, suspended: boolean, actor: Actor, reason?: string): Promise<Outcome> {
  const db = env.DB;
  const ref = randomHex(8);
  const [from, to] = suspended ? ["active", "suspended"] : ["suspended", "active"];
  const res = await db.batch([
    db.prepare("UPDATE reservations SET status = ?2, change_ref = ?3 WHERE name = ?1 AND status = ?4").bind(name, to, ref, from),
    // Reinstating clears the reports that led here, so one more report can't re-quarantine at once.
    db
      .prepare(
        `UPDATE abuse_reports SET status = 'dismissed' WHERE name = ?1 AND status = 'open' AND ?3 = 0
            AND EXISTS (SELECT 1 FROM reservations WHERE name = ?1 AND change_ref = ?2)`,
      )
      .bind(name, ref, suspended ? 1 : 0),
    db
      .prepare(
        `${outboxSelect("setSuspended", "json_object('suspended', json(?10), 'gen', r.gen)")}
           FROM reservations r WHERE r.name = ?7 AND r.change_ref = ?8 RETURNING id`,
      )
      .bind(null, null, null, null, null, null, name, ref, nowSec(), suspended ? "true" : "false"),
    auditWhen(db, entry(actor, suspended ? "name.suspend" : "name.unsuspend", name, reason ? { reason } : undefined), "EXISTS (SELECT 1 FROM reservations WHERE name = ?7 AND change_ref = ?8)", name, ref),
  ]);
  return { changed: (res[0]?.meta.changes ?? 0) === 1, outboxIds: returnedIds(res[2]) };
}

/** Suspends (or reinstates) an account. Suspension closes its live tunnels; connects are refused. */
export async function setUserSuspended(env: Env, userId: string, suspended: boolean, actor: Actor, reason?: string): Promise<Outcome> {
  const db = env.DB;
  const ref = randomHex(8);
  const [from, to] = suspended ? ["active", "suspended"] : ["suspended", "active"];
  const stmts = [
    db.prepare("UPDATE users SET status = ?2, change_ref = ?3 WHERE id = ?1 AND status = ?4").bind(userId, to, ref, from),
    auditWhen(db, entry(actor, suspended ? "user.suspend" : "user.unsuspend", userId, reason ? { reason } : undefined), "EXISTS (SELECT 1 FROM users WHERE id = ?7 AND change_ref = ?8)", userId, ref),
  ];
  if (suspended) {
    stmts.push(
      db
        .prepare(
          `${outboxSelect("revokeUser", "json_object('userId', ?7)")}
             FROM reservations r WHERE r.user_id = ?7 AND EXISTS (SELECT 1 FROM users WHERE id = ?7 AND change_ref = ?8) RETURNING id`,
        )
        .bind(null, null, null, null, null, null, userId, ref, nowSec()),
    );
  }
  const res = await db.batch(stmts);
  return { changed: (res[0]?.meta.changes ?? 0) === 1, outboxIds: suspended ? returnedIds(res[2]) : [] };
}

/**
 * Trust: `trust` (admin-granted, skips the interstitial), `untrust`/`revoke` (sticky: auto-trust no
 * longer applies). Pushes the new value to every reserved name of the account.
 */
export async function setUserTrust(env: Env, userId: string, mode: "trust" | "untrust", actor: Actor, reason?: string): Promise<Outcome> {
  const db = env.DB;
  const ref = randomHex(8);
  const trusted = mode === "trust";
  const res = await db.batch([
    db.prepare("UPDATE users SET trusted = ?2, trust_revoked = ?3, change_ref = ?4 WHERE id = ?1").bind(userId, trusted ? 1 : 0, trusted ? 0 : 1, ref),
    db
      .prepare(
        `${outboxSelect("setTrusted", "json_object('trusted', json(?10))")}
           FROM reservations r WHERE r.user_id = ?7 AND EXISTS (SELECT 1 FROM users WHERE id = ?7 AND change_ref = ?8) RETURNING id`,
      )
      .bind(null, null, null, null, null, null, userId, ref, nowSec(), trusted ? "true" : "false"),
    auditWhen(db, entry(actor, mode === "trust" ? "user.trust" : "user.untrust", userId, reason ? { reason } : undefined), "EXISTS (SELECT 1 FROM users WHERE id = ?7 AND change_ref = ?8)", userId, ref),
  ]);
  return { changed: (res[0]?.meta.changes ?? 0) === 1, outboxIds: returnedIds(res[1]) };
}

/**
 * Blocks a name for everyone (brand/abuse). A current reservation is removed (agent GOAWAY deleted,
 * default promoted) and the name is held for `__blocked__` indefinitely.
 */
export async function blockName(env: Env, ctx: { waitUntil(p: Promise<unknown>): void }, name: string, actor: Actor, reason?: string): Promise<Outcome> {
  const db = env.DB;
  const owner = await db.prepare("SELECT user_id FROM reservations WHERE name = ?1").bind(name).first<{ user_id: string }>();
  const now = nowSec();
  const res = await db.batch([
    db
      .prepare(
        `INSERT INTO do_outbox (id, name, action, payload, next_at, created_at)
         SELECT 'obx_' || lower(hex(randomblob(8))), name, 'goaway',
                json_object('reason', 'deleted', 'gen', gen, 'message', 'this name was blocked'), ?2, ?2
           FROM reservations WHERE name = ?1 RETURNING id`,
      )
      .bind(name, now),
    db.prepare("DELETE FROM reservations WHERE name = ?1").bind(name),
    db
      .prepare(
        `UPDATE reservations SET is_default = 1
          WHERE name = (SELECT name FROM reservations WHERE user_id = ?1 ORDER BY created_at, name LIMIT 1)
            AND NOT EXISTS (SELECT 1 FROM reservations WHERE user_id = ?1 AND is_default = 1)`,
      )
      .bind(owner?.user_id ?? ""),
    db.prepare("INSERT OR REPLACE INTO released_names (name, held_for, until, renamed_to) VALUES (?1, ?2, ?3, NULL)").bind(name, BLOCKED, now + BLOCK_SECONDS),
    auditWhen(db, entry(actor, "name.block", name, { owner: owner?.user_id ?? null, reason: reason ?? null }), "1"),
  ]);
  ctx.waitUntil(env.NAMES_KV.delete(markerKey(name)));
  return { changed: true, outboxIds: returnedIds(res[0]) };
}

/** Lifts any hold (including a block) so the name can be reserved again. */
export async function releaseName(env: Env, name: string, actor: Actor): Promise<Outcome> {
  const db = env.DB;
  const res = await db.batch([
    db.prepare("DELETE FROM released_names WHERE name = ?1").bind(name),
    auditWhen(db, entry(actor, "name.release", name), "changes() > 0"),
  ]);
  return { changed: (res[0]?.meta.changes ?? 0) > 0, outboxIds: [] };
}

export async function setMaxNames(env: Env, userId: string, max: number, actor: Actor): Promise<Outcome> {
  const db = env.DB;
  const res = await db.batch([
    db.prepare("UPDATE users SET max_names = ?2 WHERE id = ?1").bind(userId, max),
    auditWhen(db, entry(actor, "user.set_max_names", userId, { max }), "changes() > 0"),
  ]);
  return { changed: (res[0]?.meta.changes ?? 0) === 1, outboxIds: [] };
}

/** Reserves `name` for a user regardless of quota and holds (admin override). Throws on a taken name. */
export async function reserveForUser(env: Env, name: string, userId: string, actor: Actor): Promise<void> {
  const db = env.DB;
  const now = nowSec();
  await db.batch([
    db.prepare("DELETE FROM released_names WHERE name = ?1").bind(name),
    db
      .prepare(
        `INSERT INTO reservations (name, user_id, gen, is_default, created_at)
         SELECT ?1, ?2, ?3, CASE WHEN EXISTS (SELECT 1 FROM reservations WHERE user_id = ?2) THEN 0 ELSE 1 END, ?4`,
      )
      .bind(name, userId, randomHex(8), now),
    auditWhen(db, entry(actor, "name.reserve_for_user", name, { user: userId }), "1"),
  ]);
}

export { HOLD_SECONDS };
