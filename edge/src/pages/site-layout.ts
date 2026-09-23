/** Shared shell for apex pages: no inline script, strict CSP, light/dark. */

export const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

const CSP =
  "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'";

const STYLE = `body{font:16px/1.6 system-ui,sans-serif;max-width:46rem;margin:0 auto;padding:2rem 1rem 4rem;color:#1d1d1f;background:#fff}
@media(prefers-color-scheme:dark){body{background:#111;color:#ddd}a{color:#8ab4f8}pre,code{background:#1d1d1f!important}}
header{display:flex;gap:1.2rem;align-items:baseline;margin-bottom:2rem}header a{text-decoration:none}header b{font-size:1.3rem;margin-right:auto}
pre{background:#f4f4f5;padding:.8rem 1rem;border-radius:8px;overflow-x:auto}code{background:#f4f4f5;padding:.1rem .3rem;border-radius:4px}pre code{padding:0}
footer{margin-top:3rem;font-size:.9rem;color:#888}label{display:block;margin:.8rem 0 .3rem}input,select,textarea{width:100%;box-sizing:border-box;padding:.5rem;font:inherit}
button{margin-top:1rem;padding:.6rem 1.2rem;font:inherit;cursor:pointer}.hp{position:absolute;left:-9999px}h1{line-height:1.2}`;

export function page(title: string, body: string, opts: { script?: string; status?: number } = {}): Response {
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${escapeHtml(title)}</title><style>${STYLE}</style></head><body><header><b><a href="/">tuzy</a></b><a href="/aup">AUP</a><a href="/terms">Terms</a><a href="/privacy">Privacy</a><a href="/abuse">Report abuse</a></header>${body}<footer>tuzy · <a href="https://github.com/nhtera/tuzy">source</a> · <a href="/.well-known/security.txt">security</a> · abuse@tuzy.dev</footer>${opts.script ? `<script src="${opts.script}"></script>` : ""}</body></html>`;
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
