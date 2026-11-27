#!/usr/bin/env bash
# Adds Potluck.zip to a published release, whole or not at all: it is uploaded under another name, read back, and only
# then given its own. GitHub renames an asset in one step, so nobody can download a part of it.
#
# usage: scripts/ci/publish-app.sh TAG ZIP
# A Potluck.zip the release already has stays as it is. A release that is gone, a draft or yanked (a prerelease) gets
# none. Neither is a failure; GITHUB_OUTPUT gets published=true when the release has it. The caller has checked the zip (Developer ID, notarized, stapled).
# Needs gh and jq, GH_TOKEN with contents: write, and GH_REPO.
set -euo pipefail

usage="usage: publish-app.sh TAG ZIP"
tag="${1:?$usage}" zip="${2:?$usage}"
name=Potluck.zip
part=Potluck-upload.zip # never a name anything downloads
[[ -f "$zip" ]] || { echo "publish-app: no $zip" >&2; exit 1; }
sum=$( (sha256sum "$zip" 2>/dev/null || shasum -a 256 "$zip") | cut -d' ' -f1)
[[ "$sum" =~ ^[0-9a-f]{64}$ ]] || { echo "publish-app: cannot hash $zip" >&2; exit 1; }

work=$(mktemp -d)
release() { gh api "repos/$GH_REPO/releases/tags/$tag" 2>"$work/err"; }
assets() { gh api "repos/$GH_REPO/releases/$id/assets?per_page=100"; }
drop() { # remove what an upload left, and fail if it cannot be
	local ids a
	ids=$(assets | jq -r --arg n "$part" '.[] | select(.name == $n) | .id') || return 1
	for a in $ids; do
		gh api --method DELETE "repos/$GH_REPO/releases/assets/$a" >/dev/null || return 1
	done
}
renamed() { # the answer to the rename may be lost though it was done
	[[ $(assets | jq --arg n "$name" --arg d "sha256:$sum" 'any(.[]; .name == $n and .digest == $d)') == true ]]
}
done_() { [[ -z "${GITHUB_OUTPUT:-}" ]] || echo "published=true" >>"$GITHUB_OUTPUT"; }
id=
started=
finish() {
	local status=$?
	if [[ -n "$started" ]] && ! drop; then
		echo "::error::an unfinished $part may be left on $tag: delete it (gh release delete-asset $tag $part)"
		status=1
	fi
	rm -rf "$work"
	exit "$status"
}
trap finish EXIT

if ! json=$(release); then
	grep -q 'HTTP 404' "$work/err" || { cat "$work/err" >&2; exit 1; }
	echo "::warning::there is no release $tag any more: $name is not published"
	exit 0
fi
id=$(jq -r '.id' <<<"$json")
if [[ $(jq -r '.draft or .prerelease' <<<"$json") == true ]]; then
	echo "::warning::$tag is a draft or yanked: $name is not published"
	exit 0
fi
have=$(assets | jq --arg n "$name" 'any(.[]; .name == $n)')
if [[ "$have" == true ]]; then
	echo "$tag has $name already"
	done_
	exit 0
fi

cp "$zip" "$work/$part"
started=1
for pause in 10 30 0; do
	drop # what an earlier try or run left
	if gh release upload "$tag" "$work/$part"; then
		got=$(assets | jq -r --arg n "$part" '[.[] | select(.name == $n)] | first // {} | [(.id // ""), (.state // ""), (.digest // "")] | join("|")')
		IFS='|' read -r asset state digest <<<"$got"
		if [[ "$state" == uploaded && "$digest" == "sha256:$sum" ]]; then
			gh api --method PATCH "repos/$GH_REPO/releases/assets/$asset" -f name="$name" >/dev/null || renamed
			echo "published $name on $tag ($sum)"
			done_
			exit 0
		fi
		echo "::warning::GitHub holds $part as state ${state:-none}, ${digest:-no digest}, not uploaded and sha256:$sum"
	fi
	[[ "$pause" == 0 ]] || sleep "$pause"
done
echo "publish-app: could not upload $name to $tag" >&2
exit 1
