/** GET /api/v1/me — who am I (connect-scope tokens allowed). */
import { Hono } from "hono";
import { longStreamBudget, monthKey, readUsage } from "../lib/usage";
import { requireAuth, type AppEnv } from "./auth-middleware";

export const meRoutes = new Hono<AppEnv>();

meRoutes.get("/", requireAuth({ allowConnectScope: true }), async (c) => {
  const auth = c.get("auth");
  const user = await c.env.DB.prepare("SELECT created_at, max_names FROM users WHERE id = ?1")
    .bind(auth.userId)
    .first<{ created_at: number; max_names: number }>();
  const token = await c.env.DB.prepare("SELECT label, created_at, expires_at FROM tokens WHERE id = ?1")
    .bind(auth.tokenId)
    .first<{ label: string; created_at: number; expires_at: number | null }>();
  const month = monthKey();
  const used = await readUsage(c.env.DB, auth.userId, month);
  return c.json({
    usage: { month, long_stream_seconds: used, long_stream_budget_seconds: longStreamBudget(c.env) },
    user: { id: auth.userId, email: auth.email, role: auth.role, trusted: auth.trusted, created_at: user?.created_at, max_names: user?.max_names },
    token: { id: auth.tokenId, scope: auth.scope, ...token },
  });
});
