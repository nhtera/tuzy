/** Email normalization, syntax check and disposable-domain rejection. */
import disposable from "../data/disposable-domains.json";

const DISPOSABLE = new Set<string>(disposable as string[]);
const EMAIL_RE = /^[^\s@"<>(),;:\\[\]]+@[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$/;

export const normalizeEmail = (email: string) => email.trim().toLowerCase();

export function isValidEmail(email: string): boolean {
  return email.length <= 254 && EMAIL_RE.test(email);
}

/**
 * Canonical mailbox for per-email caps: drops "+tag" and, for Gmail, dots in the local part, so
 * address variants that reach one inbox share one cap (codes are still sent to the exact address).
 */
export function canonicalMailbox(email: string): string {
  const at = email.lastIndexOf("@");
  let local = email.slice(0, at);
  let domain = email.slice(at + 1);
  const plus = local.indexOf("+");
  if (plus > 0) local = local.slice(0, plus);
  if (domain === "googlemail.com") domain = "gmail.com";
  if (domain === "gmail.com") local = local.replace(/\./g, "");
  return `${local}@${domain}`;
}

/** True when the domain, or any parent domain, is a known disposable provider. */
export function isDisposable(email: string): boolean {
  const labels = email.slice(email.lastIndexOf("@") + 1).split(".");
  for (let i = 0; i < labels.length - 1; i++) {
    if (DISPOSABLE.has(labels.slice(i).join("."))) return true;
  }
  return false;
}
