#!/usr/bin/env bash
# smoke.sh — Wingman ship gate.
#
# Builds every module, runs all unit and integration tests, then exercises the
# real binaries end to end: relay + daemon + pairing + encrypted session
# traffic over both the LAN and relay paths.
#
# Requires: Go 1.25+, python3. Does NOT require the Copilot CLI: session flows
# are covered by the test suite via the fakecopilot ACP stand-in.
#
# Usage: scripts/smoke.sh
set -euo pipefail
cd "$(dirname "$0")/.."

LOOPBACK_PORT=17420
EXTERNAL_PORT=17421
RELAY_PORT=18443

log()  { printf '\n== %s ==\n' "$*"; }
fail() {
  printf 'SMOKE FAILED: %s\n' "$*" >&2
  [ -f "$TMP/last-err" ] && { echo '--- last client error ---' >&2; cat "$TMP/last-err" >&2; }
  [ -f "$TMP/daemon.log" ] && { echo '--- daemon log ---' >&2; cat "$TMP/daemon.log" >&2; }
  [ -f "$TMP/relay.log" ] && { echo '--- relay log ---' >&2; cat "$TMP/relay.log" >&2; }
  exit 1
}

log "build + vet"
(cd daemon && go build ./... && go vet ./...)
(cd relay && go build ./... && go vet ./...)

log "unit + integration tests"
(cd daemon && go test ./...)
(cd relay && go test ./...)

log "binary smoke: relay + daemon + pairing + secure traffic"
TMP=$(mktemp -d)
cleanup() {
  kill "${RELAY_PID:-}" "${DAEMON_PID:-}" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

go build -o "$TMP/relayd" ./relay/cmd/relayd
go build -o "$TMP/wingmand" ./daemon/cmd/wingmand
go build -o "$TMP/wingman-cli" ./daemon/cmd/wingman-cli

"$TMP/relayd" --listen "127.0.0.1:$RELAY_PORT" >"$TMP/relay.log" 2>&1 &
RELAY_PID=$!

"$TMP/wingmand" serve \
  --listen "127.0.0.1:$LOOPBACK_PORT" \
  --external "127.0.0.1:$EXTERNAL_PORT" \
  --relay "ws://127.0.0.1:$RELAY_PORT" \
  --home "$TMP/daemon-home" >"$TMP/daemon.log" 2>&1 &
DAEMON_PID=$!

# Wait for a health endpoint with retries (new binaries can be slow to start).
wait_healthy() {
  local url=$1 name=$2
  for _ in $(seq 1 100); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  fail "$name did not become healthy"
}

wait_healthy "http://127.0.0.1:$RELAY_PORT/healthz" "relay"
wait_healthy "http://127.0.0.1:$LOOPBACK_PORT/healthz" "daemon"

PAYLOAD=$("$TMP/wingmand" pair --addr "http://127.0.0.1:$LOOPBACK_PORT" --json)
[ -n "$PAYLOAD" ] || fail "empty pairing payload"

# The test client stores its identity under $HOME.
export HOME="$TMP/client-home"
mkdir -p "$HOME"

# The daemon reconnects to the relay with backoff; a join can race the host
# registration (503), exactly as a phone would. Retry like a phone.
retry() {
  local attempts=$1; shift
  for _ in $(seq 1 "$attempts"); do
    if "$@" 2>"$TMP/last-err"; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}
retry 30 "$TMP/wingman-cli" pair --payload "$PAYLOAD" --name smoke-client --via relay \
  || fail "pairing via relay"

# Point the LAN address at the loopback-bound external listener.
python3 - "$HOME/.wingman-cli.json" "127.0.0.1:$EXTERNAL_PORT" <<'EOF'
import json, sys
path, lan = sys.argv[1], sys.argv[2]
cfg = json.load(open(path))
cfg["lan"] = lan
json.dump(cfg, open(path, "w"))
EOF

retry 30 "$TMP/wingman-cli" list --secure --via relay >/dev/null || fail "encrypted list via relay"
retry 10 "$TMP/wingman-cli" list --secure --via lan   >/dev/null || fail "encrypted list via LAN"

# A second pairing attempt with the consumed token must fail.
if "$TMP/wingman-cli" pair --payload "$PAYLOAD" --name replay-attacker --via relay 2>/dev/null; then
  fail "single-use pairing token was accepted twice"
fi

# Loopback (unencrypted, local-only) path.
"$TMP/wingman-cli" list --addr "ws://127.0.0.1:$LOOPBACK_PORT/ws" >/dev/null || fail "loopback list"

log "optional: real Copilot CLI ACP probe"
if command -v copilot >/dev/null 2>&1; then
  "$TMP/wingmand" doctor || fail "copilot ACP probe"
else
  echo "copilot not installed; skipping (covered by fakecopilot tests)"
fi

log "SMOKE OK — ready to ship"
