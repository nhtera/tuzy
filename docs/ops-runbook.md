# Ops runbook

## Deploys

**Edge:** tag `edge-vX.Y.Z` (or run *deploy-edge* manually). The `production` environment needs
approval. The job then:
1. runs the edge tests;
2. records a D1 Time Travel bookmark in the job summary;
3. applies migrations;
4. deploys;
5. smoke-tests `/api/v1/health`, `/install.sh`, `/install.ps1` and the offline page.

Every edge deploy reconnects every agent once (GOAWAY / 1012 → U(0, 3 s) jitter).

**CLI:** tag `vX.Y.Z` from `main`.
- `release-cli` first checks that prod's `/api/v1/health` protocol range includes the CLI's
  protocol, then runs GoReleaser.
- GoReleaser uploads:
  - the archives, `checksums.txt` and `checksums.txt.sig` (ed25519, `TUZY_RELEASE_KEY`);
  - the cosign bundle `checksums.txt.sigstore.json`;
  - the Homebrew cask and the Scoop manifest.
- The job then verifies the published signature with the key embedded in the CLI.
- A CLI tag never deploys the edge, and an `edge-v*` tag never publishes a CLI.

**Rehearsal and hotfixes:**
- `vX.Y.Z-rc.N` tags become GitHub prereleases. `tuzy update` ignores them unless given
  `--version`, and the taps are not touched (`skip_upload: auto`).
- `releases/latest` sorts by the tagged commit's date. Tag hotfixes from `main`; stable
  releases are marked latest explicitly.

## Migrations

- **Expand-only:** old code must keep working on the new schema.
- **Before a destructive admin action:** `wrangler d1 export tuzy --remote --output backup.sql`.
- **Time Travel (30 days) is a last resort.** It rewinds revocations, suspensions and claims made
  after the bookmark. Before restoring, export `audit_log` and `do_outbox` since the bookmark and
  re-apply those actions after.

## Rollback

- **Edge:** `wrangler rollback`. This is itself a deploy, so agents reconnect once. It cannot
  cross a Durable Object migration: new DO classes or DO migrations are only ever rolled forward.
- **CLI:** previous releases stay installable (`tuzy update --version vX.Y.Z`). The edge keeps
  accepting protocol 1 for all v1.x.

## Admin

**Bootstrap the first admin:**

```sh
wrangler d1 execute tuzy --remote --command "UPDATE users SET role='admin' WHERE email='you@example.com'"
```

`tuzy admin` is hidden from `--help`. Every admin action writes an audit row
(`tuzy admin audit`).

| Situation | Response |
|---|---|
| Phishing report | `tuzy admin reports` → `admin suspend-name <name> --reason …` (451 in < 5 s) → `admin report <id> actioned` (revokes the owner's auto-trust) |
| Repeat offender | `admin suspend-user <id|email>` (closes all their tunnels) |
| Brand/abuse name | `admin block-name <name>`; undo with `admin release-name` |
| False positive | `admin unsuspend-name <name>` (also dismisses its open reports) |
| Abuse wave | `QUARANTINE_THRESHOLD=2` (edge deploy); `admin set-max-names --new-accounts 2` |
| Trusted partner | `admin trust-user <id|email>` (no interstitial) |
| Stuck pushes | `admin outbox`; the */5 cron retries, and an admin email goes out after 5 min |

A signal to tune: more than 2 unsuspends a week means the quarantine threshold is too low.

## Email

- **Quota:** the Cloudflare Email Service allows 1,000 sends a day per account. The
  `DAILY_SEND_BUDGET` of 900 keeps logins inside it. At 80 % the breaker stops new signups
  (existing users can still log in) and alerts the admin.
- **Admin alerts** have their own caps: 50 report notices and 200 auto-action notices a day.
- **Fallback:** if the provider fails, switch `EMAIL_DRIVER` to a second provider (not built in
  v1; plan it before scale).

## Cost model and alerts

- **Where the cost is:** DO duration while requests are in flight (hibernated sockets are free),
  Worker and DO requests, KV reads for connected markers (cached for 60 s), and D1 queries.
- **Long streams:** 8 per name and 50 h per account per month.
- **Alerts to configure:** Cloudflare usage and billing thresholds at 50 % of the monthly budget
  for DO duration, Worker requests, KV reads and D1 writes.
- **Levers:** lower `MAX_STREAM_SECONDS`; lower the long-stream cap or budget; raise the marker
  cache TTL.

## Load test (pre-launch, prod)

1. **Seed**, without OTP:
   - 10 users and 10 connect-scope tokens via `wrangler d1 execute --remote`. Hash tokens like
     `edge/src/lib/tokens.ts`.
   - `admin set-max-names <u> 100`.
   - Names `lt-<u>-<n>`.
2. **Storm:** the `e2e/storm` driver connects 1,000 agents. Then `wrangler deploy` three times,
   5 minutes apart. Record the reconnect time p50/p95/max, 429s, 5xx and `cf-mitigated`, which
   the driver counts separately. The target is p95 < 5 s and no D1 errors.
3. **Soak:** 1 hour with an edge deploy every 15 minutes, visitor traffic ≤ 60 rps per name, a
   few 50 MB sha256 transfers each cycle and one 6-minute long stream. Record the driver's
   goroutines and RSS every minute.
4. **Watch:**
   - D1 latency p50/p95/p99, reads and writes, errors;
   - Worker status codes and CPU p99;
   - DO requests, errors and duration;
   - `wrangler tail --status error`, for `last_used_at update failed`,
     `last_seen_at update failed` and `recording tunnel session failed`.
5. **Clean up**, in this order:
   1. Stop the driver.
   2. `names rm` every `lt-*` name.
   3. `DELETE FROM released_names WHERE name LIKE 'lt-%'`.
   4. Revoke the tokens.
   5. `admin suspend-user` the 10 users.
   6. Verify that `SELECT count(*) FROM reservations WHERE name LIKE 'lt-%'` returns 0.
6. Record the results and the actual Cloudflare usage below **the same day**; analytics
   retention is short.

### Results

_Not run yet._

## Monitoring

- **External uptime probe:** `/api/v1/health` plus one synthetic tunnel.
- **Mailboxes:** watch abuse@ and security@. Both forward to the admin inbox through Email
  Routing.
- **Zone settings:** keep Bot Fight Mode off. Make sure Browser Integrity Check and Security
  Level never challenge webhooks or the API on `*.tuzy.dev`: turn them off, or add a WAF skip
  rule.

## Platform notes

### Wrangler config
- **One config file:** `edge/wrangler.jsonc`, with **no named environments** (no staging: local `wrangler dev` + production only).
- **Worker:** named `tuzy`, with routes `tuzy.dev/*` and `*.tuzy.dev/*` (zone `tuzy.dev`).
- **`compatibility_date`:** capped by the workerd bundled with `@cloudflare/vitest-pool-workers` (currently `2026-08-22`). Raise it only after upgrading the pool.

### Host routing under `wrangler dev`
- **The problem:** when `routes` are configured, `wrangler dev` rewrites the request URL and `Host` to the first route's zone. That breaks Host-based tunnel routing. `--local-upstream`, `--host` and `--routes` don't help.
- **The workaround:** `npm run dev` runs `scripts/gen-dev-config.mjs`. It writes the gitignored `edge/wrangler.dev.gen.jsonc`: the same config without `routes`, with `BASE_DOMAIN=localhost`. `wrangler.jsonc` stays the single source of truth.
- **The rule:** edge code reads the tunnel name from the **`Host` header** (`edge/src/lib/host.ts`), never from `request.url`.

### Email Service
- **Sending domain:** `tuzy.dev` is onboarded, which added the `cf-bounce` MX/SPF records, DKIM `cf-bounce`, and `_dmarc` with `p=reject`.
- **Recipients:** sending to arbitrary recipients needs an onboarded domain **and** Workers Paid.
- **Binding:** `send_email` is restricted to the sender `login@tuzy.dev`.
- **Quota:** 1,000 a day. 3,000 a month are included, then $0.35 per 1,000 (not a hard cap: `DAILY_SEND_BUDGET` is the ceiling).
- **Tested:** delivery to Gmail went to the Inbox, so DMARC alignment passes.

### TLS
- **Hosts:** `https://tuzy.dev` is the apex and `https://<name>.tuzy.dev` a tunnel.
- **Redirects and versions:** `http://` → 301 to https, and TLS below 1.2 is refused.
- **Two-level hosts** (`a.b.tuzy.dev`) fail TLS: Universal SSL covers one wildcard level, which matches the one-label name rule.

### Cloudflare account and repo checklist
- [x] Domain `tuzy.dev` at Porkbun (expires 2027-09-23, transfer lock on). Zone on Cloudflare, proxied `AAAA @/* 100::`.
- [ ] Porkbun auto-renew on. Extend to > 2 years before applying to the Public Suffix List.
- [x] Always Use HTTPS, minimum TLS 1.2, WebSockets on. Bot Fight Mode **off**.
- [ ] Browser Integrity Check / Security Level: they must not challenge webhooks or the API (turn them off, or add a WAF skip for `*.tuzy.dev`).
- [x] Workers Paid (startup credits). [ ] Usage and billing notifications at 50 %.
- [x] Email Service + DMARC. Email Routing: `abuse@`, `security@`, `dmarc@` → admin inbox.
- [x] GitHub `nhtera/tuzy` (public):
  - environments `production` (owner approval) and `release`;
  - a "release tags" ruleset;
  - secrets `CLOUDFLARE_API_TOKEN` (scoped deploy token), `CLOUDFLARE_ACCOUNT_ID`, `TAP_GITHUB_TOKEN` and `TUZY_RELEASE_KEY`.
- [x] `nhtera/homebrew-tap` (cask in `Casks/`) and `nhtera/scoop-bucket`.
- [ ] Search Console domain property; external uptime probe on `/api/v1/health`.
