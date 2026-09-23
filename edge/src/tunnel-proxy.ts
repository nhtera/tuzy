/**
 * Visitor traffic for `<name>.BASE_DOMAIN`: capture meta, strip edge-internal headers, check the
 * connected marker, then hand the request to the tunnel's Durable Object.
 *
 * Every path except the reserved `/__tuzy/` prefix reaches the user's app. The DO only treats a
 * request as an agent connect when its URL hostname is `connect.internal`, which a visitor request
 * (URL host = the tunnel host) can never have.
 */
import { hasConnectedMarker } from "./lib/connected-marker";
import { META_CONTINENT, META_PROTO, META_REMOTE_IP, stripEdgeInternal } from "./lib/headers";
import { statusPage } from "./pages/status-pages";

export const RESERVED_PREFIX = "/__tuzy/";

export async function proxyToTunnel(request: Request, env: Env, ctx: ExecutionContext, name: string): Promise<Response> {
  const url = new URL(request.url);
  if (url.pathname.startsWith(RESERVED_PREFIX)) return statusPage("not_found"); // phase 7 edge pages

  // Meta is read BEFORE stripping, then re-set under edge-owned names.
  const remoteIp = request.headers.get("cf-connecting-ip") ?? "";
  const continent = String((request.cf as { continent?: string } | undefined)?.continent ?? "");
  const headers = new Headers(request.headers);
  stripEdgeInternal(headers);
  headers.set(META_REMOTE_IP, remoteIp);
  headers.set(META_CONTINENT, continent);
  headers.set(META_PROTO, url.protocol.replace(":", ""));

  if (!(await hasConnectedMarker(env, ctx, name))) return statusPage("offline");

  // Rebuild the URL from the validated name (defence in depth for the DO's connect.internal check).
  const target = `${url.protocol}//${name}.${env.BASE_DOMAIN}${url.pathname}${url.search}`;
  const stub = env.TUNNEL.get(env.TUNNEL.idFromName(name));
  return stub.fetch(new Request(target, { method: request.method, headers, body: request.body, redirect: "manual" }));
}
