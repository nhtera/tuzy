/**
 * Pinned names API (phase 5).
 *
 *   GET    /api/v1/names[?live=1]   list (live: DO status for names that ever connected)
 *   GET    /api/v1/names/suggest    a free auto name (first-run prompt)
 *   POST   /api/v1/names            {name} | {auto:true}
 *   PATCH  /api/v1/names/:name      {rename:"new"} | {default:true}
 *   DELETE /api/v1/names/:name
 *
 * Mutations need a full-scope token; connect-scope (CI) tokens may only list.
 */
import { Hono } from "hono";
import { markerKey } from "../lib/connected-marker";
import { nowSec } from "../lib/ids";
import { NameRepoError, getReservation, listNames, remove, rename, reserve, setDefault, type Reservation } from "../lib/name-repo";
import { ipPrefix } from "../lib/ip-prefix";
import { autoName, checkName } from "../lib/names";
import { pushOutbox } from "../lib/outbox";
import type { TunnelObject } from "../tunnel-object";
import { requireAuth, type AppEnv } from "./auth-middleware";
import { ApiError, jsonBody } from "./errors";

export const namesRoutes = new Hono<AppEnv>();

const REPO_ERRORS: Record<string, [number, string]> = {
  same_name: [400, "that's already its name"],
  hold_quota: [403, "you have too many released names on hold (20); contact abuse@tuzy.dev"],
  name_taken: [409, "that name is taken"],
  name_on_hold: [409, "that name was recently released by another account and is on hold"],
  quota_exceeded: [403, "you've reached your name limit; remove one with `tuzy names rm`"],
  not_found: [404, "you don't have a name like that"],
  suspended: [403, "that name is suspended"],
};

function repoError(e: unknown): never {
  if (e instanceof NameRepoError) {
    const [status, message] = REPO_ERRORS[e.code]!;
    throw new ApiError(status, e.code, message);
  }
  throw e;
}

const actorOf = (c: { get(k: "auth"): { userId: string }; req: { header(n: string): string | undefined } }) => ({
  userId: c.get("auth").userId,
  ipPrefix: ipPrefix(c.req.header("cf-connecting-ip")),
});

function validated(name: unknown): string {
  const n = String(name ?? "").toLowerCase();
  const check = checkName(n);
  if (!check.ok) throw new ApiError(400, check.code, check.message);
  return n;
}

type LiveState = "online" | "offline" | "never_connected" | "unknown";

async function liveState(env: Env, name: string): Promise<LiveState> {
  if ((await env.NAMES_KV.get(markerKey(name))) === null) return "never_connected";
  const stub = env.TUNNEL.get(env.TUNNEL.idFromName(name)) as unknown as DurableObjectStub<TunnelObject>;
  const timeout = new Promise<LiveState>((r) => setTimeout(() => r("unknown"), 2000));
  const status = stub.status().then((s) => s.state as LiveState, () => "unknown" as const);
  return Promise.race([status, timeout]);
}

function publicName(c: { req: { header(n: string): string | undefined; url: string } }, env: Env, r: Reservation) {
  const proto = new URL(c.req.url).protocol;
  const host = c.req.header("host") ?? env.BASE_DOMAIN;
  return {
    name: r.name,
    url: `${proto}//${r.name}.${host}`,
    default: r.is_default === 1,
    status: r.status,
    created_at: r.created_at,
    last_seen_at: r.last_seen_at,
  };
}

namesRoutes.get("/", requireAuth({ allowConnectScope: true }), async (c) => {
  const auth = c.get("auth");
  const rows = await listNames(c.env.DB, auth.userId);
  const user = await c.env.DB.prepare("SELECT max_names FROM users WHERE id = ?1").bind(auth.userId).first<{ max_names: number }>();
  const names: Record<string, unknown>[] = rows.map((r) => publicName(c, c.env, r));
  if (c.req.query("live") === "1") {
    // At most 10 DO status calls per request (quota can be raised by an admin).
    const states = await Promise.all(rows.map((r, i) => (i < 10 ? liveState(c.env, r.name) : Promise.resolve<LiveState>("unknown"))));
    names.forEach((n, i) => (n.state = states[i]));
  }
  // Names released by the caller and still held for them (reclaim only via an explicit `names add`).
  const held = await c.env.DB.prepare("SELECT name, until, renamed_to FROM released_names WHERE held_for = ?1 AND until > ?2 ORDER BY name")
    .bind(auth.userId, nowSec())
    .all<{ name: string; until: number; renamed_to: string | null }>();
  return c.json({ names, used: rows.length, limit: user?.max_names ?? 10, held: held.results });
});

namesRoutes.get("/suggest", requireAuth({ allowConnectScope: true }), async (c) => {
  for (let i = 0; i < 5; i++) {
    const name = autoName();
    const taken = await c.env.DB.prepare(
      "SELECT 1 AS x FROM reservations WHERE name = ?1 UNION ALL SELECT 1 FROM released_names WHERE name = ?1 AND until > ?2",
    )
      .bind(name, nowSec())
      .first();
    if (!taken) return c.json({ name });
  }
  throw new ApiError(503, "no_suggestion", "couldn't find a free name; pick one yourself");
});

namesRoutes.post("/", requireAuth(), async (c) => {
  const auth = c.get("auth");
  const body = await jsonBody(c.req.raw);
  const now = nowSec();
  if (body.auto === true) {
    for (let i = 0; i < 5; i++) {
      try {
        const r = await reserve(c.env.DB, auth.userId, autoName(), now, actorOf(c));
        return c.json(publicName(c, c.env, r), 201);
      } catch (e) {
        if (e instanceof NameRepoError && (e.code === "name_taken" || e.code === "name_on_hold")) continue;
        repoError(e);
      }
    }
    throw new ApiError(503, "no_suggestion", "couldn't find a free name; pick one yourself");
  }
  try {
    const r = await reserve(c.env.DB, auth.userId, validated(body.name), now, actorOf(c));
    return c.json(publicName(c, c.env, r), 201);
  } catch (e) {
    repoError(e);
  }
});

namesRoutes.patch("/:name", requireAuth(), async (c) => {
  const auth = c.get("auth");
  const name = c.req.param("name").toLowerCase();
  const body = await jsonBody(c.req.raw);
  try {
    if (typeof body.rename === "string") {
      const { reservation, outboxIds } = await rename(c.env.DB, auth.userId, name, validated(body.rename), nowSec(), actorOf(c));
      c.executionCtx.waitUntil(
        Promise.all([
          outboxIds.length ? pushOutbox(c.env, outboxIds).catch((e) => console.error("outbox push failed", e)) : null,
          c.env.NAMES_KV.delete(markerKey(name)), // old-host visitors: offline page without waking a DO
        ]),
      );
      return c.json(publicName(c, c.env, reservation));
    }
    if (body.default === true) {
      await setDefault(c.env.DB, auth.userId, name, actorOf(c));
      return c.json(publicName(c, c.env, (await getReservation(c.env.DB, name))!));
    }
  } catch (e) {
    repoError(e);
  }
  throw new ApiError(400, "bad_request", 'send {"rename":"new-name"} or {"default":true}');
});

namesRoutes.delete("/:name", requireAuth(), async (c) => {
  const auth = c.get("auth");
  const name = c.req.param("name").toLowerCase();
  let outboxIds: string[] = [];
  try {
    outboxIds = await remove(c.env.DB, auth.userId, name, nowSec(), actorOf(c));
  } catch (e) {
    repoError(e);
  }
  c.executionCtx.waitUntil(
    Promise.all([
      outboxIds.length ? pushOutbox(c.env, outboxIds).catch((e) => console.error("outbox push failed", e)) : null,
      c.env.NAMES_KV.delete(markerKey(name)),
    ]),
  );
  return c.body(null, 204);
});
