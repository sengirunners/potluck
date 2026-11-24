#!/usr/bin/env bash
# Regression for go-test-json.sh: the JSON file names a pass, skip and fail per test, the log is the plain `go test`
# one (package lines; held-back output only for a test that did not pass), and the exit status is go test's.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp"; rm -rf "$tmp"' EXIT
mkdir "$tmp/a" "$tmp/b"
printf 'module gtj\n\ngo 1.21\n' >"$tmp/go.mod"
cat >"$tmp/a/a_test.go" <<'EOF'
package a

import "testing"

func TestOK(t *testing.T)   { t.Log("quiet-pass-line") }
func TestSkip(t *testing.T) { t.Skip("skipped-because") }
EOF
cat >"$tmp/b/b_test.go" <<'EOF'
package b

import "testing"

func TestBad(t *testing.T) { t.Log("failure-detail"); t.Fail() }
func TestGood(t *testing.T) {}
EOF

fail() { echo "FAIL: $*"; exit 1; }
verdict() { jq -r --arg t "$1" 'select(.Test == $t and (.Action == "pass" or .Action == "skip" or .Action == "fail")) | .Action' "$2"; }

cd "$tmp"
# One passing package: status 0, no failure section, no test lines in the log.
status=0
log=$("$here/go-test-json.sh" "$tmp/ok.json" -count=1 ./a 2>&1) || status=$?
[ "$status" = 0 ] || fail "passing run exited $status"
[ "$(verdict TestOK ok.json)" = pass ] || fail "TestOK not recorded as pass"
[ "$(verdict TestSkip ok.json)" = skip ] || fail "TestSkip not recorded as skip"
grep -q 'skipped-because' ok.json || fail "skip reason missing from the JSON"
case "$log" in *quiet-pass-line* | *'=== RUN'* | *PASS*) fail "passing tests leaked into the log: $log" ;; esac
case "$log" in *"ok  	gtj/a"*) ;; *) fail "package line missing from the log: $log" ;; esac
echo "ok   passing run"

# A failing package: go test's status, the failed test's output at the end, the passing test's not.
status=0
log=$("$here/go-test-json.sh" "$tmp/bad.json" -count=1 ./... 2>&1) || status=$?
[ "$status" = 1 ] || fail "failing run exited $status, want 1"
[ "$(verdict TestBad bad.json)" = fail ] || fail "TestBad not recorded as fail"
[ "$(verdict TestGood bad.json)" = pass ] || fail "TestGood not recorded as pass"
case "$log" in *failure-detail*) ;; *) fail "failed test's output missing from the log: $log" ;; esac
case "$log" in *quiet-pass-line*) fail "a passing test's output reached the log" ;; esac
case "$log" in *"FAIL	gtj/b"*) ;; *) fail "FAIL package line missing from the log: $log" ;; esac
echo "ok   failing run"

# A build error is a non-pass the log shows.
printf 'package b\n\nfunc broken( {\n' >"$tmp/b/broken.go"
status=0
log=$("$here/go-test-json.sh" "$tmp/build.json" -count=1 ./b 2>&1) || status=$?
[ "$status" -ne 0 ] || fail "build error exited 0"
case "$log" in *broken.go*) ;; *) fail "build error missing from the log: $log" ;; esac
echo "ok   build error"

# A hang: the timeout's stacks are in the log when the package ends, and the status is go test's.
rm "$tmp/b/broken.go"
mkdir "$tmp/c"
cat >"$tmp/c/c_test.go" <<'EOF'
package c

import (
	"testing"
	"time"
)

func TestHang(t *testing.T) { t.Log("before-hang"); time.Sleep(time.Minute) }
EOF
status=0
log=$("$here/go-test-json.sh" "$tmp/hang.json" -count=1 -timeout 2s ./c 2>&1) || status=$?
[ "$status" -ne 0 ] || fail "timeout exited 0"
case "$log" in *"panic: test timed out"*) ;; *) fail "timeout stacks missing from the log: $log" ;; esac
case "$log" in *before-hang*) ;; *) fail "hung test's output missing from the log: $log" ;; esac
echo "ok   timeout"

# A stray scalar, array or text line from go (not an event) neither breaks the filter nor hides the failure output.
cat >"$tmp/fakego" <<'EOF'
#!/bin/sh
echo 42
echo '["x"]'
echo 'not json at all'
exec go "$@"
EOF
chmod +x "$tmp/fakego"
status=0
log=$(GO="$tmp/fakego" "$here/go-test-json.sh" "$tmp/stray.json" -count=1 ./b 2>&1) || status=$?
[ "$status" = 1 ] || fail "stray-line run exited $status, want 1"
for want in 42 'not json at all' failure-detail; do
	case "$log" in *"$want"*) ;; *) fail "'$want' missing from the log: $log" ;; esac
done
echo "ok   stray lines"
echo "all passed"
