// Package netguard keeps the daemon's outbound HTTP calls to URLs a caller
// named off this machine and its private networks (SSRF): a URL check at
// registration and a dial check after DNS resolution (rebinding). A shared
// library, not a brick: webhooks and connections call it.
package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// ErrBlockedAddress is a dial refused because the address is not public.
var ErrBlockedAddress = errors.New("refused: the address is loopback, private, link-local, tailnet or reserved")

var blockedNets = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",           // "this network"
		"10.0.0.0/8",          // private
		"100.64.0.0/10",       // CGNAT: Tailscale addresses live here
		"127.0.0.0/8",         // loopback
		"169.254.0.0/16",      // link-local, cloud metadata (169.254.169.254)
		"172.16.0.0/12",       // private
		"192.0.0.0/24",        // IETF protocol assignments
		"192.0.2.0/24",        // TEST-NET-1
		"192.88.99.0/24",      // 6to4 relay
		"192.168.0.0/16",      // private
		"198.18.0.0/15",       // benchmarking
		"198.51.100.0/24",     // TEST-NET-2
		"203.0.113.0/24",      // TEST-NET-3
		"224.0.0.0/4",         // multicast
		"240.0.0.0/4",         // reserved, broadcast
		"::/128",              // unspecified
		"::1/128",             // loopback
		"::ffff:0:0/96",       // IPv4-mapped (checked unmapped too)
		"::/96",               // IPv4-compatible (deprecated): ::127.0.0.1
		"64:ff9b::/96",        // NAT64: embeds an IPv4 address
		"64:ff9b:1::/48",      // local-use NAT64
		"100::/64",            // discard
		"2001::/32",           // Teredo: embeds an IPv4 address
		"2001:db8::/32",       // documentation
		"2002::/16",           // 6to4: embeds an IPv4 address
		"fc00::/7",            // unique local (fd00:ec2::254 is AWS metadata)
		"fe80::/10",           // link-local
		"ff00::/8",            // multicast
		"fd7a:115c:a1e0::/48", // Tailscale IPv6 (inside fc00::/7; named for clarity)
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Blocked reports whether ip may not be reached unless the caller allows
// private addresses: anything but a public unicast address.
func Blocked(ip netip.Addr) bool {
	ip = ip.WithZone("")
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	for _, p := range blockedNets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// blockedName is a host name that only ever means this machine or its
// private network, refused at creation (the dial check catches the rest).
func blockedName(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	switch {
	case h == "localhost", strings.HasSuffix(h, ".localhost"),
		strings.HasSuffix(h, ".local"), strings.HasSuffix(h, ".internal"),
		strings.HasSuffix(h, ".ts.net"), strings.HasSuffix(h, ".home.arpa"),
		h == "metadata", !strings.Contains(h, "."): // a bare name resolves through search domains
		return true
	}
	// An address in another notation (0x7f.1, 0177.0.0.1): no real top-level
	// domain is numeric.
	tld := h[strings.LastIndexByte(h, '.')+1:]
	if strings.HasPrefix(tld, "0x") || strings.Trim(tld, "0123456789") == "" {
		return true
	}
	return false
}

// CheckURL validates a URL at registration; noun names it in errors
// ("webhook url"). Without allowPrivate the URL must be https to a public
// host: an IP literal must be public and names that only mean this machine
// or its network are refused. Every dial is checked again after DNS
// resolution (rebinding), see Client.
func CheckURL(raw string, allowPrivate bool, noun string) (*url.URL, error) {
	bad := func(why string) error {
		return api.Errorf(api.CodeBadRequest, "bad %s: %s", noun, why)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n") {
		return nil, bad("empty, too long or contains spaces")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, bad("does not parse")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, bad("want an http(s) URL")
	}
	if u.User != nil {
		return nil, bad("credentials in the URL are refused")
	}
	host := u.Hostname()
	if host == "" {
		return nil, bad("no host")
	}
	if u.Fragment != "" {
		u.Fragment = ""
	}
	if allowPrivate {
		return u, nil
	}
	private := api.Errorf(api.CodeBadRequest, "%s %s is not a public address", noun, host).
		WithHint("loopback, private, link-local, tailnet and metadata addresses need --allow-private (the machine owner on its socket only)")
	if u.Scheme != "https" {
		return nil, api.Errorf(api.CodeBadRequest, "%s must be https", noun).
			WithHint("plain http is allowed only with --allow-private (the machine owner on its socket only)")
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if Blocked(ip) {
			return nil, private
		}
		return u, nil
	}
	if blockedName(host) {
		return nil, private
	}
	return u, nil
}

// Client is the HTTP client for caller-named URLs: no proxy (a proxy would dial
// for us, past the check), no redirects (a 3xx is a failed attempt, never
// followed into a private network), and, unless allowPrivate, a dialer that
// refuses every non-public address after DNS resolution.
func Client(timeout time.Duration, allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: min(timeout, 10*time.Second)}
	if !allowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrBlockedAddress
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || Blocked(ip) {
				return ErrBlockedAddress
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           func(ctx context.Context, n, a string) (net.Conn, error) { return d.DialContext(ctx, n, a) },
		TLSHandshakeTimeout:   min(timeout, 10*time.Second),
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: tr, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
