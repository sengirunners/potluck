# zm's default Linux box image (zm-eakh.12.23.6): what `zm host share` and `zm pool host` lend on the oci layer
# when no --image is given. .github/workflows/box-images.yml builds it for linux/amd64 and linux/arm64, pushes
# ghcr.io/sengirunners/potluck-box-linux and writes its digest into imagepins/images.pin.
#
# The base is GitHub's own runner image (github.com/actions/runner, images/Dockerfile; MIT): Ubuntu 24.04 with
# .NET's runtime-deps (the runner's prerequisites), git, sudo and the runner in /home/runner (a box's
# runner_dir). It lacks what zm's box checks need, so this adds nc (netcat-openbsd), ip (iproute2), ping
# (iputils-ping) and ps (procps). Bump the base by its digest: ghcr.io/actions/actions-runner:<version>.
#
# Docker in a box (zm-eakh.12.5.26, a binding with docker=on): the base already has Docker's static binaries
# (dockerd, containerd, runc, docker-init) and the runner's subuid/subgid range; this adds what rootless dockerd
# needs beside them: newuidmap (uidmap), slirp4netns, and Docker's rootless extras (rootlesskit,
# dockerd-rootless.sh) of the same version as the base's dockerd, pinned by SHA-256, plus zm-dockerd, which
# starts it. Bump DOCKER_VERSION and both sums with the base:
# https://download.docker.com/linux/static/stable/<x86_64|aarch64>/docker-rootless-extras-<version>.tgz
ARG BASE=ghcr.io/actions/actions-runner:2.338.2@sha256:759a1d94a15b05941aba5196974ff43fe15b4cbba29b694b35cbc32a1166b232
# Ubuntu's uidmap is built without capability support. Its setuid helpers
# become UID 0 and cannot administer a runner-owned child user namespace
# without outer SYS_ADMIN. Build the same patched source with libcap headers:
# shadow retains SETUID/GID while returning to the namespace owner's UID.
# Keep these helpers separate so gVisor retains its existing uidmap package.
FROM ${BASE} AS native-idmap
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG SHADOW_SOURCE=4.13+dfsg1-4ubuntu3.2
ARG SHADOW_DSC_SHA256=0be17fd044f3e23f714a5b286a04bd040f246af1ac32fcc406b63756baa9c368
ARG SHADOW_ORIG_SHA256=a8bb3a2aceff1cbe39d0f50687dcc1d7e7be0516a9d954d8e2eedb93f5906207
ARG SHADOW_PATCHES_SHA256=ff01a3e94111ca8c4dca097ebe54edbee912cbdeba042c214bfa4c34e7839f61
RUN apt-get update \
  && apt-get install -y --no-install-recommends gcc make dpkg-dev libcap-dev libcrypt-dev autoconf automake libtool gettext curl ca-certificates binutils \
  && rm -rf /var/lib/apt/lists/*
RUN mkdir /tmp/idmap && cd /tmp/idmap \
  && for name in "shadow_${SHADOW_SOURCE}.dsc" shadow_4.13+dfsg1.orig.tar.xz "shadow_${SHADOW_SOURCE}.debian.tar.xz"; do \
       curl --proto =https -fsSL -O "https://archive.ubuntu.com/ubuntu/pool/main/s/shadow/$name" || exit 1; \
     done \
  && printf '%s  %s\n' \
       "$SHADOW_DSC_SHA256" "shadow_${SHADOW_SOURCE}.dsc" \
       "$SHADOW_ORIG_SHA256" shadow_4.13+dfsg1.orig.tar.xz \
       "$SHADOW_PATCHES_SHA256" "shadow_${SHADOW_SOURCE}.debian.tar.xz" | sha256sum -c - \
  && dpkg-source --no-check -x "shadow_${SHADOW_SOURCE}.dsc" source \
  && cd source \
  && ./configure --disable-nls --disable-man --disable-shared --without-audit --without-selinux --without-acl --without-attr --without-tcb --without-libpam \
  && grep -qx '#define HAVE_SYS_CAPABILITY_H 1' config.h \
  && make -j2 -C lib && make -j2 -C libmisc && make -j2 -C src newuidmap newgidmap \
  && for name in newuidmap newgidmap; do \
       nm -D "src/$name" | grep -q ' U capset@' \
         && nm -D "src/$name" | grep -q ' U seteuid@' \
         && install -D -m 4755 "src/$name" "/out/$name" || exit 1; \
     done
FROM ${BASE}
ARG TARGETARCH
ARG DOCKER_VERSION=29.9.2
ARG ROOTLESS_SHA256_AMD64=f2663b230a7afc13dea88fc589685331d01f5f3fc458b92ab40e2388db452ed2
ARG ROOTLESS_SHA256_ARM64=6b7da07c75eee49353657fdc3b9d7e8f4abe44bdd3fd735eaa8e1b0bc3fbc797
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN apt-get update \
  && apt-get install -y --no-install-recommends netcat-openbsd iproute2 iputils-ping procps uidmap slirp4netns iptables \
  && rm -rf /var/lib/apt/lists/*
RUN case "$TARGETARCH" in \
      amd64) arch=x86_64 sum=$ROOTLESS_SHA256_AMD64 ;; \
      arm64) arch=aarch64 sum=$ROOTLESS_SHA256_ARM64 ;; \
      *) echo "no pinned rootless extras for $TARGETARCH" >&2; exit 1 ;; \
    esac \
  && dockerd --version | grep -q "version $DOCKER_VERSION," \
  && tmp=$(mktemp -d) \
  && curl --proto =https -fsSL -o "$tmp/rx.tgz" "https://download.docker.com/linux/static/stable/$arch/docker-rootless-extras-$DOCKER_VERSION.tgz" \
  && echo "$sum  $tmp/rx.tgz" | sha256sum -c - \
  && tar -xzf "$tmp/rx.tgz" -C "$tmp" \
  && install -m 0755 "$tmp/docker-rootless-extras/rootlesskit" "$tmp/docker-rootless-extras/dockerd-rootless.sh" /usr/local/bin/ \
  && rm -rf "$tmp"
COPY --chmod=0755 zm-dockerd /usr/local/bin/zm-dockerd
COPY --from=native-idmap --chmod=4755 /out/newuidmap /out/newgidmap /usr/local/libexec/zm-rootless-idmap/
# sudo keeps DOCKER_HOST (and XDG_RUNTIME_DIR), which a docker=on box sets for every exec, so `sudo docker` reaches
# the same rootless daemon. In /etc/sudoers itself: the base's has no @includedir, so sudoers.d is never read.
RUN printf 'Defaults env_keep += "DOCKER_HOST XDG_RUNTIME_DIR"\n' >> /etc/sudoers \
  && visudo -c >/dev/null \
  && [ "$(runuser -u runner -- env DOCKER_HOST=unix:///zm-check sudo -n printenv DOCKER_HOST 2>/dev/null)" = unix:///zm-check ]
# What zm-run-conformance --kind box and the exec wrapper need, and the runner: fail the build without any.
RUN for t in sh bash timeout awk sleep cat kill ps perl git nc ip ping \
             iptables ip6tables sudo docker dockerd containerd runc docker-init newuidmap newgidmap slirp4netns rootlesskit dockerd-rootless.sh zm-dockerd; do \
      command -v "$t" >/dev/null || { echo "box image lacks $t" >&2; exit 1; }; \
    done \
  && test -u "$(command -v newuidmap)" \
  && test -u /usr/local/libexec/zm-rootless-idmap/newuidmap \
  && test -u /usr/local/libexec/zm-rootless-idmap/newgidmap \
  && grep -q '^runner:' /etc/subuid && grep -q '^runner:' /etc/subgid \
  && test -x /home/runner/run.sh
USER runner
WORKDIR /home/runner
