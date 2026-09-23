/**
 * "Connected marker" `c:<name>` in NAMES_KV: set once a tunnel's agent has completed HELLO/READY.
 *
 * The Worker checks it before routing a visitor to the DO, so traffic to names that never
 * connected gets the offline page WITHOUT creating a Durable Object (which would otherwise be
 * placed near the visitor instead of near the agent). Positive results are cached per colo for
 * 60 s via the Cache API to keep KV reads off the hot path. Negative results are not cached, so a
 * brand-new tunnel is visible as soon as KV propagates.
 */

const CACHE_TTL_SECONDS = 60;

export const markerKey = (name: string) => `c:${name}`;

function cacheKey(baseDomain: string, name: string): Request {
  return new Request(`https://${baseDomain}/__tuzy/marker/${name}`);
}

export async function hasConnectedMarker(env: Env, ctx: ExecutionContext, name: string): Promise<boolean> {
  const key = cacheKey(env.BASE_DOMAIN, name);
  const cache = caches.default;
  if (await cache.match(key)) return true;
  if ((await env.NAMES_KV.get(markerKey(name))) === null) return false;
  ctx.waitUntil(
    cache.put(key, new Response("1", { headers: { "cache-control": `max-age=${CACHE_TTL_SECONDS}` } })).catch(() => {}),
  );
  return true;
}

export function writeConnectedMarker(kv: KVNamespace, name: string): Promise<void> {
  return kv.put(markerKey(name), JSON.stringify({ at: Date.now() }));
}
