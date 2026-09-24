/** /abuse: a report form submitted as JSON by /abuse.js (no inline script under the CSP). */
import { CATEGORIES } from "../lib/abuse";
import { escapeHtml, page } from "./site-layout";

export function abusePage(name: string | null): Response {
  const options = CATEGORIES.map((c) => `<option value="${c}">${c}</option>`).join("");
  return page(
    "Report abuse | tuzy",
    `<h1>Report abuse</h1>
<p class="lede">Seen phishing, malware or other abuse on a <code>*.tuzy.dev</code> address? Tell us. Reports from several networks can take a new tunnel offline automatically.</p>
<form id="report">
<div class="field"><label for="name">Tunnel address</label><input id="name" name="name" required placeholder="shop.tuzy.dev" autocapitalize="off" spellcheck="false" value="${escapeHtml(name ?? "")}"></div>
<div class="field"><label for="category">What is it?</label><select id="category" name="category">${options}</select></div>
<div class="field"><label for="details">Details <span class="hint">(optional)</span></label><textarea id="details" name="details" rows="5" maxlength="4000" placeholder="What did you see? Which page?"></textarea></div>
<div class="field"><label for="email">Your email <span class="hint">(optional, if we may contact you)</span></label><input id="email" name="email" type="email" autocomplete="email"></div>
<div class="hp" aria-hidden="true"><label for="website">Leave empty</label><input id="website" name="website" tabindex="-1" autocomplete="off"></div>
<button class="btn" type="submit">Send report</button>
<p id="result" role="status"></p>
</form>
<p><small>You can also email abuse@tuzy.dev.</small></p>`,
    { scripts: ["/abuse.js"], path: "/abuse", description: "Report phishing, malware or other abuse on a tuzy.dev tunnel." },
  );
}

export const ABUSE_JS = `"use strict";
document.getElementById("report").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target, out = document.getElementById("result"), btn = f.querySelector("button");
  const v = (id) => document.getElementById(id).value;
  btn.disabled = true;
  out.textContent = "Sending…";
  out.dataset.state = "";
  try {
    const res = await fetch("/api/v1/abuse", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: v("name"), category: v("category"), details: v("details"), email: v("email"), website: v("website") }),
    });
    const body = await res.json().catch(() => ({}));
    out.textContent = res.ok ? "Thank you. We received your report." : (body.error && body.error.message) || "Something went wrong; email abuse@tuzy.dev.";
    out.dataset.state = res.ok ? "ok" : "error";
    if (res.ok) f.reset();
  } catch {
    out.textContent = "Network error; email abuse@tuzy.dev.";
    out.dataset.state = "error";
  } finally {
    btn.disabled = false;
  }
});
`;

export function abuseScript(): Response {
  return new Response(ABUSE_JS, {
    headers: { "content-type": "text/javascript; charset=utf-8", "cache-control": "public, max-age=300", "x-content-type-options": "nosniff" },
  });
}
