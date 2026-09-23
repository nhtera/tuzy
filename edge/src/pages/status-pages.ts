/**
 * Minimal HTML status pages served to tunnel visitors by the edge itself (not the user's app).
 * Every page is marked `x-tuzy-edge: 1` so users and the CLI can tell edge errors from app errors.
 */

export type StatusPage = "not_found" | "offline" | "draining" | "timeout" | "busy" | "bad_gateway" | "suspended" | "header_too_large";

const PAGES: Record<StatusPage, { status: number; title: string; body: string }> = {
  not_found: { status: 404, title: "Tunnel not found", body: "There is no tunnel at this address." },
  offline: { status: 502, title: "Tunnel offline", body: "This tunnel exists, but its agent is not connected right now." },
  draining: { status: 502, title: "Tunnel restarting", body: "The tunnel agent is shutting down. Try again in a moment." },
  timeout: { status: 504, title: "Gateway timeout", body: "The local app did not respond in time." },
  busy: { status: 503, title: "Tunnel busy", body: "This tunnel has too many requests in flight. Try again shortly." },
  bad_gateway: { status: 502, title: "Bad gateway", body: "The tunnel agent could not complete this request." },
  suspended: { status: 403, title: "Tunnel suspended", body: "This tunnel has been suspended." },
  header_too_large: { status: 431, title: "Request headers too large", body: "The request headers are too large to relay." },
};

const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

export function statusPage(page: StatusPage, detail?: string): Response {
  const p = PAGES[page];
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${p.title}</title><style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:15vh auto;padding:0 1rem;color:#222}@media(prefers-color-scheme:dark){body{background:#111;color:#ddd}}small{color:#888}</style></head><body><h1>${p.title}</h1><p>${p.body}</p>${detail ? `<p><small>${escapeHtml(detail)}</small></p>` : ""}<p><small>tuzy · ${p.status}</small></p></body></html>`;
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
