/**
 * Pinned-name persistence (D1 is the source of truth). Every mutation is a guarded statement with
 * `user_id = caller`, so a non-owned name behaves exactly like a missing one (404, never 403).
 * DO-side effects (GOAWAY on rename/delete) are enqueued as outbox rows in the same batch.
 */
import { auditIfChanged, type AuditEntry } from "./audit";
import { randomHex } from "./ids";
import { AUTO_TRUST_SECONDS } from "./trust";
import { returnedIds } from "./outbox";

export const HOLD_SECONDS = 365 * 24 * 3600;
/** Active holds per user: bounds name squatting through rename/remove churn. */
export const MAX_HOLDS = 20;

/** Who is acting (for the audit row written in the same batch). */
export interface RepoActor {
  userId: string;
  ipPrefix?: string | null;
}

const auditOf = (actor: RepoActor, action: string, target: string, meta?: Record<string, unknown>): AuditEntry => ({
  actor: actor.userId,
  action,
  target,
  meta,
  ipPrefix: actor.ipPrefix ?? null,
});

export interface Reservation {
  name: string;
  user_id: string;
  gen: string;
  is_default: number;
  status: "active" | "suspended";
  last_seen_at: number | null;
  created_at: number;
}

export type RepoError =
  | "same_name"
  | "hold_quota"
  | "name_taken"
  | "name_on_hold"
  | "quota_exceeded"
  | "not_found"
  | "suspended";

export class NameRepoError extends Error {
  constructor(readonly code: RepoError) {
    super(code);
    this.name = "NameRepoError";
  }
}

export const newGen = () => randomHex(8);

function isNameTaken(e: unknown): boolean {
  return String((e as Error)?.message ?? e).includes("UNIQUE constraint failed: reservations.name");
}

export async function listNames(db: D1Database, userId: string): Promise<Reservation[]> {
  const r = await db.prepare("SELECT * FROM reservations WHERE user_id = ?1 ORDER BY created_at, name").bind(userId).all<Reservation>();
  return r.results;
}

export function getReservation(db: D1Database, name: string): Promise<Reservation | null> {
  return db.prepare("SELECT * FROM reservations WHERE name = ?1").bind(name).first<Reservation>();
}

/** A hold on `name` for this user (after they renamed or removed it). */
export function heldFor(db: D1Database, name: string, userId: string, now: number): Promise<{ renamed_to: string | null } | null> {
  return db
    .prepare("SELECT renamed_to FROM released_names WHERE name = ?1 AND held_for = ?2 AND until > ?3")
    .bind(name, userId, now)
    .first<{ renamed_to: string | null }>();
}

async function holdCount(db: D1Database, userId: string, now: number, except: string): Promise<number> {
  const r = await db
    .prepare("SELECT COUNT(*) AS n FROM released_names WHERE held_for = ?1 AND until > ?2 AND name <> ?3")
    .bind(userId, now, except)
    .first<{ n: number }>();
  return r?.n ?? 0;
}

/** Is the name held for someone else (or admin-blocked)? */
async function heldForOther(db: D1Database, name: string, userId: string, now: number): Promise<boolean> {
  const r = await db
    .prepare("SELECT 1 AS x FROM released_names WHERE name = ?1 AND held_for <> ?2 AND until > ?3")
    .bind(name, userId, now)
    .first();
  return r !== null;
}

/**
 * Atomically reserves `name` for `userId` (the first name becomes the default). Respects the quota
 * and 12-month holds; an explicit reclaim of the caller's own hold clears it in the same batch.
 */
export async function reserve(db: D1Database, userId: string, name: string, now: number, actor?: RepoActor): Promise<Reservation> {
  const gen = newGen();
  let changes = 0;
  try {
    const [ins] = await db.batch([
      db
        .prepare(
          `INSERT INTO reservations (name, user_id, gen, is_default, created_at)
           SELECT ?1, ?2, ?4, CASE WHEN EXISTS (SELECT 1 FROM reservations WHERE user_id = ?2) THEN 0 ELSE 1 END, ?3
            WHERE (SELECT COUNT(*) FROM reservations WHERE user_id = ?2) < (SELECT max_names FROM users WHERE id = ?2 AND status = 'active')
              AND NOT EXISTS (SELECT 1 FROM released_names WHERE name = ?1 AND held_for <> ?2 AND until > ?3)`,
        )
        .bind(name, userId, now, gen),
      ...(actor ? [auditIfChanged(db, auditOf(actor, "name.reserve", name))] : []),
      // Explicit reclaim: only when the insert above actually happened (gen matches).
      db
        .prepare(
          `DELETE FROM released_names WHERE name = ?1 AND held_for = ?2
             AND EXISTS (SELECT 1 FROM reservations WHERE name = ?1 AND user_id = ?2 AND gen = ?3)`,
        )
        .bind(name, userId, gen),
    ]);
    changes = ins?.meta.changes ?? 0;
  } catch (e) {
    if (isNameTaken(e)) throw new NameRepoError("name_taken");
    throw e;
  }
  if (changes === 1) return (await getReservation(db, name))!;
  if (await heldForOther(db, name, userId, now)) throw new NameRepoError("name_on_hold");
  throw new NameRepoError("quota_exceeded");
}

const outboxInsert = (action: string) =>
  `INSERT INTO do_outbox (id, name, action, payload, next_at, created_at)
   SELECT 'obx_' || lower(hex(randomblob(8))), ?1, '${action}', ?2, ?3, ?3`;

/** Classifies a failed guarded mutation on `name` by the caller. */
async function classify(db: D1Database, userId: string, name: string): Promise<never> {
  const r = await getReservation(db, name);
  if (!r || r.user_id !== userId) throw new NameRepoError("not_found");
  if (r.status === "suspended") throw new NameRepoError("suspended");
  throw new NameRepoError("not_found");
}

/**
 * Renames in place (quota-neutral, default kept, new gen). The old name is held 12 months for the
 * caller with a `renamed_to` hint; the old DO gets GOAWAY renamed via the outbox.
 */
export async function rename(
  db: D1Database,
  userId: string,
  oldName: string,
  newName: string,
  now: number,
  actor?: RepoActor,
): Promise<{ reservation: Reservation; outboxIds: string[] }> {
  if (oldName === newName) throw new NameRepoError("same_name");
  const cur = await getReservation(db, oldName);
  if (!cur || cur.user_id !== userId) throw new NameRepoError("not_found");
  if (cur.status === "suspended") throw new NameRepoError("suspended");
  const gen = newGen();
  let changes = 0;
  let outboxIds: string[] = [];
  try {
    const res = await db.batch([
      db
        .prepare(
          `UPDATE reservations SET name = ?2, gen = ?4
            WHERE name = ?1 AND user_id = ?3 AND status = 'active' AND gen = ?6
              AND EXISTS (SELECT 1 FROM users WHERE id = ?3 AND status = 'active')
              AND NOT EXISTS (SELECT 1 FROM released_names WHERE name = ?2 AND held_for <> ?3 AND until > ?5)
              AND (SELECT COUNT(*) FROM released_names WHERE held_for = ?3 AND until > ?5 AND name <> ?1) < ${MAX_HOLDS}`,
        )
        .bind(oldName, newName, userId, gen, now, cur.gen),
      auditIfChanged(db, auditOf(actor ?? { userId }, "name.rename", oldName, { to: newName })),
      db
        .prepare(
          `INSERT OR REPLACE INTO released_names (name, held_for, until, renamed_to)
           SELECT ?1, ?2, ?3, ?4 WHERE EXISTS (SELECT 1 FROM reservations WHERE name = ?4 AND user_id = ?2 AND gen = ?5)`,
        )
        .bind(oldName, userId, now + HOLD_SECONDS, newName, gen),
      // The caller's own hold on the new name (if any) is consumed by this rename.
      db
        .prepare(
          `DELETE FROM released_names WHERE name = ?1 AND held_for = ?2
             AND EXISTS (SELECT 1 FROM reservations WHERE name = ?1 AND user_id = ?2 AND gen = ?3)`,
        )
        .bind(newName, userId, gen),
      // Older holds that pointed at the old name now point at the new one (A→B→C hints say C).
      db
        .prepare(
          `UPDATE released_names SET renamed_to = ?1 WHERE renamed_to = ?2 AND held_for = ?3
             AND EXISTS (SELECT 1 FROM reservations WHERE name = ?1 AND user_id = ?3 AND gen = ?4)`,
        )
        .bind(newName, oldName, userId, gen),
      db
        .prepare(`${outboxInsert("goaway")} WHERE EXISTS (SELECT 1 FROM reservations WHERE name = ?4 AND user_id = ?5 AND gen = ?6) RETURNING id`)
        .bind(oldName, JSON.stringify({ reason: "renamed", newName, gen: cur.gen }), now, newName, userId, gen),
    ]);
    changes = res[0]?.meta.changes ?? 0;
    outboxIds = returnedIds(res[5]);
  } catch (e) {
    if (isNameTaken(e)) throw new NameRepoError("name_taken");
    throw e;
  }
  if (changes === 1) return { reservation: (await getReservation(db, newName))!, outboxIds };
  if (await heldForOther(db, newName, userId, now)) throw new NameRepoError("name_on_hold");
  if ((await holdCount(db, userId, now, oldName)) >= MAX_HOLDS) throw new NameRepoError("hold_quota");
  return classify(db, userId, oldName);
}

/**
 * Removes a name: hold first (12 months for the caller), GOAWAY deleted via the outbox, delete, and
 * promote the oldest remaining name to default if the default was removed. One batch.
 */
export async function remove(db: D1Database, userId: string, name: string, now: number, actor?: RepoActor): Promise<string[]> {
  const until = now + HOLD_SECONDS;
  // At the hold cap the name is released without a hold (nothing to squat; the owner's choice).
  const noHold = (await holdCount(db, userId, now, name)) >= MAX_HOLDS;
  // Everything after the hold is gated on it (or on the no-hold decision).
  const held = noHold ? "1" : `EXISTS (SELECT 1 FROM released_names WHERE name = ?1 AND held_for = ?2 AND until = ${until})`;
  const res = await db.batch([
    db
      .prepare(
        `INSERT OR REPLACE INTO released_names (name, held_for, until, renamed_to)
         SELECT name, user_id, ?3, NULL FROM reservations WHERE name = ?1 AND user_id = ?2 AND status = 'active'
            AND ?5 = 0 AND (SELECT COUNT(*) FROM released_names WHERE held_for = ?2 AND until > ?4 AND name <> ?1) < ${MAX_HOLDS}`,
      )
      .bind(name, userId, until, now, noHold ? 1 : 0),
    db
      .prepare(
        `INSERT INTO do_outbox (id, name, action, payload, next_at, created_at)
         SELECT 'obx_' || lower(hex(randomblob(8))), name, 'goaway',
                json_object('reason', 'deleted', 'gen', gen, 'message', 'this name was removed'), ?3, ?3
           FROM reservations WHERE name = ?1 AND user_id = ?2 AND status = 'active' AND ${held}
         RETURNING id`,
      )
      .bind(name, userId, now),
    db.prepare(`DELETE FROM reservations WHERE name = ?1 AND user_id = ?2 AND status = 'active' AND ${held}`).bind(name, userId),
    auditIfChanged(db, auditOf(actor ?? { userId }, "name.remove", name, noHold ? { hold: false } : undefined)),
    db
      .prepare(
        `UPDATE reservations SET is_default = 1
          WHERE name = (SELECT name FROM reservations WHERE user_id = ?1 ORDER BY created_at, name LIMIT 1)
            AND NOT EXISTS (SELECT 1 FROM reservations WHERE user_id = ?1 AND is_default = 1)`,
      )
      .bind(userId),
  ]);
  if ((res[2]?.meta.changes ?? 0) !== 1) await classify(db, userId, name);
  return returnedIds(res[1]);
}

/** Makes `name` the caller's default (the unique partial index keeps exactly one). */
export async function setDefault(db: D1Database, userId: string, name: string, actor?: RepoActor): Promise<void> {
  const res = await db.batch([
    db
      .prepare(
        `UPDATE reservations SET is_default = 0
          WHERE user_id = ?1 AND is_default = 1 AND EXISTS (SELECT 1 FROM reservations WHERE name = ?2 AND user_id = ?1)`,
      )
      .bind(userId, name),
    db.prepare("UPDATE reservations SET is_default = 1 WHERE name = ?2 AND user_id = ?1").bind(userId, name),
    auditIfChanged(db, auditOf(actor ?? { userId }, "name.default", name)),
  ]);
  if ((res[1]?.meta.changes ?? 0) !== 1) throw new NameRepoError("not_found");
}

export type ConnectAuthz = { ok: true; gen: string; trusted: boolean } | { ok: false; status: number; code: string; message: string; extra?: Record<string, unknown> };

/**
 * May `userId` connect an agent to `name`? Used by the Worker (fast path) and again by the DO at
 * connect time (closes the race with a concurrent rename/delete). Non-owned names answer 404 so
 * their existence isn't leaked; names the caller released answer 410 with the rename hint.
 */
export async function connectAuthz(db: D1Database, name: string, userId: string, now: number): Promise<ConnectAuthz> {
  const r = await db
    .prepare(
      `SELECT r.user_id, r.gen, r.status, u.status AS user_status, u.trusted, u.trust_revoked, u.created_at
         FROM reservations r JOIN users u ON u.id = r.user_id WHERE r.name = ?1`,
    )
    .bind(name)
    .first<{ user_id: string; gen: string; status: string; user_status: string; trusted: number; trust_revoked: number; created_at: number }>();
  if (r && r.user_id === userId) {
    if (r.status !== "active" || r.user_status !== "active") {
      return { ok: false, status: 403, code: "name_suspended", message: `\`${name}\` is suspended; contact abuse@tuzy.dev` };
    }
    // Same rule as auth-middleware, from this serialized read (a racing setTrusted can't be undone).
    const trusted = r.trust_revoked === 0 && (r.trusted === 1 || now - r.created_at >= AUTO_TRUST_SECONDS);
    return { ok: true, gen: r.gen, trusted };
  }
  const held = await heldFor(db, name, userId, now);
  if (held) {
    return held.renamed_to
      ? { ok: false, status: 410, code: "name_released", message: `\`${name}\` was renamed to \`${held.renamed_to}\``, extra: { new_name: held.renamed_to } }
      : { ok: false, status: 410, code: "name_released", message: `\`${name}\` was removed; reclaim it with \`tuzy names add ${name}\`` };
  }
  return { ok: false, status: 404, code: "name_not_reserved", message: `you don't own \`${name}\`: run \`tuzy names add ${name}\`` };
}
