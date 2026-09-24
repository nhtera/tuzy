/**
 * The browser interstitial shown on untrusted tunnels (phase 7). Every dynamic value is
 * HTML-escaped; the page carries no script and a strict CSP.
 */
import { CONTINUE_PATH } from "../lib/interstitial";
import { AUTO_TRUST_DAYS } from "../lib/trust";
import { BASE_CSS, CARD_CSS, FAVICON } from "./site-style";

const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

export function interstitialPage(name: string, host: string, path: string, baseDomain: string): Response {
  const cont = `${CONTINUE_PATH}?to=${encodeURIComponent(path)}`;
  const report = `https://${baseDomain}/abuse?name=${encodeURIComponent(name)}`;
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>You are visiting a tuzy tunnel</title>${FAVICON}<style>${BASE_CSS}
${CARD_CSS}
.warn{border-left:3px solid var(--ink-warn);background:var(--warn-soft);border-radius:0 8px 8px 0;padding:.75rem .9rem;color:var(--text);margin:0 0 1rem}
.why{border-top:1px solid var(--border);padding-top:1rem;margin:1.4rem 0 0}</style></head><body><main class="card"><a class="brand" href="https://${escapeHtml(baseDomain)}/">tuzy</a><h1>You are about to visit a tunnel</h1><code class="host">${escapeHtml(host)}</code><p>This address is served through <b>tuzy</b>, a service that exposes someone's own computer to the internet. It is not operated or checked by tuzy.</p><p class="warn"><b>Continue only if you trust whoever sent you this link.</b> Never enter passwords or payment details for a bank, email provider or other well-known service here.</p><div class="actions"><a class="btn" href="${escapeHtml(cont)}">Visit site</a><a class="btn btn-ghost" href="${escapeHtml(report)}">Report abuse</a></div><p class="note">You won't see this page again on this address for 7 days.</p><p class="note why"><b>Why this page?</b> tuzy shows it on tunnels of accounts younger than ${AUTO_TRUST_DAYS} days, or of accounts with an upheld abuse report, to protect visitors from phishing. It disappears by itself once the account is ${AUTO_TRUST_DAYS} days old. Webhooks and API calls never see it; automated tools can send the header <code>tuzy-skip-warning: 1</code>. <a href="https://${escapeHtml(baseDomain)}/aup#browser-warning">Read the policy</a></p></main></body></html>`;
  return new Response(html, {
    status: 200,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
      "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; frame-ancestors 'none'; base-uri 'none'",
      "x-robots-tag": "noindex",
      "referrer-policy": "same-origin", // the continue check falls back to Referer on old browsers
      "x-frame-options": "DENY",
      "x-tuzy-edge": "1",
    },
  });
}
