# Tuzy

[![ci](https://github.com/nhtera/tuzy/actions/workflows/ci.yml/badge.svg)](https://github.com/nhtera/tuzy/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

`tuzy http 3000` exposes localhost at `https://<name>.tuzy.dev`, and the URL never changes between runs.

Free tunnels for webhooks, demos and mobile testing. Your subdomain is yours (up to 10 per account),
so a webhook URL you register once keeps working across restarts, laptops and CI.

> **Status:** pre-release. The edge runs at [tuzy.dev](https://tuzy.dev) (`edge-v1.0.0`); CLI release
> candidates are on [GitHub Releases](https://github.com/nhtera/tuzy/releases). `v1.0.0` follows the
> pre-launch load test.

## Install

| Platform | Command |
|---|---|
| macOS / Linux | `curl -fsSL https://tuzy.dev/install.sh \| sh` |
| macOS (Homebrew) | `brew install nhtera/tap/tuzy` *(from v1.0.0)* |
| Windows (Scoop) | `scoop bucket add nhtera https://github.com/nhtera/scoop-bucket` then `scoop install tuzy` *(from v1.0.0)* |
| Go | `go install github.com/nhtera/tuzy/cmd/tuzy@latest` |

The installer checks the release's ed25519 signature (when `openssl` can) and sha256 before installing to
`~/.local/bin`. Every `tuzy update` verifies the signature. See [getting started](docs/getting-started.md)
for the full trust story.

## Quickstart

```sh
tuzy login                  # email code, no password; token kept in the OS keychain
tuzy http 3000              # → https://<your-name>.tuzy.dev  (first run: pick a permanent name)
open http://127.0.0.1:4040  # inspect, replay and copy-as-curl every request
```

```sh
tuzy http 3000 --name shop                            # a specific name (claimed on first use)
tuzy http https://localhost:8443 --upstream-insecure  # self-signed local HTTPS
tuzy http file://./public                             # serve a folder read-only (dotfiles hidden)
tuzy start --all                                      # every [tunnels.*] in ./tuzy.toml
tuzy names ls | add | rename | default | rm           # manage your names
tuzy tokens create --scope connect --expires 90d      # CI token → TUZY_TOKEN
tuzy service install && tuzy service start            # run tuzy.toml's tunnels at login
tuzy diagnose                                         # proxy, DNS, TLS, WebSocket, token, clock checks
tuzy update                                           # verified self-update
```

Also: structured logs (`--log`, `--log-format term|logfmt|json`), `tuzy config check|edit|path|add-token -`,
shell completion (`tuzy completion <shell>`), and the `tuzy-skip-warning: 1` request header for automation.

## Documentation

- [Getting started](docs/getting-started.md): install, login, first tunnel, `tuzy.toml`, service, CI
- [Webhooks and the inspector](docs/webhooks-and-inspector.md)
- [Limits and FAQ](docs/limits-and-faq.md): quotas, `Invalid Host header`, the browser warning page, proxies
- [CLI reference](docs/cli-reference.md): generated from `--help`
- [Wire protocol v1](protocol/PROTOCOL.md): the normative agent ↔ edge protocol, golden vectors in `protocol/testdata/frames.json`
- [Ops runbook](docs/ops-runbook.md): deploys, releases, admin, abuse response, load test, platform notes
- [Acceptable use](https://tuzy.dev/aup) · [Terms](https://tuzy.dev/terms) · [Privacy](https://tuzy.dev/privacy) · [Report abuse](https://tuzy.dev/abuse)

## How it works

- **Edge** (`edge/`): a Cloudflare Worker routes `<name>.tuzy.dev` by the `Host` header to one Durable
  Object per name. The DO holds the agent's hibernatable WebSocket and multiplexes visitor HTTP streams
  and WebSockets over it with per-stream flow control. D1 stores accounts, names and audit data; KV
  caches "connected" markers.
- **CLI** (repo-root Go module `github.com/nhtera/tuzy`, entry `cmd/tuzy`): the tunnel agent, name and
  token management, the local inspector, and the file server / HTTPS upstreams.
- **Safety:** email-OTP accounts, a browser warning page on new accounts' tunnels, abuse reports with
  auto-quarantine, admin tooling, and per-tunnel and per-account limits.

## Layout

```
cmd/tuzy/            CLI entry
internal/            Go packages: tunnel, cli, inspector, upstream, fileserver, update, service, …
protocol/            PROTOCOL.md + golden frame vectors
edge/                Cloudflare Worker: wrangler.jsonc, src/, migrations/, test/
e2e/                 end-to-end suite (build tag e2e) + storm load driver
scripts/             install.sh (served at tuzy.dev/install.sh) + its test, data scripts
tools/               release-sign (release checksums), gen-cli-reference
docs/                user and ops documentation
.github/workflows/   ci.yml, deploy.yml (edge), release.yml (CLI)
```

## Development

Requirements: Go ≥ 1.26 (toolchain pinned in `go.mod`), Node ≥ 22.12.

```sh
# CLI
go run ./cmd/tuzy version
go vet ./... && go test -race ./... && golangci-lint run

# Edge
cd edge && npm ci
npm run dev -- --port 8788    # wrangler dev (see "Local tunnel hosts" below)
npx tsc --noEmit && npx vitest run

# End-to-end: wrangler dev + a sample app + the real CLI
go test -tags e2e -count=1 ./e2e/...

# Release dry run (artifacts in dist/)
goreleaser release --snapshot --clean --skip=sign && sh scripts/install_test.sh
```

**Local tunnel hosts** use `*.localhost`, which resolves to 127.0.0.1 without DNS setup. `npm run dev`
generates `edge/wrangler.dev.gen.jsonc` (no `routes`, `BASE_DOMAIN=localhost`) because `wrangler dev`
rewrites `Host` when routes exist:

```sh
curl http://shop.localhost:8788/            # routed as tunnel "shop"
curl http://localhost:8788/api/v1/health    # apex / API
```

After changing CLI commands, regenerate the reference with
`go run ./tools/gen-cli-reference > docs/cli-reference.md`. CI fails if it is stale.

## Releasing

- **Edge:** push a tag `edge-vX.Y.Z`. `deploy.yml` waits for approval in the `production` environment,
  then records a D1 restore point, applies migrations, deploys and smoke-tests.
- **CLI:** push a tag `vX.Y.Z`. `release.yml` checks that prod speaks the CLI's protocol, runs
  GoReleaser (archives, `checksums.txt`, its ed25519 `.sig`, a cosign bundle, the Homebrew cask and
  the Scoop manifest), then verifies the published signature against the key embedded in the CLI.
  `-rc.N` tags are prereleases: `tuzy update` and the taps ignore them.
- A CLI tag never deploys the edge, and an edge tag never publishes a CLI. Details:
  [ops runbook](docs/ops-runbook.md).

## Contributing and security

Issues and pull requests are welcome. Report security problems to security@tuzy.dev (see
[security.txt](https://tuzy.dev/.well-known/security.txt)), not in public issues.

## License

Apache-2.0, see [LICENSE](LICENSE).
