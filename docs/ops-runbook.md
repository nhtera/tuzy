# Ops runbook

## Deploys

**Edge:** tag `edge-vX.Y.Z` (or run *deploy-edge* manually). The `production` environment needs
approval. The job then:
1. runs the edge tests;
2. records a D1 Time Travel bookmark in the job summary;
3. applies migrations;
4. deploys;
5. smoke-tests `/api/v1/health`, `/install.sh` and the offline page.

Every edge deploy reconnects every agent once (GOAWAY / 1012 → U(0, 3 s) jitter).

**CLI:** tag `vX.Y.Z` from `main`.
- `release-cli` first checks that prod's `/api/v1/health` protocol range includes the CLI's
  protocol, then runs GoReleaser.
- GoReleaser uploads:
  - the archives, `checksums.txt` and `checksums.txt.sig` (ed25519, `TUZY_RELEASE_KEY`);
  - cosign files (`checksums.txt.cosign.sig` and `.pem`);
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
