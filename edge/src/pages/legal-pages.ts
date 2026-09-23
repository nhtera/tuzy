/**
 * Terms, privacy and acceptable-use pages. TEMPLATES: the operator must review them (not legal
 * advice) before public launch — a phase 9 launch-checklist blocker.
 */
import { page } from "./site-layout";

const UPDATED = "2026-09-24";

export function aupPage(): Response {
  return page(
    "Acceptable use — tuzy",
    `<h1>Acceptable use policy</h1><p><small>Last updated ${UPDATED}</small></p>
<p>tuzy exposes services running on your own computer. You are responsible for everything served through your tunnels. You must not use tuzy to:</p>
<ul>
<li>phish, impersonate a brand, bank, email provider or any other service, or collect credentials or payment details that aren't for your own service;</li>
<li>distribute malware, run command-and-control servers, or host exploit kits;</li>
<li>send spam or host content promoted by spam;</li>
<li>host content that is illegal where you or your visitors are, including child sexual abuse material, or that infringes others' rights;</li>
<li>run open proxies, VPNs or relays for third parties, mine cryptocurrency, or serve as a general-purpose CDN or file host;</li>
<li>attack, scan or overload other systems, or evade tuzy's limits (for example by creating many accounts).</li>
</ul>
<p>We may suspend names or accounts that break these rules, sometimes automatically after reports from several networks, and we cooperate with valid legal requests. Report abuse at <a href="/abuse">tuzy.dev/abuse</a> or abuse@tuzy.dev.</p>
<h2>Limits</h2>
<p>Each account has up to 10 names, a request-rate limit per tunnel, and a monthly allowance of long-running streams (50 hours of streams past their first 5 minutes). Limits may change to keep the service free and healthy.</p>`,
  );
}

export function termsPage(): Response {
  return page(
    "Terms — tuzy",
    `<h1>Terms of service</h1><p><small>Last updated ${UPDATED}</small></p>
<p>By creating an account or running the tuzy CLI you agree to these terms and to the <a href="/aup">acceptable use policy</a>.</p>
<h2>The service</h2>
<p>tuzy is provided free of charge, as is, without warranty of any kind, and may change, be limited or be discontinued at any time. We don't promise any uptime or that data will be delivered.</p>
<h2>Your account</h2>
<p>You log in with a code sent to your email address. Keep your tokens secret; you are responsible for traffic through your tunnels. Names you release stay reserved for you for 12 months, then may be reused by others.</p>
<h2>Suspension and termination</h2>
<p>We may suspend or delete names and accounts that violate these terms or the AUP, or that put the service or its users at risk. You can delete your account at any time with <code>tuzy account delete</code>.</p>
<h2>Liability</h2>
<p>To the maximum extent permitted by law, the operators of tuzy are not liable for any indirect, incidental or consequential damages, or for any loss of data, revenue or profits, arising from your use of the service.</p>
<h2>Changes</h2>
<p>We may update these terms; the date above changes when we do. Continued use after a change means you accept it.</p>
<p>Questions: abuse@tuzy.dev.</p>`,
  );
}

export function privacyPage(): Response {
  return page(
    "Privacy — tuzy",
    `<h1>Privacy policy</h1><p><small>Last updated ${UPDATED}</small></p>
<h2>What we store</h2>
<ul>
<li><b>Account:</b> your email address, when you created the account and last logged in, your names and tokens (only a hash of each token).</li>
<li><b>Login codes:</b> a hash of each emailed code, the address it was sent to and your network address, kept 1 day.</li>
<li><b>Security records:</b> an audit log of account actions (login, token and name changes) with your network address (the full IPv4 address, or the /64 network for IPv6), kept 180 days.</li>
<li><b>Abuse reports:</b> the report, the reporter's optional email and network (IPv4 /24, IPv6 /48), kept up to a year after they are closed.</li>
<li><b>Usage:</b> a monthly count of long-running stream time per account, kept 13 months.</li>
</ul>
<h2>What we don't store</h2>
<p>Traffic through your tunnels is relayed, not recorded: we don't keep request or response bodies, and we don't log visitor URLs. The request inspector in the CLI keeps traffic only in your computer's memory.</p>
<h2>Processors</h2>
<p>tuzy runs on Cloudflare (Workers, Durable Objects, D1, KV, Email). Login codes are sent through Cloudflare's email service.</p>
<h2>Deleting your data</h2>
<p>Run <code>tuzy account delete</code> (confirmed by an emailed code). Your tokens are revoked and your tunnels closed at once. Your names stay reserved (not usable by others) for 12 months so nobody can take over your webhook URLs. Your email address is erased 30 days after deletion, including from abuse reports you filed; only a one-way hash is kept to link abuse history.</p>
<h2>Contact</h2>
<p>abuse@tuzy.dev</p>`,
  );
}

export function securityTxt(baseDomain: string): Response {
  const expires = new Date(Date.now() + 180 * 86400_000).toISOString().replace(/\.\d+Z$/, "Z");
  const body = `Contact: mailto:security@${baseDomain}\nContact: mailto:abuse@${baseDomain}\nExpires: ${expires}\nPreferred-Languages: en, vi\nCanonical: https://${baseDomain}/.well-known/security.txt\nPolicy: https://${baseDomain}/aup\n`;
  return new Response(body, { headers: { "content-type": "text/plain; charset=utf-8", "cache-control": "public, max-age=3600" } });
}
