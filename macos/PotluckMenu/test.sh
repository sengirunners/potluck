#!/bin/sh
# Compiles the menu's status structs (StatusModel.swift) with swiftc, no
# packages, and decodes the Go test fixtures (menubar/testdata/status-*.json)
# with them: the check that the Go answer and the Swift window still agree.
# Also decodes the action answers in test/answers, runs the action rule against
# a fake potluck (no password dialog), checks the move to the Trash with a stand-in, the
# release an app carries with a stand-in installer, and the
# admin step's quoting (Admin.swift) through sh and AppleScript.
# Usage: macos/PotluckMenu/test.sh   (needs swiftc; run `go test ./menubar -run TestStatusFixtures -update` after changing the answer)
set -eu
here=$(cd "$(dirname "$0")" && pwd)
data="$here/../../menubar/testdata"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
swiftc -o "$tmp/decode" "$here/StatusModel.swift" "$here/SetupFlow.swift" "$here/Admin.swift" "$here/Run.swift" "$here/Trash.swift" "$here/Place.swift" "$here/InviteLink.swift" "$here/Bootstrap.swift" "$here/test/main.swift"
"$tmp/decode" "$data" "$here/test/answers"
