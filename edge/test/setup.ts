/**
 * Per test file: apply D1 migrations and seed a known user + full-scope token (TEST_TOKEN) so
 * relay tests can connect agents without the email flow.
 */
import { applyD1Migrations } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { hashToken } from "../src/lib/tokens";

export const TEST_TOKEN = `tzy_${"t".repeat(43)}`;
export const TEST_USER = { id: "usr_test000000000000", email: "tester@example.test" };

await applyD1Migrations(env.DB, env.TEST_MIGRATIONS);
const now = Math.floor(Date.now() / 1000);
await env.DB.batch([
  env.DB.prepare("INSERT OR IGNORE INTO users (id, email, created_at) VALUES (?1, ?2, ?3)").bind(TEST_USER.id, TEST_USER.email, now),
  env.DB.prepare(
    "INSERT OR IGNORE INTO tokens (id, user_id, token_hash, scope, label, created_at, last_used_at) VALUES ('tok_test000000000000', ?1, ?2, 'full', 'test', ?3, ?3)",
  ).bind(TEST_USER.id, await hashToken(TEST_TOKEN), now),
]);
