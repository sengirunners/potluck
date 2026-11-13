package teamlink

// zm join <project> with no code (SPEC 22 "Join by GitHub";
// owners-2026-11-01 Q11), the member's half. This daemon's node comes up as
// for any join; then each zm peer on the tailnet is asked whether it is a
// zm authority that runs GitHub logins (GET /v0/join/github, which says
// nothing about its projects). Nothing starts until the person has chosen
// one by its machine and its node's tailnet owner (From): any zm node can
// answer that it is an authority, so the one that answers alone is never
// taken on trust. The chosen node is pinned, asked to start a GitHub join
// for the project, and the next call collects the member token; the join
// then goes on as a redeemed invite's, and the remote records the
// authority's tailnet owner (a later connection to another owner warns).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
)

// ghJoin is a GitHub join this daemon started, until it ends.
type ghJoin struct {
	id, secret string // the authority's login id and poll secret (never shown)
	hc         *http.Client
	base, fp   string
	owner      string // the authority node's tailnet owner ("" over https)
	via        string // core.ViaTailnet or core.ViaTunnel
	extra      http.Header
	project    string
	expires    time.Time
	view       api.JoinGitHub
	mu         sync.Mutex // one poll at a time
}

// DiscoverWait bounds asking each tailnet peer whether it is an authority;
// NameJoinWait bounds a join by name's bring-up and discovery together.
var (
	DiscoverWait = 3 * time.Second
	NameJoinWait = 25 * time.Second
)

func (s *Service) joinGitHub(ctx context.Context, req api.JoinRequest) (any, error) {
	if req.Login != "" {
		return s.continueGitHub(ctx, req)
	}
	s.ghJoins.Range(func(k, v any) bool { // forget joins nobody continued
		if time.Now().After(v.(*ghJoin).expires.Add(time.Minute)) {
			s.ghJoins.Delete(k)
		}
		return true
	})
	if f := strings.ToLower(strings.TrimSpace(req.From)); strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
		return s.joinGitHubTunnel(ctx, req)
	}
	// A join by name is bounded: bringing the node up and asking the peers
	// takes NameJoinWait at most, and no authority is a prompt refusal
	// (a typo, or Tailscale down, never waits for a browser login).
	ctx, cancel := context.WithTimeout(ctx, NameJoinWait)
	defer cancel()
	if req.WaitMS <= 0 || time.Duration(req.WaitMS)*time.Millisecond > NameJoinWait/2 {
		req.WaitMS = int(NameJoinWait / 2 / time.Millisecond)
	}
	machine, early, err := s.joinPrep(ctx, req, s.d.Transport(core.ViaTailnet), core.JoinTarget{Via: core.ViaTailnet})
	switch {
	case early != nil && early.AuthURL != "":
		return nil, noAuthorityAt(0, nil, "this machine's tailnet node is not logged in: "+early.AuthURL)
	case api.IsCode(err, api.CodeUnavailable) || errors.Is(err, context.DeadlineExceeded):
		return nil, noAuthorityAt(0, nil, "this machine's tailnet node is not up")
	case err != nil || early != nil:
		return early, err
	}
	if s.ByName(req.Project) != nil {
		return nil, api.Errorf(api.CodeConflict, "this machine already joined %s", req.Project).WithHint("zm leave " + req.Project + " first")
	}
	found, skipped, err := s.findAuthorities(ctx, req.From)
	if err != nil {
		return nil, err
	}
	if req.From == "" || len(found) != 1 {
		// The person chooses: by machine and tailnet owner, never on trust.
		if len(found) == 0 {
			return nil, noAuthorityAt(len(skipped), skipped, "")
		}
		return api.JoinResponse{Authorities: found, Skipped: skipped,
			Notes: []string{"choose the authority: zm join " + req.Project + " --from <machine>"}}, nil
	}
	at := found[0]
	fp := project.NodeFingerprint(at.Node)
	base := hostURL(at.Host)
	hc := s.client(core.JoinTarget{Via: core.ViaTailnet, URL: base, Authority: at.Machine, Fingerprint: fp}, nil)
	var st api.GitHubLoginStart
	if err := s.call(ctx, core.ViaTailnet, hc, http.MethodPost, base+"/v0/join/github", "", api.GitHubJoinRequest{Project: req.Project, Machine: machine, Area: req.Area, Role: req.Role}, &st); err != nil {
		return nil, err
	}
	j := &ghJoin{id: st.ID, secret: st.Secret, hc: hc, base: base, fp: fp, owner: at.Owner, via: core.ViaTailnet, project: req.Project, expires: st.Expires,
		view: api.JoinGitHub{Login: ids.New(), UserCode: st.UserCode, VerificationURI: st.VerificationURI, Expires: st.Expires,
			Authority: st.Authority, Owner: at.Owner, For: st.For}}
	s.ghJoins.Store(j.view.Login, j)
	v := j.view
	return api.JoinResponse{GitHub: &v, Skipped: skipped, Notes: []string{"approve the GitHub login, then this join goes on"}}, nil
}

// joinGitHubTunnel is the GitHub join over https (SPEC 22, 17.11): no
// tailnet. From names the home's tunnel URL with its key fingerprint
// (https://<host>#<fingerprint>, as its owner gives it, like an invite's
// code but with no secret): the pin that stands in for the peer's tailnet
// owner, and the only trust anchor. Every call is signed by this daemon's
// key, which is the node the authority binds the login to. The join asks for
// one area, as a guest unless Role says more.
func (s *Service) joinGitHubTunnel(ctx context.Context, req api.JoinRequest) (any, error) {
	base, fp, err := parseHome(req.From)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Area) == "" {
		return nil, api.Errorf(api.CodeBadRequest, "a GitHub join over https names one area").WithHint("zm join " + req.Project + "/<area> --from " + req.From)
	}
	if req.Role == "" {
		req.Role = api.RoleGuest
	}
	if req.Role != api.RoleGuest {
		return nil, api.Errorf(api.CodeBadRequest, "a GitHub join over https is a guest's, not %s", req.Role).
			WithHint("a member or owner joins over the tailnet, or with an invite")
	}
	ctx, cancel := context.WithTimeout(ctx, NameJoinWait)
	defer cancel()
	t := s.d.Transport(core.ViaTunnel)
	target := core.JoinTarget{Via: core.ViaTunnel, URL: base, Fingerprint: fp}
	machine, early, err := s.joinPrep(ctx, req, t, target)
	if err != nil || early != nil {
		return early, err
	}
	if s.ByName(req.Project) != nil {
		return nil, api.Errorf(api.CodeConflict, "this machine already joined %s", req.Project).WithHint("zm leave " + req.Project + " first")
	}
	extra, err := t.Extra(req)
	if err != nil {
		return nil, err
	}
	hc := s.client(target, extra)
	var au api.GitHubAuthority
	if err := s.call(ctx, core.ViaTunnel, hc, http.MethodGet, base+"/v0/join/github", "", nil, &au); err != nil {
		return nil, err
	}
	if !au.Authority || !au.GitHub {
		return nil, api.Errorf(api.CodeNotFound, "%s is not a zm authority that runs GitHub logins", base).
			WithHint("ask the project's owner for an invite: zm join <code>")
	}
	var st api.GitHubLoginStart
	if err := s.call(ctx, core.ViaTunnel, hc, http.MethodPost, base+"/v0/join/github", "", api.GitHubJoinRequest{Project: req.Project, Machine: machine, Area: req.Area, Role: req.Role}, &st); err != nil {
		return nil, err
	}
	j := &ghJoin{id: st.ID, secret: st.Secret, hc: hc, base: base, fp: fp, via: core.ViaTunnel, extra: extra, project: req.Project, expires: st.Expires,
		view: api.JoinGitHub{Login: ids.New(), UserCode: st.UserCode, VerificationURI: st.VerificationURI, Expires: st.Expires,
			Authority: st.Authority, For: st.For}}
	s.ghJoins.Store(j.view.Login, j)
	v := j.view
	return api.JoinResponse{GitHub: &v, Notes: []string{"approve the GitHub login, then this join goes on"}}, nil
}

// parseHome reads https://<host>#<fingerprint>: the home's tunnel URL and
// the key fingerprint to pin.
func parseHome(from string) (base, fp string, err error) {
	bad := api.Errorf(api.CodeBadRequest, "--from over https looks like https://<host>#<fingerprint> (the home's key fingerprint, from its owner)")
	u, frag, ok := strings.Cut(strings.TrimSpace(from), "#")
	if !ok {
		return "", "", bad
	}
	if base, err = project.TunnelURL(u); err != nil {
		return "", "", err
	}
	fp = strings.ToLower(frag)
	if len(fp) != project.KeyFingerprintLen {
		return "", "", bad
	}
	return base, fp, nil
}

// hostURL is a tailnet home's base URL by the host dialled (its full
// MagicDNS name when the tailnet gave one: zm-my5).
func hostURL(host string) string { return "http://" + host + ":7433" }

// noAuthorityAt is the prompt refusal of a join by name that found no
// authority: how many zm peers were asked, why each was skipped, and what
// else was wrong (the node not logged in or not up).
func noAuthorityAt(checked int, skipped []api.JoinSkipped, why string) error {
	detail := fmt.Sprintf("checked %d zm-* peers", checked)
	for _, k := range skipped {
		detail += "; zm-" + k.Machine + ": " + k.Reason
	}
	if why != "" {
		detail += "; " + why
	}
	return api.Errorf(api.CodeNotFound, "no zm authority answered on the tailnet for GitHub joins (%s); is Tailscale up? or pass a join code (it contains ':')", detail).
		WithHint("ask the project's owner to share their zm node with you in Tailscale, or for an invite: zm join <code>")
}

// findAuthorities asks every zm peer (or only from, without case) whether
// it is an authority that runs GitHub logins: those that are, with their
// node's tailnet owner, and every other one with why it was skipped.
func (s *Service) findAuthorities(ctx context.Context, from string) ([]api.JoinAuthority, []api.JoinSkipped, error) {
	self, err := s.d.Net.Self(ctx)
	if err != nil {
		return nil, nil, err
	}
	from = strings.ToLower(strings.TrimSpace(from))
	var (
		mu      sync.Mutex
		found   []api.JoinAuthority
		skipped []api.JoinSkipped
		wg      sync.WaitGroup
	)
	// Every peer, by its full MagicDNS name (a shared node resolves only by
	// it, zm-my5): two peers that report the same zm-<machine> host name are
	// both asked, and the one that answers is told apart by its node.
	for _, p := range self.PeerList {
		_, m, ok := api.NodeMachine(p.Host)
		if !ok || m == strings.ToLower(s.d.Config.Machine) || (from != "" && m != from) {
			continue
		}
		full := p.DNSName
		if full == "" {
			full = p.Host
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, reason := s.askAuthority(ctx, m, full)
			mu.Lock()
			defer mu.Unlock()
			if reason != "" {
				skipped = append(skipped, api.JoinSkipped{Machine: m, Reason: reason})
			} else {
				found = append(found, a)
			}
		}()
	}
	wg.Wait()
	slices.SortFunc(found, func(a, b api.JoinAuthority) int { return strings.Compare(a.Machine, b.Machine) })
	slices.SortFunc(skipped, func(a, b api.JoinSkipped) int { return strings.Compare(a.Machine, b.Machine) })
	return found, skipped, nil
}

// askAuthority asks zm-<machine> whether it is an authority that runs
// GitHub logins, through this daemon's node, and who owns the node that
// answered (WhoIs). A non-empty reason is why it is not one.
func (s *Service) askAuthority(ctx context.Context, machine, host string) (api.JoinAuthority, string) {
	var peerNode, peerOwner string
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := s.d.Net.Dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		peer, err := s.d.Net.Peer(ctx, c.RemoteAddr().String())
		if err != nil || peer.NodeID == "" {
			c.Close()
			return nil, core.ErrPinMismatch
		}
		peerNode, peerOwner = peer.NodeID, peer.Login
		return c, nil
	}, DisableKeepAlives: true}
	ctx, cancel := context.WithTimeout(ctx, DiscoverWait)
	defer cancel()
	var a api.GitHubAuthority
	err := s.call(ctx, core.ViaTailnet, &http.Client{Transport: tr}, http.MethodGet, hostURL(host)+"/v0/join/github", "", nil, &a)
	switch {
	case api.IsCode(err, api.CodeNotFound):
		return api.JoinAuthority{}, "not a zm authority (no GitHub join route)"
	case err != nil:
		return api.JoinAuthority{}, "no answer"
	case !a.Authority || a.Machine != machine || peerNode == "":
		return api.JoinAuthority{}, "not a zm authority"
	case !a.GitHub:
		return api.JoinAuthority{}, "runs no GitHub logins (github:false)"
	}
	if peerOwner == "" {
		peerOwner = "(tagged node)"
	}
	return api.JoinAuthority{Machine: machine, Host: host, Owner: peerOwner, Node: peerNode}, ""
}

// continueGitHub collects a GitHub join this daemon started: still
// pending (the same answer again), refused, or admitted (the join goes on).
func (s *Service) continueGitHub(ctx context.Context, req api.JoinRequest) (any, error) {
	v, ok := s.ghJoins.Load(req.Login)
	if !ok {
		return nil, api.NotFound("GitHub join", req.Login).WithHint("start again: zm join <project>")
	}
	j := v.(*ghJoin)
	j.mu.Lock()
	defer j.mu.Unlock()
	wait := 25 * time.Second
	if req.WaitMS > 0 && time.Duration(req.WaitMS)*time.Millisecond < wait {
		wait = time.Duration(req.WaitMS) * time.Millisecond
	}
	var out api.GitHubLoginOutcome
	err := s.call(ctx, j.via, j.hc, http.MethodPost, j.base+"/v0/login/github/"+j.id, "", api.GitHubLoginPoll{Secret: j.secret, WaitMS: int(wait / time.Millisecond)}, &out)
	if err == nil && out.Pending {
		if time.Now().After(j.expires.Add(time.Minute)) {
			s.ghJoins.Delete(req.Login)
			return nil, api.Errorf(api.CodeForbidden, "the GitHub login expired").WithHint("start again: zm join " + j.project)
		}
		v := j.view
		return api.JoinResponse{GitHub: &v}, nil
	}
	s.ghJoins.Delete(req.Login)
	if err != nil {
		return nil, err
	}
	if out.Join == nil {
		return nil, api.Errorf(api.CodeInternal, "the authority answered no admission")
	}
	req.Project = j.project
	return s.finishJoin(ctx, req, *out.Join, j.hc, j.base, j.fp, j.owner, j.via, j.extra)
}

// ownerSeen is told the tailnet owner of the authority node behind each
// new pinned connection: a remote joined by GitHub records its owner, and
// another one is a warning (zm remotes shows it) until it is resolved.
func (s *Service) ownerSeen(fingerprint, login string) {
	s.mu.RLock()
	var hit []*Remote
	for _, x := range s.remotes {
		if x.R.Fingerprint == fingerprint && x.R.AuthorityOwner != "" {
			hit = append(hit, x)
		}
	}
	s.mu.RUnlock()
	for _, x := range hit {
		x.Mu.Lock()
		changed := login != x.R.AuthorityOwner && login != x.R.OwnerNow
		if changed {
			x.R.OwnerNow = login
		} else if login == x.R.AuthorityOwner {
			x.R.OwnerNow = ""
		}
		x.Mu.Unlock()
		if changed {
			s.d.Log.Warn("team: the authority's node has another tailnet owner", "project", x.R.Project.Slug,
				"recorded", x.R.AuthorityOwner, "now", login)
		}
	}
}
