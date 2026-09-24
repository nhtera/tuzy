/** Shared shell for apex pages: no inline script, strict CSP, light/dark. */
import { BASE_CSS, FAVICON } from "./site-style";

export const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

const CSP =
  "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'";

export const REPO = "https://github.com/nhtera/tuzy";
export const DOCS = `${REPO}/blob/main/docs/getting-started.md`;

/** Site chrome and the two content layouts: `landing` (wide sections) and `prose` (legal, forms). */
const LAYOUT_CSS = `.wrap{width:100%;max-width:72rem;margin:0 auto;padding:0 clamp(1rem,4vw,2rem)}
.top{position:sticky;top:0;z-index:10;background:color-mix(in srgb,var(--bg) 86%,transparent);-webkit-backdrop-filter:blur(12px);backdrop-filter:blur(12px);border-bottom:1px solid var(--border)}
.top .wrap{display:flex;align-items:center;gap:1.4rem;height:60px}
.logo{display:inline-flex;align-items:center;gap:.5rem;margin-right:auto;font:700 1.1rem/1 var(--mono);color:var(--text);text-decoration:none;letter-spacing:-.02em}
.logo::before{content:"";width:.7rem;height:.7rem;border-radius:3px;background:var(--accent)}
.logo:hover{color:var(--text)}
.top nav{display:flex;align-items:center;gap:1.4rem;font-size:.92rem}
.top nav a{color:var(--muted);text-decoration:none;white-space:nowrap}
.top nav a:hover,.top nav a[aria-current]{color:var(--text)}
@media(max-width:640px){.top nav .wide{display:none}.top nav{gap:1.1rem}}
main{display:block}
.prose{max-width:44rem;padding-top:clamp(2rem,6vw,3.5rem);padding-bottom:4rem}
.prose h1{font-size:clamp(2rem,5vw,2.6rem);margin-bottom:.3em}
.prose h2{font-size:1.25rem;margin:2.2em 0 .6em;scroll-margin-top:80px}
.prose p,.prose li{color:var(--muted)}
.prose b,.prose strong{color:var(--text);font-weight:600}
.prose ul{padding-left:1.2rem;margin:0 0 1.2em}.prose li{margin:.35em 0}.prose li::marker{color:var(--faint)}
.prose small{font:500 .82rem/1.4 var(--mono);color:var(--faint)}
.prose .lede{font-size:1.08rem;color:var(--muted);max-width:60ch}
.prose h2:target{color:var(--accent)}
form{margin-top:1.8rem;display:grid;gap:1.15rem}
.field{display:grid;gap:.4rem}
label{font-weight:600;font-size:.93rem;color:var(--text)}
.hint{font-weight:400;color:var(--faint)}
input,select,textarea{width:100%;font:inherit;color:var(--text);background:var(--surface);border:1px solid var(--border-strong);border-radius:8px;padding:.65rem .8rem;min-height:44px}
textarea{resize:vertical;min-height:7.5rem}
input::placeholder,textarea::placeholder{color:var(--faint);opacity:1}
input:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-soft)}
form .btn{justify-self:start;margin-top:.3rem}
form+p{margin-top:1.6rem}
#result:empty{display:none}
#result{margin:0;padding:.7rem .9rem;border-radius:8px;border:1px solid var(--border);background:var(--surface-2);color:var(--text);font-size:.93rem}
#result[data-state=ok]{border-color:var(--accent-line);background:var(--accent-soft)}
#result[data-state=error]{border-color:var(--warn-line);background:var(--warn-soft)}
.hp{position:absolute;left:-9999px}
.foot{border-top:1px solid var(--border);padding:clamp(2.2rem,5vw,3rem) 0 2rem;font-size:.9rem}
.foot .wrap{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.6fr);gap:2rem clamp(2rem,6vw,5rem)}
.fbrand .logo{margin-right:0}
.fbrand p{margin:.7rem 0 0;color:var(--faint)}
.fcols{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:1.5rem}
.fcols div{display:grid;gap:.6rem;align-content:start}
.fh{margin:0 0 .15rem;font-size:.8rem;font-weight:600;color:var(--text)}
.fcols a{width:fit-content;color:var(--muted);text-decoration:none;overflow-wrap:anywhere}
.fcols a:hover{color:var(--text)}
.fbase{grid-column:1 / -1;border-top:1px solid var(--border);padding-top:1.2rem;color:var(--faint);font-size:.82rem}
@media(max-width:720px){.foot .wrap{grid-template-columns:minmax(0,1fr);gap:1.6rem}.fbrand{display:flex;flex-wrap:wrap;align-items:baseline;gap:.4rem .9rem}.fbrand p{margin:0}}
@media(max-width:480px){.foot{padding:2rem 0 1.5rem;font-size:.85rem}.fcols{grid-template-columns:repeat(3,auto);justify-content:space-between;gap:.9rem}.fcols div{gap:.45rem}.fbase{padding-top:1rem}}`;

const NAV: Array<[href: string, label: string, wide?: boolean]> = [
  [DOCS, "Docs"],
  ["/aup", "Acceptable use", true],
  ["/abuse", "Report abuse"],
  [REPO, "GitHub", true],
];

interface PageOpts {
  status?: number;
  /** Same-origin scripts (the CSP forbids inline script). */
  scripts?: string[];
  /** `prose` (default) wraps the body in a readable column; `landing` hands the body full width. */
  layout?: "prose" | "landing";
  description?: string;
  /** Page-specific CSS, appended to the shared stylesheet. */
  css?: string;
  /** The current path, marked `aria-current` in the nav. */
  path?: string;
}

export function page(title: string, body: string, opts: PageOpts = {}): Response {
  const nav = NAV.map(([href, label, wide]) => `<a href="${href}"${wide ? ' class="wide"' : ""}${href === opts.path ? ' aria-current="page"' : ""}>${label}</a>`).join("");
  const desc = opts.description ?? "Free tunnels that expose localhost at a stable https://<name>.tuzy.dev URL.";
  const main = opts.layout === "landing" ? `<main>${body}</main>` : `<main class="wrap prose">${body}</main>`;
  const scripts = (opts.scripts ?? []).map((s) => `<script src="${s}" defer></script>`).join("");
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${escapeHtml(title)}</title><meta name="description" content="${escapeHtml(desc)}"><meta property="og:title" content="${escapeHtml(title)}"><meta property="og:description" content="${escapeHtml(desc)}"><meta name="theme-color" content="#fbfbfc" media="(prefers-color-scheme: light)"><meta name="theme-color" content="#0b0c0e" media="(prefers-color-scheme: dark)">${FAVICON}<style>${BASE_CSS}
${LAYOUT_CSS}
${opts.css ?? ""}</style>${scripts}</head><body><header class="top"><div class="wrap"><a class="logo" href="/">tuzy</a><nav aria-label="Main">${nav}</nav></div></header>${main}<footer class="foot"><div class="wrap"><div class="fbrand"><a class="logo" href="/">tuzy</a><p>Stable localhost tunnels.</p></div><nav class="fcols" aria-label="Footer"><div><p class="fh">Product</p><a href="${DOCS}">Docs</a><a href="${REPO}">GitHub</a><a href="${REPO}/releases">Releases</a></div><div><p class="fh">Policies</p><a href="/aup">Acceptable use</a><a href="/terms">Terms</a><a href="/privacy">Privacy</a></div><div><p class="fh">Contact</p><a href="/abuse">Report abuse</a><a href="/.well-known/security.txt">Security</a><a href="mailto:abuse@tuzy.dev">abuse@tuzy.dev</a></div></nav><p class="fbase">Free and open source under Apache-2.0.</p></div></footer></body></html>`;
  return new Response(html, {
    status: opts.status ?? 200,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "content-security-policy": CSP,
      "x-content-type-options": "nosniff",
      "referrer-policy": "strict-origin-when-cross-origin",
      "cache-control": "public, max-age=300",
    },
  });
}
