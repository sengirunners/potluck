#!/bin/sh
# Builds potluck.mcpb from mcp-registry/bundle into <out> and writes <out>/server.json: mcp-registry/server.json
# with the bundle's SHA-256 filled in. The bundle is the same bytes for the same files (fixed times, no extra
# attributes), so a rebuild of an unchanged bundle has the same hash.
# usage: mcp-registry/build.sh <out dir>
set -eu
export TZ=UTC LC_ALL=C
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out dir>}
mkdir -p "$out"
out=$(cd "$out" && pwd)
command -v jq >/dev/null || { echo "build.sh needs jq" >&2; exit 1; }
command -v zip >/dev/null || { echo "build.sh needs zip" >&2; exit 1; }

v=$(jq -r .version "$here/bundle/manifest.json")
sv=$(jq -r .version "$here/server.json")
want="https://github.com/sengirunners/potluck/releases/download/mcp-bundle-v$v/potluck.mcpb"
got=$(jq -r '.packages[0].identifier' "$here/server.json")
[ "$v" = "$sv" ] || { echo "manifest.json is version $v but server.json is $sv" >&2; exit 1; }
[ "$got" = "$want" ] || { echo "server.json's identifier is $got, want $want" >&2; exit 1; }
[ -x "$here/bundle/server/potluck-mcp" ] || { echo "bundle/server/potluck-mcp is not executable" >&2; exit 1; }

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cp -R "$here/bundle/." "$stage/"
find "$stage" -exec touch -t 202601010000 {} +
rm -f "$out/potluck.mcpb"
(cd "$stage" && find . -type f | LC_ALL=C sort | sed 's|^\./||' | zip -q -X -D "$out/potluck.mcpb" -@)
sum=$(openssl dgst -sha256 -r "$out/potluck.mcpb" | cut -d' ' -f1)
jq --arg s "$sum" '.packages[0].fileSha256 = $s' "$here/server.json" >"$out/server.json"
echo "potluck.mcpb $v sha256 $sum"
