#!/bin/sh
# Builds Potluck Menu.app (docs/menubar.md) with Apple's swiftc: no packages.
# Usage: macos/PotluckMenu/build.sh [output dir, default ./dist]
# POTLUCK_MENU_UNIVERSAL=1 builds arm64 and x86_64 and joins them (the release does); otherwise this Mac's own.
# The app is signed ad hoc: enough to run on the Mac that built it, not a Developer ID signature (docs/menubar.md).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:-dist}
app="$out/Potluck Menu.app"
bin="$app/Contents/MacOS/PotluckMenu"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS"
cp "$here/Info.plist" "$app/Contents/Info.plist"
build() { # build ARCH OUTPUT
	swiftc -O -target "$1-apple-macos13" -framework AppKit -o "$2" "$here/main.swift"
}
if [ "${POTLUCK_MENU_UNIVERSAL:-}" = 1 ]; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	build arm64 "$tmp/arm64"
	build x86_64 "$tmp/x86_64"
	lipo -create -output "$bin" "$tmp/arm64" "$tmp/x86_64"
else
	build "$(uname -m)" "$bin"
fi
codesign --force --sign - "$app" >/dev/null 2>&1 || true
echo "$app"
