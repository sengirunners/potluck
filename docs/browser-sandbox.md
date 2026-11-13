# Browser testing in a sandbox

The browser image adds Playwright 1.65.0 and its matching Chromium to
`agent`. The OCI image supports linux/amd64 and linux/arm64. The `browser`
template selects the published immutable browser image in `imagepins/images.pin`.
The existing dispatch-only `box-images` workflow builds the ordered chain:
default Linux → agent → browser. Each child receives its freshly published
parent digest, so browser includes Linux helper updates from the same source
commit. The browser pin names its complete built image; changing it does not
also change the default Linux or agent template pins.

Chromium must pass acceptance with its sandbox enabled in the host's selected
boundary runtime. An image build or successful installation check does not
prove nested namespaces, isolation, lease cleanup or browser execution.

For an exact-head staging build through the same workflow, dispatch
`box-images.yml` with `source_commit` set to the reviewed full commit. Staging
publishes all three images with `staging-<full-commit>` tags and prints their
immutable digests. It never writes or pushes a pin branch. After runtime
acceptance, a reviewed pin change can select the new immutable image. No write
to main is needed to publish a reviewed staging artifact. Authentication, builder
setup and both-platform checks remain in the one workflow; the shared
`scripts/images/publish-box-image.sh` step only builds and returns a digest.
Retain the source commit, digests and job log for activation review.

Create a Linux sandbox with `--template browser` on a host lending that image.
Use the existing sandbox exec and background-process commands to run:

```sh
potluck-browser screenshot http://127.0.0.1:3000 /home/runner/app.png
potluck-browser serve 9222 3600
```

The first command visits the app in a new browser and saves a full-page PNG.
Copy it out with sandbox file transfer. The second keeps Chromium available
on **guest loopback only**, for at most the supplied number of seconds
(default one hour, maximum one day). SIGTERM/SIGINT close Chromium. The
sandbox lease remains the outer lifetime: release or expiry destroys all
browser processes and temporary profiles with the guest. Start the app and
browser in the same sandbox; port 3000 above is the app's own listener.

Installed scripts can import Playwright from
`/opt/potluck-browser/node_modules/playwright/index.mjs` and launch with
`chromiumSandbox: true`. Use `chromium.connectOverCDP('http://127.0.0.1:9222')`
from a sandbox-local client. For remote clients, use the existing authorized
lease-scoped port attachment for 9222; raw CDP must never be exposed as a public
preview. CDP gives full browser control. It is not a read-only interface;
no read-only desktop grant is claimed by this template. It carries no external
credentials and the helper prints only the loopback host, port and readiness.

Chromium runs as `runner` with its own sandbox enabled. The selected OCI
boundary runtime must permit Chromium's nested user namespaces. Failure to
start is a runtime failure: do not add `--no-sandbox`, host networking,
privileged mode or relax the host isolation policy to pass the test. An
ordinary Docker image installation check is not a runtime conformance proof.

Run the real local-app/CDP/click/screenshot test inside the selected sandbox:

```sh
node /opt/potluck-browser/smoke.mjs
SCREENSHOT_PATH=/home/runner/browser-smoke.png node /opt/potluck-browser/smoke.mjs
BROKEN_APP=1 node /opt/potluck-browser/smoke.mjs
```

The first two must succeed; the last must fail its `Passed` assertion. The
script owns and closes its app server and browser. Do not run it concurrently
with another listener on 9222. Release the test sandbox afterward and verify
the lease and its container are gone.

A live desktop viewer with separate read-only/control authorization is not
implemented here. That needs a desktop compositor/capture service, authenticated
viewer attachment and enforcement of input permissions, using the same generic
attachment transport. Headless CDP and PNG screenshots do not satisfy that
separate desktop requirement.
