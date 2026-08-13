# ADR-0005: JSONL session persistence with lazy session/load resume

- **Status:** Accepted
- **Date:** 2026-08-13

## Context

`docs/PROTOCOL.md` promises that session ids are "stable across daemon
restarts", but through Phase 3 both the session map and the event log lived
in memory: a daemon restart emptied `session.list` and broke every client's
`session.watch { fromSeq }` cursor. The original plan (recorded in a code
comment) was "a persistent (SQLite) implementation replaces this in Phase 2".
ADR-0002 additionally deferred a log retention policy. Copilot CLI advertises
`loadSession: true`, so the agent side can reattach to an existing
conversation after its process is respawned.

## Options considered

1. **SQLite.** Real queries and transactional writes, but the daemon reads
   each log fully at startup and only ever appends — no query shapes exist
   that need an index. The dependency is heavy either way: mattn/go-sqlite3
   drags in CGo (cross-compilation pain), modernc.org/sqlite is a very large
   pure-Go port. The daemon's dependency set is deliberately tiny.
2. **One JSON state file for everything.** Matches `devices.json` /
   `recent-dirs.json`, but rewriting the whole file per transcript event is
   O(log²) I/O and a crash corrupts all sessions at once.
3. **Per-session directory: `meta.json` + append-only `log.jsonl`.** Events
   append as one JSON line each — O(1) per event, crash damage bounded to one
   torn line in one session. Matches both the log's append-only semantics and
   the repo's plain-JSON persistence style.

## Decision

Option 3. Each session persists under `<StateDir>/sessions/<id>/`:

- `meta.json` — id, cwd, ACP session id, status, creation time; rewritten
  (atomically, via rename) on every status change.
- `log.jsonl` — one seq-numbered event per line, written through on append.
  On load, a torn final line (crash mid-write) is discarded and truncated so
  later appends remain readable.

Restore and resume semantics:

- On startup the manager restores every persisted session as **dormant** —
  no subprocess. `session.list` and `session.watch { fromSeq }` work
  immediately from the persisted log. A session persisted mid-turn is
  settled to `idle` (the turn died with the old daemon) with the transition
  appended for replaying watchers.
- The next `session.prompt` respawns Copilot and reattaches via ACP
  `session/load` before prompting. History replayed by `session/load` is
  suppressed — the daemon already holds it.
- Graceful shutdown (`CloseAll`) no longer marks idle sessions `done`:
  sessions are durable conversations, and killing the subprocess is a
  daemon lifecycle event, not a session outcome. `done`/`error` now mean
  the subprocess ended outside a shutdown.
- Consequently `session.remove` accepts any session not in an active turn
  (previously: terminal only) — otherwise restored idle sessions could
  never be deleted.

Retention (deferred by ADR-0002): at startup, only the newest 50 sessions
are kept; older ones are pruned from disk regardless of status. Within a
session the log is unbounded — transcripts are the product.

## Consequences

- Session ids, statuses, and full transcripts survive daemon restarts and
  crashes; phones resume with their persisted `fromSeq` cursor unchanged.
- A resumed conversation keeps its Copilot context via `session/load` — not
  just its transcript.
- Every event costs one small write; there is no fsync, so an OS crash can
  lose the tail of a log (the torn-line recovery bounds the damage).
- Agents without `loadSession` support fail the resume prompt with an
  error event; the transcript remains readable either way.
- The 50-session cap is a startup-time policy only; a long-running daemon
  can exceed it until its next restart.
