/**
 * POST /api/v1/auth/email/start   {email}                → {login_id, expires_in}
 * POST /api/v1/auth/email/verify  {login_id, code, label} → {token, user}
 *
 * The first successful verify creates the account. Responses for known and unknown emails are
 * identical in normal mode (no enumeration).
 */
import { Hono } from "hono";
import { isDisposable, isValidEmail, normalizeEmail } from "../lib/email-validation";
import { newId, nowSec } from "../lib/ids";
import { ipPrefix } from "../lib/ip-prefix";
import { CODE_TTL_SECONDS, normalizeCode } from "../lib/otp";
import { generateToken, hashToken } from "../lib/tokens";
import type { AppEnv } from "./auth-middleware";
import { ApiError, jsonBody } from "./errors";
import { consumeCodes, consumeMatched, issueCode, verifyCode } from "./login-codes";

export const authRoutes = new Hono<AppEnv>();

/** Strips C0/C1 controls and bidi overrides (labels are shown in terminals and lists). */
export function tokenLabel(v: unknown, fallback: string): string {
  const s = typeof v === "string" ? v.replace(/[\u0000-\u001f\u007f-\u009f\u200e\u200f\u202a-\u202e\u2066-\u2069]/g, "").trim() : "";
  return (s || fallback).slice(0, 64);
}

authRoutes.post("/email/start", async (c) => {
  const body = await jsonBody(c.req.raw);
  const email = normalizeEmail(String(body.email ?? ""));
  if (!isValidEmail(email)) throw new ApiError(400, "invalid_email", "that doesn't look like an email address");
  if (isDisposable(email)) throw new ApiError(400, "disposable_email", "disposable email addresses can't be used; use your real address");
  const loginId = await issueCode(c.env, c.executionCtx, email, "login", ipPrefix(c.req.header("cf-connecting-ip")));
  return c.json({ login_id: loginId, expires_in: CODE_TTL_SECONDS });
});

authRoutes.post("/email/verify", async (c) => {
  const body = await jsonBody(c.req.raw);
  const code = normalizeCode(body.code);
  if (!code) throw new ApiError(400, "invalid_code", "the code is 6 digits");
  const { email, id: codeId } = await verifyCode(c.env, String(body.login_id ?? ""), code, "login");

  const now = nowSec();
  const token = generateToken();
  const tokenId = newId("tok");
  const db = c.env.DB;
  // One transaction: consuming the matched code gates everything after it via changes(), so two
  // concurrent verifies of one code can never mint two tokens.
  const results = await db.batch([
    consumeMatched(db, codeId),
    db
      .prepare(
        `INSERT INTO users (id, email, created_at, last_login_at) SELECT ?1, ?2, ?3, ?3 WHERE changes() = 1
         ON CONFLICT(email) DO UPDATE SET last_login_at = excluded.last_login_at WHERE users.status = 'active'`,
      )
      .bind(newId("usr"), email, now),
    db
      .prepare(
        `INSERT INTO tokens (id, user_id, token_hash, scope, label, created_at)
         SELECT ?1, id, ?2, 'full', ?3, ?4 FROM users WHERE email = ?5 AND status = 'active' AND changes() = 1`,
      )
      .bind(tokenId, await hashToken(token), tokenLabel(body.label, "cli"), now, email),
    consumeCodes(db, email, "login"),
    db.prepare("SELECT id, status FROM users WHERE email = ?1").bind(email),
  ]);
  if (results[0]?.meta.changes !== 1) throw new ApiError(400, "invalid_code", "that code was already used; request a new one");
  const user = results[4]?.results[0] as { id: string; status: string } | undefined;
  if (!user || results[2]?.meta.changes !== 1) {
    if (user?.status === "suspended") throw new ApiError(403, "account_suspended", "this account is suspended; contact abuse@tuzy.dev");
    throw new ApiError(403, "account_deleted", "this account was deleted");
  }
  return c.json({ token, user: { id: user.id, email } });
});
