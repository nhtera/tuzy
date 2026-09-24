/**
 * Apex (BASE_DOMAIN) application: the CLI API under /api/v1 and, later, the site pages.
 */
import { Hono } from "hono";
import { abuseRoutes } from "./abuse-routes";
import { accountRoutes } from "./account-routes";
import { adminRoutes } from "./admin-routes";
import { authRoutes } from "./auth-routes";
import type { AppEnv } from "./auth-middleware";
import { handleConnect } from "./connect";
import { healthRoutes } from "./health-routes";
import { errorResponse, toResponse } from "./errors";
import { meRoutes } from "./me-routes";
import { namesRoutes } from "./names-routes";
import { tokenRoutes } from "./token-routes";
import { abusePage, abuseScript } from "../pages/abuse-page";
import { installPowerShell, installScript } from "../pages/install-script";
import { landingPage, siteScript } from "../pages/landing-page";
import { aupPage, privacyPage, securityTxt, termsPage } from "../pages/legal-pages";

export const apiApp = new Hono<AppEnv>();

apiApp.onError((err) => toResponse(err));
apiApp.route("/api/v1", healthRoutes);
apiApp.get("/api/v1/connect", (c) => handleConnect(c.req.raw, c.env, c.executionCtx as ExecutionContext));
apiApp.route("/api/v1/auth", authRoutes);
apiApp.route("/api/v1/me", meRoutes);
apiApp.route("/api/v1/tokens", tokenRoutes);
apiApp.route("/api/v1/account", accountRoutes);
apiApp.route("/api/v1/names", namesRoutes);
apiApp.route("/api/v1/abuse", abuseRoutes);
apiApp.route("/api/v1/admin", adminRoutes);
apiApp.all("/api/*", () => errorResponse(404, "not_found", "unknown API endpoint"));
apiApp.get("/", () => landingPage());
apiApp.get("/terms", () => termsPage());
apiApp.get("/privacy", () => privacyPage());
apiApp.get("/aup", () => aupPage());
apiApp.get("/abuse", (c) => abusePage(c.req.query("name") ?? null));
apiApp.get("/abuse.js", () => abuseScript());
apiApp.get("/site.js", () => siteScript());
apiApp.get("/.well-known/security.txt", (c) => securityTxt(c.env.BASE_DOMAIN));
apiApp.get("/install.sh", () => installScript());
apiApp.get("/install.ps1", () => installPowerShell());
apiApp.notFound(() => new Response("tuzy: not found\n", { status: 404 }));
