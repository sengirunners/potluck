#!/usr/bin/env bash
# One open issue per failing thing, in the repository GH_REPO names, so a failure that blocks nothing visible reaches
# the people watching it (GitHub mails them a new issue and its comments).
#
# usage: scripts/ci/failing-issue.sh TITLE failure|success URL WHAT
# failure opens an issue titled TITLE ("WHAT failed: URL. ..."), or comments "Failed again: URL" on the open one;
# success comments "Passing again: URL" on the open one and closes it, and does nothing when none is open.
# Needs gh and jq, GH_TOKEN with issues: write, and GH_REPO.
set -euo pipefail

usage="usage: failing-issue.sh TITLE failure|success URL WHAT"
title="${1:?$usage}" result="${2:?$usage}" url="${3:?$usage}" what="${4:?$usage}"
open=$(gh issue list --state open --limit 100 --json number,title)
n=$(jq -r --arg t "$title" '[.[] | select(.title == $t) | .number] | first // empty' <<<"$open")
case "$result" in
failure)
	if [[ -n "$n" ]]; then
		gh issue comment "$n" --body "Failed again: $url"
	else
		gh issue create --title "$title" --body "$what failed: $url. Later failures comment here; the next pass closes this issue."
	fi
	;;
success)
	if [[ -n "$n" ]]; then
		gh issue comment "$n" --body "Passing again: $url"
		gh issue close "$n" --reason completed
	fi
	;;
*)
	echo "failing-issue: result $result: $usage" >&2
	exit 2
	;;
esac
