#!/bin/bash
# Build & launch SpeedbondBar.app (Open Speedify macOS menu bar controller) + login LaunchAgent.
set -euo pipefail
cd "$(dirname "$0")"

pkill -x SpeedbondBar 2>/dev/null || true
APP="SpeedbondBar.app"
mkdir -p "$APP/Contents/MacOS"
cat > "$APP/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key><string>SpeedbondBar</string>
  <key>CFBundleIdentifier</key><string>dev.open-speedify.SpeedbondBar</string>
  <key>CFBundleName</key><string>SpeedbondBar</string>
  <key>CFBundleVersion</key><string>1.1</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
EOF
swiftc -O -swift-version 5 -framework Cocoa -framework CoreWLAN SpeedbondBar.swift \
  -o "$APP/Contents/MacOS/SpeedbondBar"
echo "built $PWD/$APP"

PLIST="$HOME/Library/LaunchAgents/com.speedbondbar.plist"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.speedbondbar</string>
  <key>ProgramArguments</key><array><string>open</string><string>-g</string><string>$PWD/$APP</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
</dict></plist>
EOF
UID_NUM=$(id -u)
launchctl bootout "gui/$UID_NUM/com.speedbondbar" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null || true
launchctl bootstrap "gui/$UID_NUM" "$PLIST" 2>/dev/null || launchctl load "$PLIST"
open -g "$PWD/$APP"
echo "SpeedbondBar launched + LaunchAgent installed"
