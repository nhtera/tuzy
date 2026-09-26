/**
 * The "where did the request stop" diagram on visitor error pages: Visitor → tuzy edge → tuzy agent
 * → Your service, greyed out from the broken hop on with a red ✕ on the link that failed.
 * The agent's own 502 page (internal/tunnel/local_error_page.html) carries the same markup and
 * classes; keep the two in sync.
 */

/** The hop the request could not reach: the agent (edge ✕ agent) or the local service (agent ✕ service). */
export type BreakAt = "agent" | "service";

export const REQUEST_PATH_CSS = `.path{list-style:none;margin:0 0 1.8rem;padding:0;display:grid;grid-template-columns:repeat(4,1fr);text-align:center}
.path li{position:relative;display:flex;flex-direction:column;align-items:center;gap:.55rem;color:var(--accent);font:600 .85rem/1.25 var(--sans)}
.path svg{width:2rem;height:2rem;fill:none;stroke:currentColor;stroke-width:1.6;stroke-linecap:round;stroke-linejoin:round}
.path .dot{position:relative;z-index:1;width:.7rem;height:.7rem;border-radius:50%;background:currentColor;box-shadow:0 0 0 4px var(--bg)}
.path li+li::before{content:"";position:absolute;top:calc(2.55rem + .35rem - 1.5px);left:-50%;right:50%;height:3px;border-radius:2px;background:var(--accent)}
.path .label{color:var(--text)}
.path .down{color:var(--faint)}
.path .down .label{color:var(--faint)}
.path .down::before{background:var(--border-strong)}
.path .x{position:absolute;z-index:2;top:calc(2.55rem + .35rem);left:0;transform:translate(-50%,-50%);width:1.5rem;height:1.5rem;border-radius:50%;background:var(--bad);color:#fff;display:grid;place-items:center;box-shadow:0 0 0 4px var(--bg)}
.path .x svg{width:.8rem;height:.8rem;stroke-width:2.6}
@media(max-width:26rem){.path li{font-size:.75rem}.path svg{width:1.7rem;height:1.7rem}}`;

const HOPS = [
  { label: "Visitor", icon: `<rect x="6" y="2.5" width="12" height="19" rx="2.5"/><path d="M11 18h2"/>` },
  {
    label: "tuzy edge",
    icon: `<circle cx="12" cy="12" r="9.5"/><path d="M2.5 12h19M12 2.5c2.6 2.8 3.9 6 3.9 9.5s-1.3 6.7-3.9 9.5c-2.6-2.8-3.9-6-3.9-9.5s1.3-6.7 3.9-9.5z"/>`,
  },
  { label: "tuzy agent", icon: `<rect x="2.5" y="3.5" width="19" height="17" rx="2.5"/><path d="M7 9.5l3 2.5-3 2.5M12.5 15h4.5"/>` },
  { label: "Your service", icon: `<rect x="4" y="2.5" width="16" height="19" rx="2"/><path d="M4 8.5h16M4 14.5h16M8 5.5h1M8 11.5h1M8 17.5h1"/>` },
];

const BREAK_INDEX: Record<BreakAt, number> = { agent: 2, service: 3 };

const CROSS = `<span class="x" role="img" aria-label="connection failed"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg></span>`;

export function requestPath(breakAt: BreakAt): string {
  const broken = BREAK_INDEX[breakAt];
  const items = HOPS.map((hop, i) => {
    const down = i >= broken;
    return `<li${down ? ` class="down"` : ""}>${i === broken ? CROSS : ""}<svg viewBox="0 0 24 24" aria-hidden="true">${hop.icon}</svg><span class="dot"></span><span class="label">${hop.label}</span></li>`;
  });
  return `<ol class="path" aria-label="Request path">${items.join("")}</ol>`;
}
