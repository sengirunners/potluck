# zm's coding-agent box image (zm-eakh.12.5.20): the `agent` template, what `zm sandbox create --template agent`
# boots. .github/workflows/box-images.yml builds it for linux/amd64 and linux/arm64 FROM the potluck-box-linux image it
# has just pushed (BASE, by digest), pushes ghcr.io/sengirunners/potluck-box-agent and writes its digest on the
# `template=agent` line of imagepins/images.pin; `zm host share` lends it beside the default.
#
# Built FROM the default, so it keeps the runner in /home/runner, the user runner (passwordless sudo, GitHub's
# runner image) and every tool a box needs, and adds what a coding agent runs: git (the base's), gh, Node 22 LTS
# with npm and corepack, Go 1.27 (GOTOOLCHAIN left at auto, so a go.mod asking for a newer Go fetches it),
# Python 3 with pip and venv, ripgrep, jq, build-essential, and the Claude Code and Codex CLIs. Every download is pinned by version and, for tarballs, by SHA-256; never a latest. Bump a version and its
# sums together (Node: https://nodejs.org/dist/v<NODE_VERSION>/SHASUMS256.txt; gh: the release's checksums.txt;
# Go: https://go.dev/dl/?mode=json&include=all).
# The workflow always passes BASE; this default is the potluck-box-linux build pinned in imagepins/images.pin.
ARG BASE=ghcr.io/sengirunners/potluck-box-linux@sha256:b47c4d9f2e92103e6a64ab4c51642a0328ab599c4580d5c560fb93f0c306c6f0
FROM ${BASE}
ARG TARGETARCH
ARG NODE_VERSION=22.23.4
ARG NODE_SHA256_AMD64=1805bcd35e6815559bd2eb97db438d3fdb0eb3a5e6a977b7c1badca76337e9ce
ARG NODE_SHA256_ARM64=6287aa46b6c375d816e63ae66d024fbbfd70404825aabdfdf898a970871d2f4b
ARG GH_VERSION=2.103.3
ARG GH_SHA256_AMD64=4c806978ffce1cec70853302a56d70ac6da4f19bae7adca4e1a8ab00633dd91d
ARG GH_SHA256_ARM64=ee32df2323c8847f5fac6cb6b4b69ca75828e9329a8f22f677021f7b3306ce63
ARG GO_VERSION=1.27.4
ARG GO_SHA256_AMD64=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840
ARG GO_SHA256_ARM64=3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92
ARG CLAUDE_CODE_VERSION=2.1.319
ARG CODEX_VERSION=0.167.0
USER root
# Go lives in /usr/local/go; on PATH for every user and for the checks below.
ENV PATH=/usr/local/go/bin:$PATH
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl xz-utils build-essential \
       python3 python3-pip python3-venv ripgrep jq \
  && rm -rf /var/lib/apt/lists/*
RUN case "$TARGETARCH" in \
      amd64) node_arch=x64 node_sum=$NODE_SHA256_AMD64 gh_sum=$GH_SHA256_AMD64 go_sum=$GO_SHA256_AMD64 ;; \
      arm64) node_arch=arm64 node_sum=$NODE_SHA256_ARM64 gh_sum=$GH_SHA256_ARM64 go_sum=$GO_SHA256_ARM64 ;; \
      *) echo "no pinned Node, gh or Go for $TARGETARCH" >&2; exit 1 ;; \
    esac \
  && tmp=$(mktemp -d) \
  && curl --proto =https -fsSL -o "$tmp/node.tar.xz" "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-$node_arch.tar.xz" \
  && echo "$node_sum  $tmp/node.tar.xz" | sha256sum -c - \
  && tar -xJf "$tmp/node.tar.xz" -C /usr/local --strip-components=1 --no-same-owner \
  && curl --proto =https -fsSL -o "$tmp/gh.tar.gz" "https://github.com/cli/cli/releases/download/v$GH_VERSION/gh_${GH_VERSION}_linux_$TARGETARCH.tar.gz" \
  && echo "$gh_sum  $tmp/gh.tar.gz" | sha256sum -c - \
  && tar -xzf "$tmp/gh.tar.gz" -C "$tmp" \
  && install -m 0755 "$tmp/gh_${GH_VERSION}_linux_$TARGETARCH/bin/gh" /usr/local/bin/gh \
  && curl --proto =https -fsSL -o "$tmp/go.tar.gz" "https://go.dev/dl/go$GO_VERSION.linux-$TARGETARCH.tar.gz" \
  && echo "$go_sum  $tmp/go.tar.gz" | sha256sum -c - \
  && tar -xzf "$tmp/go.tar.gz" -C /usr/local --no-same-owner \
  && rm -rf "$tmp" \
  && corepack enable \
  && npm install -g --no-fund --no-audit "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
  && npm cache clean --force \
  && rm -rf /root/.npm /tmp/*
# What a box needs (as the default's build checks) and what an agent runs: fail the build without any.
RUN for t in sh bash timeout awk sleep cat kill ps perl git nc ip ping \
             gh node npm npx corepack go gofmt python3 pip3 rg jq make gcc g++ claude codex; do \
      command -v "$t" >/dev/null || { echo "agent image lacks $t" >&2; exit 1; }; \
    done \
  && python3 -m venv --help >/dev/null \
  && test -x /home/runner/run.sh
USER runner
WORKDIR /home/runner
# The CLIs' updaters would rewrite the pinned install in a box nobody keeps: off.
ENV DISABLE_AUTOUPDATER=1
RUN sudo -n true && claude --version && codex --version && node --version && go version && gh --version && python3 --version
