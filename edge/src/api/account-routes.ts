/**
 * POST /api/v1/account/delete/start                     → emails a confirmation code
 * POST /api/v1/account/delete/confirm {login_id, code}  → soft-deletes the account
 *
 * Deletion (one D1 batch): status 'deleted', every token revoked, live tunnels get GOAWAY deleted
 * through the outbox. Phase 5 moves reservations into 12-month holds; phase 7 erases the email
 * after 30 days (hash kept for abuse linkage).
 */
import { Hono } from "hono";
import { nowSec } from "../lib/ids";
import { ipPrefix } from "../lib/ip-prefix";
import { CODE_TTL_SECONDS, normalizeCode } from "../lib/otp";
import { pushOutbox, returnedIds } from "../lib/outbox";
import { requireAuth, type AppEnv } from "./auth-middleware";
import { ApiError, jsonBody } from "./errors";
import { issueCode, verifyCode } from "./login-codes";

export const accountRoutes = new Hono<AppEnv>();
accountRoutes.use("*", requireAuth());

accountRoutes.post("/delete/start", async (c) => {
  const { email } = c.get("auth");
  const loginId = await issueCode(c.env, c.executionCtx, email, "delete", ipPrefix(c.req.header("cf-connecting-ip")));
  return c.json({ login_id: loginId, expires_in: CODE_TTL_SECONDS });
});

accountRoutes.post("/delete/confirm", async (c) => {
  const auth = c.get("auth");
  const body = await jsonBody(c.req.raw);
  const code = normalizeCode(body.code);
  if (!code) throw new ApiError(400, "invalid_code", "the code is 6 digits");
  await verifyCode(c.env, String(body.login_id ?? ""), code, "delete", auth.email);

  const now = nowSec();
  const db = c.env.DB;
  // Every statement after the first only acts if THIS request deleted the account.
  const deletedNow = "EXISTS (SELECT 1 FROM users WHERE id = ?2 AND status = 'deleted' AND deleted_at = ?1)";
  const [upd, , , obx] = await db.batch([
    db.prepare("UPDATE users SET status = 'deleted', deleted_at = ?1 WHERE id = ?2 AND status = 'active'").bind(now, auth.userId),
    db.prepare(`UPDATE tokens SET revoked_at = ?1 WHERE user_id = ?2 AND revoked_at IS NULL AND ${deletedNow}`).bind(now, auth.userId),
    db.prepare(`UPDATE login_codes SET consumed_at = ?1 WHERE email = ?3 AND consumed_at IS NULL AND ${deletedNow}`).bind(now, auth.userId, auth.email),
    // Close this user's live tunnels (revokeUser only touches sockets of this user).
    db
      .prepare(
        `INSERT INTO do_outbox (id, name, action, payload, next_at, created_at)
         SELECT 'obx_' || lower(hex(randomblob(8))), name, 'revokeUser', json_object('userId', ?2), ?1, ?1
           FROM (SELECT DISTINCT name FROM tunnel_sessions WHERE user_id = ?2) WHERE ${deletedNow}
         RETURNING id`,
      )
      .bind(now, auth.userId),
    db.prepare(`DELETE FROM tunnel_sessions WHERE user_id = ?2 AND ${deletedNow}`).bind(now, auth.userId),
  ]);
  if (!upd || upd.meta.changes !== 1) throw new ApiError(409, "not_active", "this account is not active");
  const ids = returnedIds(obx);
  if (ids.length) c.executionCtx.waitUntil(pushOutbox(c.env, ids).catch((e) => console.error("outbox push failed", e)));
  return c.json({ deleted: true });
});
