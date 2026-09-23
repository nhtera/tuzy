/**
 * Tunnel-name policy (phase 5 "Name rules"): syntax, reserved words, brand/phishing substrings,
 * and auto-generated `adjective-noun-NN` names.
 */
import blocked from "../data/blocked-names.json";
import words from "../data/name-words.json";
import { isValidTunnelLabel } from "./host";

const RESERVED = new Set<string>(blocked.reserved);
const SUBSTRINGS: readonly string[] = blocked.substrings;

export type NameCheck = { ok: true } | { ok: false; code: "invalid_name" | "reserved_name" | "blocked_name"; message: string };

export function checkName(name: string): NameCheck {
  if (!isValidTunnelLabel(name)) {
    return { ok: false, code: "invalid_name", message: "names are 3-32 chars of a-z, 0-9 and single hyphens, starting and ending with a letter or digit" };
  }
  if (RESERVED.has(name)) return { ok: false, code: "reserved_name", message: `"${name}" is reserved` };
  const hit = SUBSTRINGS.find((s) => name.includes(s));
  if (hit) return { ok: false, code: "blocked_name", message: `names containing "${hit}" are not allowed (phishing protection)` };
  return { ok: true };
}

function pick<T>(list: readonly T[]): T {
  const buf = new Uint32Array(1);
  crypto.getRandomValues(buf);
  return list[buf[0]! % list.length]!;
}

/** e.g. "brave-otter-42" (~4M combinations). Always passes checkName. */
export function autoName(): string {
  for (;;) {
    const n = `${pick(words.adjectives)}-${pick(words.nouns)}-${10 + (crypto.getRandomValues(new Uint8Array(1))[0]! % 90)}`;
    if (checkName(n).ok) return n;
  }
}
