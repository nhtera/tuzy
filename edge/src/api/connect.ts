/**
 * GET /api/v1/connect?name=<name>&instance=<instance_id>[&force=1]  (WebSocket upgrade)
 *
 * Authenticates the agent, then forwards the upgrade to the tunnel's Durable Object as a NEW
 * internal request to `https://connect.internal/` carrying the upgrade headers plus JSON meta.
 * (Workers RPC cannot return a 101: workerd #2319.) Phase 2 auth = DEV_TOKEN; phase 4 replaces it.
 */
import { isValidTunnelLabel } from "../lib/host";
import { META_CONNECT, stripEdgeInternal } from "../lib/headers";
import { apiError } from "../pages/status-pages";
import type { ConnectMeta } from "../tunnel-object";

/** PROTOCOL.md §1: 16–64 chars of [A-Za-z0-9_-]. */
const INSTANCE_RE = /^[A-Za-z0-9_-]{16,64}$/;

/** Constant-time string compare (both operands are hashed to equal length first). */
async function safeEqual(a: string, b: string): Promise<boolean> {
  const enc = new TextEncoder();
  const [ha, hb] = await Promise.all([crypto.subtle.digest("SHA-256", enc.encode(a)), crypto.subtle.digest("SHA-256", enc.encode(b))]);
  return crypto.subtle.timingSafeEqual(ha, hb);
}

function bearer(request: Request): string | null {
  const m = /^Bearer\s+(\S+)$/i.exec(request.headers.get("authorization") ?? "");
  return m?.[1] ?? null;
}

/** Durable Object placement near the agent (best effort from the connect request's colo). */
export function locationHint(cf: IncomingRequestCfProperties | undefined): DurableObjectLocationHint | undefined {
  const lon = Number(cf?.longitude);
  switch (cf?.continent) {
    case "NA":
      return Number.isFinite(lon) && lon < -100 ? "wnam" : "enam";
    case "SA":
      return "sam";
    case "EU":
      return Number.isFinite(lon) && lon > 20 ? "eeur" : "weur";
    case "AS":
      return "apac";
    case "OC":
      return "oc";
    case "AF":
      return "afr";
    default:
      return undefined;
  }
}

export async function handleConnect(request: Request, env: Env): Promise<Response> {
  if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
    return apiError(400, "websocket_required", "connect requires a WebSocket upgrade");
  }
  const token = bearer(request);
  if (!token || !env.DEV_TOKEN || !(await safeEqual(token, env.DEV_TOKEN))) {
    return apiError(401, "unauthorized", "not logged in: run `tuzy login`");
  }

  const url = new URL(request.url);
  const name = (url.searchParams.get("name") ?? "").toLowerCase();
  const instanceId = url.searchParams.get("instance") ?? "";
  if (!isValidTunnelLabel(name)) return apiError(400, "invalid_name", `invalid tunnel name: ${JSON.stringify(name.slice(0, 64))}`);
  if (!INSTANCE_RE.test(instanceId)) return apiError(400, "invalid_instance", "instance must be 16-64 chars of [A-Za-z0-9_-]");

  // Public URL mirrors how the apex was reached (https://shop.tuzy.dev, or http://shop.localhost:8787
  // in dev): normalized host (lowercase, no trailing dot), keeping a non-default port.
  const reached = new URL(`${url.protocol}//${request.headers.get("host") ?? env.BASE_DOMAIN}`);
  const apexHost = reached.hostname.replace(/\.$/, "") + (reached.port ? `:${reached.port}` : "");
  const meta: ConnectMeta = {
    name,
    url: `${url.protocol}//${name}.${apexHost}`,
    instanceId,
    force: url.searchParams.get("force") === "1",
  };

  const headers = new Headers(request.headers);
  headers.delete("authorization");
  headers.delete("cookie");
  stripEdgeInternal(headers);
  headers.set(META_CONNECT, JSON.stringify(meta));

  const ns = env.TUNNEL;
  const stub = ns.get(ns.idFromName(name), { locationHint: locationHint(request.cf as IncomingRequestCfProperties | undefined) });
  return stub.fetch(new Request("https://connect.internal/", { headers }));
}
