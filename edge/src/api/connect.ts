/**
 * GET /api/v1/connect?name=<name>&instance=<instance_id>[&force=1]  (WebSocket upgrade)
 *
 * Authenticates the agent (connect-scope tokens allowed), records where the token is connected
 * (tunnel_sessions, so revocation can reach the DO), then forwards the upgrade to the tunnel's
 * Durable Object as a NEW internal request to `https://connect.internal/` carrying the upgrade
 * headers plus JSON meta. (Workers RPC cannot return a 101: workerd #2319.)
 * Phase 5 adds name ownership and in-DO re-validation.
 */
import { isValidTunnelLabel } from "../lib/host";
import { META_CONNECT, stripEdgeInternal } from "../lib/headers";
import { nowSec } from "../lib/ids";
import { connectAuthz } from "../lib/name-repo";
import type { ConnectMeta, TunnelObject } from "../tunnel-object";
import { authenticate } from "./auth-middleware";
import { ApiError, errorResponse } from "./errors";

/** PROTOCOL.md §1: 16–64 chars of [A-Za-z0-9_-]. */
const INSTANCE_RE = /^[A-Za-z0-9_-]{16,64}$/;

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

export async function handleConnect(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
  if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
    return errorResponse(400, "websocket_required", "connect requires a WebSocket upgrade");
  }
  const auth = await authenticate(env, ctx, request.headers.get("authorization"));

  const url = new URL(request.url);
  const name = (url.searchParams.get("name") ?? "").toLowerCase();
  const instanceId = url.searchParams.get("instance") ?? "";
  if (!isValidTunnelLabel(name)) return errorResponse(400, "invalid_name", `invalid tunnel name: ${JSON.stringify(name.slice(0, 64))}`);
  if (!INSTANCE_RE.test(instanceId)) return errorResponse(400, "invalid_instance", "instance must be 16-64 chars of [A-Za-z0-9_-]");
  const rl = await env.RL_CONNECT.limit({ key: `${auth.userId}:${name}` });
  if (!rl.success) throw new ApiError(429, "rate_limited", "reconnecting too fast; slow down", {}, { "retry-after": "10" });
  const authz = await connectAuthz(env.DB, name, auth.userId, nowSec());
  if (!authz.ok) return errorResponse(authz.status, authz.code, authz.message, authz.extra);

  // Public URL mirrors how the apex was reached (https://shop.tuzy.dev, or http://shop.localhost:8787
  // in dev): normalized host (lowercase, no trailing dot), keeping a non-default port.
  const reached = new URL(`${url.protocol}//${request.headers.get("host") ?? env.BASE_DOMAIN}`);
  const apexHost = reached.hostname.replace(/\.$/, "") + (reached.port ? `:${reached.port}` : "");
  const meta: ConnectMeta = {
    name,
    url: `${url.protocol}//${name}.${apexHost}`,
    instanceId,
    force: url.searchParams.get("force") === "1",
    userId: auth.userId,
    tokenId: auth.tokenId,
    scope: auth.scope,
    trusted: auth.trusted,
    gen: authz.gen,
  };

  const headers = new Headers(request.headers);
  headers.delete("authorization");
  headers.delete("cookie");
  stripEdgeInternal(headers);
  headers.set(META_CONNECT, JSON.stringify(meta));

  const ns = env.TUNNEL;
  const stub = ns.get(ns.idFromName(name), { locationHint: locationHint(request.cf as IncomingRequestCfProperties | undefined) }) as unknown as DurableObjectStub<TunnelObject>;
  const res = await stub.fetch(new Request("https://connect.internal/", { headers }));
  if (res.status === 101) ctx.waitUntil(recordSession(env, stub, name, auth.userId, auth.tokenId));
  return res;
}

/**
 * Records that this token is connected to `name` (read by token revocation / account deletion),
 * only once the DO accepted it and only while the token is unrevoked. If a revocation won the race
 * (its outbox snapshot missed this row), revoke the fresh session right away.
 */
async function recordSession(env: Env, stub: DurableObjectStub<TunnelObject>, name: string, userId: string, tokenId: string): Promise<void> {
  try {
    const r = await env.DB.prepare(
      `INSERT INTO tunnel_sessions (name, token_id, user_id, connected_at)
       SELECT ?1, ?2, ?3, ?4 WHERE EXISTS (SELECT 1 FROM tokens WHERE id = ?2 AND revoked_at IS NULL)
       ON CONFLICT(name, token_id) DO UPDATE SET connected_at = excluded.connected_at`,
    )
      .bind(name, tokenId, userId, nowSec())
      .run();
    if (r.meta.changes === 0) await stub.revokeToken(tokenId);
  } catch (e) {
    console.error("recording tunnel session failed", e);
  }
}
