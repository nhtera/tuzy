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

**My dev server says "Invalid Host header" (Vite, webpack, Rails, Django).**
Visitors send `Host: <name>.tuzy.dev`. Either allow it (Vite: `server.allowedHosts:
['.tuzy.dev']`; Rails: `config.hosts << ".tuzy.dev"`), or run with `--host-header rewrite` to
send your local address instead.

**Browsers see a "You are about to visit a tunnel" page.**
New accounts' tunnels show it once per browser every 7 days, as phishing protection.
Accounts older than 30 days with no upheld abuse reports don't show it. Webhooks, `curl` and
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
