/**
 * The browser interstitial shown on untrusted tunnels (phase 7). Every dynamic value is
 * HTML-escaped; the page carries no script and a strict CSP.
 */
import { CONTINUE_PATH } from "../lib/interstitial";
import { AUTO_TRUST_DAYS } from "../lib/trust";

const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

export function interstitialPage(name: string, host: string, path: string, baseDomain: string): Response {
  const cont = `${CONTINUE_PATH}?to=${encodeURIComponent(path)}`;
  const report = `https://${baseDomain}/abuse?name=${encodeURIComponent(name)}`;
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>You are visiting a tuzy tunnel</title><style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:12vh auto;padding:0 1rem;color:#222}@media(prefers-color-scheme:dark){body{background:#111;color:#ddd}a{color:#8ab4f8}}code{word-break:break-all}.btn{display:inline-block;padding:.6rem 1.1rem;border-radius:6px;background:#2458d6;color:#fff;text-decoration:none;margin-right:1rem}small{color:#888}</style></head><body><h1>You are about to visit a tunnel</h1><p><code>${escapeHtml(host)}</code> is served through <b>tuzy</b>, a service that exposes someone's own computer to the internet. It is not operated or checked by tuzy.</p><p><b>Continue only if you trust whoever sent you this link.</b> Never enter passwords or payment details for a bank, email provider or other well-known service here.</p><p><a class="btn" href="${escapeHtml(cont)}">Visit site</a><a href="${escapeHtml(report)}">Report abuse</a></p><p><small>You won't see this page again on this address for 7 days.</small></p><hr><p><small><b>Why this page?</b> tuzy shows it on tunnels of accounts younger than ${AUTO_TRUST_DAYS} days, or of accounts with an upheld abuse report, to protect visitors from phishing. It disappears by itself once the account is ${AUTO_TRUST_DAYS} days old. Webhooks and API calls never see it; automated tools can send the header <code>tuzy-skip-warning: 1</code>. <a href="https://${escapeHtml(baseDomain)}/aup#browser-warning">Policy</a></small></p></body></html>`;
  return new Response(html, {
    status: 200,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
      "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'",
      "x-robots-tag": "noindex",
      "referrer-policy": "same-origin", // the continue check falls back to Referer on old browsers
      "x-frame-options": "DENY",
      "x-tuzy-edge": "1",
    },
  });
}
