#!/usr/bin/env bash
# go-test-json.sh OUT.json [GO TEST FLAGS AND PACKAGES]: runs `go test -json ...`, keeps the raw JSON in OUT.json
# (every test's pass, fail or skip, with its output: the improvement loop reads it), and prints the log a person reads
# (scripts/ci/gotestlog): what a plain `go test` prints, with the output of a package's tests that did not pass shown
# when that package ends, so a hang's stacks are in the log when its package ends. Exit status is go test's.
# Needs bash (PIPESTATUS). A build failure of the log filter fails the run: nothing here quietly drops output.
set -u
out=${1:?usage: go-test-json.sh OUT.json [go test flags and packages]}
shift
here=$(cd "$(dirname "$0")" && pwd)

gobin=$(command -v "${GO:-go}") || { echo "go-test-json: no ${GO:-go}" >&2; exit 2; }
case $gobin in /*) ;; *) gobin=$PWD/$gobin ;; esac
bin=$(mktemp -d)
trap 'rm -rf "$bin"' EXIT
(cd "$here" && "$gobin" build -o "$bin/gotestlog" ./gotestlog) || { echo "go-test-json: building gotestlog failed" >&2; exit 2; }

"$gobin" test -json "$@" 2>&1 | tee "$out" | "$bin/gotestlog"
exit "${PIPESTATUS[0]}"
