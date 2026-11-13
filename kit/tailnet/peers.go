package tailnet

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Peers lists the zm nodes this daemon has seen on the tailnet: nodes named
// zm-<machine> (up refuses any other hostname, so every zm daemon is
// findable) that answer GET /v0/health with our protocol major. The list
// lives in memory; a peer that drops off stays listed offline with its last
// sighting until the daemon restarts. Refreshed every sweepEvery while
// serving, and on demand when the last sweep is older than peersFresh.
func (s *Service) Peers(ctx context.Context) []api.Peer {
	s.peerMu.Lock()
	stale := time.Since(s.lastSweep) > peersFresh
	s.peerMu.Unlock()
	if stale {
		s.refreshPeers(ctx)
	}
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	out := make([]api.Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out
}

// refreshPeers discovers zm nodes from the node's status and probes each
// online one. With no serving node it only marks every known peer offline.
func (s *Service) refreshPeers(ctx context.Context) {
	s.mu.Lock()
	r := s.run
	s.mu.Unlock()
	found := map[string]api.Peer{}
	if r != nil && r.serving.Load() {
		if st, err := r.node.Status(ctx); err == nil && st.State == StateRunning {
			s.probeAll(ctx, r.node.HTTPClient(), st.Peers, found)
		}
	}
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	for m, p := range s.peers {
		if _, ok := found[m]; !ok {
			p.Online = false
			s.peers[m] = p
		}
	}
	for m, p := range found {
		prev, known := s.peers[m]
		switch {
		case p.Online:
		case known: // still on the tailnet but not answering: keep its last sighting
			p.LastSeen, p.Version = prev.LastSeen, prev.Version
		default: // never answered as zm: not a peer
			continue
		}
		s.peers[m] = p
	}
	s.lastSweep = time.Now()
}

// probeAll fills found with every zm node in nps, probing the online ones
// in parallel. The machine name is the one the peer's /v0/health reports
// when it carries one, else the zm-<machine> part of the node's own
// hostname (Tailscale's HostName, which it never de-duplicates; the -1
// suffixes go on DNS names only). Two nodes claiming one machine name
// collapse to one entry deterministically: an answering node wins, then the
// lower URL.
func (s *Service) probeAll(ctx context.Context, hc *http.Client, nps []NodePeer, found map[string]api.Peer) {
	self := s.d.Config.Machine
	port := s.port()
	var mu sync.Mutex
	var wg sync.WaitGroup
	add := func(p api.Peer) {
		mu.Lock()
		defer mu.Unlock()
		if p.Machine == "" || p.Machine == self {
			return
		}
		if prev, ok := found[p.Machine]; ok {
			s.d.Log.Warn("tailnet: two nodes claim one zm machine name", "machine", p.Machine, "urls", prev.URL+" "+p.URL)
			if prev.Online && !p.Online || prev.Online == p.Online && prev.URL < p.URL {
				return
			}
		}
		found[p.Machine] = p
	}
	for _, np := range nps {
		m, ok := machineFromHost(np.Host)
		if !ok {
			continue
		}
		host := first(np.DNSName, np.IP)
		if host == "" {
			continue
		}
		p := api.Peer{Machine: m, URL: baseURL(host, port), Owner: np.Login}
		if !np.Online { // Tailscale already knows it is away
			add(p)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			h, err := client.NewRemote(p.URL, hc).Health(pctx)
			if err == nil && h.OK && api.SameProtocol(h.Protocol, api.WireProtocol) {
				p.Online, p.Version, p.LastSeen = true, h.Version, time.Now().UTC()
				if hm := strings.ToLower(strings.TrimSpace(h.Machine)); api.ValidName(hm) {
					p.Machine = hm
				}
			}
			add(p)
		}()
	}
	wg.Wait()
}

// machineFromHost maps a tailnet hostname to a zm machine name: zm nodes
// are named zm-<machine> (or potluck-<machine>, a later daemon's name); any
// other node is not a zm peer.
func machineFromHost(host string) (string, bool) {
	_, m, ok := api.NodeMachine(host)
	return m, ok && api.ValidName(m)
}

// IsDaemonNode reports whether a node name is a daemon's (zm-* or
// potluck-*): api.IsDaemonNode.
func IsDaemonNode(name string) bool { return api.IsDaemonNode(name) }
