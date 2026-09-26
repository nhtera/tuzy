/**
 * HTML status pages served to tunnel visitors by the edge itself (not the user's app).
 * Every page is marked `x-tuzy-edge: 1` so users and the CLI can tell edge errors from app errors,
 * and carries a stable `tuzy-error` code that is shown on the page and documented in
 * docs/limits-and-faq.md (one heading per code, so the docs link lands on it).
 */
import { type BreakAt, REQUEST_PATH_CSS, requestPath } from "./request-path";
import { BASE_CSS, FAVICON } from "./site-style";

export type StatusPage =
  | "not_found"
  | "offline"
  | "draining"
  | "timeout"
  | "stream_limit"
  | "busy"
  | "bad_gateway"
  | "suspended"
  | "header_too_large"
  | "rate_limited";

interface PageSpec {
  status: number;
  /** `TUZY-<status>-<NAME>`; also the lowercase docs anchor. */
  code: string;
  title: string;
  body: string;
  /** Hint for whoever runs the tunnel (trusted HTML). */
  owner?: string;
  /** Hint for visitors (trusted HTML). */
  visitor?: string;
  /** Draws the request path with this hop unreachable. */
  breakAt?: BreakAt;
  retryAfter?: number;
}

const RELOAD = `Wait a moment and <a href="">reload the page</a>. If it keeps happening, contact the owner of this site.`;

const PAGES: Record<StatusPage, PageSpec> = {
  not_found: {
    status: 404,
    code: "TUZY-404-TUNNEL-NOT-FOUND",
    title: "Tunnel not found",
    body: "There is no tunnel at this address.",
    owner: "Check the name in the address, or start a tunnel with it: <code>tuzy http 3000 --name &lt;name&gt;</code>.",
  },
  offline: {
    status: 502,
    code: "TUZY-502-AGENT-OFFLINE",
    title: "Tunnel offline",
    body: "This tunnel exists, but its tuzy agent is not connected right now, so the request could not go any further than the tuzy edge.",
    owner: "Start the agent on the machine that serves this site, e.g. <code>tuzy http 3000</code>, and keep it running. If it is running, check its output for connection errors.",
    visitor: `The site is temporarily unavailable. ${RELOAD}`,
    breakAt: "agent",
    retryAfter: 5,
  },
  draining: {
    status: 502,
    code: "TUZY-502-AGENT-RESTARTING",
    title: "Tunnel restarting",
    body: "The tuzy agent for this tunnel is shutting down or reconnecting.",
    owner: "This clears by itself when the agent reconnects. If it doesn't, restart <code>tuzy</code>.",
    visitor: `The site should be back in a moment. ${RELOAD}`,
    breakAt: "agent",
    retryAfter: 5,
  },
  bad_gateway: {
    status: 502,
    code: "TUZY-502-BAD-RESPONSE",
    title: "Bad gateway",
    body: "The tuzy agent could not complete this request.",
    owner: "Check the <code>tuzy</code> output for errors and make sure it is up to date: <code>tuzy update</code>.",
    visitor: RELOAD,
    breakAt: "agent",
  },
  timeout: {
    status: 504,
    code: "TUZY-504-LOCAL-TIMEOUT",
    title: "Gateway timeout",
    body: "The request reached the tunnel, but the local app did not start answering in time.",
    owner: "Your app must send the first byte of its response within 300 seconds. Check it for slow or stuck requests.",
    visitor: RELOAD,
    breakAt: "service",
  },
  stream_limit: {
    status: 504,
    code: "TUZY-504-STREAM-LIMIT",
    title: "Stream time limit reached",
    body: "This request ran longer than tuzy allows for a single stream and was stopped.",
    owner: "A single HTTP stream may stay open for up to 1 hour, and streams open longer than 5 minutes count toward your monthly long-stream time (<code>tuzy whoami</code> shows usage).",
    visitor: RELOAD,
  },
  busy: {
    status: 503,
    code: "TUZY-503-TUNNEL-BUSY",
    title: "Tunnel busy",
    body: "This tunnel has too many requests in flight.",
    owner: "Your app is holding many requests open at once. Check for slow or stuck responses.",
    visitor: `Try again shortly.`,
    retryAfter: 5,
  },
  suspended: {
    status: 451,
    code: "TUZY-451-TUNNEL-SUSPENDED",
    title: "Tunnel suspended",
    body: "This tunnel has been suspended for violating the tuzy acceptable use policy.",
    owner: `If you think this is a mistake, contact <a href="mailto:abuse@tuzy.dev">abuse@tuzy.dev</a>.`,
  },
  rate_limited: {
    status: 429,
    code: "TUZY-429-RATE-LIMITED",
    title: "Too many requests",
    body: "This tunnel is receiving too many requests.",
    owner: "A tunnel accepts up to 600 requests per 10 seconds.",
    visitor: "Try again in a few seconds.",
    retryAfter: 10,
  },
  header_too_large: {
    status: 431,
    code: "TUZY-431-HEADERS-TOO-LARGE",
    title: "Request headers too large",
    body: "The request headers are too large to relay.",
    visitor: "Clear this site's cookies and try again.",
  },
};

const DOCS = "https://github.com/nhtera/tuzy/blob/main/docs/limits-and-faq.md#";

/** Same lock-down as the agent's error page: inline styles and a data: favicon, nothing else. */
const CSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'";

const PAGE_CSS = `body{min-height:100vh;min-height:100dvh;display:grid;place-items:center;padding:1.5rem 1rem}
main{width:100%;max-width:42rem}
.brand{display:inline-flex;align-items:center;gap:.45rem;font:700 .95rem/1 var(--mono);color:var(--text);margin-bottom:1.6rem}
.brand::before{content:"";width:.6rem;height:.6rem;border-radius:3px;background:var(--accent)}
.alert{background:var(--surface);border:1px solid var(--bad-line);border-left:4px solid var(--bad);border-radius:12px;padding:clamp(1.1rem,4vw,1.6rem);box-shadow:var(--shadow);margin:0 0 2rem}
.alert .code{font:700 .8rem/1.3 var(--mono);color:var(--bad);letter-spacing:.02em;margin:0 0 .7rem}
.alert h1{font-size:clamp(1.35rem,4.5vw,1.7rem)}
.alert p:last-child{margin:0}
.alert pre{margin:0;padding:.75rem .9rem;background:var(--surface-2);border:1px solid var(--border);border-radius:8px;font:.85rem/1.5 var(--mono);white-space:pre-wrap;overflow-wrap:anywhere}
section{margin:0 0 1.6rem}
section h2{font-size:1.1rem}
section p{color:var(--muted)}
.meta{font:500 .8rem/1.4 var(--mono);color:var(--faint);margin:2rem 0 0;padding-top:1.1rem;border-top:1px solid var(--border)}
${REQUEST_PATH_CSS}`;

const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

export function statusPage(page: StatusPage, detail?: string): Response {
  const p = PAGES[page];
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>${p.title} · tuzy</title>${FAVICON}<style>${BASE_CSS}
${PAGE_CSS}</style></head><body><main><span class="brand">tuzy</span>${p.breakAt ? requestPath(p.breakAt) : ""}<div class="alert" role="alert"><p class="code">${p.status} · ${p.code}</p><h1>${p.title}</h1><p>${p.body}</p>${detail ? `<pre>${escapeHtml(detail)}</pre>` : ""}</div>${p.owner ? `<section><h2>If you run this tunnel</h2><p>${p.owner}</p></section>` : ""}${p.visitor ? `<section><h2>If you're visiting this site</h2><p>${p.visitor}</p></section>` : ""}<p class="meta">Error ${p.code} · served by the tuzy edge, not by the tunnel's app · <a href="${DOCS}${p.code.toLowerCase()}">What does this mean?</a></p></main></body></html>`;
  const headers: Record<string, string> = {
    "content-type": "text/html; charset=utf-8",
    "cache-control": "no-store",
    "content-security-policy": CSP,
    "x-content-type-options": "nosniff",
    "x-tuzy-edge": "1",
    "tuzy-error": p.code,
  };
  if (p.retryAfter) headers["retry-after"] = String(p.retryAfter);
  return new Response(html, { status: p.status, headers });
}

const PAGE_BY_CODE = new Map(Object.values(PAGES).map((p) => [p.code, p]));

const acceptsHtml = (req: Request) => (req.headers.get("accept") ?? "").toLowerCase().includes("text/html");

/**
 * Non-browser clients (curl, webhook senders, WebSocket handshakes) get one plain-text line instead
 * of an edge status page, with the same status and headers. Applied once at the Worker entry, so it
 * covers pages built in the Worker and in the Durable Object alike. Responses from the tunnel's app
 * are never touched: the edge strips `x-tuzy-*` from them, so they can't carry `x-tuzy-edge`.
 */
export function negotiateStatusPage(req: Request, res: Response): Response {
  const code = res.headers.get("tuzy-error");
  const page = code ? PAGE_BY_CODE.get(code) : undefined;
  if (!page || res.headers.get("x-tuzy-edge") !== "1" || acceptsHtml(req)) return res;
  void res.body?.cancel();
  const headers = new Headers(res.headers);
  headers.set("content-type", "text/plain; charset=utf-8");
  headers.delete("content-security-policy");
  return new Response(`tuzy: ${page.title.toLowerCase()} (${page.code})\n`, { status: res.status, headers });
}

/** Every page's status and code, for the docs check and tests. */
export const STATUS_PAGES: Readonly<Record<StatusPage, Readonly<{ status: number; code: string }>>> = PAGES;

/** JSON error for API clients (the CLI): `{error: {code, message, ...extra}}`. */
export { errorResponse as apiError } from "../api/errors";
