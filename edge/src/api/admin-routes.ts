/**
 * Admin API (phase 7), role=admin + full-scope token. Every mutation is one D1 batch
 * {change, audit, outbox} via lib/moderation; outbox rows are pushed now and retried by cron.
 *
 *   GET  /admin/reports[?status=open|actioned|dismissed|all]
 *   POST /admin/reports/:id            {status: "actioned"|"dismissed"}  (actioned → owner trust revoked)
 *   POST /admin/users/:user/suspend | unsuspend | trust | untrust
 *   POST /admin/users/:user/max-names  {max}
 *   POST /admin/names/:name/suspend | unsuspend | block | release
 *   POST /admin/names/:name/reserve    {user}
 *   POST /admin/settings/new-account-max-names {max}   (quota for accounts created from now on)
 *   GET  /admin/audit[?target=&actor=&action=&limit=]
 *   GET  /admin/outbox
 *
 * `:user` is a user id (usr_…) or an email address.
 */
import { Hono, type Context } from "hono";
import { audit, auditIfChanged } from "../lib/audit";
import { normalizeEmail } from "../lib/email-validation";
import { ipPrefix } from "../lib/ip-prefix";
import { nowSec } from "../lib/ids";
import { NEW_ACCOUNT_MAX_NAMES_KEY, blockName, pushLater, releaseName, reserveForUser, setMaxNames, setNameSuspended, setUserSuspended, setUserTrust, type Actor } from "../lib/moderation";
import { MAX_OUTBOX_ATTEMPTS } from "../lib/outbox";
import { requireAuth, type AppEnv } from "./auth-middleware";
import { ApiError, jsonBody } from "./errors";

export const adminRoutes = new Hono<AppEnv>();

adminRoutes.use("*", requireAuth(), async (c, next) => {
  if (c.get("auth").role !== "admin") throw new ApiError(403, "forbidden", "admin only");
  await next();
});

const actorOf = (c: Context<AppEnv>): Actor => ({ userId: c.get("auth").userId, ipPrefix: ipPrefix(c.req.header("cf-connecting-ip")) });

async function resolveUser(db: D1Database, ref: string): Promise<{ id: string; email: string; status: string }> {
  const byEmail = ref.includes("@");
  const u = await db
    .prepare(`SELECT id, email, status FROM users WHERE ${byEmail ? "email" : "id"} = ?1`)
    .bind(byEmail ? normalizeEmail(ref) : ref)
    .first<{ id: string; email: string; status: string }>();
  if (!u) throw new ApiError(404, "not_found", `no user ${ref}`);
  return u;
}

async function reason(c: Context<AppEnv>): Promise<string | undefined> {
  if (!c.req.header("content-type")?.includes("json")) return undefined;
  const b = await jsonBody(c.req.raw);
  return typeof b.reason === "string" ? b.reason.slice(0, 500) : undefined;
}

adminRoutes.get("/reports", async (c) => {
  const status = c.req.query("status") ?? "open";
  const where = status === "all" ? "1" : "r.status = ?1";
  const rows = await c.env.DB.prepare(
    `SELECT r.id, r.name, r.category, r.details, r.reporter_email, r.reporter_user_id IS NOT NULL AS reporter_logged_in,
            r.reporter_prefix, r.status, r.alert_sent, r.created_at, res.user_id AS owner, res.status AS name_status
       FROM abuse_reports r LEFT JOIN reservations res ON res.name = r.name
      WHERE ${where} ORDER BY r.created_at DESC LIMIT 200`,
  )
    .bind(...(status === "all" ? [] : [status]))
    .all();
  return c.json({ reports: rows.results });
});

adminRoutes.post("/reports/:id", async (c) => {
  const body = await jsonBody(c.req.raw);
  const status = String(body.status ?? "");
  if (status !== "actioned" && status !== "dismissed") throw new ApiError(400, "bad_request", 'status must be "actioned" or "dismissed"');
  const id = c.req.param("id");
  const actor = actorOf(c);
  const db = c.env.DB;
  const [upd] = await db.batch([
    db.prepare("UPDATE abuse_reports SET status = ?2 WHERE id = ?1 AND status <> ?2").bind(id, status),
    auditIfChanged(db, { actor: actor.userId, action: `abuse.${status}`, target: id, ipPrefix: actor.ipPrefix }),
  ]);
  if ((upd?.meta.changes ?? 0) === 0) {
    const exists = await db.prepare("SELECT 1 AS x FROM abuse_reports WHERE id = ?1").bind(id).first();
    if (!exists) throw new ApiError(404, "not_found", "no such report");
    return c.json({ id, status, changed: false });
  }
  let trustRevoked = false;
  if (status === "actioned") {
    // An upheld report makes the owner's tunnels show the interstitial again (sticky).
    // The owner when the report was filed (the name may have changed hands since).
    const owner = await db
      .prepare("SELECT owner_user_id AS user_id FROM abuse_reports WHERE id = ?1 AND owner_user_id IS NOT NULL")
      .bind(id)
      .first<{ user_id: string }>();
    if (owner) {
      const out = await setUserTrust(c.env, owner.user_id, "untrust", actor, `report ${id} upheld`);
      pushLater(c.env, c.executionCtx, out.outboxIds);
      trustRevoked = out.changed;
    }
  }
  return c.json({ id, status, changed: true, trust_revoked: trustRevoked });
});

for (const op of ["suspend", "unsuspend", "trust", "untrust"] as const) {
  adminRoutes.post(`/users/:user/${op}`, async (c) => {
    const u = await resolveUser(c.env.DB, c.req.param("user"));
    const why = await reason(c);
    const out =
      op === "suspend" || op === "unsuspend"
        ? await setUserSuspended(c.env, u.id, op === "suspend", actorOf(c), why)
        : await setUserTrust(c.env, u.id, op, actorOf(c), why);
    pushLater(c.env, c.executionCtx, out.outboxIds);
    return c.json({ user: u.id, action: op, changed: out.changed });
  });
}

adminRoutes.post("/users/:user/max-names", async (c) => {
  const u = await resolveUser(c.env.DB, c.req.param("user"));
  const max = Number((await jsonBody(c.req.raw)).max);
  if (!Number.isInteger(max) || max < 0 || max > 1000) throw new ApiError(400, "bad_request", "max must be an integer 0–1000");
  const out = await setMaxNames(c.env, u.id, max, actorOf(c));
  return c.json({ user: u.id, max_names: max, changed: out.changed });
});

adminRoutes.post("/settings/new-account-max-names", async (c) => {
  const max = Number((await jsonBody(c.req.raw)).max);
  if (!Number.isInteger(max) || max < 0 || max > 1000) throw new ApiError(400, "bad_request", "max must be an integer 0–1000");
  const actor = actorOf(c);
  await c.env.NAMES_KV.put(NEW_ACCOUNT_MAX_NAMES_KEY, String(max));
  await audit(c.env.DB, { actor: actor.userId, action: "settings.new_account_max_names", target: String(max), ipPrefix: actor.ipPrefix }).run();
  return c.json({ new_account_max_names: max });
});

adminRoutes.get("/audit", async (c) => {
  const target = c.req.query("target");
  const actor = c.req.query("actor");
  const limit = Math.min(Math.max(Number(c.req.query("limit") ?? 100) || 100, 1), 500);
  const where: string[] = [];
  const params: unknown[] = [];
  if (target) where.push(`target = ?${params.push(target)}`);
  if (actor) where.push(`actor_user_id = ?${params.push(actor)}`);
  const action = c.req.query("action");
  if (action) where.push(`action LIKE ?${params.push(action.replace(/[%_]/g, "") + "%")}`);
  const rows = await c.env.DB.prepare(
    `SELECT id, actor_user_id, action, target, meta, ip_prefix, created_at FROM audit_log
      ${where.length ? `WHERE ${where.join(" AND ")}` : ""} ORDER BY created_at DESC LIMIT ${limit}`,
  )
    .bind(...params)
    .all();
  return c.json({ entries: rows.results });
});

adminRoutes.post("/names/:name/:op{suspend|unsuspend|block|release}", async (c) => {
  const name = c.req.param("name").toLowerCase();
  const op = c.req.param("op");
  const why = await reason(c);
  const actor = actorOf(c);
  let out;
  if (op === "suspend" || op === "unsuspend") {
    out = await setNameSuspended(c.env, name, op === "suspend", actor, why);
    if (!out.changed) {
      const r = await c.env.DB.prepare("SELECT status FROM reservations WHERE name = ?1").bind(name).first<{ status: string }>();
      if (!r) throw new ApiError(404, "not_found", `${name} is not reserved`);
    }
  } else if (op === "block") {
    out = await blockName(c.env, c.executionCtx, name, actor, why);
  } else {
    out = await releaseName(c.env, name, actor);
  }
  pushLater(c.env, c.executionCtx, out.outboxIds);
  return c.json({ name, action: op, changed: out.changed });
});

adminRoutes.post("/names/:name/reserve", async (c) => {
  const name = c.req.param("name").toLowerCase();
  if (!/^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/.test(name)) throw new ApiError(400, "invalid_name", "invalid name");
  const u = await resolveUser(c.env.DB, String((await jsonBody(c.req.raw)).user ?? ""));
  try {
    await reserveForUser(c.env, name, u.id, actorOf(c));
  } catch (e) {
    if (String((e as Error).message).includes("UNIQUE constraint failed: reservations.name")) throw new ApiError(409, "name_taken", `${name} is reserved already`);
    throw e;
  }
  return c.json({ name, user: u.id }, 201);
});

adminRoutes.get("/outbox", async (c) => {
  const now = nowSec();
  const counts = await c.env.DB.prepare(
    `SELECT SUM(done_at IS NULL) AS pending,
            SUM(done_at IS NULL AND created_at < ?1) AS stale,
            SUM(done_at IS NULL AND attempts >= ?2) AS dead
       FROM do_outbox`,
  )
    .bind(now - 300, MAX_OUTBOX_ATTEMPTS)
    .first<{ pending: number | null; stale: number | null; dead: number | null }>();
  const failing = await c.env.DB.prepare(
    `SELECT id, name, action, attempts, created_at, next_at FROM do_outbox
      WHERE done_at IS NULL ORDER BY created_at LIMIT 50`,
  ).all();
  return c.json({ pending: counts?.pending ?? 0, stale: counts?.stale ?? 0, dead: counts?.dead ?? 0, rows: failing.results });
});
