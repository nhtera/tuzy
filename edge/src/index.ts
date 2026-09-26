/**
 * Tuzy edge Worker: routes by the `Host` header (never request.url, see README "GATE B").
 *
 *   BASE_DOMAIN            → apex app (CLI API, site)
 *   www.BASE_DOMAIN        → 301 to the apex
 *   <name>.BASE_DOMAIN     → tunnel proxy → TunnelObject DO
 *   anything else          → 404 page
 */
import { apiApp } from "./api/app";
import { tunnelLabel } from "./lib/host";
import { negotiateStatusPage, statusPage } from "./pages/status-pages";
import { scheduled } from "./scheduled";
import { proxyToTunnel } from "./tunnel-proxy";

export { TunnelObject } from "./tunnel-object";

export default {
  async fetch(request, env, ctx): Promise<Response> {
    return negotiateStatusPage(request, await route(request, env, ctx));
  },
  scheduled,
} satisfies ExportedHandler<Env>;

async function route(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
  const label = tunnelLabel(request.headers.get("host"), env.BASE_DOMAIN);
  if (label === null) return statusPage("not_found");
  if (label === "") return apiApp.fetch(request, env, ctx);
  if (label === "www") {
    const url = new URL(request.url);
    const host = (request.headers.get("host") ?? env.BASE_DOMAIN).replace(/^www\./i, "");
    return Response.redirect(`${url.protocol}//${host}${url.pathname}${url.search}`, 301);
  }
  return proxyToTunnel(request, env, ctx, label);
}
