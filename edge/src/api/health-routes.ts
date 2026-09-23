/**
 * Unauthenticated endpoints for `tuzy diagnose` (phase 8).
 *
 *   GET /api/v1/health        {ok, version, min_proto, max_proto}; the Date header doubles as the
 *                             clock-skew reference
 *   GET /api/v1/diagnose/ws   WebSocket echo in the plain Worker (no DO), closed after 10 s;
 *                             RL_DIAGNOSE 10/min per network prefix
 */
import { Hono } from "hono";
import { ipPrefix } from "../lib/ip-prefix";
import type { AppEnv } from "./auth-middleware";
import { errorResponse } from "./errors";

export const MIN_PROTO = 1;
export const MAX_PROTO = 1;
const ECHO_LIFETIME_MS = 10_000;

export const healthRoutes = new Hono<AppEnv>();

healthRoutes.get("/health", (c) =>
  c.json(
    { ok: true, version: c.env.CF_VERSION_METADATA?.id ?? "dev", min_proto: MIN_PROTO, max_proto: MAX_PROTO },
    200,
    { "cache-control": "no-store" },
  ),
);

healthRoutes.get("/diagnose/ws", async (c) => {
  if (c.req.header("upgrade")?.toLowerCase() !== "websocket") return errorResponse(426, "websocket_required", "send a WebSocket upgrade");
  const rl = await c.env.RL_DIAGNOSE.limit({ key: ipPrefix(c.req.header("cf-connecting-ip")) });
  if (!rl.success) return errorResponse(429, "rate_limited", "too many diagnose runs; try again in a minute", {}, { "retry-after": "60" });
  const pair = new WebSocketPair();
  const [client, server] = [pair[0], pair[1]];
  server.accept();
  let bytes = 0;
  server.addEventListener("message", (e) => {
    const size = typeof e.data === "string" ? e.data.length : (e.data as ArrayBuffer).byteLength;
    bytes += size;
    if (bytes > 64 * 1024) return server.close(1009, "echo limit");
    server.send(e.data);
  });
  const timer = setTimeout(() => server.close(1000, "done"), ECHO_LIFETIME_MS);
  server.addEventListener("close", () => clearTimeout(timer));
  return new Response(null, { status: 101, webSocket: client });
});
