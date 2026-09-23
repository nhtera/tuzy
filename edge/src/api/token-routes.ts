/**
 * GET    /api/v1/tokens          list your tokens
 * POST   /api/v1/tokens          {label?, scope?="connect", expires_in_days?} → token (shown once)
 * DELETE /api/v1/tokens/:id|current
 *
 * Revoking a token also closes its live tunnels: outbox rows (same D1 batch) tell every Durable
 * Object the token was connected to via RPC `revokeToken` → GOAWAY revoked.
 */
import { Hono } from "hono";
import { newId, nowSec } from "../lib/ids";
import { pushOutbox, returnedIds } from "../lib/outbox";
import { generateToken, hashToken, TOKEN_IDLE_SECONDS, type TokenScope } from "../lib/tokens";
import { tokenLabel } from "./auth-routes";
import { requireAuth, type AppEnv } from "./auth-middleware";
import { ApiError, jsonBody } from "./errors";

export const tokenRoutes = new Hono<AppEnv>();
tokenRoutes.use("*", requireAuth());

interface TokenListRow {
  id: string;
  scope: TokenScope;
  label: string;
  created_at: number;
  expires_at: number | null;
  last_used_at: number | null;
}

tokenRoutes.get("/", async (c) => {
  const { userId, tokenId } = c.get("auth");
  const now = nowSec();
  const rows = await c.env.DB.prepare(
    `SELECT id, scope, label, created_at, expires_at, last_used_at FROM tokens
      WHERE user_id = ?1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?2)
        AND COALESCE(last_used_at, created_at) >= ?3
      ORDER BY created_at DESC`,
  )
    .bind(userId, now, now - TOKEN_IDLE_SECONDS)
    .all<TokenListRow>();
  return c.json({ tokens: rows.results.map((t) => ({ ...t, current: t.id === tokenId })) });
});

tokenRoutes.post("/", async (c) => {
  const { userId } = c.get("auth");
  const body = await jsonBody(c.req.raw);
  const scope = (body.scope ?? "connect") as TokenScope;
  if (scope !== "connect" && scope !== "full") throw new ApiError(400, "invalid_scope", 'scope must be "connect" or "full"');
  let expiresAt: number | null = null;
  if (body.expires_in_days !== undefined) {
    const d = Number(body.expires_in_days);
    if (!Number.isInteger(d) || d < 1 || d > 3650) throw new ApiError(400, "invalid_expiry", "expires_in_days must be 1–3650");
    expiresAt = nowSec() + d * 86400;
  }
  const now = nowSec();
  const count = await c.env.DB.prepare(
    `SELECT COUNT(*) AS n FROM tokens WHERE user_id = ?1 AND revoked_at IS NULL
       AND (expires_at IS NULL OR expires_at > ?2) AND COALESCE(last_used_at, created_at) >= ?3`,
  )
    .bind(userId, now, now - TOKEN_IDLE_SECONDS)
    .first<{ n: number }>();
  if ((count?.n ?? 0) >= 50) throw new ApiError(409, "too_many_tokens", "you have 50 active tokens; remove some with `tuzy tokens rm`");

  const token = generateToken();
  const id = newId("tok");
  const label = tokenLabel(body.label, scope === "connect" ? "ci" : "cli");
  await c.env.DB.prepare(
    "INSERT INTO tokens (id, user_id, token_hash, scope, label, created_at, expires_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
  )
    .bind(id, userId, await hashToken(token), scope, label, nowSec(), expiresAt)
    .run();
  return c.json({ token, id, scope, label, expires_at: expiresAt }, 201);
});

tokenRoutes.delete("/:id", async (c) => {
  const auth = c.get("auth");
  const id = c.req.param("id") === "current" ? auth.tokenId : c.req.param("id");
  const now = nowSec();
  const db = c.env.DB;
  const [upd, obx] = await db.batch([
    db.prepare("UPDATE tokens SET revoked_at = ?1 WHERE id = ?2 AND user_id = ?3 AND revoked_at IS NULL").bind(now, id, auth.userId),
    // Only if the revoke above happened (ownership predicate), notify every DO this token used.
    db
      .prepare(
        `INSERT INTO do_outbox (id, name, action, payload, next_at, created_at)
         SELECT 'obx_' || lower(hex(randomblob(8))), name, 'revokeToken', json_object('tokenId', ?1), ?2, ?2
           FROM tunnel_sessions
          WHERE token_id = ?1 AND EXISTS (SELECT 1 FROM tokens WHERE id = ?1 AND user_id = ?3 AND revoked_at = ?2)
         RETURNING id`,
      )
      .bind(id, now, auth.userId),
  ]);
  if (!upd || upd.meta.changes !== 1) throw new ApiError(404, "not_found", "no such token");
  const ids = returnedIds(obx);
  if (ids.length) c.executionCtx.waitUntil(pushOutbox(c.env, ids).catch((e) => console.error("outbox push failed", e)));
  return c.json({ revoked: id });
});

