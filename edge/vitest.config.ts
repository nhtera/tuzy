import { cloudflareTest } from "@cloudflare/vitest-pool-workers";
import { defineConfig } from "vitest/config";

// Test-only secrets and shrunken edge policy (PROTOCOL.md §2.2 policy values are vars precisely so
// tests don't burn minutes of wall clock).
const testBindings = {
  DEV_TOKEN: "test-dev-token",
  HEAD_TIMEOUT_SECONDS: "3",
  CREDIT_TIMEOUT_SECONDS: "1",
  DEAD_SOCKET_SECONDS: "2",
  LONG_STREAM_AFTER_SECONDS: "1",
  MAX_STREAM_SECONDS: "3600",
  HELLO_TIMEOUT_SECONDS: "1",
};

const worker = cloudflareTest({
  wrangler: { configPath: "./wrangler.jsonc" },
  miniflare: { bindings: testBindings },
});

export default defineConfig({
  test: {
    projects: [
      {
        plugins: [worker],
        test: { name: "unit", include: ["test/unit/**/*.test.ts"] },
      },
      {
        // Durable Object + WebSocket relay tests: sequential (DO WebSocket tests share state and
        // some rely on wall-clock timeouts).
        plugins: [worker],
        test: {
          name: "do",
          include: ["test/do/**/*.test.ts"],
          fileParallelism: false,
          testTimeout: 30_000,
        },
      },
    ],
  },
});
