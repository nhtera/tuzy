/**
 * Tuzy edge Worker (phase 1 skeleton).
 *
 * Plain-text host check used to prove GATE B (Host-based routing under `wrangler dev`) and the
 * production wildcard route. Phase 2 replaces this with the tunnel/API dispatch.
 */
import { Hono } from "hono";
import { tunnelLabel } from "./lib/host";

const app = new Hono<{ Bindings: Env }>();

app.all("*", (c) => {
  const label = tunnelLabel(c.req.header("host"), c.env.BASE_DOMAIN);
  if (label === null) return c.text("tuzy: unknown host\n", 404);
  if (label === "") return c.text("tuzy: apex\n");
  return c.text(`tuzy: tunnel ${label}\n`);
});

export default app;
