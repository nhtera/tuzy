/** 6-digit one-time login codes. */
import { sha256Hex, timingSafeEqualHex } from "./ids";

export const CODE_TTL_SECONDS = 600;
export const MAX_CODE_ATTEMPTS = 5;

/** Uniform 6-digit code via rejection sampling (no modulo bias). */
export function generateCode(): string {
  const limit = 2 ** 32 - (2 ** 32 % 1_000_000);
  const buf = new Uint32Array(1);
  for (;;) {
    crypto.getRandomValues(buf);
    if (buf[0]! < limit) return String(buf[0]! % 1_000_000).padStart(6, "0");
  }
}

/** The stored hash binds the code to its login id, so codes never collide across rows. */
export const hashCode = (loginId: string, code: string) => sha256Hex(`${loginId}:${code}`);

/** Accepts "482913", "482 913", "482-913". Returns null when it is not 6 digits. */
export function normalizeCode(input: unknown): string | null {
  if (typeof input !== "string") return null;
  const digits = input.replace(/[\s-]/g, "");
  return /^\d{6}$/.test(digits) ? digits : null;
}

export async function codeMatches(loginId: string, code: string, storedHash: string): Promise<boolean> {
  return timingSafeEqualHex(await hashCode(loginId, code), storedHash);
}
