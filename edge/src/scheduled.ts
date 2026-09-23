/**
 * Cron handlers. Every 5 minutes: retry the D1 → DO outbox and alert the admin (once per day) when
 * rows stay stuck for more than 5 minutes. Phase 7 adds the daily cleanup.
 */
import { sendEmail } from "./lib/email";
import { pushOutbox, staleOutboxCount } from "./lib/outbox";

export async function scheduled(controller: ScheduledController, env: Env): Promise<void> {
  if (controller.cron === "*/5 * * * *") {
    await pushOutbox(env);
    const stale = await staleOutboxCount(env);
    if (stale > 0 && env.ADMIN_EMAIL) {
      const key = `alert:outbox:${new Date().toISOString().slice(0, 10)}`;
      if ((await env.NAMES_KV.get(key)) === null) {
        try {
          await sendEmail(env, {
            to: env.ADMIN_EMAIL,
            subject: `[tuzy] ${stale} outbox row(s) stuck > 5 min`,
            text: `The D1 → Durable Object outbox has ${stale} row(s) pending for more than 5 minutes.\nCheck \`wrangler tail\` for "outbox push failed".\n`,
          });
          await env.NAMES_KV.put(key, "1", { expirationTtl: 86400 }); // only once it was actually sent
        } catch (e) {
          console.error("admin alert failed", e);
        }
      }
    }
  }
}
