/** /abuse: a report form submitted as JSON by /abuse.js (no inline script under the CSP). */
import { CATEGORIES } from "../lib/abuse";
import { escapeHtml, page } from "./site-layout";

export function abusePage(name: string | null): Response {
  const options = CATEGORIES.map((c) => `<option value="${c}">${c}</option>`).join("");
  return page(
    "Report abuse — tuzy",
    `<h1>Report abuse</h1>
<p>Seen phishing, malware or other abuse on a <code>*.tuzy.dev</code> address? Tell us. Reports from several networks can take a new tunnel offline automatically.</p>
<form id="report">
<label for="name">Tunnel address</label><input id="name" name="name" required placeholder="shop.tuzy.dev" value="${escapeHtml(name ?? "")}">
<label for="category">What is it?</label><select id="category" name="category">${options}</select>
<label for="details">Details (optional)</label><textarea id="details" name="details" rows="5" maxlength="4000" placeholder="What did you see? Which page?"></textarea>
<label for="email">Your email (optional, if we may contact you)</label><input id="email" name="email" type="email">
<div class="hp" aria-hidden="true"><label for="website">Leave empty</label><input id="website" name="website" tabindex="-1" autocomplete="off"></div>
<button type="submit">Send report</button>
<p id="result" role="status"></p>
</form>`,
    { script: "/abuse.js" },
  );
}

export const ABUSE_JS = `"use strict";
document.getElementById("report").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target, out = document.getElementById("result"), btn = f.querySelector("button");
  const v = (id) => document.getElementById(id).value;
  btn.disabled = true;
  out.textContent = "Sending…";
  try {
    const res = await fetch("/api/v1/abuse", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: v("name"), category: v("category"), details: v("details"), email: v("email"), website: v("website") }),
    });
    const body = await res.json().catch(() => ({}));
    out.textContent = res.ok ? "Thank you — we received your report." : (body.error && body.error.message) || "Something went wrong; email abuse@tuzy.dev.";
    if (res.ok) f.reset();
  } catch {
    out.textContent = "Network error; email abuse@tuzy.dev.";
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
