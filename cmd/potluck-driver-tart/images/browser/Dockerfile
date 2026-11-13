# Published through box-images.yml after its freshly built agent. Both supported Linux architectures use
# the same locked Playwright package and its matching Chromium revision.
ARG BASE
FROM ${BASE}
ARG TARGETARCH
USER root
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/potluck-browsers
WORKDIR /opt/potluck-browser
COPY package.json package-lock.json ./
RUN case "$TARGETARCH" in amd64|arm64) ;; *) echo "unsupported browser architecture" >&2; exit 1 ;; esac \
    && npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
    && node node_modules/playwright/cli.js install --with-deps chromium \
    && chmod -R a+rX /opt/potluck-browsers \
    && rm -rf /var/lib/apt/lists/* /root/.npm
COPY browser.mjs smoke.mjs ./
RUN printf '#!/bin/sh\nexec node /opt/potluck-browser/browser.mjs "$@"\n' > /usr/local/bin/potluck-browser \
    && chmod 0755 /usr/local/bin/potluck-browser
USER runner
WORKDIR /home/runner
