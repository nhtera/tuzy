// Bindings wrangler types can't see (kept out of the generated worker-configuration.d.ts).
interface Env {
  /** Test/local only: lets EMAIL_DRIVER=log "send" (vitest bindings). */
  ALLOW_LOG_EMAIL?: string;
}
