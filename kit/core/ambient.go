package core

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// A mapped tailnet login is an ambient credential: the person's browser
// presents it (the node's WhoIs) with every request any page makes to this
// node, the way a cookie would. So a request that carries a WhoIs login and
// no token must also show that a zm client sent it on purpose (SPEC section
// 1) before the login is even looked up:
//
//   - the Host names this node (OwnTailnetHost): a page whose own name was
//     rebound to the node's address (DNS rebinding) is refused, reads included;
//   - nothing cross-site: Sec-Fetch-Site, when sent, is same-origin or none, and
//     an Origin, when sent, names this Host;
//   - every write (not GET, HEAD or OPTIONS) carries Zm-Protocol, a header no
//     page can add to a cross-origin request without a CORS preflight, which
//     the daemon never grants.
//
// Every refusal is the same 401 whether or not the login is mapped, so a
// rebound page cannot learn that either. Token callers are not checked: a
// bearer token is never sent by itself.
func (d *Deps) ambientGuard(r *http.Request) error {
	refuse := func(msg string) error {
		return api.Errorf(api.CodeUnauthenticated, "%s", msg).
			WithHint("a tailnet login works from zm clients at this node's own name; scripts send Potluck-Protocol: potluck/1, browsers sign in with a token")
	}
	if !d.OwnTailnetHost(r.Context(), r.Host) {
		return refuse("a tailnet login is accepted only at this node's own tailnet name or address")
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return refuse("cross-site request refused")
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || u.User != nil || normHost(u.Host) != normHost(r.Host) {
			return refuse("cross-origin request refused")
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if api.GetHeader(r.Header, api.HeaderProtocol) == "" {
			return refuse("a write by a tailnet login needs the " + api.HeaderProtocol + " header")
		}
	}
	return nil
}

// hostOnly is the host of host[:port], lower case, unbracketed, without a
// trailing dot.
func hostOnly(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
}

// normHost is hostOnly plus the port, if any.
func normHost(hostport string) string {
	if _, port, err := net.SplitHostPort(hostport); err == nil && port != "" {
		return net.JoinHostPort(hostOnly(hostport), port)
	}
	return hostOnly(hostport)
}

var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// OwnTailnetHost reports whether a request's Host (host or host:port) names
// this daemon's tailnet node, exactly: a Tailscale address, or the node's
// names. Once the node is up those are the name the tailnet gave it (bare
// and its full MagicDNS name: a taken zm-mini becomes zm-mini-1, and a
// device asking for zm-mini then resolves it somewhere else); before that,
// zm-<machine> and the configured node name. A prefix never counts:
// "zm-mini.attacker.example" is not this node.
func (d *Deps) OwnTailnetHost(ctx context.Context, hostport string) bool {
	host := hostOnly(hostport)
	if host == "" {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return tailscaleV4.Contains(ip.Unmap()) || tailscaleV6.Contains(ip)
	}
	for _, n := range d.tailnetNames() {
		if host == n {
			return true
		}
	}
	return false
}

// hostNames caches the node's names: asking the node is a local round trip,
// too much for every request.
type hostNames struct {
	mu         sync.Mutex
	names      []string // from the node; nil until it has a MagicDNS name
	at         time.Time
	refreshing bool
}

const (
	hostNamesTTL   = 30 * time.Second
	hostNamesRetry = 2 * time.Second // while the node has no name yet, or failed
)

func (d *Deps) staticNames() []string {
	m := strings.ToLower(d.Config.Machine)
	names := []string{api.NodePrefixNew + m, api.NodePrefix + m}
	if h := strings.ToLower(strings.TrimSpace(d.Config.File.Tailnet.Hostname)); h != "" {
		names = append(names, h)
	}
	return names
}

// tailnetNames returns the node's names. One caller at a time refreshes a
// stale cache, outside the lock and on its own short deadline; the others
// keep using the last answer meanwhile. A failed refresh keeps the old names.
func (d *Deps) tailnetNames() []string {
	if d.Net == nil {
		return d.staticNames()
	}
	c := &d.hosts
	c.mu.Lock()
	ttl := hostNamesTTL
	if c.names == nil {
		ttl = hostNamesRetry
	}
	if c.refreshing || (!c.at.IsZero() && time.Since(c.at) < ttl) {
		names := c.names
		c.mu.Unlock()
		if names == nil {
			return d.staticNames()
		}
		return names
	}
	c.refreshing = true
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	self, err := d.Net.Self(ctx)
	cancel()
	var fresh []string
	if full := strings.TrimSuffix(strings.ToLower(self.DNSName), "."); err == nil && full != "" {
		fresh = []string{full}
		if i := strings.IndexByte(full, '.'); i > 0 {
			fresh = append(fresh, full[:i])
		}
	}
	c.mu.Lock()
	c.refreshing, c.at = false, time.Now()
	if fresh != nil {
		c.names = fresh
	} else if err == nil && self.DNSName == "" {
		c.names = nil // the node is down or not logged in: the static names again
	} // an error keeps the last names
	names := c.names
	c.mu.Unlock()
	if names == nil {
		return d.staticNames()
	}
	return names
}
