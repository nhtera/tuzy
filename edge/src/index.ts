/**
 * Tuzy edge Worker (phase 1 skeleton).
 *
 * Hello-world + host echo used to prove GATE B (Host-based routing under `wrangler dev`)
 * and the production wildcard route. Phase 2 replaces this with the tunnel/API dispatch.
 */
import { Hono } from "hono";
import { tunnelLabel } from "./lib/host";

const app = new Hono<{ Bindings: Env }>();

app.all("*", (c) => {
  const host = c.req.header("host") ?? null;
  const label = tunnelLabel(host, c.env.BASE_DOMAIN);
  return c.json({
    service: "tuzy",
    host,
    url: c.req.url,
    baseDomain: c.env.BASE_DOMAIN,
    // "apex" = API/site host, "tunnel" = <name>.BASE_DOMAIN, "foreign" = not ours.
    kind: label === null ? "foreign" : label === "" ? "apex" : "tunnel",
    name: label || null,
  });
});

export default app;
