# ADR-0001: Noise XX end-to-end encryption over a zero-knowledge relay

- **Status:** Accepted — in production since Phase 2
- **Date:** 2026-08-12 (records a decision made during Phase 2 design)

## Context

Away from the LAN, phone↔daemon traffic must traverse a relay on public
infrastructure. That traffic is sensitive twice over: the transcript stream
can contain source code, and the approval path carries security decisions
("run this tool call on my dev machine"). Compromise of the relay host must
not compromise sessions. Further constraints: pairing has to be a single QR
scan, the client is a native app (no browser requirement), and Copilot's
GitHub credentials must never leave the development machine.

## Options considered

1. **TLS to the relay; trust the relay.** Simplest to build, but the relay
   terminates TLS and sees plaintext. It becomes the most attractive target
   in the system, and whoever hosts it joins the trusted computing base.
2. **mTLS end-to-end with a private CA.** End-to-end confidentiality, but
   issuing and renewing device certificates is heavy ceremony for a QR-scan
   pairing flow, and it adds a CA to operate, rotate, and revoke against.
3. **WireGuard tunnel per device.** Strong crypto but network-level: it
   needs VPN entitlements on iOS, conflicts with any other VPN the user
   runs, and is oversized for a single WebSocket.
4. **Noise XX inside the WebSocket.** Mutual static-key authentication with
   keys generated on each device and pinned at first pairing; the relay
   carries only ciphertext.

## Decision

Option 4. The phone and daemon run a Noise XX handshake (X25519,
ChaCha20-Poly1305, SHA-256) inside the WebSocket, identically over the
external LAN listener and the relay path. `relayd` routes opaque frames by
rendezvous room id and authenticates connections with a bearer token carried
inside the pairing QR code; it never holds key material. Device static keys
are pinned at first pairing (`~/.wingman/keys` on the daemon, Keychain on
iOS) and serve as the authorization principal from then on.

## Consequences

- The relay is untrusted by construction: it can drop or delay frames but
  cannot read, forge, or splice them. Fly.io — or any other host — stays
  outside the trusted computing base.
- No certificate lifecycle. Pairing is one QR scan; revoking a device is
  deleting a key.
- We own handshake and channel code on both ends (Go `securechan`, Swift
  WingmanKit on CryptoKit). Mitigated with table-driven unit tests plus a
  live Go↔Swift Noise interop test against a running daemon.
- A future web client would need a JS/WASM Noise implementation. Accepted.
- The loopback listener (`127.0.0.1`) deliberately skips the Noise layer:
  the local user already owns the machine.
