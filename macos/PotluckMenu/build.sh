#!/bin/sh
# Builds Potluck.app (docs/menubar.md) with Apple's swiftc: no packages. Compiles every *.swift beside this script.
# Usage: macos/PotluckMenu/build.sh [output dir, default ./dist]
# POTLUCK_MENU_UNIVERSAL=1 builds arm64 and x86_64 and joins them (the release does); otherwise this Mac's own.
# POTLUCK_BUNDLE_RELEASE=<dir>, a signed release's directory, puts it in the app (its Mac tarballs, manifest and
# signatures, and this tree's install.sh, which checks them), so the app installs Potluck on a Mac with none.
# The app is signed ad hoc: enough to run on the Mac that built it, not a Developer ID signature (docs/menubar.md).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:-dist}
app="$out/Potluck.app"
bin="$app/Contents/MacOS/PotluckMenu"
rm -rf "$app"
res="$app/Contents/Resources"
mkdir -p "$app/Contents/MacOS" "$res"
cp "$here/Info.plist" "$app/Contents/Info.plist"
(cd "$here/icon" && shasum -a 256 -c made.sha256 >/dev/null) || {
	echo "build.sh: the pictures in macos/PotluckMenu/icon are not those its drawings make: run macos/PotluckMenu/icon/make.sh" >&2
	exit 1
}
cp "$here/icon/AppIcon.icns" "$here/icon/MenuBar.png" "$here/icon/MenuBar@2x.png" "$res/"
build() { # build ARCH OUTPUT
	swiftc -O -target "$1-apple-macos13" -framework AppKit -framework ServiceManagement -o "$2" "$here"/*.swift
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
if [ -n "${POTLUCK_BUNDLE_RELEASE:-}" ]; then
	mkdir -p "$res/release"
	cp "$here/../../install.sh" "$res/install.sh"
	for f in manifest.json manifest.json.sshsig manifest.json.sig; do
		cp "$POTLUCK_BUNDLE_RELEASE/$f" "$res/release/$f"
	done
	version=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([A-Za-z0-9._-]*\)".*/\1/p' "$res/release/manifest.json" | head -n 1)
	for arch in arm64 amd64; do
		cp "$POTLUCK_BUNDLE_RELEASE/potluck_${version}_darwin_$arch.tar.gz" "$res/release/"
	done
fi
codesign --force --sign - "$app" >/dev/null 2>&1 || true
echo "$app"
