package core

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// fakeNet is a node that only describes itself, counting the questions.
type fakeNet struct {
	dns   string
	fail  bool
	calls atomic.Int32
}

func (n *fakeNet) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("no")
}
func (n *fakeNet) Peer(context.Context, string) (Remote, error) { return Remote{}, errors.New("no") }
func (n *fakeNet) Self(context.Context) (NetSelf, error) {
	n.calls.Add(1)
	if n.fail {
		return NetSelf{}, errors.New("status: timeout")
	}
	return NetSelf{Running: n.dns != "", Hostname: "zm-mini", DNSName: n.dns}, nil
}
func (n *fakeNet) Up(context.Context, string, time.Duration) (api.TailnetStatus, error) {
	return api.TailnetStatus{}, nil
}

func TestOwnTailnetHost(t *testing.T) {
	ctx := context.Background()
	// The tailnet renamed this node zm-mini-1 (zm-mini was taken).
	n := &fakeNet{dns: "zm-mini-1.tail1234.ts.net."}
	d := &Deps{Config: &config.Config{Machine: "Mini", File: config.File{Tailnet: config.Tailnet{Hostname: "zm-box"}}}, Net: n}
	for host, want := range map[string]bool{
		// the tailnet gave the node zm-mini-1: zm-mini is someone else's now (a
		// device resolving it falls through to its LAN search domain)
		"zm-mini": false, "zm-mini:7433": false, "zm-box:7433": false,
		"zm-mini-1:7433": true, "ZM-MINI-1.:7433": true, "zm-mini-1.tail1234.ts.net:7433": true, "zm-mini-1.tail1234.ts.net.": true,
		"100.64.0.1:7433": true, "100.127.255.254": true, "[fd7a:115c:a1e0::5]:7433": true,
		// a name that starts with the node's, or ends like it, is someone else's
		"zm-mini.attacker.example": false, "zm-mini.tail1234.ts.net": false, "evilzm-mini": false,
		"zm-mini-1.tail1234.ts.net.attacker.example": false, "attacker.example": false, "": false,
		"100.128.0.1": false, "127.0.0.1:7433": false, "[::1]:7433": false, "192.168.1.9": false, "[fd7a:115c:a1e1::5]": false,
	} {
		if got := d.OwnTailnetHost(ctx, host); got != want {
			t.Errorf("OwnTailnetHost(%q) = %v, want %v", host, got, want)
		}
	}
	// The node is asked at most once per TTL, not per request.
	if c := n.calls.Load(); c != 1 {
		t.Fatalf("asked the node %d times", c)
	}
	// Without a node only zm-<machine>, the configured name and addresses count.
	d2 := &Deps{Config: &config.Config{Machine: "mini", File: config.File{Tailnet: config.Tailnet{Hostname: "zm-box"}}}}
	if !d2.OwnTailnetHost(ctx, "zm-mini:7433") || !d2.OwnTailnetHost(ctx, "zm-box") || d2.OwnTailnetHost(ctx, "zm-mini.tail1234.ts.net") {
		t.Fatal("no node: wrong answer")
	}
	// A failed refresh keeps the names the node gave.
	d.hosts.mu.Lock()
	d.hosts.at = time.Now().Add(-time.Hour)
	d.hosts.mu.Unlock()
	n.fail = true
	if !d.OwnTailnetHost(ctx, "zm-mini-1:7433") || d.OwnTailnetHost(ctx, "zm-mini:7433") {
		t.Fatal("a failed refresh changed the names")
	}
	// A node not up yet is asked again soon, so its MagicDNS name works
	// within seconds of login.
	n3 := &fakeNet{}
	d3 := &Deps{Config: &config.Config{Machine: "mini"}, Net: n3}
	if d3.OwnTailnetHost(ctx, "zm-mini.tail9.ts.net") {
		t.Fatal("an unknown name before login")
	}
	if !d3.OwnTailnetHost(ctx, "zm-mini") {
		t.Fatal("before login the static name works")
	}
	n3.dns = "zm-mini.tail9.ts.net."
	d3.hosts.mu.Lock()
	d3.hosts.at = d3.hosts.at.Add(-3 * time.Second)
	d3.hosts.mu.Unlock()
	if !d3.OwnTailnetHost(ctx, "zm-mini.tail9.ts.net") {
		t.Fatal("the MagicDNS name after login")
	}
}

func TestAmbientGuardOrigin(t *testing.T) {
	d := &Deps{Config: &config.Config{Machine: "mini"}}
	for origin, ok := range map[string]bool{"http://zm-mini:7433": true, "http://ZM-MINI.:7433": true, "http://zm-mini:7434": false,
		"http://u@zm-mini:7433": false, "null": false, "http://evil.example": false, "zm-mini:7433": false} {
		r := httptest.NewRequest("POST", "/v0/work", nil)
		r.Host = "zm-mini:7433"
		r.Header.Set(api.HeaderProtocol, api.Protocol)
		r.Header.Set("Origin", origin)
		if err := d.ambientGuard(r); (err == nil) != ok {
			t.Errorf("Origin %q: %v", origin, err)
		}
	}
}
