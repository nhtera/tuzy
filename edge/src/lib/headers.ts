/**
 * Header policy between visitors and agents (phase 2 "Header policy").
 *
 * Visitor → agent: drop hop-by-hop, every `cf-*` / `x-tuzy-*`, `tuzy-skip-warning`, `tuzy_*`
 * cookies and client-supplied `x-forwarded-*`; then set `x-forwarded-*` from edge meta.
 * Agent → visitor: drop hop-by-hop and any `Set-Cookie` named `tuzy_*`.
 */

export type HeaderPairs = [string, string][];

/** Meta the Worker captures from the visitor request BEFORE stripping headers. */
export interface VisitorMeta {
  remoteIp: string;
  continent: string;
  /** "https" in production, "http" under local dev. */
  proto: string;
}

/** Headers the Worker uses to pass meta to the DO (client copies are always stripped first). */
export const META_REMOTE_IP = "x-tuzy-remote-ip";
export const META_CONTINENT = "x-tuzy-continent";
export const META_PROTO = "x-tuzy-proto";
/** Set by the Worker when the visitor sent `tuzy-skip-warning` (automation opts out of the interstitial). */
export const META_SKIP_WARNING = "x-tuzy-skip-warning";
/** JSON connect meta on the internal `connect.internal` request. */
export const META_CONNECT = "x-tuzy-meta";

const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "proxy-connection",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);

/** WebSocket handshake headers the edge terminates; only the subprotocol is forwarded. */
const WS_HANDSHAKE = new Set(["sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions", "sec-websocket-accept"]);

const TUZY_COOKIE_PREFIX = "tuzy_";

/** Client-IP headers apps commonly trust; visitors must not be able to set them. */
const CLIENT_IP_HEADERS = new Set(["x-real-ip", "true-client-ip", "client-ip", "x-client-ip", "x-cluster-client-ip"]);

/** Names listed in a `Connection` header are hop-by-hop too (RFC 9110 §7.6.1). */
function connectionListed(headers: Headers): Set<string> {
  const listed = new Set<string>();
  for (const token of (headers.get("connection") ?? "").split(",")) {
    const t = token.trim().toLowerCase();
    if (t) listed.add(t);
  }
  return listed;
}

function isEdgeInternal(name: string): boolean {
  return name.startsWith("cf-") || name.startsWith("x-tuzy-") || name === "cdn-loop" || name === "tuzy-skip-warning";
}

/** Removes `tuzy_*` pairs from a Cookie header value; returns null when nothing is left. */
export function stripTuzyCookies(cookie: string): string | null {
  const kept = cookie
    .split(";")
    .map((c) => c.trim())
    .filter((c) => c.length > 0 && !c.toLowerCase().startsWith(TUZY_COOKIE_PREFIX));
  return kept.length > 0 ? kept.join("; ") : null;
}

/**
 * Strips every client-supplied edge-internal header (`cf-*`, `x-tuzy-*`, …) in place. The Worker
 * calls this on tunnel and connect requests after reading meta, so meta headers the DO sees can
 * only have been set by the edge.
 */
export function stripEdgeInternal(headers: Headers): void {
  for (const name of [...headers.keys()]) {
    if (isEdgeInternal(name)) headers.delete(name);
  }
}

/**
 * Cloudflare's front line rewrites the visitor's `Accept-Encoding` (to "gzip, br") before the
 * Worker runs and keeps the original in `request.cf.clientAcceptEncoding`. Put the original back:
 * signed requests (AWS SigV4 from aws-sdk-go-v2 signs `Accept-Encoding: identity`) must reach the
 * app unchanged. No `cf` (local dev) or no original recorded: left as is.
 */
export function restoreAcceptEncoding(headers: Headers, cf: { clientAcceptEncoding?: string } | undefined): void {
  const original = cf?.clientAcceptEncoding;
  if (typeof original !== "string") return;
  if (original === "") headers.delete("accept-encoding");
  else headers.set("accept-encoding", original);
}

/** Builds the REQ_HEAD header list for the agent. `host` is the visitor Host header. */
export function visitorToAgentHeaders(headers: Headers, meta: VisitorMeta): HeaderPairs {
  const listed = connectionListed(headers);
  const out: HeaderPairs = [];
  let host = "";
  for (const [rawName, value] of headers) {
    const name = rawName.toLowerCase();
    if (name === "host") host = value;
    if (HOP_BY_HOP.has(name) || listed.has(name) || WS_HANDSHAKE.has(name) || isEdgeInternal(name)) continue;
    if (name.startsWith("x-forwarded-") || name === "forwarded" || CLIENT_IP_HEADERS.has(name)) continue;
    if (name === "cookie") {
      const kept = stripTuzyCookies(value);
      if (kept !== null) out.push([name, kept]);
      continue;
    }
    out.push([name, value]);
  }
  out.push(["x-forwarded-for", meta.remoteIp], ["x-real-ip", meta.remoteIp], ["x-forwarded-proto", meta.proto]);
  if (host) out.push(["x-forwarded-host", host]);
  return out;
}

function setCookieName(value: string): string {
  const eq = value.indexOf("=");
  return (eq === -1 ? value : value.slice(0, eq)).trim().toLowerCase();
}

/**
 * Builds visitor response headers from RES_HEAD pairs. Duplicate names (e.g. Set-Cookie) are
 * preserved. For a 101 only the negotiated subprotocol is forwarded.
 */
export function agentToVisitorHeaders(pairs: HeaderPairs, status: number): Headers {
  const out = new Headers();
  const listed = new Set<string>();
  for (const [name, value] of pairs) {
    if (name.toLowerCase() === "connection") for (const t of value.split(",")) listed.add(t.trim().toLowerCase());
  }
  for (const [rawName, value] of pairs) {
    const name = rawName.toLowerCase();
    if (status === 101 && name !== "sec-websocket-protocol") continue;
    if (HOP_BY_HOP.has(name) || listed.has(name) || WS_HANDSHAKE.has(name) || name.startsWith("x-tuzy-")) continue;
    if (name === "set-cookie" && setCookieName(value).startsWith(TUZY_COOKIE_PREFIX)) continue;
    try {
      out.append(name, value);
    } catch {
      // Invalid header name/value from the agent: drop it rather than failing the response.
    }
  }
  return out;
}

/** Validates a RES_HEAD/REQ_HEAD `headers` field: an array of [string, string] pairs. */
export function isHeaderPairs(v: unknown): v is HeaderPairs {
  return Array.isArray(v) && v.every((p) => Array.isArray(p) && p.length === 2 && typeof p[0] === "string" && typeof p[1] === "string");
}
