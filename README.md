# Wingman

Remote control for GitHub Copilot CLI. Monitor, prompt, and approve agent sessions from your phone.

![Wingman architecture](docs/architecture.svg)

## Overview

Wingman consists of three components:

- **`wingmand`** — a Go daemon on your development machine that drives [GitHub Copilot CLI](https://github.com/github/copilot-cli) sessions through its [Agent Client Protocol (ACP) server](https://docs.github.com/en/copilot/reference/copilot-cli-reference/acp-server), one subprocess per session.
- **`relayd`** — a zero-knowledge relay that routes end-to-end encrypted frames between daemon and phone, and delivers push notifications. *(Phase 2)*
- **Wingman for iOS** — a native SwiftUI app for observing and steering sessions. *(Phase 3)*

### Capabilities

- List sessions across machines with live status
- Stream transcripts in real time and send follow-up prompts
- Approve or deny tool-use permission requests remotely
- Start new sessions in trusted directories
- Raw terminal access and diff review *(Phase 5)*

## Security model

- **End-to-end encryption.** Phone-to-daemon traffic is carried inside a Noise XX channel (X25519, ChaCha20-Poly1305). The relay routes opaque ciphertext by rendezvous ID and cannot read payloads.
- **Credentials stay local.** Copilot CLI retains its own GitHub authentication on the development machine. No GitHub tokens transit the relay.
- **Explicit device pairing.** Devices pair once by scanning a QR code printed in the terminal. Keys are stored in the iOS Keychain and in `~/.wingman/keys`.
- **Fail-safe approvals.** If no paired device responds, pending permission requests are denied after a configurable timeout (default: 5 minutes).
- **Loopback by default.** In Phase 1 the daemon listens on `127.0.0.1` only.

## Repository layout

| Path | Description |
|------|-------------|
| `daemon/` | `wingmand` daemon: ACP client, session manager, transport |
| `relay/` | `relayd` relay service |
| `ios/WingmanKit/` | Swift package: Noise channel (CryptoKit), protocol types, async client |
| `ios/Wingman/` | Wingman iOS app (SwiftUI) |
| `docs/` | [Protocol specification](docs/PROTOCOL.md) and architecture |

## Getting started

Requirements: Go 1.25+ and an authenticated [GitHub Copilot CLI](https://github.com/github/copilot-cli) 1.0.44 or later.

Verify that the Copilot CLI ACP server responds:

```sh
go run ./daemon/cmd/wingmand doctor
```

Start the daemon (loopback WebSocket on `127.0.0.1:7420`):

```sh
go run ./daemon/cmd/wingmand serve
```

Drive a session end to end with the test client:

```sh
go run ./daemon/cmd/wingman-cli run --cwd ~/some/project --prompt "explain this repo"
```

The client streams the transcript, surfaces permission requests for interactive approval, and exits when the turn completes. Use `wingman-cli list` to enumerate sessions and `wingman-cli watch --session ID` to attach to a running session.

## iOS app

Requires Xcode 16+. The app is generated from [ios/Wingman/project.yml](ios/Wingman/project.yml) with [XcodeGen](https://github.com/yonaskolb/XcodeGen); the generated project is checked in.

```sh
# Library tests, including a live Noise interop test against a running daemon
cd ios/WingmanKit && swift test

# Open the app
open ios/Wingman/Wingman.xcodeproj
```

To pair: run `wingmand serve --external :7421` and `wingmand pair` on the dev
machine, then scan the QR code from the app. The app prefers the LAN path and
falls back to the relay automatically.

## Ship gate

```sh
scripts/smoke.sh
```

Builds both Go modules, runs the full test suite, then exercises the real
binaries end to end: relay and daemon startup, pairing via relay, encrypted
traffic over both paths, and single-use token replay rejection.

## Protocol

The phone and daemon exchange JSON messages over a WebSocket, defined in [docs/PROTOCOL.md](docs/PROTOCOL.md). Every session event carries a monotonic sequence number; clients resume after a disconnect by replaying from their last acknowledged sequence. Permission requests block the CLI until answered and fail safe to deny.

## Public relay

To use Wingman away from your LAN, host the relay on a public HTTPS address.
The relay stays zero-knowledge: it routes ciphertext by room id and
authenticates connections with a bearer token that travels inside the pairing
QR code.

```sh
# One-command deploy to Fly.io (needs flyctl + fly auth login)
scripts/deploy-relay.sh

# Point the daemon at it (printed by the deploy script)
wingmand serve --external :7421 --relay wss://<app>.fly.dev --relay-token <token>

# Re-pair the phone so it learns the relay
wingmand pair
```

Any host works: the relay is a single static binary (see [relay/Dockerfile](relay/Dockerfile))
that reads `RELAY_TOKEN` and `RELAY_LISTEN` from the environment. Hardening
included: token auth, keepalive pings, per-IP rate limiting, and a room cap.

## Roadmap

| Phase | Scope | Status |
|-------|-------|--------|
| 0 | Repository, architecture, protocol specification | Complete |
| 1 | Daemon core, ACP integration, loopback transport | Complete |
| 2 | Noise E2E channel, QR pairing, relay | Complete |
| 3 | iOS app: pairing, dashboard, live transcript, approvals | Complete |
| 4 | Push notifications, lock-screen approvals | Planned |
| 5 | PTY terminal, diff viewer, usage statistics | Planned |
| 6 | Hardening, Android | Planned |

## License

[MIT](LICENSE)
