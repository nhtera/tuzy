# Webhooks and the inspector

## Stable webhook URLs

Your name is permanent, so a webhook registered at `https://shop.tuzy.dev/webhooks/stripe` keeps
working across restarts, laptops and CI runs. Renaming or removing a name holds the old one for
you for 12 months. A renamed tunnel's agent follows the new name automatically, and senders get
`410 Gone` with a hint.

Webhooks, `curl` and API clients reach your app directly. The browser warning page that new
accounts' tunnels show applies only to browser page navigations.

## The inspector

Every `tuzy http` / `tuzy start` serves a local UI at `http://127.0.0.1:4040`. If that port is
busy it uses 4041–4049; pick your own with `--inspect-addr`, or turn it off with
`--no-inspect`.

- **Live list:** filter by tunnel, method and status.
- **Detail view:** headers and body as pretty JSON, form, text or hex. gzip, deflate and br
  bodies are decoded for display.
- **Replay:** sends the captured request to your local app again (`r`). Signed webhooks such as
  Stripe may reject a replay once their timestamp tolerance has passed.
- **Edit & Replay:** change the method, path, headers or body first.
- **Copy as curl:** a POSIX shell (bash/zsh) command. Text bodies use `--data-raw`; binary
  bodies are piped in.
- **Keyboard:** ↑/↓ select, `r` replays.

**Privacy:** captured data never leaves your machine and is not written to disk. The inspector
keeps up to 500 requests, 1 MiB per body side and 64 MiB in total. It has no password: anything
running on your machine can open it. Use `--no-inspect` on shared hosts.
