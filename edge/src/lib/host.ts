/**
 * Host → tunnel label resolution.
 *
 * Always reads the `Host` header, never `request.url`. Under `wrangler dev` with `routes`,
 * both are rewritten to the route zone, which is why local dev runs a generated config
 * without routes (README "GATE B"); in production the two agree, and `Host` is the one we trust.
 */

/**
 * Syntactic tunnel-name rule (phase 5 "Name rules"): 3–32 chars of [a-z0-9-], alphanumeric at
 * both ends. `--` is rejected separately (blocks punycode `xn--`). Reserved/blocked words are a
 * policy check done by the names module, not here.
 */
export const TUNNEL_LABEL_RE = /^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$/;

/** True when `label` is a syntactically valid tunnel name. */
export function isValidTunnelLabel(label: string): boolean {
  return TUNNEL_LABEL_RE.test(label) && !label.includes("--");
}

/**
 * Lowercases the host and strips an optional numeric `:port` and one trailing dot (FQDN form).
 * Returns `null` for a malformed host (bad port, empty). Bracketed IPv6 is returned as-is
 * (it can never match a tunnel domain).
 */
export function normalizeHost(host: string): string | null {
  let h = host.trim().toLowerCase();
  if (h.startsWith("[")) {
    const end = h.indexOf("]");
    return end === -1 ? null : h.slice(0, end + 1);
  }
  const colon = h.lastIndexOf(":");
  if (colon !== -1) {
    if (!/^\d{1,5}$/.test(h.slice(colon + 1))) return null;
    h = h.slice(0, colon);
  }
  if (h.endsWith(".")) h = h.slice(0, -1);
  return h.length > 0 ? h : null;
}

/**
 * Returns the tunnel name under `baseDomain` (e.g. "shop" for "shop.tuzy.dev"), `""` for the
 * apex, or `null` when the host is not ours, is nested deeper than one label, or the label is
 * not a valid tunnel name.
 */
export function tunnelLabel(host: string | null | undefined, baseDomain: string): string | null {
  if (!host) return null;
  const h = normalizeHost(host);
  if (h === null) return null;
  const base = baseDomain.toLowerCase();
  if (h === base) return "";
  if (!h.endsWith("." + base)) return null;
  const label = h.slice(0, -(base.length + 1));
  return isValidTunnelLabel(label) ? label : null;
}
