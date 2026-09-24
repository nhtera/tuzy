import { AUTO_TRUST_DAYS } from "../lib/trust";
import { page } from "./site-layout";

export function landingPage(): Response {
  return page(
    "tuzy — expose localhost at a stable URL",
    `<h1>Expose localhost at a stable <code>https://&lt;name&gt;.tuzy.dev</code> URL</h1>
<p>tuzy is a free tunnel for developers: webhooks, demos and mobile testing against your own machine. Your subdomain stays yours, so webhook URLs don't change between runs.</p>
<h2>Install</h2>
<pre><code># macOS / Linux
brew install nhtera/tap/tuzy
curl -fsSL https://tuzy.dev/install.sh | sh

# Windows
scoop bucket add nhtera https://github.com/nhtera/scoop-bucket
scoop install tuzy

# Anywhere with Go
go install github.com/nhtera/tuzy/cmd/tuzy@latest</code></pre>
<h2>Quickstart</h2>
<pre><code>tuzy login          # email code, no password
tuzy http 3000      # → https://&lt;your-name&gt;.tuzy.dev
open http://127.0.0.1:4040   # inspect &amp; replay requests</code></pre>
<ul>
<li>Up to 10 permanent names per account (<code>tuzy names</code>)</li>
<li>HTTP, WebSockets and streaming responses</li>
<li>A local inspector with replay and copy-as-curl</li>
<li>Several tunnels from one <code>tuzy.toml</code> (<code>tuzy start --all</code>)</li>
</ul>
<h2>Fair use</h2>
<p>tuzy is free. Tunnels are for your own development traffic; see the <a href="/aup">acceptable use policy</a>. Browsers see a warning page on tunnels of accounts younger than ${AUTO_TRUST_DAYS} days (visitors skip it for 7 days after clicking through); webhooks and API calls never do. See the <a href="/aup#browser-warning">browser warning policy</a>.</p>`,
  );
}
