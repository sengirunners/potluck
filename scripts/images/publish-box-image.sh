#!/usr/bin/env bash
# Shared publication step for the ordered Linux -> agent -> browser chain.
# Authentication/buildx setup and platform verification belong to box-images.
set -euo pipefail
[[ $# == 4 ]] || { echo 'usage: publish-box-image image context metadata base-or-dash' >&2; exit 1; }
image=$1 context=$2 metadata=$3 base=$4
: "${TAG:?missing image tag}" "${SOURCE_HEAD:?missing exact source head}" "${SOURCE_REPOSITORY:?missing source repository}" "${GITHUB_OUTPUT:?missing step output}"
[[ "$SOURCE_HEAD" =~ ^[0-9a-f]{40}$ ]] || { echo 'source head must be exact' >&2; exit 1; }
set --
if [ "$base" != - ]; then
  [[ "$base" =~ @sha256:[0-9a-f]{64}$ ]] || { echo 'base must be immutable' >&2; exit 1; }
  set -- --build-arg "BASE=$base"
fi
docker buildx build --platform linux/amd64,linux/arm64 --provenance=false --push \
  "$@" \
  --label "org.opencontainers.image.source=https://github.com/$SOURCE_REPOSITORY" \
  --label "org.opencontainers.image.revision=$SOURCE_HEAD" \
  --metadata-file "$metadata" -t "$image:$TAG" "$context"
digest=$(jq -r '."containerimage.digest"' "$metadata")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo 'no digest in buildx metadata' >&2; exit 1; }
echo "pin=$image@$digest" >> "$GITHUB_OUTPUT"
