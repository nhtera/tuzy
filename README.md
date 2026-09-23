# Tuzy

`tuzy http 3000` exposes localhost at `https://<name>.tuzy.dev`, and the URL never changes between runs.

- **Edge** (`edge/`): Cloudflare Worker + one Durable Object per tunnel name (TypeScript, Hono).
- **CLI** (repo root Go module `github.com/nhtera/tuzy`, entry `cmd/tuzy`): tunnel agent, name management, local inspector.
- **Protocol** (`protocol/PROTOCOL.md`): the normative agent ↔ edge wire protocol v1, with golden vectors in `protocol/testdata/frames.json`.

> Status: phase 1 skeleton. See `Plans/` (local only) for the roadmap.

## Layout

```
cmd/tuzy/           CLI entry
internal/           Go packages (cli, …)
protocol/           PROTOCOL.md + testdata/frames.json
edge/               Cloudflare Worker (wrangler.jsonc, src/, test/)
scripts/            install / data scripts (later phases)
.github/workflows/  CI
```

## Development

Requirements: Go ≥ 1.26 (toolchain pinned in `go.mod`), Node ≥ 22.12.

```sh
# CLI
go run ./cmd/tuzy version
go vet ./... && go test -race ./... && golangci-lint run

# Edge
cd edge
npm ci
npm run dev                  # wrangler dev on :8787 (use `npm run dev -- --port 8788` if 8787 is busy)
npx tsc --noEmit && npm run test:unit
```

Local tunnel hosts use `*.localhost`, which resolves to 127.0.0.1 without any DNS setup:

```sh
curl http://shop.localhost:8787/        # routed as tunnel "shop"
curl http://localhost:8787/api/v1/...   # apex / API
```

Deterministic dev ports: edge `8787`, sample app `3000`.

## Ops notes

### Wrangler config
- One top-level `edge/wrangler.jsonc`, **no named environments** (no staging: local `wrangler dev` + production only).
- Worker name `tuzy`; routes `tuzy.dev/*` and `*.tuzy.dev/*` (zone `tuzy.dev`).
- `compatibility_date` is capped by the workerd bundled with `@cloudflare/vitest-pool-workers` (currently `2026-08-22`). Raise it only after upgrading the pool.

### GATE B: Host routing under `wrangler dev` (resolved 2026-09-24)
- **Finding:** whenever `routes` are in the config, `wrangler dev` (4.137) rewrites both the request URL and the `Host` header to the first route's zone (`tuzy.dev`). `--local-upstream`, `--host` and `--routes` all rewrite to a single fixed host, so Host-based tunnel routing is impossible. Without `routes`, `Host` passes through unchanged.
- **Workaround:** `npm run dev` runs `scripts/gen-dev-config.mjs`, which writes a gitignored `edge/wrangler.dev.gen.jsonc`: the same config minus `routes`, with `BASE_DOMAIN="localhost"`. Then it runs `wrangler dev -c wrangler.dev.gen.jsonc`. `wrangler.jsonc` stays the single source of truth, and bare `wrangler deploy` stays correct.
- **Rule:** edge code reads the tunnel name from the **`Host` header** (`edge/src/lib/host.ts`), never from `request.url`.
- Verified: `curl -H 'Host: shop.localhost:8788' http://127.0.0.1:8788/` and `curl http://shop.localhost:8788/` both reach the Worker as tunnel `shop`.

### GATE A: Email Service to arbitrary recipients (PASSED 2026-09-24)
- **Sending domain:** `tuzy.dev` onboarded via API (`POST /zones/{zone}/email/sending/subdomains {name:"tuzy.dev"}`), status `ready`. Onboarding added `cf-bounce.tuzy.dev` MX ×3 + SPF, DKIM selector `cf-bounce`, and `_dmarc`.
- **Arbitrary recipients:** allowed once a sending domain is onboarded **and** the account is on **Workers Paid** (Workers Free cannot send to non-verified addresses). Sends to verified destination addresses are free on every plan.
- **`wrangler.jsonc` block** (phase 4), restricted to our sender:
  ```jsonc
  "send_email": [{ "name": "EMAIL", "allowed_sender_addresses": ["login@tuzy.dev"] }]
  ```
  With no `destination_address` / `allowed_destination_addresses`, the binding has no recipient restriction. Caveat: the send-bindings doc still says an unrestricted binding reaches "any verified destination". The REST path is proven; if the binding turns out restricted in phase 4, use the REST API (`POST /accounts/{id}/email/sending/send`) behind `lib/email.ts`.
- **Daily quota:** 1,000/day for this account (`GET /accounts/{id}/email/sending/limits`). New accounts scale up automatically with good reputation.
- **Monthly / overage:** 3,000 included per month, then **billed** at $0.35 per 1,000. Not a hard cap, so the phase 4 global send budget is the only ceiling.
- **Live test:** `POST /accounts/{id}/email/sending/send` from `login@tuzy.dev` to a non-verified Gmail plus-address → `queued`, daily usage 1. On Workers Free this is rejected, so the account has paid Email Sending.
- **Result:** delivered to the Gmail **Inbox** (not spam). With `p=reject` published, inbox delivery implies DMARC alignment passed. The Worker `send_email` binding path is re-tested in phase 4.

### Cloudflare account checklist (phase 1)
- [x] Domain `tuzy.dev`, registered at **Porkbun** (not Cloudflare Registrar), expires 2027-09-23; transfer lock on (`clientTransferProhibited`)
- [ ] Porkbun: auto-renew on. Extend to > 2 years remaining before the PSL application (phase 9)
- [x] Zone on Cloudflare (Free Website plan), nameservers `andy`/`lucy.ns.cloudflare.com`
- [x] DNS (proxied): `AAAA @ 100::`, `AAAA * 100::`
- [x] SSL/TLS: Always Use HTTPS on, minimum TLS 1.2 (SSL mode `full`, WebSockets on)
- [x] Bot Fight Mode **off** (breaks webhooks)
- [x] Paid Email Sending active (implies Workers Paid). Confirm usage/billing notifications in the dashboard
- [x] Email Service: `tuzy.dev` onboarded (`cf-bounce` MX/SPF, DKIM `cf-bounce`), status `ready`
- [x] DMARC: `_dmarc TXT "v=DMARC1; p=reject; rua=mailto:dmarc@tuzy.dev"` (kept the onboarding's stricter `p=reject` rather than the plan's `quarantine`)
- [x] Email Routing enabled (apex MX + SPF, DKIM `cf2024-1`): `abuse@`, `security@`, `dmarc@` → admin inbox (verified destination)
- [ ] CI API token (Workers Scripts Edit, Workers Routes Edit, D1 Edit, KV Edit, Account Read) → GitHub secrets `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID` (needed from phase 9 deploys)

## License

TBD
