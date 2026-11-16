#!/bin/sh
# Builds Potluck Menu.app (docs/menubar.md) with Apple's swiftc: no packages.
# Usage: macos/PotluckMenu/build.sh [output dir, default ./dist]
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:-dist}
app="$out/Potluck Menu.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS"
cp "$here/Info.plist" "$app/Contents/Info.plist"
swiftc -O -target "$(uname -m)-apple-macos13" -framework AppKit \
	-o "$app/Contents/MacOS/PotluckMenu" "$here/main.swift"
codesign --force --sign - "$app" >/dev/null 2>&1 || true # ad hoc, for this Mac only
echo "$app"
