# Wingman wire protocol — v1

The protocol spoken between the Wingman app (phone) and `wingmand` (daemon).
In Phase 1 it runs over a loopback WebSocket; from Phase 2 the same messages are
carried inside a Noise XX end-to-end encrypted channel, either via the relay or
a direct LAN connection. The relay never sees these messages in plaintext.

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
| `session.list` | – | `{ sessions: [SessionInfo] }` |
| `session.create` | `{ cwd, prompt? }` | `SessionInfo` |
| `session.prompt` | `{ text }` | `{}` (progress arrives as events) |
| `session.approve` | `{ requestId, optionId }` | `{}` |
| `session.cancel` | – | `{}` |
| `session.watch` | `{ fromSeq }` | `{}` then event stream |
| `session.unwatch` | – | `{}` |

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

## Versioning

`v` is bumped only on breaking changes. Unknown message types and unknown
payload fields MUST be ignored by both ends.
