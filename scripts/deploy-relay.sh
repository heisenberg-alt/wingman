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

# Exact-match the app name; a prefix grep would confuse e.g. "wingman-relay"
# with "wingman-relay-foo".
app_exists() {
  fly apps list 2>/dev/null | awk '{print $1}' | grep -Fqx -- "$APP"
}

# Generate a relay auth token only for a new app or one without a token.
# Distinguish "no RELAY_TOKEN set" from "fly secrets list failed": rotating
# the token on a transient failure would break every already-paired device.
TOKEN=""
if app_exists; then
  SECRETS=$(fly secrets list -a "$APP") || {
    echo "error: could not read secrets for $APP; refusing to risk rotating RELAY_TOKEN" >&2
    exit 1
  }
  echo "$SECRETS" | grep -qw RELAY_TOKEN || TOKEN=$(openssl rand -hex 16)
else
  echo "== creating app $APP =="
  fly launch --copy-config --name "$APP" --no-deploy --yes
  TOKEN=$(openssl rand -hex 16)
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
