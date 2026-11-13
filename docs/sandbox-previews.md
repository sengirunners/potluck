# Sandbox ports and HTTPS previews

Private access uses `computeclient.DialRunTargetPort(ctx, client, target, id,
port)`. It returns `net.Conn`; keep the context alive and close the connection
when done. Changing, extending or clearing read/write deadlines affects pending
operations as well as future calls. Browser/CDP integrations use this same transport. No port is public
by default. Only the lease's holder can open or operate channels.

Publish an application explicitly:

```sh
potluck sandbox preview publish <sandbox-id> 3000 --public
potluck sandbox preview ls <sandbox-id>
potluck sandbox preview revoke <sandbox-id> <preview-id>
```

The publish result includes a unique HTTPS hostname, port and expiration. Anyone
with the URL can access the application until revocation, the publication's
lease deadline, stop, release or loss of the sandbox. Renewing a lease does not
extend an already published URL: publish again for the new deadline. Each URL
has its own origin. Root-relative assets, redirects, HTTP streaming and
WebSocket upgrades retain their paths and query strings. The application must
listen on guest loopback or all interfaces. Ordinary dev-server hostname
allowlists may need to allow the configured preview domain.

The MCP tools are `sandbox_preview_publish` (requires `public:true`),
`sandbox_preview_list`, and `sandbox_preview_revoke`. Their handle is the
`box:<target>/<id>` returned by the sandbox tools.

## Existing-domain ingress setup

The daemon owner must first provide an existing owned domain with wildcard DNS
and an already available wildcard HTTPS certificate. This feature does not
purchase a domain, account or certificate. A Cloudflare Universal SSL wildcard
normally covers one hostname label below its zone; a nested preview domain
needs existing certificate coverage of that depth.

The existing named Cloudflare tunnel and loopback listener serve previews.
`cfsetup.Setup.PreviewDomain` opts into routing `*.<domain>` to that same tunnel,
records the domain in its existing state, and writes the wildcard ingress after
the exact authority/broker host rules and before the final 404. Preview ingress
preserves the original Host; the authority's rule keeps its edge-secret Host.
No extra proxy service or listener is required. Existing cfsetup callers can
pass this field when running their normal setup.

For an already managed tunnel, the equivalent reviewed changes are:

```yaml
# Add after the existing exact API/broker hostname rules:
- hostname: '*.example.com'
  service: http://127.0.0.1:7435
# Keep the existing final http_status:404 rule after this.
```

Route the wildcard DNS record to that existing named tunnel using the existing
Cloudflare login, then validate/reload only that tunnel's owned configuration.
After verifying wildcard TLS and ingress, configure the running daemon:

```sh
potluck sandbox preview configure example.com
```

This only sets `tunnel.preview_domain`; it does not change DNS or TLS. The local
owner API is `POST /v0/tunnel/preview-domain {"domain":"example.com"}`. Empty
domain disables new publishing and matching public ingress. `POST
/v0/tunnel/up` and `cfsetup.Setup` also accept the domain. The listener follows
the live domain without a daemon restart. Public preview entries are memory
only; a daemon restart revokes every old URL. Removing the tunnel also clears
the domain.

Before any live rollout, inspect the actual owned domain, named tunnel,
certificate coverage and existing ingress file. Preserve its exact authority,
broker and final refusal rules. Test an owned throwaway application only;
then revoke its URL and release that sandbox.

## Transport and limits

`POST /v0/run_targets/{target}/handles/{id}/port` takes `protocol.PortRequest`:
`open` with `port`, then `read`, `write` or `close` with the returned opaque
`stream`. Pages are at most 64 KiB; binary bytes use JSON base64. Reads can
answer an empty page; EOF can include a final page. Never retry a write or read
automatically after an ambiguous network failure.

The driver connects only to `127.0.0.1` inside that sandbox through its guest
execution channel. Callers cannot choose a hostname, host-network address,
metadata endpoint, LAN target or another sandbox. A private guest Unix socket
retains each TCP connection between bounded driver/relay calls. Its helper ends
on close, the original lease deadline, or 60 seconds without a channel call.
Independent read/write workers preserve full-duplex progress under opposing
backpressure; each direction has at most one active worker and one queued page.
Socket send/receive queues are set before connect and verified to remain bounded;
no host-wide TCP setting changes.
Read EOF leaves writes available until the holder closes the channel.
The producer binds every channel to its project, sandbox and original start-or-create
lease identity, so restarting a kept disk never revives an old URL or channel. It checks the holder
and ready lease on every page, and caps it at 32 channels per sandbox and 128
channels per daemon. There are at most 128 public previews per daemon.

HTTP requests have an 8 MiB body limit, 64 KiB response-header limit and 15 second
response-header timeout. Guest commands and socket operations have bounded
timeouts and output. A lease-end hook and absolute deadlines revoke URLs and
cancel streams. The VM/container teardown removes guest helpers. Authenticated
private calls and public proxy calls use one channel registry and one relay.
Public requests never enter the signed daemon API, and daemon credentials are
not forwarded to the application or retained in published routes.
