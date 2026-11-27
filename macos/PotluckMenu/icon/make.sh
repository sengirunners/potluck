#!/bin/sh
# Makes the app's pictures from this directory's two drawings, with Apple's swiftc and iconutil: AppIcon.svg becomes
# AppIcon.icns, MenuBar.svg the menu bar's MenuBar.png and MenuBar@2x.png. The results are kept in the repository, so
# an app ships the pictures that were looked at, whatever WebKit its build machine has; made.sha256 records which
# drawings they are from, and build.sh refuses a set that does not match it.
# Usage: macos/PotluckMenu/icon/make.sh   (then look at what it made)
set -eu
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
swiftc -O -framework AppKit -framework WebKit -o "$tmp/render" render.swift
mkdir "$tmp/app" "$tmp/bar" "$tmp/AppIcon.iconset"
"$tmp/render" AppIcon.svg "$tmp/app" 16 32 64 128 256 512 1024
for n in 16 32 128 256 512; do
	cp "$tmp/app/$n.png" "$tmp/AppIcon.iconset/icon_${n}x$n.png"
	cp "$tmp/app/$((n * 2)).png" "$tmp/AppIcon.iconset/icon_${n}x$n@2x.png"
done
iconutil -c icns -o AppIcon.icns "$tmp/AppIcon.iconset"
"$tmp/render" MenuBar.svg "$tmp/bar" 18 36
cp "$tmp/bar/18.png" MenuBar.png
cp "$tmp/bar/36.png" MenuBar@2x.png
shasum -a 256 AppIcon.svg MenuBar.svg AppIcon.icns MenuBar.png MenuBar@2x.png >made.sha256
