#!/usr/bin/env bash
# Runs potluck-conformance against potluck-driver-local (no root, VM or container; docs/conformance.md, "Build and
# run") and writes its results, with the skipped checks' names, to $1 (default conformance-results/results.json)
# through --json-out. The text report goes to results.txt beside it. The script exits with the suite's own code, so
# the caller decides whether that gates anything: ci.yml's conformance-report job does not.
set -euo pipefail
out=${1:-conformance-results/results.json}
mkdir -p "$(dirname "$out")"
out=$(cd "$(dirname "$out")" && pwd)/$(basename "$out")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

go build -o "$work/bin/" ./cmd/potluck-conformance ./cmd/potluck-driver-local
"$work/bin/potluck-conformance" --print-probe > "$work/probe.sh"
chmod +x "$work/probe.sh"

# A repository whose first commit holds probe.sh and one more on top, so --commit is not the tip of --ref (else the
# HEAD check is a SKIP).
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
git() { command git -c user.name=ci -c user.email=ci@example.invalid -c commit.gpgsign=false -c init.defaultBranch=main "$@"; }
git init -q --bare "$work/remote.git"
git init -q "$work/repo"
cp "$work/probe.sh" "$work/repo/probe.sh"
git -C "$work/repo" add .
git -C "$work/repo" commit -q -m first
commit=$(git -C "$work/repo" rev-parse HEAD)
echo tip > "$work/repo/tip.txt"
git -C "$work/repo" add .
git -C "$work/repo" commit -q -m tip
git -C "$work/repo" push -q "$work/remote.git" HEAD:refs/heads/main

# A throwaway HOME: the suite makes and ends real resources and must not touch the runner's dotfiles.
set +e
env HOME="$work/home" "$work/bin/potluck-conformance" "$work/bin/potluck-driver-local" \
  --ref main --commit "$commit" --repo "$work/remote.git" --script probe.sh \
  --config "remote=$work/remote.git" --config "state_dir=$work/state" \
  --ttl-short 2 --grace 5s --run-wait 30s --json-out "$out" 2>&1 | tee "$(dirname "$out")/results.txt"
code=${PIPESTATUS[0]}
exit "$code"
