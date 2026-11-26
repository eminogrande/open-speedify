#!/bin/bash
# One-command build & launch for Open Speedify:
# Builds speedbond (macOS + ARM64 VM bridge), compiles SpeedbondBar.app, installs LaunchAgent, and launches it.
set -euo pipefail
cd "$(dirname "$0")"

echo "==> Building speedbond binaries..."
go build -o speedbond .
GOOS=linux GOARCH=arm64 go build -o speedbond-arm64 .

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
  <key>CFBundleVersion</key><string>1.2</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
EOF

echo "==> Building $APP..."
swiftc -O -swift-version 5 -framework Cocoa -framework CoreWLAN SpeedbondBar.swift \
  -o "$APP/Contents/MacOS/SpeedbondBar"

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
echo "✅ Open Speedify ready (SpeedbondBar launched + LaunchAgent installed)"
