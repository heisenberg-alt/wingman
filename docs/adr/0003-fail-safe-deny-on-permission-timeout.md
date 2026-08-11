# ADR-0003: Fail-safe deny for unanswered permission requests

- **Status:** Accepted — in production since Phase 1
- **Date:** 2026-08-12 (records a decision made during Phase 0–1 design)

## Context

Copilot CLI blocks on ACP `session/request_permission` whenever the agent
wants to take an action its policy does not auto-allow — typically running a
shell command on the developer's machine. With Wingman, the approver is a
phone that may be offline, asleep, or out of reach. Something must
eventually answer the held-open JSON-RPC call, and the default answer
decides what an unattended agent is allowed to do.

## Options considered

1. **Allow on timeout.** Keeps agent turns moving, but converts "nobody was
   watching" into "the agent ran it anyway" — precisely the failure mode the
   permission gate exists to prevent.
2. **Block indefinitely.** Never wrong, but wedges the CLI process forever
   and accumulates stuck sessions holding open RPC calls.
3. **Deny on timeout.** After a configurable window, answer `cancelled`;
   the turn ends early but safely, and the outcome is recorded.

## Decision

Option 3. The daemon holds the JSON-RPC call open, appends
`permission.request` and sets state `awaiting_permission`; if no paired
device answers within the timeout (default: 5 minutes, configurable) it
replies `cancelled` to the CLI and appends
`permission.resolved { resolvedBy: "timeout" }` to the session log.

## Consequences

- An unattended machine can never self-approve a dangerous action. A denied
  request is recoverable — re-run the prompt and approve; a wrongly allowed
  destructive action may not be.
- The deny path is exercised routinely by real timeouts, so it stays tested
  in practice, not just in CI.
- Long unattended turns can end early. Accepted: that is the safety
  posture, the transcript records why, and push notifications (Phase 4)
  will shrink the window in which approvals get missed.
- Every resolution is an auditable event with its origin
  (`phone · timeout · cancel`).
