package tailnet

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// core.NetService: a team replica dials its authority through this daemon's
// own node and asks the node who answered (SPEC 17.4). Nothing here decides
// who may do what.

var _ core.NetService = netService{}

// Net is the service as a core.NetService.
func (s *Service) Net() core.NetService { return netService{s} }

type netService struct{ *Service }

func (n netService) Up(ctx context.Context, authKey string, wait time.Duration) (api.TailnetStatus, error) {
	return n.UpAndWait(ctx, authKey, wait)
}

// node is the running node, or an unavailable error.
func (s *Service) node() (Node, error) {
	s.mu.Lock()
	r := s.run
	s.mu.Unlock()
	if r == nil {
		return nil, api.Errorf(api.CodeUnavailable, "this machine's tailnet node is not running").WithHint("zm tailnet up")
	}
	return r.node, nil
}

// Dial implements core.NetService.
func (s *Service) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	n, err := s.node()
	if err != nil {
		return nil, err
	}
	return dial(n, ctx, network, addr)
}

// Peer implements core.NetService.
func (s *Service) Peer(ctx context.Context, remoteAddr string) (core.Remote, error) {
	n, err := s.node()
	if err != nil {
		return core.Remote{}, err
	}
	who, err := n.WhoIs(ctx, remoteAddr)
	if err != nil {
		return core.Remote{}, err
	}
	return core.Remote{Login: who.Login, Node: who.Node, NodeID: who.NodeID, NodeKey: who.NodeKey, Tagged: who.Tagged, OS: who.OS}, nil
}

// Self implements core.NetService.
func (s *Service) Self(ctx context.Context) (core.NetSelf, error) {
	s.mu.Lock()
	r := s.run
	s.mu.Unlock()
	if r == nil {
		return core.NetSelf{}, nil
	}
	st, err := r.node.Status(ctx)
	if err != nil {
		return core.NetSelf{Hostname: r.cfg.Hostname}, err
	}
	out := core.NetSelf{Running: st.State == StateRunning && r.serving.Load(), NodeID: st.SelfID, Hostname: r.cfg.Hostname,
		DNSName: st.SelfDNS, HTTPS: r.https.Load()}
	if st.State != StateRunning {
		out.AuthURL = st.AuthURL
	}
	for _, p := range st.Peers {
		out.Peers = append(out.Peers, p.Host)
		out.PeerList = append(out.PeerList, core.NetPeer{Host: p.Host, DNSName: strings.TrimSuffix(p.DNSName, "."), NodeID: p.ID,
			NodeKey: p.Key, Login: p.Login, OS: p.OS, Tagged: p.Tagged, Relays: p.Relays, Shared: p.Shared})
	}
	return out, nil
}

// UpAndWait implements core.NetService's Up: zm tailnet up from inside the daemon (zm
// join runs it first), saving tailnet.enabled once the node started.
func (s *Service) UpAndWait(ctx context.Context, authKey string, wait time.Duration) (api.TailnetStatus, error) {
	req := api.TailnetUpRequest{AuthKey: authKey}
	if err := s.up(req); err != nil {
		return s.Status(ctx), err
	}
	st := s.waitStatus(ctx, wait)
	if st.Error != "" && !st.Running {
		return st, api.Errorf(api.CodeUnavailable, "tailnet: %s", st.Error)
	}
	if err := s.saveEnabled(true, req); err != nil {
		return st, err
	}
	return st, nil
}
