package core

import (
	"context"
	"testing"
)

// OverTunnel fails closed: only the socket and a tailnet node WhoIs named
// are not a tunnel; no Remote at all (an unknown transport), the tunnel,
// and a Remote with no node id are.
func TestOverTunnel(t *testing.T) {
	bg := context.Background()
	for name, c := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"the socket":           {WithLocal(bg), false},
		"a tailnet node":       {WithRemote(bg, Remote{NodeID: "nABC"}), false},
		"the tunnel":           {WithRemote(bg, Remote{Via: ViaTunnel, NodeID: "zmkey:x"}), true},
		"no Remote":            {bg, true},
		"a Remote with no id":  {WithRemote(bg, Remote{}), true},
		"an unknown transport": {WithRemote(bg, Remote{Via: "carrier-pigeon", NodeID: "n1"}), true},
		"socket then remote":   {WithRemote(WithLocal(bg), Remote{Via: ViaTunnel, NodeID: "k"}), true},
	} {
		if got := OverTunnel(c.ctx); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
