# ADR-0002: Sequence-numbered session event log with client-driven replay

- **Status:** Accepted — in production since Phase 1
- **Date:** 2026-08-12 (records a decision made during Phase 1 design)

## Context

Phones disconnect constantly: radios drop, iOS suspends backgrounded apps,
users walk between networks. A remote observer must never miss a transcript
event or — worse — a permission request, and more than one client may watch
the same session. The daemon should not have to track per-device delivery
state to make any of that true.

## Options considered

1. **Stateless fan-out.** Broadcast events to whoever is connected.
   Disconnected clients silently miss events; a permission request can
   vanish into a dead socket. Unacceptable for the approval path.
2. **Server-tracked cursors.** The daemon remembers what each device has
   acknowledged. Reliable, but it accumulates per-client delivery state and
   every reconnect needs a reconciliation handshake.
3. **Append-only per-session event log; clients own their cursor.** The
   daemon assigns every event a per-session, monotonically increasing `seq`.
   A client persists the last `seq` it applied and resumes with
   `session.watch { fromSeq }`; the daemon replays everything newer.

## Decision

Option 3. Events (`session.state`, `transcript.delta`,
`permission.request`, `permission.resolved`, `turn.ended`) are appended to a
per-session log and carry `seq` in the protocol envelope. Replay is the
client's responsibility to request and the daemon's to serve; the daemon
keeps no per-device state.

## Consequences

- At-least-once delivery with trivial resume after any disconnect, and any
  number of concurrent watchers for free.
- The log doubles as the session's source of truth: replaying it
  reconstructs client state, and daemon-generated session ids stay stable
  across daemon restarts (backed by ACP `session/load`).
- Clients must apply events idempotently and dedupe by `seq`.
- Log growth eventually needs a retention policy (truncation after
  `turn.ended`, size caps). Deferred: sessions are short-lived today.
