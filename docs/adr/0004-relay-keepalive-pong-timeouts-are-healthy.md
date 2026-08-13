# ADR-0004: Treat pong timeouts on relay-parked hosts as healthy

- **Status:** Accepted
- **Date:** 2026-08-13 (records the decision behind the Phase 5 keepalive fix)

## Context

Between sessions, the daemon parks a WebSocket connection on the relay's
`/v1/host` endpoint and waits for a phone to claim the room. While parked,
the relay pings the host on an interval: the traffic keeps proxy
connections (Fly's edge) from idling out, and a failed ping is the relay's
only signal that a host died without a close handshake.

The original implementation tore the room down on *any* ping error,
including a pong timeout. That was wrong for a structural reason: with
`coder/websocket`, `Ping` only learns about the answering pong when a
concurrent `Read` is pumping the connection — and nothing reads a parked
host connection on the relay side. The pong can arrive on the wire, but no
reader ever surfaces it, so every ping ends in `context.DeadlineExceeded`
and every healthy parked host was reaped on the first keepalive cycle,
forcing the daemon into a permanent redial loop.

## Options considered

1. **Run a `Read` loop on parked connections just to surface pongs.** Makes
   pong confirmation meaningful, but a parked host must not be read from:
   the first bytes belong to the future Noise handshake with a joining
   client, and a relay read would consume them.
2. **Verify liveness out of band** (e.g., require the daemon to send
   periodic application-level heartbeats the relay can observe). Adds a
   relay-visible protocol surface to a component that is deliberately
   zero-knowledge, for little gain.
3. **Treat a pong timeout as healthy; tear down only when the ping cannot
   be written.** The ping write itself keeps proxy traffic flowing (the
   daemon's auto-pong makes it bidirectional), a hard write failure still
   reaps dead-socket hosts, and hosts that die silently are replaced when
   the daemon redials (`/v1/host` replaces stale rooms).

## Decision

Option 3, applied to both connection states:

- **Parked hosts:** a pong deadline is expected and healthy; the room is
  torn down only when the ping write fails outright.
- **Active sessions:** the relay also pings both sides on the same interval
  so an idle session isn't dropped by the proxy. Here concurrent `Read`s
  (the pumps) do exist, but a peer busy with a large transfer may answer
  late, so the same rule applies: pong deadline is healthy, write failure
  tears down. Dead peers are otherwise detected by the pumps' reads.

## Consequences

- A silently dead parked host stays advertised until its daemon redials or
  a client join fails — the relay accepts that it cannot distinguish "dead"
  from "quiet" without reading the connection. Stale rooms are bounded by
  the daemon's redial loop replacing them.
- Keepalive traffic flows on every relay connection, parked or active, so
  proxy idle timeouts (Fly's edge) no longer sever long-lived connections.
- The pong-timeout-is-healthy rule is pinned by tests
  (`TestParkedHostSurvivesPongDeadline`, `TestIdleSessionSurvivesKeepalive`)
  so a future websocket-library change that starts surfacing pongs cannot
  silently reintroduce the reaping bug.
