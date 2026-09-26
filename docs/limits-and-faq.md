# Limits and FAQ

## Limits

| What | Limit |
|---|---|
| Names per account | 10 (`tuzy names`) |
| Released or renamed names held for you | 12 months (20 at a time; beyond that, `names rm` releases without a hold) |
| Requests per tunnel | 600 per 10 s (then 429) |
| Request body | 100 MB (Cloudflare) |
| One HTTP stream | 1 hour (then reset) |
| Time to first response byte | 300 s (then 504) |
| Long streams (open > 5 min) per tunnel | 8 at a time |
| Long-stream time per account | 50 hours per month, counting only time after each stream's first 5 minutes. Past it, new streams are cut at 5 minutes. `tuzy whoami` shows usage. |
| Protocols | HTTP(S) and WebSockets. No raw TCP or UDP. |

WebSocket connection time does not count toward the long-stream allowance.

## FAQ

**Dev servers with a host check (Vite, webpack, Rails, Django) work out of the box.**
Visitors send `Host: <name>.tuzy.dev`. By default (`--host-header auto`) tuzy passes it through.
The first time your dev server rejects it ("Blocked request. This host … is not allowed", "Invalid
Host header"), tuzy switches that tunnel to `Host: localhost:<port>` and retries the request, so the
page loads. The public host is always in `X-Forwarded-Host`.
- `--host-header rewrite` always sends the local host.
- `--host-header preserve` always sends the public host (for apps that need it, such as multi-tenant
  routing); then allow `.tuzy.dev` in the dev server's config.

**Browsers see a "You are about to visit a tunnel" page.**
Tunnels of accounts younger than 7 days show it (phishing protection), once per browser and
address every 7 days. It stops by itself when the account turns 7 days old, unless an abuse report
against the account was upheld. Policy: https://tuzy.dev/aup#browser-warning. Webhooks, `curl` and
`fetch` never see it. Automation (Playwright, screenshot tools) can send the request header
`tuzy-skip-warning: 1`.

Two side effects while the page is shown:
- A cross-site **form POST** navigation (e.g. an OAuth `form_post` or a payment return) shows the
  page instead, and its body is lost. Open the tunnel URL in that browser once first.
- An untrusted tunnel **can't be framed** by another site.

**Why did my tunnel reconnect by itself?**
The edge was redeployed. Agents reconnect within seconds, and in-flight requests are finished or
retried by the visitor.

**Cookies across tunnels.**
All `*.tuzy.dev` tunnels are the same *site* for browsers, until tuzy.dev is on the Public
Suffix List. Don't rely on SameSite to isolate your tunnel from other people's tunnels. Cookies
named `tuzy_*` are reserved and never reach your app.

**The file server hides files.**
Dotfiles (and anything reached through a symlink into one) are never served. On Windows, names
that look like 8.3 short names (`~1`) are hidden too. Serve a folder you would publish as a
whole.

**Corporate proxy.**
tuzy honours `HTTPS_PROXY`. It needs WebSockets to `wss://tuzy.dev`. If `tuzy diagnose` reports
TLS interception or a WebSocket failure, ask IT to exempt `tuzy.dev`.

**Updates.**
`tuzy update` verifies the release signature and checksum and replaces the binary in one step.
Homebrew, Scoop and `go install` users get told the right command instead. Turn off the
once-a-day update notice with `TUZY_NO_UPDATE_CHECK=1` or `update_check = false`.

## Errors

When a request can't reach your app, the visitor sees an error page served by tuzy itself (not by
your app). Every such page has a code: on the page, in the `tuzy-error` response header, and as a
heading below. Browsers get an HTML page that shows where the request stopped. `curl`, webhook
senders and other clients get one line, e.g. `tuzy: tunnel offline (TUZY-502-AGENT-OFFLINE)`.
Pages served by the edge also carry `x-tuzy-edge: 1`.

### TUZY-502-LOCAL-UNREACHABLE
The tunnel is up, but the agent could not connect to your local app. Start the app, or check that
the port or address you gave `tuzy http` is the one it listens on (`curl http://localhost:3000`).
For an `https://` target with a self-signed certificate, add `--upstream-insecure`; if the app
speaks plain HTTP, use an `http://` target.

### TUZY-502-AGENT-OFFLINE
No agent is connected for this name. Run `tuzy http <port> --name <name>` (or `tuzy start`) on the
machine that serves the site. Clients may retry after the `Retry-After` delay.

### TUZY-502-AGENT-RESTARTING
The agent is shutting down or reconnecting (for example during an edge redeploy). It clears by
itself within seconds. If it doesn't, restart `tuzy`.

### TUZY-502-BAD-RESPONSE
The agent's reply broke the tunnel protocol or the stream was cancelled. Check the `tuzy` output
and update it with `tuzy update`.

### TUZY-504-LOCAL-TIMEOUT
Your app did not send the first byte of its response within 300 seconds.

### TUZY-504-STREAM-LIMIT
The request ran past a stream limit: 1 hour per stream, or the monthly long-stream time (see
Limits above).

### TUZY-503-TUNNEL-BUSY
Too many requests are in flight on this tunnel at once, usually because the app is slow to
answer. Retry after the `Retry-After` delay.

### TUZY-429-RATE-LIMITED
The tunnel received more than 600 requests in 10 seconds. Retry after the `Retry-After` delay.

### TUZY-431-HEADERS-TOO-LARGE
The request headers (usually cookies) are too large to relay. Clear the site's cookies.

### TUZY-404-TUNNEL-NOT-FOUND
There is no tunnel at this address. Check the name.

### TUZY-451-TUNNEL-SUSPENDED
The tunnel was suspended under the acceptable use policy. Contact abuse@tuzy.dev if you think this
is a mistake.
