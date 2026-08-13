# ADR-0006: Daemon-direct APNs pushes for permission requests

- **Status:** Accepted
- **Date:** 2026-08-13

## Context

Phase 4 promises push notifications and lock-screen approvals. Today the
iOS app polls `session.list` every 4 seconds while foregrounded and learns
nothing in the background, so a permission request can sit unanswered until
the fail-safe timeout denies it (ADR-0003). Something must call Apple's
APNs HTTP/2 API when the daemon appends `permission.request`. APNs requires
a provider token signed with the app's `.p8` auth key (team id + key id),
and the phone's device token must reach whoever sends the push.

## Options considered

1. **Daemon → APNs directly.** The daemon holds the APNs auth key and
   pushes to tokens the phone registers over the secure channel. Wingman is
   self-hosted — whoever runs `wingmand` also builds and signs the app, so
   the key stays with its owner. No new component; the relay stays
   zero-knowledge; device tokens never leave the host.
2. **Relay → APNs.** One key location for a shared relay, but the relay
   would learn device tokens and push timing (a side channel on "the agent
   wants something"), breaking its only design invariant. It would also
   need a daemon→relay push API and its own token store.
3. **Separate push service.** Same trust drawbacks as the relay plus a
   third deployable. Only worth it for a hosted multi-tenant product,
   which Wingman is not.

## Decision

Option 1. The daemon speaks APNs directly:

- `wingmand serve` takes `--apns-key` (path to the `.p8`), `--apns-key-id`,
  `--apns-team-id`, and `--apns-bundle-id`. Push is off unless configured.
- Provider JWTs (ES256) are minted with the standard library and cached for
  ~50 minutes, within Apple's 20–60 minute refresh window.
- The phone registers its device token over the Noise channel with a new
  `push.register { token, env, deviceName }` command; `env` is `sandbox`
  or `production` (per build), and each token is pushed via the matching
  APNs endpoint. Tokens persist in `<StateDir>/push.json`, keyed by token.
- On `permission.request` the daemon pushes to every registered token:
  a time-sensitive alert carrying the request title plus `sessionId`,
  `requestId`, and the offered option ids, in a category with Approve and
  Deny actions so the answer can come from the lock screen. The payload
  travels through Apple, not the relay.
- Tokens rejected by APNs as gone (`410 Unregistered`, `BadDeviceToken`)
  are dropped from the registry.

## Consequences

- The relay remains zero-knowledge; nothing about push flows through it.
- Payloads transit Apple's servers in cleartext, so they carry only the
  permission title and routing ids — never tool-call contents or
  transcripts. The phone fetches details over the encrypted channel.
- The daemon needs outbound HTTPS to `api.push.apple.com` /
  `api.sandbox.push.apple.com`; on a machine without egress, push silently
  degrades to the existing polling behavior.
- Anyone distributing Wingman to users who do not build the app themselves
  would need to revisit this (their users cannot hold the signing key) —
  the relay option is the fallback for that world.
- `push.register` is idempotent per token, so the app can re-register on
  every connect; stale tokens age out via APNs rejections.
