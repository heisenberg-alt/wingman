<div align="center">

# Wingman

**Monitor, prompt, and approve GitHub Copilot CLI sessions — from your phone.**

</div>

![Wingman architecture](docs/architecture.svg)

Wingman is a mobile companion for the [GitHub Copilot CLI](https://github.com/github/copilot-cli). A lightweight daemon (`wingmand`) on your dev machine drives Copilot CLI through its [ACP server](https://docs.github.com/en/copilot/reference/copilot-cli-reference/acp-server), and the Wingman iOS app lets you:

- 📋 See every session across your machines — live status at a glance
- 💬 Watch transcripts stream in real time and send follow-up prompts
- ✅ Approve or deny tool-use requests — even from the lock screen
- 🚀 Kick off new sessions remotely ("fix the failing CI on branch X")
- 🖥 Drop into a raw terminal or review diffs before they land

## Security model

- **End-to-end encrypted.** Phone ↔ daemon traffic uses a Noise XX channel (X25519 + ChaCha20-Poly1305). The relay routes opaque ciphertext by rendezvous ID and can never read payloads.
- **Credentials stay home.** Copilot CLI keeps its own GitHub auth on your dev machine. Nothing GitHub-related ever transits the relay.
- **Explicit pairing.** Devices are paired once by scanning a QR code printed in your terminal; keys live in the iOS Keychain and `~/.wingman/keys`.
- **Fail-safe approvals.** If your phone is unreachable, pending permission requests time out to *deny*.

## Repository layout

| Path | Description |
|---|---|
| `daemon/` | `wingmand` — Go daemon: ACP client, session manager, transport |
| `relay/` | `relayd` — zero-knowledge relay + APNs push (Phase 2) |
| `ios/` | Wingman SwiftUI app (Phase 3) |
| `docs/` | [Protocol spec](docs/PROTOCOL.md), architecture |

## Quick start (Phase 1 — local loopback)

Requires Go 1.25+ and an authenticated [Copilot CLI](https://github.com/github/copilot-cli) ≥ 1.0.44.

```sh
# Check that Copilot CLI's ACP server responds
go run ./daemon/cmd/wingmand doctor

# Start the daemon (loopback WebSocket on :7420)
go run ./daemon/cmd/wingmand serve

# In another terminal: run a session end-to-end from the test client
go run ./daemon/cmd/wingman-cli run --cwd ~/some/project --prompt "explain this repo"
```

## Status

- [x] Phase 0 — repo, architecture, protocol spec
- [ ] Phase 1 — daemon core + ACP integration *(in progress)*
- [ ] Phase 2 — Noise E2E channel, QR pairing, relay
- [ ] Phase 3 — iOS app MVP
- [ ] Phase 4 — APNs push + lock-screen approvals
- [ ] Phase 5 — PTY terminal, diff viewer, usage stats
- [ ] Phase 6 — hardening, Android
