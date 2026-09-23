import { cloudflareTest, readD1Migrations } from "@cloudflare/vitest-pool-workers";
import { defineConfig } from "vitest/config";

// Test-only bindings: the `log` email driver (tests read codes from memory), shrunken edge policy
// (PROTOCOL.md §2.2 policy values are vars precisely so tests don't burn wall-clock minutes).
const testBindings = {
  EMAIL_DRIVER: "log",
  ALLOW_LOG_EMAIL: "1",
  ADMIN_EMAIL: "admin@example.test",
  DAILY_SEND_BUDGET: "900",
  HEAD_TIMEOUT_SECONDS: "3",
  CREDIT_TIMEOUT_SECONDS: "1",
  DEAD_SOCKET_SECONDS: "2",
  LONG_STREAM_AFTER_SECONDS: "1",
  MAX_STREAM_SECONDS: "3600",
  HELLO_TIMEOUT_SECONDS: "1",
  INTERSTITIAL_SECRET: "test-interstitial-secret",
  QUARANTINE_THRESHOLD: "3",
};

const worker = cloudflareTest(async () => ({
  wrangler: { configPath: "./wrangler.jsonc" },
  miniflare: {
    bindings: { ...testBindings, TEST_MIGRATIONS: await readD1Migrations("./migrations") }, // path relative to edge/
  },
}));

const setupFiles = ["./test/setup.ts"];

export default defineConfig({
  test: {
    projects: [
      {
        plugins: [worker],
        test: { name: "unit", include: ["test/unit/**/*.test.ts"], setupFiles },
      },
      {
        // Durable Object + WebSocket relay tests: sequential (DO WebSocket tests share state and
        // some rely on wall-clock timeouts).
        plugins: [worker],
        test: {
          name: "do",
          include: ["test/do/**/*.test.ts"],
          setupFiles,
          fileParallelism: false,
          testTimeout: 30_000,
        },
      },
    ],
  },
});
