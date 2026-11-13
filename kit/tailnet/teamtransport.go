package tailnet

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// The tailnet's team transport (core.TeamTransport, SPEC 17.4): a member
// dials the home through this daemon's own node, and every new connection's
// peer must be the node the join code pinned (WhoIs on the dialled peer,
// before a byte of the request goes out).

// Tunables (tests shorten them).
var (
	// NodeUpWait: how long after this daemon starts a dial waits for its
	// tailnet node to come up (seconds, on a real node) before dialling.
	NodeUpWait = 15 * time.Second
	// DefaultJoinWait: how long a join waits for the node each try.
	DefaultJoinWait = 10 * time.Second
)

// TeamTransport is the tailnet's driver over n (this daemon's node).
func TeamTransport(d *core.Deps, n core.NetService) core.TeamTransport {
	started := d.Started
	if started.IsZero() {
		started = time.Now()
	}
	return &teamTransport{d: d, net: n, started: started}
}

type teamTransport struct {
	d       *core.Deps
	net     core.NetService
	started time.Time
}

var _ core.TeamTransport = (*teamTransport)(nil)

func (t *teamTransport) Via() string { return core.ViaTailnet }

// DialNode dials addr through n. When a short tailnet name (zm-host-c)
// does not resolve, it dials instead the peer that reports that host name
// and whose node is fingerprint (the join code's pin): a node shared in
// from another tailnet resolves only by its full MagicDNS name (zm-my5).
// Host names are self-reported, so two peers may share one; the pin picks,
// never the order peers are listed in. Any other failure (a refused
// connection: the home is down) is answered as it is.
func DialNode(ctx context.Context, n core.NetService, network, addr, fingerprint string) (net.Conn, error) {
	c, err := n.Dial(ctx, network, addr)
	if err == nil || fingerprint == "" || !nameNotFound(err) {
		return c, err
	}
	host, port, perr := net.SplitHostPort(addr)
	if perr != nil || strings.Contains(host, ".") {
		return nil, err
	}
	self, serr := n.Self(ctx)
	if serr != nil {
		return nil, err
	}
	for _, p := range self.PeerList {
		if strings.EqualFold(p.Host, host) && p.DNSName != "" && p.NodeID != "" && project.NodeFingerprint(p.NodeID) == fingerprint {
			return n.Dial(ctx, network, net.JoinHostPort(p.DNSName, port))
		}
	}
	return nil, err
}

// nameNotFound reports a dial that failed resolving the name (not found,
// a resolver timeout or SERVFAIL: an offline Mac, a sandboxed resolver),
// never one refused by the node: the fallback reaches only the pinned node.
func nameNotFound(err error) bool {
	var dns *net.DNSError
	return errors.As(err, &dns) || strings.Contains(err.Error(), "no such host")
}

// URLFor is the base URL of the home zm-<machine> on the tailnet: a node
// made before the rename's step 2 (a new one is potluck-<machine>; a join
// code names its own).
func URLFor(machine string) string { return "http://" + HostPrefixOld + machine + ":7433" }

func (t *teamTransport) ParseCode(code string) (core.JoinTarget, bool, error) {
	if c := strings.TrimSpace(code); !strings.HasPrefix(c, api.NodePrefix) && !strings.HasPrefix(c, api.NodePrefixNew) {
		return core.JoinTarget{}, false, nil
	}
	c, err := project.ParseJoinCode(code)
	if err != nil {
		return core.JoinTarget{}, true, err
	}
	return core.JoinTarget{Via: core.ViaTailnet, URL: "http://" + c.AuthorityHost() + ":7433", Authority: c.Machine, Fingerprint: c.Fingerprint,
		Secret: c.Secret, Code: code}, true, nil
}

// Prepare is a join's steps 1 and 2 (SPEC 17.4): this daemon's own node
// up, and potluck-<machine> (an older node's zm-<machine>) its name on the
// tailnet.
func (t *teamTransport) Prepare(ctx context.Context, req api.JoinRequest, machine string, target core.JoinTarget) (string, string, error) {
	if target.Authority != "" && target.Authority == t.d.Config.Machine {
		return "", "", api.Errorf(api.CodeBadRequest, "this join code is for %s, this machine", target.Authority).
			WithHint("run zm join on the member's machine")
	}
	if t.net == nil {
		return "", "", api.Errorf(api.CodeUnavailable, "this daemon has no tailnet module")
	}
	wait := DefaultJoinWait
	if req.WaitMS > 0 {
		wait = time.Duration(req.WaitMS) * time.Millisecond
	}
	st, err := t.net.Up(ctx, req.AuthKey, wait)
	if err != nil {
		return "", "", err
	}
	if !st.Running {
		if st.AuthURL != "" {
			return st.AuthURL, "log in to Tailscale at the URL, then run zm join again", nil
		}
		return "", "", api.Errorf(api.CodeUnavailable, "the tailnet node did not come up within %s", wait).WithHint("zm tailnet (status), then zm join again")
	}
	self, err := t.net.Self(ctx)
	if err != nil {
		return "", "", err
	}
	want := self.Hostname // the name the node asked for
	if _, m, ok := api.NodeMachine(want); !ok || m != machine {
		want = HostPrefix + machine
	}
	if got, _, _ := strings.Cut(self.DNSName, "."); got != "" && got != want {
		return "", "", api.Errorf(api.CodeConflict, "%s is taken on this tailnet (this node got %s)", want, got).
			WithHint("join under another machine name: --machine <name> (after setting it as this daemon's machine)")
	}
	return "", "", nil
}

func (t *teamTransport) Extra(req api.JoinRequest) (http.Header, error) {
	if req.AccessID != "" || req.AccessSecret != "" {
		return nil, api.Errorf(api.CodeBadRequest, "a Cloudflare Access token is for a tunnel code (https://...)")
	}
	return nil, nil
}

func (t *teamTransport) RedeemRequest(target core.JoinTarget, machine string) (api.RedeemRequest, error) {
	return api.RedeemRequest{Code: target.Code, Machine: machine}, nil
}

// Client dials through this daemon's own node and refuses any connection
// whose peer is not the pinned node (SPEC 17.4: checked on every call;
// connections are reused, so every new one is checked).
func (t *teamTransport) Client(target core.JoinTarget, opts core.ClientOptions) *http.Client {
	fp := target.Fingerprint
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if t.net == nil {
				return nil, errors.New("no tailnet module")
			}
			t.waitNode(ctx)
			if opts.DialTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opts.DialTimeout)
				defer cancel()
			}
			c, err := DialNode(ctx, t.net, network, addr, fp)
			if err != nil {
				return nil, err
			}
			peer, err := t.net.Peer(ctx, c.RemoteAddr().String())
			if err != nil || peer.NodeID == "" || project.NodeFingerprint(peer.NodeID) != fp {
				c.Close()
				return nil, core.ErrPinMismatch
			}
			if opts.Peer != nil {
				opts.Peer(fp, peer.Login)
			}
			return c, nil
		},
		MaxIdleConns: 32, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
	}
	// Every call names this daemon's seal key and seals what it is asked to
	// (seal.go); a run-target body with secrets never goes in the clear.
	var next http.RoundTripper = tr
	if opts.Extra != nil {
		next = headerTransport{tr, opts.Extra}
	}
	st := newSealTransport(t.d, next)
	st.fp = fp
	st.self = func(r *http.Request) string {
		if t.net == nil {
			return ""
		}
		self, err := t.net.Self(r.Context())
		if err != nil {
			return ""
		}
		return self.NodeID
	}
	return &http.Client{Transport: st}
}

// headerTransport adds headers to every request (unused by the tailnet's
// own joins; kept so a remote with stored headers still sends them).
type headerTransport struct {
	*http.Transport
	h http.Header
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.h {
		r.Header[k] = v
	}
	return h.Transport.RoundTrip(r)
}

// NoReuse is the same transport without connection reuse (a hedge).
func (h headerTransport) NoReuse() http.RoundTripper {
	c := h.Transport.Clone()
	c.DisableKeepAlives = true
	return headerTransport{c, h.h}
}

// waitNode holds a dial while this daemon has just started and its node is
// not up yet, so a call made in those seconds (bd has no copy to fall back
// on) does not fail for nothing. Nothing has been sent, so a write may wait
// too. A node that is up never waits; a node that will not come up costs
// the wait once, in a daemon's first seconds.
func (t *teamTransport) waitNode(ctx context.Context) {
	until := t.started.Add(NodeUpWait)
	for time.Now().Before(until) {
		if self, err := t.net.Self(ctx); err == nil && self.Running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (t *teamTransport) Explain(url string, err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae // the transport's own refusal (sealing: an older home, or secrets it was not asked to seal)
	}
	if errors.Is(err, core.ErrPinMismatch) || strings.Contains(err.Error(), core.ErrPinMismatch.Error()) {
		return api.Errorf(api.CodeForbidden, "%s", core.ErrPinMismatch.Error()).
			WithHint("the join code names another node than the one answering under its name; ask for a new invite")
	}
	return api.Errorf(api.CodeUnavailable, "authority %s unreachable: %v", url, err).
		WithHint("check both machines are on the tailnet: zm tailnet, zm peers")
}

// Invite names this node as the tailnet named it: a replica dials
// zm-<machine>, and a duplicate gets renamed (zm-mini-1), so a code naming
// zm-mini would reach the other node and fail its fingerprint check.
func (t *teamTransport) Invite(ctx context.Context) (core.InviteTarget, error) {
	down := api.Errorf(api.CodeUnavailable, "this machine's tailnet node is not running").
		WithHint("a join code names the authority's tailnet node: run zm tailnet up first (or set tunnel.url and use --via tunnel)")
	if t.net == nil {
		return core.InviteTarget{}, down
	}
	self, err := t.net.Self(ctx)
	if err != nil || !self.Running || self.NodeID == "" {
		return core.InviteTarget{}, down
	}
	name, _, _ := strings.Cut(self.DNSName, ".")
	if name == "" {
		name = self.Hostname
	}
	if want := self.Hostname; want != "" && name != want {
		return core.InviteTarget{}, api.Errorf(api.CodeConflict, "%s is taken on this tailnet (this node got %s): a join code naming it would not reach this machine", want, name).
			WithHint("in the Tailscale admin console, remove or rename the other " + want + " and give this node the name " + want + ", then zm invite again")
	}
	// The code carries the node's full MagicDNS name: a node shared in from
	// another tailnet resolves only by it (zm-my5).
	authority := name
	for _, p := range api.NodePrefixes {
		if m, ok := strings.CutPrefix(name, p); ok {
			authority = m
			break
		}
	}
	return core.InviteTarget{Authority: authority, Host: strings.TrimSuffix(self.DNSName, "."), Node: name,
		Fingerprint: project.NodeFingerprint(self.NodeID)}, nil
}

func (t *teamTransport) RateKey(rm *core.Remote) string { return rm.NodeID }

func (t *teamTransport) RedeemInput(ctx context.Context, rm *core.Remote, req api.RedeemRequest) project.RedeemInput {
	in := project.RedeemInput{Node: rm.NodeID, Tagged: rm.Tagged, Machine: req.Machine, Secret: project.NormalizeJoinSecret(req.Code)}
	if c, err := project.ParseJoinCode(req.Code); err == nil {
		in.Secret = c.Secret
	}
	return in
}
