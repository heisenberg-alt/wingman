#!/usr/bin/env bash
# deploy-relay.sh — deploy the Wingman relay to Fly.io with token auth.
#
# Prerequisites: flyctl installed (brew install flyctl) and `fly auth login`.
#
# Usage: scripts/deploy-relay.sh [app-name]
set -euo pipefail
cd "$(dirname "$0")/../relay"

APP=${1:-wingman-relay-$(whoami)}
command -v fly >/dev/null || { echo "error: install flyctl first: brew install flyctl" >&2; exit 1; }

# Generate a relay auth token once; reuse if the app already has one.
TOKEN=$(fly secrets list -a "$APP" 2>/dev/null | grep -q RELAY_TOKEN && echo "" || openssl rand -hex 16)

if ! fly apps list 2>/dev/null | grep -q "^$APP"; then
  echo "== creating app $APP =="
  fly launch --copy-config --name "$APP" --no-deploy --yes
fi

if [ -n "$TOKEN" ]; then
  echo "== setting relay token =="
  fly secrets set -a "$APP" RELAY_TOKEN="$TOKEN" --stage
fi

echo "== deploying =="
fly deploy -a "$APP"

URL="wss://$APP.fly.dev"
echo
echo "== relay deployed =="
echo "relay URL:   $URL"
if [ -n "$TOKEN" ]; then
  echo "relay token: $TOKEN"
  echo
  echo "Start your daemon with:"
  echo "  wingmand serve --external :7421 --relay $URL --relay-token $TOKEN"
  echo "Then re-pair your phone (wingmand pair) so it learns the relay."
else
  echo "relay token: (unchanged; pass the existing token via --relay-token)"
fi
