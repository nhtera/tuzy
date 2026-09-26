/**
 * Design tokens and base CSS shared by every HTML page the edge serves: the apex site, the tunnel
 * interstitial and the edge status pages. Pages inline it (their CSPs allow inline styles only, and
 * no web fonts), so type uses the platform's UI and monospace stacks.
 *
 * Shape rule: panels 12px, controls (buttons, inputs, command bars) 8px, inline code 6px.
 * One accent (signal green, matching the CLI's "● online"), light and dark via prefers-color-scheme.
 */

export const BASE_CSS = `:root{color-scheme:light dark;
--bg:#fbfbfc;--surface:#fff;--surface-2:#f3f4f6;--border:#e4e4e7;--border-strong:#d4d4d8;
--text:#18181b;--muted:#52525b;--faint:#6b6b75;
--accent:#0a7a4b;--accent-hover:#086a41;--accent-ink:#fff;--accent-soft:rgba(10,122,75,.08);--accent-line:rgba(10,122,75,.28);
--ink:#121316;--ink-2:#1b1c20;--ink-text:#e7e7ea;--ink-dim:#9a9aa3;--ink-accent:#4ad295;--ink-warn:#f0b54a;
--warn-soft:rgba(180,120,10,.09);--warn-line:rgba(180,120,10,.35);
--bad:#d93036;--bad-soft:rgba(217,48,54,.06);--bad-line:rgba(217,48,54,.45);
--shadow:0 1px 2px rgba(24,24,27,.04),0 8px 24px -12px rgba(24,24,27,.12);
--sans:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",sans-serif;
--mono:ui-monospace,"SF Mono","JetBrains Mono",Menlo,Consolas,"Liberation Mono",monospace}
@media(prefers-color-scheme:dark){:root{
--bg:#0b0c0e;--surface:#121316;--surface-2:#18191d;--border:#24252a;--border-strong:#303137;
--text:#ececef;--muted:#a3a3ad;--faint:#8c8c96;
--accent:#3ecf8e;--accent-hover:#5ad9a0;--accent-ink:#04170d;--accent-soft:rgba(62,207,142,.09);--accent-line:rgba(62,207,142,.3);
--ink:#131418;--ink-2:#1e1f24;
--warn-soft:rgba(240,181,74,.08);--warn-line:rgba(240,181,74,.3);
--bad:#f26268;--bad-soft:rgba(242,98,104,.08);--bad-line:rgba(242,98,104,.45);
--shadow:0 1px 2px rgba(0,0,0,.3),0 12px 32px -16px rgba(0,0,0,.6)}}
*,*::before,*::after{box-sizing:border-box}
html{-webkit-text-size-adjust:100%;text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--text);font:16px/1.6 var(--sans);-webkit-font-smoothing:antialiased;overflow-wrap:break-word}
a{color:var(--accent);text-decoration-thickness:1px;text-underline-offset:3px}
a:hover{color:var(--accent-hover)}
:focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:4px}
h1,h2,h3{line-height:1.15;letter-spacing:-.02em;margin:0 0 .6em;text-wrap:balance}
p{margin:0 0 1em}
code,pre,kbd{font-family:var(--mono);font-size:.9em}
:not(pre)>code{background:var(--surface-2);border:1px solid var(--border);padding:.08em .35em;border-radius:6px;overflow-wrap:anywhere}
pre{margin:0;overflow-x:auto;-webkit-overflow-scrolling:touch}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:.5rem;min-height:44px;padding:.6rem 1.15rem;border-radius:8px;border:1px solid transparent;background:var(--accent);color:var(--accent-ink);font:600 .95rem/1 var(--sans);text-decoration:none;cursor:pointer;white-space:nowrap}
.btn:hover{background:var(--accent-hover);color:var(--accent-ink)}
.btn:active{transform:translateY(1px)}
.btn[disabled]{opacity:.6;cursor:progress}
.btn-ghost{background:transparent;color:var(--text);border-color:var(--border-strong)}
.btn-ghost:hover{background:var(--surface-2);color:var(--text)}
@media(prefers-reduced-motion:no-preference){.btn,a,.copy{transition:background-color .18s cubic-bezier(.16,1,.3,1),color .18s,border-color .18s,transform .12s}}`;

/** A simple mark (accent tile, monospace "t") as an inline data-URI favicon (img-src allows data:). */
export const FAVICON = `<link rel="icon" href="data:image/svg+xml,${encodeURIComponent(
  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="8" fill="#0a7a4b"/><text x="16" y="23" text-anchor="middle" font-family="ui-monospace,Menlo,monospace" font-size="21" font-weight="700" fill="#fff">t</text></svg>`,
)}">`;

/** Centered single-card layout for the interstitial and status pages served on tunnel hosts. */
export const CARD_CSS = `body{min-height:100vh;min-height:100dvh;display:grid;place-items:center;padding:1.5rem 1rem}
.card{width:100%;max-width:34rem;background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:clamp(1.4rem,4vw,2.2rem);box-shadow:var(--shadow)}
.card h1{font-size:clamp(1.45rem,4.5vw,1.8rem)}
.brand{display:inline-flex;align-items:center;gap:.45rem;font:700 .95rem/1 var(--mono);color:var(--text);text-decoration:none;margin-bottom:1.4rem}
.brand::before{content:"";width:.6rem;height:.6rem;border-radius:3px;background:var(--accent)}
.host{display:block;font:500 .95rem/1.4 var(--mono);background:var(--surface-2);border:1px solid var(--border);border-radius:8px;padding:.6rem .8rem;margin:0 0 1.1rem;overflow-wrap:anywhere}
.note{font-size:.88rem;color:var(--muted)}
.actions{display:flex;flex-wrap:wrap;gap:.75rem;margin:1.4rem 0}
.meta{font:500 .8rem/1.4 var(--mono);color:var(--faint);margin:1.4rem 0 0}`;
