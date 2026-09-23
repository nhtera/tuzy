/**
 * Apex (BASE_DOMAIN) application: the CLI API under /api/v1 and, later, the site pages.
 */
import { Hono } from "hono";
import { accountRoutes } from "./account-routes";
import { authRoutes } from "./auth-routes";
import type { AppEnv } from "./auth-middleware";
import { handleConnect } from "./connect";
import { errorResponse, toResponse } from "./errors";
import { meRoutes } from "./me-routes";
import { tokenRoutes } from "./token-routes";

export const apiApp = new Hono<AppEnv>();

apiApp.onError((err) => toResponse(err));
apiApp.get("/api/v1/health", (c) => c.json({ ok: true }));
apiApp.get("/api/v1/connect", (c) => handleConnect(c.req.raw, c.env, c.executionCtx as ExecutionContext));
apiApp.route("/api/v1/auth", authRoutes);
apiApp.route("/api/v1/me", meRoutes);
apiApp.route("/api/v1/tokens", tokenRoutes);
apiApp.route("/api/v1/account", accountRoutes);
apiApp.all("/api/*", () => errorResponse(404, "not_found", "unknown API endpoint"));
apiApp.get("/", (c) => c.text("tuzy: expose localhost at a stable https://<name>.tuzy.dev URL\n"));
apiApp.notFound(() => new Response("tuzy: not found\n", { status: 404 }));
