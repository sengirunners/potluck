#!/usr/bin/env bash
# Copies one commit of this (private) repository to the public repository as one new commit on its main,
# by the project identity. Only that commit's tree goes over, exactly (git commit-tree on the private tree object,
# parented on the public main): no history, branches, tags or pull request refs.
# docs/public-sync.md says when and how to run it.
#
# usage: scripts/public-sync.sh <commit> [--push]
# Without --push it makes the commit, scans the tree and prints what changed, and pushes nothing.
# It pushes nothing while a hold is on (a file under public-hold/: a security fix not yet on our machines).
# Needs git, gitleaks and write access to the public repository (an admin: main is protected).
set -euo pipefail

PUBLIC="https://github.com/sengirunners/potluck.git"
NAME="Sengi, Inc."
EMAIL="ops@sengi.run"

commit="${1:?usage: scripts/public-sync.sh <commit> [--push]}"
push="${2:-}"
command -v gitleaks >/dev/null || { echo "public-sync: gitleaks is not installed" >&2; exit 1; }
self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
# Paths below are read from the top of the repository, wherever the script was started.
cd "$(git rev-parse --show-toplevel)"
# The private main as it is now, not as this clone last saw it: the hold below is looked for there too.
git fetch --quiet --no-tags origin main || { echo "public-sync: cannot fetch origin main" >&2; exit 1; }
sha="$(git rev-parse --verify "$commit^{commit}")"
git rev-parse --verify --quiet origin/main >/dev/null || { echo "public-sync: no origin/main (git fetch origin)" >&2; exit 1; }
git merge-base --is-ancestor "$sha" origin/main || { echo "public-sync: $sha is not on origin/main" >&2; exit 1; }

# The script that runs is the one in the commit being published. A checkout that is behind that commit would
# otherwise publish with older rules. (A checkout from before this check was written cannot be stopped here.)
if [ "$(git rev-parse --verify --quiet "$sha:scripts/public-sync.sh")" != "$(git hash-object "$self")" ]; then
	echo "public-sync: $self is not the scripts/public-sync.sh of $sha; run it from a checkout of that commit" >&2
	exit 1
fi

# A hold: a file under public-hold/ names a security fix that is not yet installed on the machines we run for
# other people. Publishing shows where the flaw was, and the whole tree goes over, so nothing is pushed while one
# is there (docs/public-sync.md, "Holds"). It is looked for in the commit being published and on origin/main, so
# a commit that has the fix and not yet its hold is refused too. Any path with "public-hold" in it counts, in any
# letter case and with a dash, underscore, dot, space or nothing between the words, so a misnamed hold still
# holds. A tree that cannot be read counts as held. (-z: git quotes unusual file names otherwise.)
files="$(git ls-tree -r -z --name-only --full-tree "$sha" | tr '\0' '\n')" || { echo "public-sync: cannot read $sha for holds" >&2; exit 1; }
main_files="$(git ls-tree -r -z --name-only --full-tree origin/main | tr '\0' '\n')" || { echo "public-sync: cannot read origin/main for holds" >&2; exit 1; }
held="$(printf '%s\n%s\n' "$files" "$main_files" | { grep -i -E 'public[-_. ]?hold' || [ $? -eq 1 ]; } | sort -u)" ||
	{ echo "public-sync: cannot search for holds" >&2; exit 1; }
if [ -n "$held" ]; then
	printf '%s\n' "$held" | sed 's/^/public-sync: held by /' >&2
	if [ "$push" = "--push" ]; then
		echo "public-sync: not pushed: install the fix on our machines, remove its hold on the private main, then sync again" >&2
		exit 1
	fi
fi

git fetch --quiet --no-tags "$PUBLIC" main
public_main="$(git rev-parse FETCH_HEAD)"
if [ "$(git rev-parse "$sha^{tree}")" = "$(git rev-parse "$public_main^{tree}")" ]; then
	echo "public-sync: the public main already has this tree"
	exit 0
fi
# The whole tree goes over, so publishing a commit older than the one the public main already carries would undo
# everything merged in between. Go on only when the public main's tree is the tree of one of this commit's own
# ancestors (the public main is behind it); otherwise name the newer private commit and stop.
public_tree="$(git rev-parse "$public_main^{tree}")"
if ! git log --format=%T "$sha" | grep -x "$public_tree" >/dev/null; then
	newer="$(git log --format='%H %T' origin/main | awk -v t="$public_tree" '$2 == t && !n { print $1; n = 1 }')"
	if [ -n "$newer" ]; then
		echo "public-sync: the public main already carries private $newer, which $sha does not contain; publishing $sha would undo it. Publish origin/main (or a commit that contains $newer) instead." >&2
	else
		echo "public-sync: the public main's tree is no private main commit's tree (a change made on the public repository?); a sync would replace it. Bring that change to the private main first (docs/public-sync.md)." >&2
	fi
	exit 1
fi

# Scan exactly the tree that will go over. Read every hit by hand: test fixtures and public keys are expected; a
# real secret, a person's address, a private repository or a machine name is not.
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/tree"
git archive "$sha" | tar -x -C "$work/tree"
status=0
gitleaks detect --no-git --no-banner -s "$work/tree" || status=$?
[ "$status" -le 1 ] || { echo "public-sync: gitleaks failed (exit $status)" >&2; exit 1; }

new="$(printf 'Potluck: sync from the private main\n\nSigned-off-by: %s <%s>\n' "$NAME" "$EMAIL" |
	GIT_AUTHOR_NAME="$NAME" GIT_AUTHOR_EMAIL="$EMAIL" GIT_COMMITTER_NAME="$NAME" GIT_COMMITTER_EMAIL="$EMAIL" \
		git -c commit.gpgsign=false commit-tree "$sha^{tree}" -p "$public_main")"
git diff --stat "$public_main" "$new" >"$work/.stat"
sed -n 1,40p "$work/.stat"
echo "public-sync: commit $new (tree of $sha) on public main $public_main"
if [ "$push" = "--push" ]; then
	git push --quiet --no-verify "$PUBLIC" "$new:refs/heads/main"
	echo "public-sync: pushed $new to $PUBLIC main"
elif [ -n "$held" ]; then
	printf '%s\n' "$held" | sed 's/^/public-sync: not pushed, and --push would refuse: held by /' >&2
else
	echo "public-sync: not pushed (pass --push)"
fi
