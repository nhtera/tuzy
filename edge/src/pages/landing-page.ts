import { AUTO_TRUST_DAYS } from "../lib/trust";
import { DOCS, REPO, page } from "./site-layout";

/** Copy buttons stay hidden until /site.js wires them up (no script, no dead buttons). */
const copyBtn = (target: string, label: string) =>
  `<button class="copy" type="button" data-copy="${target}" aria-label="Copy ${label}" hidden>Copy</button>`;

const LANDING_CSS = `.hero{padding:clamp(2.5rem,7vw,5rem) 0 clamp(3rem,7vw,5rem)}
.hero .wrap{display:grid;grid-template-columns:minmax(0,1.15fr) minmax(0,1fr);gap:clamp(2rem,5vw,4rem);align-items:center}
.tag{display:inline-flex;align-items:center;gap:.45rem;font:500 .8rem/1 var(--mono);color:var(--muted);border:1px solid var(--border);background:var(--surface);border-radius:999px;padding:.45rem .8rem;text-decoration:none;margin-bottom:1.4rem}
.tag:hover{color:var(--text);border-color:var(--border-strong)}
.hero h1{font-size:clamp(2.2rem,4.3vw,3.1rem);line-height:1.04;letter-spacing:-.035em;margin-bottom:.45em}
.hero .sub{font-size:clamp(1.05rem,1.6vw,1.2rem);color:var(--muted);max-width:34ch;margin-bottom:1.8rem}
.cmd{display:flex;align-items:center;gap:.5rem;background:var(--ink);color:var(--ink-text);border-radius:8px;padding:.35rem .35rem .35rem 1rem;max-width:32rem;border:1px solid var(--ink-2)}
.cmd pre{flex:1;min-width:0;font-size:.92rem;line-height:1.5;padding:.5rem 0;scrollbar-width:none}
.cmd pre::-webkit-scrollbar{display:none}
.cmd .p{color:var(--ink-accent);user-select:none}
.copy{flex:none;font:600 .8rem/1 var(--sans);color:var(--ink-text);background:var(--ink-2);border:1px solid #2c2d33;border-radius:6px;padding:.55rem .75rem;min-height:36px;cursor:pointer}
.copy:hover{background:#26272c}
.copy[data-done]{color:var(--ink-accent);border-color:rgba(74,210,149,.4)}
.cta{display:flex;flex-wrap:wrap;align-items:center;gap:1rem 1.4rem;margin-top:1.1rem}
.cta a.more{font-weight:600;text-decoration:none}
.cta a.more:hover{text-decoration:underline}
.term{margin:0;background:var(--ink);color:var(--ink-text);border-radius:12px;border:1px solid var(--ink-2);box-shadow:var(--shadow);overflow:hidden}
.term figcaption{display:flex;justify-content:space-between;gap:1rem;padding:.7rem 1.1rem;border-bottom:1px solid var(--ink-2);font:500 .78rem/1 var(--mono);color:var(--ink-dim)}
.term pre{padding:1.1rem 1.1rem 1.3rem;font-size:.82rem;line-height:1.75}
.term .p{color:var(--ink-accent)}.term .ok{color:var(--ink-accent)}.term .dim{color:var(--ink-dim)}.term .w{color:var(--ink-warn)}.term .url{color:#fff;font-weight:600}
section{padding:clamp(2.5rem,6vw,4.5rem) 0}
section h2{font-size:clamp(1.7rem,3.4vw,2.3rem);margin-bottom:.4em}
section .intro{color:var(--muted);max-width:60ch;margin-bottom:2rem}
.setup{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:1.25rem}
.panel{min-width:0;background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:1.4rem}
.panel h3{font-size:1.02rem;margin-bottom:1rem}
.rows{display:grid;grid-template-columns:minmax(0,1fr);gap:.9rem}
.row{display:grid;grid-template-columns:minmax(0,1fr);gap:.4rem}
.row span{font-size:.85rem;font-weight:600;color:var(--muted)}
.row .cmd,.steps .cmd{max-width:none}
.steps{margin:0;padding:0;list-style:none;display:grid;grid-template-columns:minmax(0,1fr);gap:1rem}
.steps li{display:grid;grid-template-columns:minmax(0,1fr);gap:.35rem}
.steps b{font-size:.95rem}
.steps li span{color:var(--muted);font-size:.93rem}
.bento{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:1.25rem}
.tile{border-radius:12px;border:1px solid var(--border);background:var(--surface);padding:1.5rem;display:flex;flex-direction:column;gap:.4rem;min-height:12rem}
.tile h3{font-size:1.15rem;margin:0 0 .2rem}
.tile p{color:var(--muted);margin:0;font-size:.97rem}
.tile code.big{display:block;margin-top:auto;padding-top:1.2rem;background:none;border:0;font-size:.9rem;color:var(--text)}
.tile code.big::before{content:"$ ";color:var(--accent)}
.t-names{grid-column:span 2;background:radial-gradient(120% 90% at 100% 0%,var(--accent-soft),transparent 60%),var(--surface);border-color:var(--accent-line)}
.t-names .pair{display:grid;grid-template-columns:minmax(0,1.1fr) minmax(0,1fr);gap:1.5rem;align-items:start}
.t-names .urls{margin:0;padding:0;list-style:none;display:grid;gap:.5rem;font:500 .9rem/1.3 var(--mono)}
.t-names .urls li{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:.6rem .8rem;overflow-wrap:anywhere}
.t-names .urls li::before{content:"● ";color:var(--accent)}
.t-file{grid-column:span 2;background:var(--surface-2)}
.t-file .pair{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:1.2rem;align-items:end;margin-top:auto;padding-top:1rem}
.t-file pre{font-size:.84rem;line-height:1.6;background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:.8rem 1rem;color:var(--muted)}
.t-file pre b{color:var(--text);font-weight:600}
.fair{border-top:1px solid var(--border);background:var(--surface)}
.fair .wrap{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.3fr);gap:clamp(1.5rem,5vw,4rem);align-items:start}
.fair p{color:var(--muted);max-width:60ch}
.fair .links{display:flex;flex-wrap:wrap;gap:.75rem;margin-top:1.4rem}
@media(prefers-reduced-motion:no-preference){.hero .wrap>*{animation:rise .7s cubic-bezier(.16,1,.3,1) both}.hero .wrap>:nth-child(2){animation-delay:.08s}
@keyframes rise{from{opacity:0;transform:translateY(10px)}to{opacity:1;transform:none}}}
@media(max-width:960px){.bento{grid-template-columns:repeat(2,minmax(0,1fr))}.t-names,.t-file{grid-column:1 / -1}}
@media(max-width:860px){.hero .wrap,.setup,.fair .wrap{grid-template-columns:minmax(0,1fr)}.fair .wrap{gap:.4rem}.hero .sub{max-width:44ch}}
@media(max-width:640px){.bento{grid-template-columns:minmax(0,1fr)}.tile{min-height:0}.t-file .pair,.t-names .pair{grid-template-columns:minmax(0,1fr)}.term pre{font-size:.78rem}.cta .btn,.fair .links .btn{flex:1 1 auto}}`;

export function landingPage(): Response {
  return page(
    "tuzy: expose localhost at a stable URL",
    `<div class="hero"><div class="wrap">
<div>
<a class="tag" href="${REPO}">Open source, Apache-2.0</a>
<h1>Expose localhost at a URL that never changes</h1>
<p class="sub">Free tunnels for webhooks, demos and mobile testing. Claim a name once and keep it across restarts.</p>
<div class="cmd"><pre><span class="p">$ </span><code id="install-sh">curl -fsSL https://tuzy.dev/install.sh | sh</code></pre>${copyBtn("install-sh", "install command")}</div>
<div class="cta"><a class="btn" href="#start">Get started</a><a class="more" href="${DOCS}">Read the docs</a></div>
</div>
<figure class="term" aria-label="Example terminal session">
<figcaption><span>Example session</span><span>~/shop</span></figcaption>
<pre><span class="p">$</span> tuzy http 3000
<span class="dim">inspector: http://127.0.0.1:4040</span>
<span class="ok">● online</span> <span class="url">https://shop.tuzy.dev</span> → http://localhost:3000
<span class="dim">15:04:12</span> POST   /webhooks/stripe <span class="ok">200</span> 41ms
<span class="dim">15:04:15</span> GET    / <span class="ok">200</span> 12ms
<span class="dim">15:04:15</span> GET    /@vite/client <span class="ok">200</span> 8ms
<span class="dim">15:04:16</span> GET    /api/cart <span class="w">404</span> 5ms</pre>
</figure>
</div></div>

<section id="start"><div class="wrap">
<h2>Up and running in a minute</h2>
<p class="intro">Install with your package manager, log in with an emailed code, and share your app.</p>
<div class="setup">
<div class="panel"><h3>Install</h3><div class="rows">
<div class="row"><span>macOS (Homebrew)</span><div class="cmd"><pre><code id="i-brew">brew install nhtera/tap/tuzy</code></pre>${copyBtn("i-brew", "Homebrew command")}</div></div>
<div class="row"><span>Windows (Scoop)</span><div class="cmd"><pre><code id="i-scoop">scoop bucket add nhtera https://github.com/nhtera/scoop-bucket
scoop install tuzy</code></pre>${copyBtn("i-scoop", "Scoop commands")}</div></div>
<div class="row"><span>Anywhere with Go</span><div class="cmd"><pre><code id="i-go">go install github.com/nhtera/tuzy/cmd/tuzy@latest</code></pre>${copyBtn("i-go", "Go command")}</div></div>
</div></div>
<div class="panel"><h3>First run</h3><ol class="steps">
<li><b>Log in</b><div class="cmd"><pre><code id="s-login">tuzy login</code></pre>${copyBtn("s-login", "login command")}</div><span>A code arrives by email. No password, and the token stays in your OS keychain.</span></li>
<li><b>Start a tunnel</b><div class="cmd"><pre><code id="s-http">tuzy http 3000</code></pre>${copyBtn("s-http", "tunnel command")}</div><span>The first run asks you to pick your permanent name.</span></li>
<li><b>Watch the traffic</b><div class="cmd"><pre><code id="s-open">open http://127.0.0.1:4040</code></pre>${copyBtn("s-open", "inspector address")}</div><span>Inspect, replay and copy any request as curl.</span></li>
</ol></div>
</div>
</div></section>

<section><div class="wrap">
<h2>Built for the loop you already run</h2>
<p class="intro">Point a webhook at your laptop, open your dev server on a phone, or show a client work in progress.</p>
<div class="bento">
<div class="tile t-names"><div class="pair"><div><h3>Names that stay yours</h3><p>Claim up to 10 subdomains per account. Register a webhook URL once and it keeps working across restarts, laptops and CI.</p></div>
<ul class="urls"><li>shop.tuzy.dev</li><li>stripe-hooks.tuzy.dev</li><li>client-preview.tuzy.dev</li></ul></div>
<code class="big">tuzy http 3000 --name shop</code></div>
<div class="tile"><h3>Inspect and replay</h3><p>Every request lands in a local inspector. Replay it after a fix, or copy it as curl.</p><code class="big">open http://127.0.0.1:4040</code></div>
<div class="tile"><h3>Dev servers just work</h3><p>WebSockets, HMR and streaming pass through. If Vite rejects the tunnel host, tuzy switches the Host header for you.</p><code class="big">tuzy http 5173</code></div>
<div class="tile t-file"><h3>Many tunnels, one file</h3><p>Describe your tunnels in <code>tuzy.toml</code>, start them together, or run them as a login service.</p>
<div class="pair"><pre><b>[tunnels.web]</b>
addr = "3000"

<b>[tunnels.api]</b>
addr = "8080"</pre><code class="big">tuzy start --all</code></div></div>
</div>
</div></section>

<section class="fair"><div class="wrap">
<h2>Free, with guardrails</h2>
<div><p>tuzy is free for your own development traffic. To protect visitors from phishing, browsers see a short warning page on tunnels of accounts younger than ${AUTO_TRUST_DAYS} days. Webhooks, curl and API calls never do.</p>
<div class="links"><a class="btn btn-ghost" href="/aup#browser-warning">Browser warning policy</a><a class="btn btn-ghost" href="/aup">Acceptable use</a></div></div>
</div></section>`,
    { layout: "landing", css: LANDING_CSS, scripts: ["/site.js"], path: "/" },
  );
}

/** Copy-to-clipboard for the landing page's command blocks (served same-origin under the CSP). */
export const SITE_JS = `"use strict";
document.querySelectorAll("button[data-copy]").forEach((btn) => {
  const src = document.getElementById(btn.dataset.copy);
  if (!src || !navigator.clipboard) return;
  btn.hidden = false;
  let t;
  btn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(src.textContent.trim());
      btn.textContent = "Copied";
      btn.dataset.done = "";
    } catch {
      btn.textContent = "Press Ctrl+C";
    }
    clearTimeout(t);
    t = setTimeout(() => { btn.textContent = "Copy"; delete btn.dataset.done; }, 1600);
  });
});
`;

export function siteScript(): Response {
  return new Response(SITE_JS, {
    headers: { "content-type": "text/javascript; charset=utf-8", "cache-control": "public, max-age=300", "x-content-type-options": "nosniff" },
  });
}
