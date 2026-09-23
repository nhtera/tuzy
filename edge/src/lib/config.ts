/**
 * Edge policy (PROTOCOL.md §2.2 "Edge policy defaults"): tunable via Worker vars, never literals
 * in protocol code, so tests can shrink them and production can tune them without a protocol bump.
 */

export interface EdgePolicy {
  headTimeoutMs: number;
  maxStreamMs: number;
  deadSocketMs: number;
  creditTimeoutMs: number;
  helloTimeoutMs: number;
  /** A stream older than this counts as "long" for the per-name long-stream cap. */
  longStreamMs: number;
  maxLongStreams: number;
}

function seconds(v: string | undefined, fallback: number): number {
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n * 1000 : fallback * 1000;
}

export function edgePolicy(env: Env): EdgePolicy {
  return {
    headTimeoutMs: seconds(env.HEAD_TIMEOUT_SECONDS, 300),
    maxStreamMs: seconds(env.MAX_STREAM_SECONDS, 3600),
    deadSocketMs: seconds(env.DEAD_SOCKET_SECONDS, 45),
    creditTimeoutMs: seconds(env.CREDIT_TIMEOUT_SECONDS, 30),
    helloTimeoutMs: seconds(env.HELLO_TIMEOUT_SECONDS, 10),
    longStreamMs: seconds(env.LONG_STREAM_AFTER_SECONDS, 300),
    maxLongStreams: 8,
  };
}
