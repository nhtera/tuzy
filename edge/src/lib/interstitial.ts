/**
 * Browser interstitial for untrusted tunnels (phase 7). Pure helpers: the show/skip decision, the
 * signed `tuzy_ok` cookie and the `to` sanitizer. The cookie is host-bound: a sibling tunnel can't
 * reuse it (and the header policy strips `tuzy_*` cookies both ways, so apps can't read or set it).
 */

export const COOKIE_NAME = "tuzy_ok";
export const COOKIE_MAX_AGE = 7 * 24 * 3600;
export const CONTINUE_PATH = "/__tuzy/continue";

const NAVIGATION_DESTS = new Set(["document", "iframe", "frame"]);

/**
 * Browser navigations only (any method: an auto-submitted cross-site form is a navigation too;
 * frames too, so a page can't be shown inside another site). Webhooks, curl, fetch/XHR and API
 * calls send no navigation fetch metadata and pass straight through.
 */
export function wantsInterstitial(method: string, headers: Headers): boolean {
  const dest = headers.get("sec-fetch-dest");
  if (dest !== null) return NAVIGATION_DESTS.has(dest);
  if (headers.get("sec-fetch-mode") !== null || headers.get("sec-fetch-site") !== null) return false;
  return method === "GET" && /\btext\/html\b/i.test(headers.get("accept") ?? "");
}

/** `to` must be a local path: one leading slash, not `//` or `/\` (protocol-relative), else "/". */
export function sanitizeTo(to: string | null): string {
  if (!to || !/^\/(?![/\\])/.test(to) || /[\u0000-\u001f\u007f]/.test(to)) return "/";
  return to;
}

async function hmacHex(secret: string, data: string): Promise<string> {
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(data)));
  return [...sig].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export async function signCookie(secret: string, host: string, nowSec: number): Promise<string> {
  const exp = nowSec + COOKIE_MAX_AGE;
  return `${exp}.${await hmacHex(secret, `${host.toLowerCase()}|${exp}`)}`;
}

/** Every value of `name` (a sibling tunnel can plant a same-named Domain= cookie to shadow ours). */
function cookieValues(cookieHeader: string | null, name: string): string[] {
  const out: string[] = [];
  for (const part of (cookieHeader ?? "").split(";")) {
    const i = part.indexOf("=");
    if (i !== -1 && part.slice(0, i).trim() === name) out.push(part.slice(i + 1).trim());
  }
  return out.slice(0, 8);
}

/** True when the request carries an unexpired `tuzy_ok` cookie signed for exactly this host. */
export async function hasValidCookie(secret: string, host: string, cookieHeader: string | null, nowSec: number): Promise<boolean> {
  if (!secret) return false; // misconfigured: keep showing the interstitial (fail closed)
  for (const v of cookieValues(cookieHeader, COOKIE_NAME)) {
    const m = /^(\d{1,12})\.([0-9a-f]{64})$/.exec(v);
    if (!m) continue;
    const exp = Number(m[1]);
    if (exp <= nowSec || exp > nowSec + COOKIE_MAX_AGE) continue;
    const want = await hmacHex(secret, `${host.toLowerCase()}|${exp}`);
    let diff = 0;
    for (let i = 0; i < want.length; i++) diff |= want.charCodeAt(i) ^ m[2]!.charCodeAt(i);
    if (diff === 0) return true;
  }
  return false;
}

/**
 * The continue click must come from the interstitial itself (same origin, user-activated): a
 * cross-site link or a sibling tunnel's <img> to /__tuzy/continue must not mint the cookie.
 * Browsers without fetch metadata fall back to a same-origin Referer (the interstitial sends it).
 */
export function isContinueFromInterstitial(headers: Headers, origin: string): boolean {
  const site = headers.get("sec-fetch-site");
  if (site !== null) {
    return site === "same-origin" && headers.get("sec-fetch-dest") === "document" && headers.get("sec-fetch-user") === "?1";
  }
  const ref = headers.get("referer");
  if (!ref) return false;
  try {
    return new URL(ref).origin === origin;
  } catch {
    return false;
  }
}

export function setCookieHeader(value: string): string {
  return `${COOKIE_NAME}=${value}; Max-Age=${COOKIE_MAX_AGE}; Path=/; Secure; HttpOnly; SameSite=Lax`;
}
