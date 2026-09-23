/** Helpers for API tests: JSON requests to the apex and a full email-login flow. */
import { exports } from "cloudflare:workers";
import { expect } from "vitest";
import { sentEmails } from "../../src/lib/email";

let ipSeq = 0;
/** A fresh visitor IP per call site keeps per-prefix rate limits from leaking between tests. */
export const freshIp = () => `198.51.${(ipSeq >> 8) & 255}.${ipSeq++ & 255}`;

export async function api(
  path: string,
  opts: { method?: string; token?: string; body?: unknown; ip?: string } = {},
): Promise<{ status: number; json: any }> {
  const headers: Record<string, string> = { host: "tuzy.dev", "cf-connecting-ip": opts.ip ?? freshIp() };
  if (opts.token) headers.authorization = `Bearer ${opts.token}`;
  if (opts.body !== undefined) headers["content-type"] = "application/json";
  const res = await exports.default.fetch(`https://tuzy.dev/api/v1${path}`, {
    method: opts.method ?? (opts.body !== undefined ? "POST" : "GET"),
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let json: any = null;
  try {
    json = JSON.parse(text);
  } catch {
    json = text;
  }
  return { status: res.status, json };
}

/** The code from the most recent email to `to`. */
export function lastCode(to: string): string {
  const m = [...sentEmails].reverse().find((e) => e.to === to);
  if (!m) throw new Error(`no email sent to ${to}`);
  return /(\d{3}) (\d{3})/.exec(m.text)!.slice(1).join("");
}

/** Full login: start + verify. Returns the token and user id. */
export async function login(email: string, label = "test"): Promise<{ token: string; userId: string }> {
  const start = await api("/auth/email/start", { body: { email } });
  expect(start.status, JSON.stringify(start.json)).toBe(200);
  const verify = await api("/auth/email/verify", { body: { login_id: start.json.login_id, code: lastCode(email), label } });
  expect(verify.status, JSON.stringify(verify.json)).toBe(200);
  return { token: verify.json.token, userId: verify.json.user.id };
}
