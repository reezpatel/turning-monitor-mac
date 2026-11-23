#!/bin/sh
# Assembles Turing Monitor Manager.app:
#   1. the Go manager as a c-archive (arm64, macOS only),
#   2. the SwiftUI menu-bar app compiled against it with swiftc,
#   3. the .app bundle with Info.plist,
#   4. ad-hoc code signing (the app is not distributed; users build it
#      themselves, and macOS's local signature is enough for ScreenCaptureKit
#      once the user grants Screen Recording to the app).
#
# Requires: go, clang (via Xcode command line tools), swiftc, arm64 macOS.

set -eu

REPO=$(cd "$(dirname "$0")/../.." && pwd)
STAGE="$REPO/packaging/manager-app"
BUILD="$STAGE/build"
APP="$STAGE/Turing Monitor Manager.app"

rm -rf "$BUILD" "$APP"
mkdir -p "$BUILD" "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "==> building Go c-archive (arm64)"
CGO_ENABLED=1 CC=clang GOARCH=arm64 go build -buildmode=c-archive \
    -o "$BUILD/libturzx.a" "$REPO/cmd/turzxlib"

# The generated header declares the API; the modulemap makes it importable
# from Swift as CTurzx.
cp "$BUILD/libturzx.h" "$BUILD/libturzx_api.h"
cat > "$BUILD/module.modulemap" <<EOF
module CTurzx {
    header "libturzx_api.h"
    export *
}
EOF

echo "==> compiling SwiftUI app"
swiftc \
    -parse-as-library \
    -target arm64-apple-macosx15.4 \
    -I "$BUILD" \
    "$STAGE/Sources/TuringMonitorManager.swift" \
    "$BUILD/libturzx.a" \
    -framework SwiftUI -framework AppKit \
    -framework ScreenCaptureKit -framework VideoToolbox \
    -framework CoreMedia -framework CoreVideo -framework CoreGraphics \
    -framework Accelerate -framework Foundation \
    "${LIBUSB_PREFIX:-/opt/homebrew/opt/libusb}/lib/libusb-1.0.a" \
    -framework IOKit -framework Security -framework CoreFoundation \
    -o "$APP/Contents/MacOS/Turing Monitor Manager"

echo "==> writing Info.plist"
cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>Turing Monitor Manager</string>
    <key>CFBundleIdentifier</key>
    <string>com.turzx.monitor-manager</string>
    <key>CFBundleName</key>
    <string>Turing Monitor Manager</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0.0</string>
    <key>CFBundleVersion</key>
    <string>1</string>
    <key>LSMinimumSystemVersion</key>
    <string>15.4</string>
    <!-- Menu-bar agent: no Dock icon, no main window. -->
    <key>LSUIElement</key>
    <true/>
    <!-- What the Screen Recording permission prompt explains. -->
    <key>NSScreenCaptureUsageDescription</key>
    <string>Drives TURZX USB panels as extended displays by capturing the virtual display created for each panel.</string>
    <key>NSHumanReadableCopyright</key>
    <string>Unofficial third-party tool, not affiliated with TURZX.</string>
</dict>
</plist>
EOF

echo "==> ad-hoc signing"
codesign --force --sign - --timestamp=none "$APP"

echo
echo "Built: $APP"
echo "First run needs Screen Recording permission for this app:"
echo "  System Settings > Privacy & Security > Screen Recording"
echo "  -> enable \"Turing Monitor Manager\", then launch it again."

