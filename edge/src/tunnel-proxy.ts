/**
 * Visitor traffic for `<name>.BASE_DOMAIN`: capture meta, strip edge-internal headers, check the
 * connected marker, then hand the request to the tunnel's Durable Object.
 *
 * Every path except the reserved `/__tuzy/` prefix reaches the user's app. The DO only treats a
 * request as an agent connect when its URL hostname is `connect.internal`, which a visitor request
 * (URL host = the tunnel host) can never have.
 */
import { hasConnectedMarker } from "./lib/connected-marker";
import { nowSec } from "./lib/ids";
import { CONTINUE_PATH, isContinueFromInterstitial, sanitizeTo, setCookieHeader, signCookie } from "./lib/interstitial";
import { META_CONTINENT, META_PROTO, META_REMOTE_IP, META_SKIP_WARNING, restoreAcceptEncoding, stripEdgeInternal } from "./lib/headers";
import { statusPage } from "./pages/status-pages";

export const RESERVED_PREFIX = "/__tuzy/";

export async function proxyToTunnel(request: Request, env: Env, ctx: ExecutionContext, name: string): Promise<Response> {
  const url = new URL(request.url);
  if (url.pathname.startsWith(RESERVED_PREFIX)) return reservedPath(request, env, name, url);

  const rl = await env.RL_TUNNEL.limit({ key: name });
  if (!rl.success) return statusPage("rate_limited");

  // Meta is read BEFORE stripping, then re-set under edge-owned names.
  const remoteIp = request.headers.get("cf-connecting-ip") ?? "";
  const continent = String((request.cf as { continent?: string } | undefined)?.continent ?? "");
  const skipWarning = request.headers.has("tuzy-skip-warning"); // any value, like ngrok-skip-browser-warning
  const headers = new Headers(request.headers);
  stripEdgeInternal(headers);
  restoreAcceptEncoding(headers, request.cf as { clientAcceptEncoding?: string } | undefined);
  if (skipWarning) headers.set(META_SKIP_WARNING, "1");
  headers.set(META_REMOTE_IP, remoteIp);
  headers.set(META_CONTINENT, continent);
  headers.set(META_PROTO, url.protocol.replace(":", ""));

  if (!(await hasConnectedMarker(env, ctx, name))) return statusPage("offline");

  // Rebuild the URL from the validated name (defence in depth for the DO's connect.internal check).
  const target = `${url.protocol}//${name}.${env.BASE_DOMAIN}${url.pathname}${url.search}`;
  const stub = env.TUNNEL.get(env.TUNNEL.idFromName(name));
  return stub.fetch(new Request(target, { method: request.method, headers, body: request.body, redirect: "manual" }));
}

/** `/__tuzy/` is the only path on tunnel hosts the user's app can't serve. */
async function reservedPath(request: Request, env: Env, name: string, url: URL): Promise<Response> {
  if (url.pathname !== CONTINUE_PATH || request.method !== "GET") return statusPage("not_found");
  // Host-bound cookie: signed for exactly this tunnel host, never Domain=.
  const host = `${name}.${env.BASE_DOMAIN}`;
  const to = sanitizeTo(url.searchParams.get("to"));
  if (!env.INTERSTITIAL_SECRET || !isContinueFromInterstitial(request.headers, url.origin)) {
    // Not a click on our page: send the browser to the target, which shows the interstitial.
    return new Response(null, { status: 302, headers: { location: to, "cache-control": "no-store", "x-tuzy-edge": "1" } });
  }
  return new Response(null, {
    status: 302,
    headers: {
      location: to,
      "set-cookie": setCookieHeader(await signCookie(env.INTERSTITIAL_SECRET, host, nowSec())),
      "cache-control": "no-store",
      "referrer-policy": "no-referrer",
      "x-tuzy-edge": "1",
    },
  });
}
