// tuzy inspector UI. Everything shown comes from the internet: build DOM with createElement /
// textContent only (never innerHTML), and talk only to our own origin (CSP enforces both).
"use strict";

const $ = (sel) => document.querySelector(sel);
const state = {
  entries: new Map(), order: [], selected: null, tab: "req", view: "auto", detail: null,
  tunnels: new Set(), editOriginalB64: "", editBinary: false, editBodyDirty: false,
};
const MUTATE = { "X-Tuzy-Inspector": "1", "Content-Type": "application/json" };
const MAX_CLIENT_ENTRIES = 500; // mirrors the server's cap (M5): keeps the client from holding ids the server already evicted

function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v);
  }
  for (const c of children) n.append(c instanceof Node ? c : document.createTextNode(String(c ?? "")));
  return n;
}

function b64bytes(s) {
  if (!s) return new Uint8Array(0);
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
const utf8 = new TextDecoder("utf-8", { fatal: false });

function statusClass(e) {
  if (e.error || (e.done && !e.status)) return "serr";
  return e.status ? "s" + String(e.status)[0] : "";
}

function filters() {
  return { tunnel: $("#f-tunnel").value, method: $("#f-method").value.trim(), status: $("#f-status").value.trim() };
}

function matches(e, f) {
  if (f.tunnel && e.tunnel !== f.tunnel) return false;
  if (f.method && e.method.toLowerCase() !== f.method.toLowerCase()) return false;
  if (f.status) {
    const s = String(e.status || 0), q = f.status.toLowerCase();
    if (/^\dxx$/.test(q)) return s[0] === q[0];
    return s === q;
  }
  return true;
}

function renderList() {
  const tbody = $("#list tbody");
  const f = filters();
  const rows = [];
  for (const id of state.order) {
    const e = state.entries.get(id);
    if (!e || !matches(e, f)) continue;
    const t = new Date(e.started_at);
    const status = e.kind === "ws" && e.status === 101 ? `WS ↓${e.ws_msgs_in} ↑${e.ws_msgs_out}` : e.error ? "error" : e.status || (e.done ? "—" : "…");
    rows.push(
      el("tr", { "data-id": id, class: id === state.selected ? "selected" : "", onclick: () => select(id) },
        el("td", {}, t.toLocaleTimeString()),
        el("td", {}, e.tunnel),
        el("td", {}, e.method + (e.replay_of ? " ↻" : "")),
        el("td", { class: "path", title: e.path }, e.path),
        el("td", { class: statusClass(e) }, status),
        el("td", {}, e.done ? `${e.duration_ms} ms` : ""),
      ),
    );
  }
  tbody.replaceChildren(...rows);
  $("#empty").hidden = rows.length > 0;
}

function visibleIds() {
  const f = filters();
  return state.order.filter((id) => matches(state.entries.get(id), f));
}

async function select(id) {
  state.selected = id;
  $("#detail-evicted").hidden = true;
  renderList();
  const res = await fetch(`/api/requests/${id}`);
  if (state.selected !== id) return; // M5: a newer selection happened while this was in flight
  if (!res.ok) {
    // The server has forgotten this id (evicted or cleared): forget it here too.
    forgetEntry(id);
    state.detail = null;
    renderDetail();
    renderList();
    $("#detail-evicted").hidden = false;
    return;
  }
  state.detail = await res.json();
  if (state.selected !== id) return; // race check again: a selection may have changed during the await
  renderDetail();
}

// forgetEntry drops a locally-held id the server no longer knows about (M5).
function forgetEntry(id) {
  state.entries.delete(id);
  const i = state.order.indexOf(id);
  if (i !== -1) state.order.splice(i, 1);
}

function headersTable(pairs) {
  return el("table", { class: "kv" }, ...(pairs || []).map(([k, v]) => el("tr", {}, el("td", {}, k), el("td", {}, v))));
}

function hexdump(bytes) {
  const lines = [];
  for (let off = 0; off < bytes.length; off += 16) {
    const chunk = bytes.subarray(off, off + 16);
    const hex = Array.from(chunk, (b) => b.toString(16).padStart(2, "0")).join(" ");
    const ascii = Array.from(chunk, (b) => (b >= 32 && b < 127 ? String.fromCharCode(b) : ".")).join("");
    lines.push(off.toString(16).padStart(8, "0") + "  " + hex.padEnd(48) + "  " + ascii);
  }
  return lines.join("\n");
}

function headerValue(pairs, name) {
  const h = (pairs || []).find(([k]) => k.toLowerCase() === name);
  return h ? h[1] : "";
}

function bodyView(bytes, contentType) {
  const text = utf8.decode(bytes);
  const modes = ["auto", "text", "json", "form", "hex"];
  let mode = state.view;
  if (mode === "auto") {
    if (/json/.test(contentType)) mode = "json";
    else if (/x-www-form-urlencoded/.test(contentType)) mode = "form";
    else mode = /[\u0000-\u0008\u000e-\u001f]/.test(text) ? "hex" : "text";
  }
  let content;
  try {
    if (mode === "json") content = el("pre", {}, JSON.stringify(JSON.parse(text), null, 2));
    else if (mode === "form") content = headersTable([...new URLSearchParams(text)]);
    else if (mode === "hex") content = el("pre", {}, hexdump(bytes));
    else content = el("pre", {}, text);
  } catch {
    content = el("pre", {}, text);
  }
  const bar = el("div", { class: "viewer-modes" },
    ...modes.map((m) => el("button", { class: m === state.view ? "active" : "", onclick: () => { state.view = m; renderDetail(); } }, m)));
  return [bar, content];
}

function renderDetail() {
  const d = state.detail;
  $("#detail-empty").hidden = !!d;
  $("#detail").hidden = !d;
  if (!d) return;
  $("#d-title").textContent = `#${d.id} ${d.method} ${d.path} → ${d.status || d.error || "…"} (${d.tunnel})`;
  const notes = [];
  if (d.replay_of) notes.push(`Replay of #${d.replay_of}.`);
  if (d.req_truncated || d.res_truncated) notes.push("Bodies over 1 MiB are truncated in the inspector (the tunnel relayed them in full).");
  if (d.req_incomplete) notes.push("The captured request body is incomplete (the visitor aborted, or it fell short of the declared Content-Length).");
  if (d.bodies_evicted) notes.push("Bodies were evicted to keep memory under 64 MiB.");
  if (d.kind === "ws") notes.push(`WebSocket: ${d.ws_msgs_in} messages from the visitor, ${d.ws_msgs_out} to the visitor.`);
  if (d.decode_error) notes.push(`Could not decode the response body: ${d.decode_error}`);
  if (d.req_body_decoded_truncated || d.res_body_decoded_truncated) notes.push("The decoded body shown was cut at 1 MiB.");
  $("#d-note").hidden = notes.length === 0;
  $("#d-note").textContent = notes.join(" ");
  $("#replay").disabled = d.kind === "ws";
  $("#edit").disabled = d.kind === "ws";
  for (const b of document.querySelectorAll(".tabs button")) b.classList.toggle("active", b.dataset.tab === state.tab);
  const parts = [];
  if (state.tab === "req") {
    parts.push(el("p", { class: "muted" }, `From ${d.remote_ip} · ${d.req_size} bytes`), headersTable(d.req_headers));
    const bytes = d.req_body_decoded ? b64bytes(d.req_body_decoded) : b64bytes(d.req_body);
    if (bytes.length) parts.push(...bodyView(bytes, headerValue(d.req_headers, "content-type")));
  } else {
    parts.push(el("p", { class: "muted" }, `${d.res_size} bytes${d.res_encoding ? ` (${d.res_encoding}, decoded for display)` : ""}`), headersTable(d.res_headers));
    const bytes = d.res_body_decoded ? b64bytes(d.res_body_decoded) : b64bytes(d.res_body);
    if (bytes.length) parts.push(...bodyView(bytes, headerValue(d.res_headers, "content-type")));
  }
  $("#tab-body").replaceChildren(...parts);
}

async function replay() {
  const d = state.detail;
  if (!d || d.kind === "ws") return;
  const res = await fetch(`/api/requests/${d.id}/replay`, { method: "POST", headers: MUTATE, body: "{}" });
  const out = await res.json();
  if (!res.ok) return alertNote(out.error || "replay failed");
  select(out.id);
}

function alertNote(msg) {
  $("#d-note").hidden = false;
  $("#d-note").textContent = msg;
}

// isValidUTF8 reports whether bytes decode losslessly as UTF-8 (a strict decode: no replacement
// characters swallowed).
function isValidUTF8(bytes) {
  try {
    new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return true;
  } catch {
    return false;
  }
}

function bytesToBase64(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

// openEdit fills the dialog from the original entry. The body textarea is only ever the source of
// truth for the replayed body when the user actually edits it (state.editBodyDirty): otherwise the
// original body_b64 bytes are sent unchanged (M4) — decoding to text and re-encoding an untouched
// binary body would corrupt it (invalid UTF-8 gets replaced on decode).
function openEdit() {
  const d = state.detail;
  if (!d) return;
  $("#e-method").value = d.method;
  $("#e-path").value = d.path;
  $("#e-headers").value = (d.req_headers || []).map(([k, v]) => `${k}: ${v}`).join("\n");
  const bytes = b64bytes(d.req_body);
  state.editOriginalB64 = d.req_body || "";
  state.editBinary = bytes.length > 0 && !isValidUTF8(bytes);
  state.editBodyDirty = false;
  const body = $("#e-body");
  if (state.editBinary) {
    body.value = "(binary body: not editable as text here — Send replays the original bytes unchanged)";
    body.readOnly = true;
  } else {
    body.value = utf8.decode(bytes);
    body.readOnly = false;
  }
  const warnings = [];
  if (d.req_truncated) warnings.push("the captured body is truncated (over 1 MiB)");
  if (d.bodies_evicted) warnings.push("bodies were evicted to save memory");
  if (d.req_incomplete) warnings.push("the captured request body is incomplete");
  $("#e-note").hidden = warnings.length === 0;
  $("#e-note").textContent = warnings.length ? "Warning: " + warnings.join("; ") + "." : "";
  $("#edit-dialog").showModal();
}

async function sendEdited(ev) {
  if (ev.submitter?.value !== "send") return;
  const headers = $("#e-headers").value.split("\n").map((l) => l.trim()).filter(Boolean).map((l) => {
    const i = l.indexOf(":");
    return i > 0 ? [l.slice(0, i).trim(), l.slice(i + 1).trim()] : null;
  }).filter(Boolean);
  // Unless the textarea was actually edited, replay the original bytes verbatim (M4).
  const bodyB64 = state.editBodyDirty && !state.editBinary
    ? bytesToBase64(new TextEncoder().encode($("#e-body").value))
    : state.editOriginalB64;
  const res = await fetch("/api/replay", {
    method: "POST", headers: MUTATE,
    body: JSON.stringify({ tunnel: state.detail.tunnel, method: $("#e-method").value.trim(), path: $("#e-path").value.trim(), headers, body_b64: bodyB64 }),
  });
  const out = await res.json();
  if (!res.ok) return alertNote(out.error || "replay failed");
  select(out.id);
}

async function copyCurl() {
  const d = state.detail;
  if (!d) return;
  const text = await (await fetch(`/api/requests/${d.id}/curl`)).text();
  try {
    await navigator.clipboard.writeText(text);
    alertNote("curl command copied.");
  } catch {
    alertNote(text);
  }
}

// upsert adds/updates a summary. New ids are capped at MAX_CLIENT_ENTRIES (M5), mirroring the
// server's own cap, so the client never holds on to ids the server has already forgotten.
function upsert(summary) {
  if (!state.entries.has(summary.id)) {
    state.order.unshift(summary.id);
    while (state.order.length > MAX_CLIENT_ENTRIES) {
      const dropped = state.order.pop();
      state.entries.delete(dropped);
      if (dropped === state.selected) {
        state.selected = null;
        state.detail = null;
      }
    }
  }
  state.entries.set(summary.id, summary);
}

// refreshTunnels repopulates the tunnel filter dropdown (L6), keeping the current selection.
async function refreshTunnels() {
  const tunnels = await fetch("/api/tunnels").then((r) => r.json());
  state.tunnels = new Set(tunnels);
  const sel = $("#f-tunnel");
  const current = sel.value;
  sel.replaceChildren(el("option", { value: "" }, "all tunnels"), ...tunnels.map((t) => el("option", { value: t }, t)));
  sel.value = current;
}

async function load() {
  const [list] = await Promise.all([fetch("/api/requests").then((r) => r.json()), refreshTunnels()]);
  state.entries.clear();
  state.order = [];
  for (const e of list.slice().reverse()) upsert(e);
  renderList();
}

function connect() {
  const es = new EventSource("/api/events");
  es.onopen = () => { $("#conn").textContent = "live"; load(); };
  es.onerror = () => { $("#conn").textContent = "reconnecting…"; };
  es.addEventListener("created", (m) => {
    const e = JSON.parse(m.data).entry;
    upsert(e);
    renderList();
    // L6: a tunnel that wasn't in the dropdown yet (e.g. it just came online) — refresh it.
    if (e.tunnel && !state.tunnels.has(e.tunnel)) refreshTunnels();
  });
  es.addEventListener("updated", (m) => {
    const e = JSON.parse(m.data).entry;
    upsert(e);
    renderList();
    if (e.id === state.selected && e.done) select(e.id);
  });
  es.addEventListener("cleared", () => {
    state.entries.clear();
    state.order = [];
    state.selected = null;
    state.detail = null;
    $("#detail-evicted").hidden = true;
    renderList();
    renderDetail();
  });
}

document.addEventListener("keydown", (ev) => {
  // H2: ignore modified keys (Cmd/Ctrl/Alt+R and friends) and OS auto-repeat.
  if (ev.metaKey || ev.ctrlKey || ev.altKey || ev.repeat) return;
  const t = ev.target;
  const editable = t instanceof HTMLInputElement || t instanceof HTMLTextAreaElement || t instanceof HTMLSelectElement
    || (t instanceof HTMLElement && t.isContentEditable);
  if (editable || $("#edit-dialog").open) return;
  const ids = visibleIds();
  const i = ids.indexOf(state.selected);
  if (ev.key === "ArrowDown" && ids.length) { ev.preventDefault(); select(ids[Math.min(ids.length - 1, i + 1)]); }
  else if (ev.key === "ArrowUp" && ids.length) { ev.preventDefault(); select(ids[Math.max(0, i - 1)]); }
  else if (ev.key === "r") replay();
});

$("#e-body").addEventListener("input", () => { state.editBodyDirty = true; });

$("#f-tunnel").addEventListener("change", renderList);
$("#f-method").addEventListener("input", renderList);
$("#f-status").addEventListener("input", renderList);
$("#clear").addEventListener("click", () => fetch("/api/requests", { method: "DELETE", headers: MUTATE }));
$("#replay").addEventListener("click", replay);
$("#edit").addEventListener("click", openEdit);
$("#curl").addEventListener("click", copyCurl);
$("#edit-form").addEventListener("submit", sendEdited);
for (const b of document.querySelectorAll(".tabs button")) b.addEventListener("click", () => { state.tab = b.dataset.tab; renderDetail(); });
connect();
