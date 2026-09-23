/**
 * Bearer-token authentication. The hot path is read-only: `last_used_at` is written at most
 * hourly via waitUntil (connects count as use, so a running agent keeps its token alive).
 */
import type { MiddlewareHandler } from "hono";
import { nowSec } from "../lib/ids";
import { hashToken, LAST_USED_RESOLUTION_SECONDS, looksLikeToken, TOKEN_IDLE_SECONDS, type TokenScope } from "../lib/tokens";
import { ApiError } from "./errors";

import { AUTO_TRUST_SECONDS } from "../lib/trust";
export { AUTO_TRUST_SECONDS };

export interface AuthContext {
  userId: string;
  tokenId: string;
  email: string;
  role: "user" | "admin";
  scope: TokenScope;
  trusted: boolean;
}

export type AppEnv = { Bindings: Env; Variables: { auth: AuthContext } };

interface TokenRow {
  token_id: string;
  scope: TokenScope;
  expires_at: number | null;
  last_used_at: number | null;
  token_created_at: number;
  revoked_at: number | null;
  user_id: string;
  email: string;
  status: "active" | "suspended" | "deleted";
  role: "user" | "admin";
  trusted: number;
  trust_revoked: number;
  user_created_at: number;
}

export function bearer(header: string | null | undefined): string | null {
  const m = /^Bearer\s+(\S+)$/i.exec(header ?? "");
  return m?.[1] ?? null;
}

export async function authenticate(env: Env, ctx: { waitUntil(p: Promise<unknown>): void }, authorization: string | null | undefined): Promise<AuthContext> {
  const token = bearer(authorization);
  if (!token || !looksLikeToken(token)) throw new ApiError(401, "unauthorized", "not logged in: run `tuzy login`");
  const row = await env.DB.prepare(
    `SELECT t.id AS token_id, t.scope, t.expires_at, t.last_used_at, t.created_at AS token_created_at, t.revoked_at,
            u.id AS user_id, u.email, u.status, u.role, u.trusted, u.trust_revoked, u.created_at AS user_created_at
       FROM tokens t JOIN users u ON u.id = t.user_id
      WHERE t.token_hash = ?1`,
  )
    .bind(await hashToken(token))
    .first<TokenRow>();
  const now = nowSec();
  if (!row || row.revoked_at !== null || row.status === "deleted") {
    throw new ApiError(401, "unauthorized", "not logged in: run `tuzy login`");
  }
  if ((row.expires_at !== null && row.expires_at <= now) || (row.last_used_at ?? row.token_created_at) < now - TOKEN_IDLE_SECONDS) {
    throw new ApiError(401, "token_expired", "session expired: run `tuzy login`");
  }
  if (row.status === "suspended") throw new ApiError(403, "account_suspended", "this account is suspended; contact abuse@tuzy.dev");

  if (row.last_used_at === null || now - row.last_used_at >= LAST_USED_RESOLUTION_SECONDS) {
    ctx.waitUntil(
      env.DB.prepare("UPDATE tokens SET last_used_at = ?1 WHERE id = ?2 AND (last_used_at IS NULL OR last_used_at < ?3)")
        .bind(now, row.token_id, now - LAST_USED_RESOLUTION_SECONDS)
        .run()
        .catch((e) => console.error("last_used_at update failed", e)),
    );
  }
  return {
    userId: row.user_id,
    tokenId: row.token_id,
    email: row.email,
    role: row.role,
    scope: row.scope,
    trusted: row.trust_revoked === 0 && (row.trusted === 1 || now - row.user_created_at >= AUTO_TRUST_SECONDS),
  };
}

/**
 * Hono middleware. `connect`-scope tokens (CI) may only call endpoints that opt in
 * (connect, GET /me, GET /names); everything else answers 403 insufficient_scope.
 */
export function requireAuth(opts: { allowConnectScope?: boolean } = {}): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    const auth = await authenticate(c.env, c.executionCtx, c.req.header("authorization"));
    if (auth.scope === "connect" && !opts.allowConnectScope) {
      throw new ApiError(403, "insufficient_scope", "this token can only open tunnels (scope: connect)");
    }
    const rl = await c.env.RL_API.limit({ key: auth.tokenId });
    if (!rl.success) throw new ApiError(429, "rate_limited", "too many requests; slow down", {}, { "retry-after": "60" });
    c.set("auth", auth);
    await next();
  };
}
