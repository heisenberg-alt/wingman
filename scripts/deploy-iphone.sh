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

# devicectl (CoreDevice) UUID for install/launch — iPhones only, not Watches.
UDID=${1:-$(xcrun devicectl list devices 2>/dev/null | grep 'iPhone' | grep -oE '[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}' | head -1)}
[ -n "$UDID" ] || { echo "error: no iPhone found; is it plugged in and unlocked?" >&2; exit 1; }
# xcodebuild hardware identifier (e.g. 00008140-...), needed as the build
# destination so Xcode registers the device with the (free) team.
XCID=$(xcrun xctrace list devices 2>/dev/null | grep -iv 'watch' | grep -v Simulator | grep -oE '\(0000[0-9A-F]+-[0-9A-F]+\)' | tr -d '()' | head -1)
[ -n "$XCID" ] || { echo "error: xcodebuild device id not found" >&2; exit 1; }
echo "device: $UDID (build id: $XCID)"

if ! security find-identity -p codesigning -v | grep -q "Apple Development"; then
  echo "note: no signing certificate yet; xcodebuild will create one via the Xcode account" >&2
fi

echo "== building signed app =="
cd ios/Wingman
xcodebuild -project Wingman.xcodeproj -scheme Wingman \
  -destination "platform=iOS,id=$XCID" -allowProvisioningUpdates -quiet build

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
