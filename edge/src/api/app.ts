/**
 * Apex (BASE_DOMAIN) application: the CLI API under /api/v1 and, later, the site pages.
 */
import { Hono } from "hono";
import { apiError } from "../pages/status-pages";
import { handleConnect } from "./connect";

export const apiApp = new Hono<{ Bindings: Env }>();

apiApp.get("/api/v1/health", (c) => c.json({ ok: true }));
apiApp.get("/api/v1/connect", (c) => handleConnect(c.req.raw, c.env));
apiApp.all("/api/*", () => apiError(404, "not_found", "unknown API endpoint"));
apiApp.get("/", (c) => c.text("tuzy: expose localhost at a stable https://<name>.tuzy.dev URL\n"));
apiApp.notFound(() => new Response("tuzy: not found\n", { status: 404 }));
