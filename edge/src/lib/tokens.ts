/** API tokens: "tzy_" + 32 random bytes (base64url). Only the SHA-256 is stored. */
import { base64url, sha256Hex } from "./ids";

export const TOKEN_PREFIX = "tzy_";
/** Every token idle-expires after 90 days without use (validation session 1). */
export const TOKEN_IDLE_SECONDS = 90 * 24 * 3600;
/** last_used_at is written at most this often (keeps D1 writes off the hot path). */
export const LAST_USED_RESOLUTION_SECONDS = 3600;

export type TokenScope = "full" | "connect";

export function generateToken(): string {
  return TOKEN_PREFIX + base64url(crypto.getRandomValues(new Uint8Array(32)));
}

export const hashToken = (token: string) => sha256Hex(token);

export function looksLikeToken(t: string): boolean {
  return t.startsWith(TOKEN_PREFIX) && /^[A-Za-z0-9_-]{40,60}$/.test(t.slice(TOKEN_PREFIX.length));
}
