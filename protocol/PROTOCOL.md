# Tuzy wire protocol v1 (normative)

This document is the single source of truth for the agent ↔ edge tunnel protocol. The edge
(`edge/src/protocol/*`, TypeScript) and the agent (`internal/protocol`, Go) must both pass the
golden vectors in [`testdata/frames.json`](testdata/frames.json).

The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## 1. Transport

- The agent (`tuzy` CLI) opens **one WebSocket** to the edge:
  `wss://<BASE_DOMAIN>/api/v1/connect?name=<name>&instance=<instance_id>[&force=1]` with
  `Authorization: Bearer <token>`. `instance` lets the edge apply the replace rules (§3.2) before
  the upgrade, so a 409 is a plain HTTP response. `instance` MUST be 16–64 characters of
  `[A-Za-z0-9_-]` (the agent uses 128 random bits, base64url), else HTTP 400.
  The edge terminates it in the tunnel's Durable Object (`TunnelObject`, one per name).
- HTTP-level connect rejections (400/401/403/404/409/410/429/5xx) happen **before** the upgrade and
  are defined by the API (phases 2, 4, 5). There is no HTTP 426 path; version mismatch is signalled
  in-band (GOAWAY `upgrade_required`, §6).
- **One frame per WebSocket binary message.** WebSocket already delimits messages, so frames carry
  no length prefix.
- The only text messages allowed on the agent socket are the heartbeat strings (§7). Any other
  text message → the receiver closes the socket with code **1003**.

## 2. Frame format

```
frame := type:u8 | stream_id:u32 (big-endian) | payload:bytes
```

- Header is exactly 5 bytes. A binary message shorter than 5 bytes is malformed.
- `payload` is everything after the header (may be empty).
- Multi-byte integers are unsigned big-endian.
- JSON payloads are UTF-8 JSON objects. Receivers MUST ignore unknown JSON fields (forward
  compatibility). Key order and whitespace are not significant.

### 2.1 Frame types

| Type | Hex | Dir | Stream | Payload | Meaning |
|---|---|---|---|---|---|
| HELLO | 0x01 | A→E | 0 | JSON `{proto:1, client:"tuzy/1.0.0 darwin/arm64", instance_id}` | First frame from the agent. `instance_id` = random per CLI process, stable across its reconnects |
| READY | 0x02 | E→A | 0 | JSON `{name, url, epoch}` | Accepted; tunnel live |
| GOAWAY | 0x03 | E→A | 0 | JSON `{reason, new_name?, message?}` | Edge will close; agent acts on `reason` (§6) |
| DRAIN | 0x04 | A→E | 0 | empty | Agent shutting down: no new REQ_HEAD, in-flight streams finish |
| REQ_HEAD | 0x10 | E→A | ≥1 | JSON `{method, path, headers:[[k,v]…], remote_ip, kind:"http"\|"ws"}` | New visitor request = new stream |
| REQ_BODY | 0x11 | E→A | ≥1 | raw bytes (≤ 64 KiB) | Request body chunk |
| REQ_END | 0x12 | E→A | ≥1 | empty | Request body done |
| RES_HEAD | 0x20 | A→E | ≥1 | JSON `{status, headers:[[k,v]…]}` | Response start (101 accepts a ws stream) |
| RES_BODY | 0x21 | A→E | ≥1 | raw bytes (≤ 64 KiB) | Response chunk |
| RES_END | 0x22 | A→E | ≥1 | empty | Response done |
| WS_TEXT | 0x30 | both | ≥1 | UTF-8 (≤ 1 MiB) | Visitor-WS text message |
| WS_BINARY | 0x31 | both | ≥1 | bytes (≤ 1 MiB) | Visitor-WS binary message |
| WS_CLOSE | 0x32 | both | ≥1 | JSON `{code, reason}` | Visitor-WS closed |
| WINDOW | 0x40 | both | any | u32 credit bytes | Flow control. `stream_id=0` = connection credit, else stream credit |
| RESET | 0x41 | both | ≥1 | JSON `{code, message}` | Abort stream |
| ACK | 0x42 | both | ≥1 | u32 message count | WS message credit (per-message ack) |

### 2.2 Limits and validation

| Constant | Value |
|---|---|
| `MAX_BODY_CHUNK` (REQ_BODY / RES_BODY payload) | 65 536 bytes (64 KiB) |
| `MAX_WS_MESSAGE` (WS_TEXT / WS_BINARY payload) | 1 048 576 bytes (1 MiB) |
| `MAX_JSON_PAYLOAD` (any JSON frame) | 131 072 bytes (128 KiB) |
| `STREAM_WINDOW` (initial, each direction) | 1 048 576 bytes (1 MiB) |
| `CONN_WINDOW` (initial, each direction) | 8 388 608 bytes (8 MiB) |
| `MAX_WINDOW` (credit ceiling after increments) | 2 147 483 647 bytes (2³¹−1) |
| `WS_MSG_CREDIT` (unacked messages per stream per direction) | 8 |
| `WS_MSG_TOLERANCE` (agent-side outstanding limit, §5.3) | 16 |
| `MAX_STREAMS` (concurrent per connection) | 128 |
| `CREDIT_TIMEOUT` | 30 s |
| `HEAD_TIMEOUT` (REQ_HEAD → RES_HEAD) | 60 s |
| `MAX_STREAM_SECONDS` (HTTP stream lifetime) | 3 600 s (1 h) |

A frame is **malformed** when any of these hold. On a malformed frame the receiver closes the
agent socket with code **1002** (protocol error):

- message shorter than 5 bytes, or an unknown `type`;
- a stream-0-only type (HELLO, READY, GOAWAY, DRAIN) with `stream_id ≠ 0`;
- a stream type (everything except WINDOW and the four above) with `stream_id = 0`;
- WINDOW or ACK payload not exactly 4 bytes, or a value of 0;
- ACK on stream 0;
- DRAIN, REQ_END or RES_END with a non-empty payload;
- a JSON frame whose payload is not a JSON object or exceeds `MAX_JSON_PAYLOAD`;
- REQ_BODY / RES_BODY larger than `MAX_BODY_CHUNK`;
- a HELLO / READY / GOAWAY payload missing a required field or with a wrong field type.

**Stream-level errors** affect only one stream; the tunnel stays up:

- REQ_HEAD / RES_HEAD / WS_CLOSE / RESET with a missing or wrong-typed required field → the
  receiver sends RESET `protocol_error` for that stream (a bad RESET just drops the stream);
- WS_TEXT that is not valid UTF-8 → close **that visitor WebSocket** with **1007**
  (WS_CLOSE `{code:1007}`);
- WS_TEXT / WS_BINARY larger than `MAX_WS_MESSAGE` → close that visitor WebSocket with **1009**.

## 3. Connection lifecycle

```
agent                                   edge (TunnelObject)
  │── WSS upgrade /api/v1/connect ───────▶│  auth, ownership, replace rules (§3.2)
  │◀──────────────── 101 ────────────────│  new epoch E
  │── HELLO {proto:1, client, instance_id}▶│  (within 10 s, else close 1002)
  │◀──────── READY {name, url, epoch:E} ──│  tunnel live; windows = initial values
  │            … streams …                │
```

- The agent MUST send HELLO as its first message. The edge MUST NOT send any frame except
  GOAWAY before READY.
- `proto ≠ 1` → edge sends GOAWAY `upgrade_required` and closes 1000.
- **Connection epoch:** a per-name counter persisted by the edge, incremented for every accepted
  agent socket and returned in READY. Streams belong to the epoch that opened them; events from a
  socket whose epoch is not current only clean up that socket's own streams.

### 3.1 Streams

- Stream IDs are **allocated by the edge only**, uint32, starting at 1, strictly increasing and
  **never reused within a connection** (the edge persists its high-water mark, so this holds
  across Durable Object hibernation). Stream 0 = control.
- **REQ_HEAD** MUST carry an id greater than every id the agent has seen on this connection. The
  agent tracks that high-water mark; a REQ_HEAD at or below it (duplicate or reused id) is a
  connection error → close **1002**.
- Any other frame for an unknown or already-closed stream id → drop it silently (but see §4.3 for
  credit).
- More than `MAX_STREAMS` concurrent streams: the edge does not open the stream and answers the
  visitor 503. A visitor request whose REQ_HEAD would exceed `MAX_JSON_PAYLOAD` is answered 431
  without opening a stream.
- Stream-id exhaustion (2³²−1 reached): the edge sends GOAWAY `restart` and closes; the agent
  reconnects on a fresh connection. (Unreachable in practice.)

**HTTP stream (`kind:"http"`):** `REQ_HEAD → REQ_BODY* → REQ_END` (edge→agent) and
`RES_HEAD → RES_BODY* → RES_END` (agent→edge). The stream is closed when both directions have
ended, or on RESET from either side.

**WebSocket stream (`kind:"ws"`):** `REQ_HEAD` only (no REQ_BODY/REQ_END). The agent dials the
local target and answers:
- `RES_HEAD {status:101}` → visitor WS accepted; then WS_TEXT / WS_BINARY / ACK in both
  directions; either side ends with WS_CLOSE and the peer answers WS_CLOSE. Closed when both sides
  have sent WS_CLOSE, or on RESET.
- any other status → `RES_HEAD`, `RES_BODY*`, `RES_END` exactly like HTTP; the edge returns that
  response to the visitor. The stream is closed at RES_END (a ws stream has no request body).

### 3.2 Replace rules (connect time, before the upgrade)

When a name already has a current agent socket, the edge decides using the `instance` query
parameter:
1. same `instance_id` as the current socket → replace it silently (same process reconnecting);
2. current socket dead: its auto-response (pong) timestamp is older than **45 s**, or there is no
   timestamp yet and the socket connected more than 45 s ago → replace;
3. current socket has sent DRAIN → replace;
4. `force=1` → send GOAWAY `replaced` to the current socket, then replace;
5. otherwise → HTTP **409**.

HELLO's `instance_id` MUST equal the `instance` query parameter, else the edge closes 1002.

```
Replace: laptop wakes after sleep; old socket is half-open at the edge
agent (instance I)                      edge
  │── connect?name=shop&instance=I ────▶│  current socket also has instance I → rule 1
  │                                     │  old socket closed (no GOAWAY: same process)
  │◀────────────── 101 ─────────────────│  epoch E+1
  │── HELLO {instance_id:I} ───────────▶│
  │◀──────────── READY {epoch:E+1} ─────│  streams of epoch E are cleaned up only by E's events

Other device (instance J) while I is alive and pinging:
  │── connect?name=shop&instance=J ────▶│  rule 5 → HTTP 409 ("rerun with --force")
  │── …&instance=J&force=1 ────────────▶│  rule 4 → GOAWAY{reason:"replaced"} to I, then accept J
```

## 4. HTTP flow control

### 4.1 Credits
- Two windows per direction: **stream** (initial `STREAM_WINDOW`) and **connection** (initial
  `CONN_WINDOW`). Windows start at READY (stream windows at REQ_HEAD); no WINDOW frame is needed
  to establish them.
- Only REQ_BODY / RES_BODY **payload bytes** consume credit. Head, end, WS, RESET and control
  frames are free.
- A sender MUST hold both stream and connection credit ≥ the chunk size before sending it.
- WINDOW increments are additive. A window that would exceed `MAX_WINDOW` → close 1002.
- The receiver sends WINDOW (stream id and stream 0) **as its consumer reads the data** (the local
  app for the agent, the visitor for the edge), never merely on arrival.

### 4.2 Receiver never blocks
- Inbound data goes into a per-stream queue bounded by the stream window. The socket reader never
  blocks on a slow consumer.
- Data beyond granted credit = peer protocol violation → RESET `flow_control` for that stream.

### 4.3 Early response and discarded data
- The edge pumps the request body **concurrently** with waiting for RES_HEAD.
- After RES_END the edge stops pumping and sends REQ_END.
- The agent discards late REQ_BODY for a finished stream but **still grants connection WINDOW**
  for the discarded bytes, so the connection window never leaks. Stream credit for a closed stream
  need not be returned.
- The same applies to any data dropped for a reset or unknown stream: return connection credit.

### 4.4 Credit timeout
A sender waiting more than `CREDIT_TIMEOUT` (30 s) for credit sends RESET `credit_timeout`. The
edge answers the visitor 504 if no RES_HEAD was sent yet. No RES_HEAD within `HEAD_TIMEOUT` (60 s)
→ the edge sends RESET `head_timeout` and answers 504. A stream older than `MAX_STREAM_SECONDS`
→ RESET `stream_timeout`.

```
Early response (e.g. 413 while a 20 MB upload is in flight)
edge                                     agent / local app
  │── REQ_HEAD POST /upload ────────────▶│
  │── REQ_BODY (64 KiB) … ──────────────▶│  app reads headers, rejects
  │◀──────────── RES_HEAD {status:413} ──│
  │◀──────────────────────── RES_END ────│
  │── REQ_END (pump stopped) ───────────▶│  late REQ_BODY discarded, WINDOW(0) still granted
```

## 5. WebSocket streams

### 5.1 Messages
- One visitor WS message = one WS_TEXT / WS_BINARY frame (≤ `MAX_WS_MESSAGE`, else close 1009).
- WS messages do not consume byte windows.

### 5.2 Message credit
- Each side may have at most `WS_MSG_CREDIT` (8) **unacked messages** per stream in flight.
- The receiver sends `ACK n` in the **same handler that delivers the message(s)** to the consumer
  (visitor socket or local WS). No byte credit is held across events.
- ACKs beyond the sender's outstanding count are ignored (the count never goes below 0).

### 5.3 Edge state across hibernation
- The edge keeps its per-stream outstanding (edge→agent) count in the **visitor socket's
  attachment**, updated on every send and ACK, so hibernation does not reset it.
- A visitor message that arrives while the edge has no credit is buffered in memory (at most
  `WS_MSG_CREDIT` messages per stream) and flushed on ACK. Buffer overflow → close that visitor WS
  with **1008**. The attachment records a pending flag; if the edge wakes with the flag set but an
  empty in-memory buffer (evicted), it closes that visitor WS with **1011**.
- The agent tolerates up to `WS_MSG_TOLERANCE` (16) outstanding edge→agent messages per stream as
  a safety margin before closing that visitor WS with **1008**.

### 5.4 Agent → visitor backpressure (known limitation)
The edge ACKs an agent→visitor message once it has called `send()` on the visitor socket. The
Workers WebSocket API exposes no send-buffer size, so a slow visitor can accumulate up to the
runtime's buffer in DO memory. Phase 2 spikes whether `bufferedAmount` (or an equivalent) is
available; if it is, the edge delays ACK while it exceeds 1 MiB.

```
Visitor WebSocket
visitor            edge                                   agent            local app
  │── Upgrade ────▶│── REQ_HEAD {kind:"ws", path} ───────▶│── dial ws ─────▶│
  │                │◀──────────── RES_HEAD {status:101} ──│◀── 101 ─────────│
  │◀── 101 ────────│                                      │                 │
  │── msg ────────▶│── WS_TEXT ──────────────────────────▶│── msg ─────────▶│
  │                │◀──────────────────────── ACK 1 ──────│ (same handler)  │
  │◀── msg ────────│◀──────────────────────── WS_BINARY ──│◀── msg ─────────│
  │                │── ACK 1 ────────────────────────────▶│                 │
  │── close 1000 ─▶│── WS_CLOSE {code:1000} ─────────────▶│── close ───────▶│
  │                │◀──────────── WS_CLOSE {code:1000} ───│   stream closed │
```

## 6. GOAWAY and DRAIN

GOAWAY `reason` values and required agent behaviour:

| reason | Agent action |
|---|---|
| `restart` | Reconnect (same `instance_id`) with backoff |
| `renamed` | Reconnect as `new_name`, print a notice |
| `replaced` | Exit 1: another device/process took the name |
| `deleted` | Exit 1: name removed |
| `suspended` | Exit 1 (+ abuse contact) |
| `revoked` | Exit 1: token revoked, run `tuzy login` |
| `upgrade_required` | Exit 1 with upgrade instructions |

Unknown reasons → treat as `restart`. After GOAWAY the edge closes the socket (1000).

**Reconnect policy:** unless the last GOAWAY was terminal (exit rows above), the agent reconnects
after **any** close or drop (same `instance_id`, full-jitter backoff 250 ms → 30 s). Cloudflare
deploys disconnect WebSockets without a GOAWAY (typically close **1012**); after 1012 the first
retry is uniform in 0–3 s to spread the reconnect storm.

```
GOAWAY (e.g. `tuzy names rename shop store` from another terminal)
agent                                   edge
  │◀── GOAWAY {reason:"renamed", new_name:"store"} ──│
  │◀──────────────── close 1000 ─────────────────────│  in-flight streams end with the socket
  │── connect?name=store&instance=I ────────────────▶│  reconnect as new_name, print notice
```

**DRAIN** (agent→edge, e.g. Ctrl-C): the edge stops sending REQ_HEAD on this connection (new
visitors get the "draining" 502 page) and lets in-flight streams finish. The agent closes with 1000
when in-flight streams are done or after a 5 s grace. A socket that has sent DRAIN is replaceable
(§3.2 rule 3), so a fresh `tuzy http` can take the name immediately.

```
DRAIN (Ctrl-C)
visitor        edge                                agent
  │            │◀────────────────────── DRAIN ─────│  stop accepting
  │── GET ────▶│  no REQ_HEAD sent; 502 "draining" page
  │            │── RES frames of in-flight stream ──│… finishes
  │            │◀──────────────────── close 1000 ───│  all done, or 5 s grace elapsed
```

## 7. Heartbeat

- The agent sends the **text** message `tuzy-ping` immediately after READY, then every 15 s.
- The edge auto-replies `tuzy-pong` via `setWebSocketAutoResponse` (does not wake the DO).
- No `tuzy-pong` within 10 s → the agent closes and reconnects (same `instance_id`).
- The edge treats an agent socket whose `getWebSocketAutoResponseTimestamp` is older than 45 s
  (or null while connected > 45 s) as dead (§3.2 rule 2).
- Known quirk: the auto-response applies to every hibernatable socket of the DO, so a **visitor**
  WebSocket text message that is exactly `tuzy-ping` is answered `tuzy-pong` by the edge and never
  reaches the local app. Accepted for v1.

```
agent                          edge runtime (auto-response)
  │── "tuzy-ping" ────────────▶│
  │◀──────────── "tuzy-pong" ──│   timestamp recorded; DO stays hibernated
```

## 8. RESET codes

| code | Sent by | Meaning |
|---|---|---|
| `protocol_error` | both | Stream-level protocol violation (duplicate REQ_HEAD, bad sequence) |
| `flow_control` | both | Data beyond granted credit |
| `credit_timeout` | both | Waited > 30 s for credit |
| `head_timeout` | edge | No RES_HEAD within `HEAD_TIMEOUT` (60 s) |
| `stream_timeout` | edge | Stream exceeded `MAX_STREAM_SECONDS` or the long-stream budget |
| `cancelled` | both | Visitor or local peer went away |
| `local_error` | agent | Local target failed mid-response |

Receivers MUST treat unknown codes as `cancelled`.

## 9. Close codes (agent socket)

| Code | Meaning |
|---|---|
| 1000 | Normal (after GOAWAY or DRAIN) |
| 1002 | Protocol error (malformed frame, §2.2) |
| 1003 | Unexpected text message |
| 1012 | Service restart (deploy): agent reconnects with jitter |

Visitor-WS close codes carried in WS_CLOSE: 1007 invalid UTF-8, 1008 credit/tolerance overflow,
1009 message too big, 1011 edge state lost after eviction.

## 10. Sequence: plain HTTP request

```
visitor ──GET /api?q=1──▶ edge
edge  ── REQ_HEAD {method:"GET", path:"/api?q=1", headers, remote_ip, kind:"http"} ──▶ agent
edge  ── REQ_END ──▶ agent                                  agent → local app
agent ── RES_HEAD {status:200, headers} ──▶ edge ──▶ visitor
agent ── RES_BODY … ──▶ edge  (edge WINDOWs as the visitor reads)
agent ── RES_END ──▶ edge
```

## 11. Golden vectors

`testdata/frames.json`:

```jsonc
{
  "version": 1,
  "valid":   [{ "name", "hex", "decoded": { "type", "type_code", "stream_id", "payload" } }],
  "invalid": [{ "name", "hex", "error" }]   // "malformed" (close 1002) | "stream_error" (RESET/visitor-WS close)
}
```

`payload` is one of: `{"kind":"empty"}`, `{"kind":"json","value":{…}}`,
`{"kind":"bytes","hex":"…"}`, `{"kind":"text","value":"…"}`, `{"kind":"u32","value":n}`.

Implementations MUST decode every `valid.hex` to its `decoded` form (JSON compared semantically),
re-encode every non-JSON `decoded` to the exact `hex`, and reject every `invalid.hex` with the
given error. Stream-level JSON schema errors and `MAX_WS_MESSAGE` (1009) are checked above the
codec and covered by unit tests; the `MAX_WS_MESSAGE` limit is a length check covered by unit tests, not by
vectors (a 1 MiB vector would bloat the file).
