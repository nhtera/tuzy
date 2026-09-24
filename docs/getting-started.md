# Getting started

tuzy gives a service on your machine a permanent public URL: `https://<name>.tuzy.dev`.

## 1. Install

| Platform | Command |
|---|---|
| macOS (Homebrew) | `brew install nhtera/tap/tuzy` (macOS only; on Linux use the script) |
| macOS / Linux | `curl -fsSL https://tuzy.dev/install.sh \| sh` |
| Windows (Scoop) | `scoop bucket add nhtera https://github.com/nhtera/scoop-bucket` then `scoop install tuzy` |
| Anywhere with Go | `go install github.com/nhtera/tuzy/cmd/tuzy@latest` |

`install.sh` installs to `~/.local/bin`. Set `TUZY_INSTALL_DIR` to install somewhere else, or
`TUZY_VERSION=v1.2.3` to pin a version.

**What the installer trusts:**
- `curl | sh` trusts TLS to `tuzy.dev` (the script) and `github.com` (the release).
- The script always checks the archive's sha256.
- It also checks the tuzy release key's signature when your `openssl` supports ed25519.
- Every `tuzy update` verifies that signature; `tuzy update --check` afterwards confirms the
  chain.
- To verify by hand: `cosign verify-blob` (see the release notes).

A browser-downloaded archive on macOS is quarantined by Gatekeeper; use brew or the script.

## 2. Log in

```sh
tuzy login
```

Enter your email and the 6-digit code tuzy sends you. There is no password. Your token is stored
in the OS keychain.

## 3. Share a local app

```sh
tuzy http 3000
```

The first time, you pick a permanent name (or accept a suggestion). Later runs reuse it:

```
● online https://brave-otter-42.tuzy.dev → http://localhost:3000
inspector: http://127.0.0.1:4040
```

Other ways to run a tunnel:

- **Use a specific name:** `tuzy http 3000 --name shop`. You can have up to 10 names; manage
  them with `tuzy names`.
- **HTTPS local app:** `tuzy http https://localhost:8443`. Add `--upstream-insecure` for a
  self-signed certificate.
- **Static folder:** `tuzy http file://./public` serves it read-only, with dotfiles hidden.

## 4. Several tunnels from a project file

```toml
# tuzy.toml
[tunnels.web]
addr = "3000"

[tunnels.api]
addr = "8080"
host_header = "auto"      # default: public Host, or localhost:8080 if the app rejects it (Vite, Rails)
```

```sh
tuzy start --all
```

## 5. Keep it running

```sh
tuzy service install && tuzy service start
```

This runs `tuzy start --all` at login, with launchd on macOS, `systemd --user` on Linux and a
logon task on Windows. `tuzy service status` shows where it logs.

## 6. CI

Create a token that can only open tunnels, and use it as `TUZY_TOKEN`:

```sh
tuzy tokens create --scope connect --label ci --expires 90d
```

## When something doesn't work

Run `tuzy diagnose`. It checks your proxy, DNS, TLS (including TLS interception), the API, your
clock, WebSockets and your token, and tells you what to change. Also see
[Limits and FAQ](limits-and-faq.md).
