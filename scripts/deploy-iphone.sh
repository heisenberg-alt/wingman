#!/usr/bin/env bash
# deploy-iphone.sh — build, install, and launch Wingman on a connected iPhone,
# then print the pairing QR.
#
# One-time prerequisites (GUI, cannot be scripted):
#   1. iPhone: unlock, tap "Trust This Computer", enable Settings → Privacy &
#      Security → Developer Mode (restarts the phone)
#   2. Xcode: Settings → Accounts → + → sign in with an Apple ID (free is fine)
#   3. Xcode: Wingman target → Signing & Capabilities → Automatically manage
#      signing → select your Personal Team
#
# Usage: scripts/deploy-iphone.sh [DEVICE_UDID]
set -euo pipefail
cd "$(dirname "$0")/.."

UDID=${1:-$(xcrun devicectl list devices 2>/dev/null | awk '/iPhone/ {print $(NF-2)}' | head -1)}
[ -n "$UDID" ] || { echo "error: no iPhone found; is it plugged in and unlocked?" >&2; exit 1; }
echo "device: $UDID"

if ! security find-identity -p codesigning -v | grep -q "Apple Development"; then
  echo "error: no Apple Development signing identity." >&2
  echo "Sign into Xcode (Settings → Accounts) and select a team on the Wingman target first." >&2
  exit 1
fi

echo "== building signed app =="
cd ios/Wingman
xcodebuild -project Wingman.xcodeproj -scheme Wingman \
  -destination "id=$UDID" -allowProvisioningUpdates -quiet build

APP=$(find ~/Library/Developer/Xcode/DerivedData -path '*Debug-iphoneos/Wingman.app' | head -1)
[ -n "$APP" ] || { echo "error: built app bundle not found" >&2; exit 1; }

echo "== installing to device =="
xcrun devicectl device install app --device "$UDID" "$APP"

echo "== launching =="
xcrun devicectl device process launch --device "$UDID" dev.wingman.Wingman || true

cd ../..
echo
echo "== pairing QR (scan it with the app; token valid 10 minutes) =="
WINGMAND=${WINGMAND:-/tmp/wingmand}
if [ -x "$WINGMAND" ]; then
  "$WINGMAND" pair
else
  go run ./daemon/cmd/wingmand pair
fi
