/**
 * Minimal HTML status pages served to tunnel visitors by the edge itself (not the user's app).
 * Every page is marked `x-tuzy-edge: 1` so users and the CLI can tell edge errors from app errors.
 */
import { BASE_CSS, CARD_CSS, FAVICON } from "./site-style";

export type StatusPage = "not_found" | "offline" | "draining" | "timeout" | "busy" | "bad_gateway" | "suspended" | "header_too_large" | "rate_limited";

const PAGES: Record<StatusPage, { status: number; title: string; body: string }> = {
  not_found: { status: 404, title: "Tunnel not found", body: "There is no tunnel at this address." },
  offline: { status: 502, title: "Tunnel offline", body: "This tunnel exists, but its agent is not connected right now." },
  draining: { status: 502, title: "Tunnel restarting", body: "The tunnel agent is shutting down. Try again in a moment." },
  timeout: { status: 504, title: "Gateway timeout", body: "The local app did not respond in time." },
  busy: { status: 503, title: "Tunnel busy", body: "This tunnel has too many requests in flight. Try again shortly." },
  bad_gateway: { status: 502, title: "Bad gateway", body: "The tunnel agent could not complete this request." },
  suspended: { status: 451, title: "Tunnel suspended", body: "This tunnel has been suspended for violating the tuzy acceptable use policy." },
  rate_limited: { status: 429, title: "Too many requests", body: "This tunnel is receiving too many requests. Try again in a few seconds." },
  header_too_large: { status: 431, title: "Request headers too large", body: "The request headers are too large to relay." },
};

const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

export function statusPage(page: StatusPage, detail?: string): Response {
  const p = PAGES[page];
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>${p.title}</title>${FAVICON}<style>${BASE_CSS}
${CARD_CSS}
.code{font:700 .85rem/1 var(--mono);color:var(--faint);margin:0 0 .8rem}</style></head><body><main class="card"><span class="brand">tuzy</span><p class="code">${p.status}</p><h1>${p.title}</h1><p>${p.body}</p>${detail ? `<p class="note">${escapeHtml(detail)}</p>` : ""}<p class="meta">Served by the tuzy edge, not by the tunnel's app.</p></main></body></html>`;
  return new Response(html, {
    status: p.status,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
      "x-tuzy-edge": "1",
    },
  });
}

/** JSON error for API clients (the CLI): `{error: {code, message, ...extra}}`. */
export { errorResponse as apiError } from "../api/errors";
