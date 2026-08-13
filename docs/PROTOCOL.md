# Wingman wire protocol — v1

The protocol spoken between the Wingman app (phone) and `wingmand` (daemon).
Messages are carried inside a Noise XX end-to-end encrypted channel
(X25519, ChaCha20-Poly1305, SHA-256), either over the daemon's external LAN
listener or through the relay. The relay routes opaque ciphertext by
rendezvous room id and never sees these messages in plaintext. The loopback
listener on `127.0.0.1` carries the same messages without the Noise layer.

## Pairing

1. `wingmand pair` asks the running daemon for a payload
   `{ v, pub, lan, relay, room, token }` and renders it as a QR code. The
   token is single-use and expires after 10 minutes.
2. The phone connects (LAN address or relay room), performs the Noise XX
   handshake, and pins `pub` as the expected responder key.
3. An unpaired phone must send `pair.request { token, deviceName }` as its
   first message. On success the daemon registers the phone's static key in
   `~/.wingman/devices.json` and serves the connection immediately.
4. Paired devices skip step 3; the daemon authorizes them by their Noise
   static key.

If no paired device answers a permission request, it fails safe to deny after
a configurable timeout (default 5 minutes).

All messages are JSON objects with a common envelope:

```json
{
  "v": 1,
  "id": "b3f1c2d4",        // correlation id, set on commands; echoed on "res"
  "sessionId": "9a8b7c6d", // present on session-scoped messages
  "seq": 42,               // present on replayable events
  "type": "transcript.delta",
  "payload": { }
}
```

- `id` correlates a command with its `res` reply.
- `seq` is a per-session monotonically increasing sequence number assigned by
  the daemon's event log. Clients persist the last seen `seq` and resume with
  `session.watch { fromSeq }` after a reconnect; the daemon replays everything
  newer.

## Commands (phone → daemon)

| type | payload | reply data |
|---|---|---|
| `pair.request` | `{ token, deviceName }` | `{}` — only valid as the first message from an unpaired device |
| `session.list` | – | `{ sessions: [SessionInfo] }` |
| `session.create` | `{ cwd, prompt? }` | `SessionInfo` |
| `session.prompt` | `{ text }` | `{}` (progress arrives as events) |
| `session.approve` | `{ requestId, optionId }` | `{}` |
| `session.cancel` | – | `{}` |
| `session.watch` | `{ fromSeq }` | `{}` then event stream |
| `session.unwatch` | – | `{}` |
| `session.remove` | – | `{}` — only sessions in a terminal state (`done`, `error`) |
| `dirs.list` | – | `{ dirs: [string] }` — recent working directories, most recent first |

Every command receives exactly one reply:

```json
{ "v": 1, "id": "<echoed>", "type": "res", "payload": { "ok": true, "data": { } } }
{ "v": 1, "id": "<echoed>", "type": "res", "payload": { "ok": false, "error": "..." } }
```

## Events (daemon → phone, seq-numbered, replayable)

| type | payload |
|---|---|
| `session.state` | `{ status }` — `starting · idle · running · awaiting_permission · done · error` |
| `transcript.delta` | `{ kind, data }` — `kind` mirrors ACP `sessionUpdate` (`agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `plan`, …); `data` is the raw ACP update object |
| `permission.request` | `{ requestId, title, toolCall, options: [{ optionId, name, kind }] }` |
| `permission.resolved` | `{ requestId, optionId, resolvedBy }` — `phone · timeout · cancel` |
| `turn.ended` | `{ stopReason }` |

### Permission flow

1. Copilot CLI issues an ACP `session/request_permission`; the daemon holds the
   JSON-RPC call open.
2. Daemon appends `permission.request`, sets state `awaiting_permission`, and
   (Phase 4) triggers a push notification.
3. Phone answers with `session.approve { requestId, optionId }`, where
   `optionId` is one of the offered options (`allow_once`, `allow_always`,
   `reject_once`, … as provided by the CLI).
4. If no answer arrives within the timeout (default 5 min), the daemon replies
   `cancelled` to the CLI — fail-safe deny — and appends `permission.resolved`
   with `resolvedBy: "timeout"`.

## Session identity

Session ids in this protocol are daemon-generated and stable across daemon
restarts (Phase 2+, backed by `session/load`). The underlying ACP session id is
an implementation detail and never leaves the daemon.

## Relay rendezvous

The relay never speaks the message protocol above — it pipes opaque
WebSocket frames between exactly one host (daemon) and one client (phone)
per room. Its own surface is two WebSocket endpoints:

| endpoint | who connects | query params |
|---|---|---|
| `/v1/host` | daemon | `room`, `token?` |
| `/v1/join` | phone | `room`, `token?` |

- `room` is derived from the daemon's static public key:
  `base64url(sha256(pub)[:12])`, so it is stable across restarts and both
  ends compute it independently from the pairing payload.
- `token`, when the relay is started with one, is a bearer token compared in
  constant time; it is distributed to phones inside the QR pairing payload
  (`relayToken`).
- A host connection parks until a client joins; a reconnecting host replaces
  any stale one for the same room. Joining claims the room (one client at a
  time). When either side disconnects, the relay tears the pair down and the
  daemon redials.
- The relay pings both parked and in-session connections to keep proxy
  connections alive; see ADR-0004 for why an unanswered pong is not treated
  as a dead connection.

## Versioning

`v` is bumped only on breaking changes. Unknown message types and unknown
payload fields MUST be ignored by both ends.
