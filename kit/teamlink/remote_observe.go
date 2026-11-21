package teamlink

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// poolRemoved reads a 401's message, putting the body back: the authority
// says the pool this machine joined was removed by its owner
// (project.PoolRemovedPrefix, then the pool's name).
func poolRemoved(resp *http.Response) (pool string, ok bool) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	var er api.ErrorResponse
	if json.Unmarshal(b, &er) != nil || er.Error == nil {
		return "", false
	}
	return strings.CutPrefix(er.Error.Message, project.PoolRemovedPrefix)
}

// staleHeader reports whether a header's scope_version is older than what
// this replica holds, for a request that did not send more than it: the
// authority stamps the version, so it orders answers where the send order
// (seq) cannot, and a request can reach the authority before a scope change
// though sent after one that reached it later. A version below what the
// request sent is the authority's correction (restored from a backup), never
// stale.
func staleHeader(version, held, sent int64) bool { return version < held && sent <= version }

// takeHeader is whether observe takes an answer's header: a newer version
// always (a 409's correction must not lose to the order calls were sent in,
// or the retry is refused too), else one from a call sent after the one
// whose header is held, unless its version is stale.
func takeHeader(version, held, sent int64, seq, headerSeq uint64) bool {
	return version > held || (seq > headerSeq && !staleHeader(version, held, sent))
}

// Observe records what an authority's answer says about this replica: the
// project header (areas, role, cap, scope_version, repo rules), and 401s.
// The authority writes the header when a call arrives, so a long poll
// answers with the header of when it started: takeHeader decides (seq is the
// call's number, sent the scope_version it carried). CI, PR 143: GET
// /v0/sync, woken by a project's rules changing, brought back the rules from
// before the change after a heartbeat had the new ones; zm-nda: an older
// version taken between a rescope and its retry refused the retry too.
// Three 401s, the first and the last at least PurgeAfter apart, purge the
// remote: its token file goes and its calls stop.
func (s *Service) Observe(ctx context.Context, x *Remote, resp *http.Response, seq uint64, sent int64) {
	now := time.Now()
	changed, moved := false, false
	var removedPool string
	var ok bool
	if resp.StatusCode == http.StatusUnauthorized { // read before any lock: it reads the body
		removedPool, ok = poolRemoved(resp)
	}
	x.PublicationMu.Lock()
	x.Mu.Lock()
	if x.Left || x.Recovery.Closing {
		x.Mu.Unlock()
		x.PublicationMu.Unlock()
		return
	}
	raw := api.GetHeader(resp.Header, api.HeaderTeam)
	if resp.StatusCode == http.StatusUnauthorized {
		raw = "" // rejected credentials cannot authorize role recovery
		if x.R.State == api.RemoteRevoked {
			x.Recovery.Ready = false
			x.Recovery.Revision++
		}
	}
	if raw != "" && raw == x.HeaderB64 && seq > x.HeaderSeq {
		x.HeaderSeq = seq // the same header, from a later call
		changed = x.recordRoleHeader() || changed
	}
	if raw != "" && raw != x.HeaderB64 {
		if h, err := api.DecodeTeamHeader(raw); err == nil && h.Project.ID == x.R.Project.ID && takeHeader(h.ScopeVersion, x.R.ScopeVersion, sent, seq, x.HeaderSeq) {
			x.HeaderB64, x.HeaderSeq = raw, max(seq, x.HeaderSeq)
			js, _ := json.Marshal(h)
			if string(js) != x.HeaderRaw {
				// The scope moved when its version, role, areas or cap did
				// (not on the first answer after a start: headerRaw was
				// loaded then).
				moved = x.HeaderRaw != "" && (h.ScopeVersion != x.R.ScopeVersion || h.Role != x.R.Role ||
					strings.Join(h.Areas, ",") != strings.Join(x.R.Areas, ",") || h.Cap != x.R.Cap)
				x.HeaderRaw = string(js)
				x.R.Areas, x.R.Role, x.R.Cap, x.R.ScopeVersion = h.Areas, h.Role, h.Cap, h.ScopeVersion
				if h.Project.Slug != "" {
					x.R.Project.Slug = h.Project.Slug
				}
				changed = true
			}
			changed = x.recordRoleHeader() || changed
		}
	}
	if moved && x.Copy != nil && x.R.State == api.RemoteOK && s.Hooks.ScopeMoved != nil {
		// A rescope, a role or area change: the copy is taken again from a
		// snapshot, which drops what left the scope (SPEC 17.4).
		s.Hooks.ScopeMoved(x)
	}
	purge := false
	if resp.StatusCode == http.StatusUnauthorized {
		x.Unauth = append(x.Unauth, now)
		n := len(x.Unauth)
		live := x.R.State == api.RemoteOK || x.R.State == api.RemoteRevoked
		why := ""
		if ok && live {
			// The owner removed the pool: nothing to wait out, no later call
			// will be answered.
			why = "the pool's owner removed pool " + removedPool
		} else if n >= unauthorizedLimit && now.Sub(x.Unauth[n-unauthorizedLimit]) >= PurgeAfter && live {
			why = "the authority refused the token 3 times over " + PurgeAfter.String()
		}
		if why != "" {
			x.R.State, x.R.LastError = api.RemotePurged, why
			x.Recovery.Ready, x.Recovery.Drop = false, true
			x.Recovery.Generation++
			x.Recovery.Revision++
			purge, changed = true, true
		}
	} else if len(x.Unauth) > 0 {
		x.Unauth = nil
	}
	x.Mu.Unlock()
	x.PublicationMu.Unlock()
	if purge {
		// Round B keeps no copy of the project: the token and the cached
		// header are what there is to purge.
		s.PurgeRoleToken(x)
	}
	if changed {
		if err := s.SaveRoleRemote(context.WithoutCancel(ctx), x); err != nil {
			s.d.Log.Warn("team: save remote", "err", err)
		}
	}
	s.QueueRoleReconcile(x) // never wait for this call's own replica loop
}
