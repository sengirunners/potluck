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
	"github.com/sengirunners/potluck/kit/config"
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

// ghJoin is a GitHub join this daemon started. It is the daemon's, not its
// client's: from the start the daemon itself waits on the authority for the
// outcome and, once the person approved, finishes the join (watchGitHub), so
// a client that goes away (killed, timed out) loses nothing. Clients watch it
// by its handle, and the same join asked for again (ghJoinKey) while its code
// lives answers it: the same code, or how it ended.
type ghJoin struct {
	key        string // the request it answers (ghJoinKey)
	id, secret string // the authority's login id and poll secret (never shown)
	hc         *http.Client
	base, fp   string
	owner      string // the authority node's tailnet owner ("" over https)
	via        string // core.ViaTailnet or core.ViaTunnel
	extra      http.Header
	project    string
	expires    time.Time
	view       api.JoinGitHub

	done chan struct{} // closed once res and err are set
	mu   sync.Mutex
	// The checkouts the join finishes with (keepCheckouts), whether it is
	// finishing (the authority admitted it), why the authority's last poll
	// got no answer, and how it ended.
	paths     []string
	cwd       string
	finishing bool
	away      error
	res       any
	err       error
}

// ghJoinReg runs the GitHub joins' watches; StopLoops ends them, and none
// starts after.
type ghJoinReg struct {
	mu      sync.Mutex
	ctx     context.Context
	stop    context.CancelFunc
	stopped bool
	wg      sync.WaitGroup
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
	s.ghJoins.Range(func(k, v any) bool { // forget ended joins nobody asked after
		if j := v.(*ghJoin); j.ended() && time.Now().After(j.expires.Add(time.Minute)) {
			s.ghJoins.Delete(k)
		}
		return true
	})
	key := s.ghJoinKey(req)
	if j := s.pendingGitHub(key); j != nil {
		// zm join again, the same way: this join, not a second code.
		j.keepCheckouts(req)
		wait := time.Duration(0)
		joined := s.ByName(req.Project) != nil // first: this join sets finishing before it saves
		if j.isFinishing() {
			wait = 2 * time.Minute // admitted: the answer is moments away
		} else if !j.ended() && joined {
			return nil, api.Errorf(api.CodeConflict, "this machine already joined %s", req.Project).WithHint(config.Command("leave", req.Project) + " first")
		}
		return s.watchOn(ctx, j, wait, true)
	}
	if f := strings.ToLower(strings.TrimSpace(req.From)); strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
		return s.joinGitHubTunnel(ctx, req, key)
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
		return nil, api.Errorf(api.CodeConflict, "this machine already joined %s", req.Project).WithHint(config.Command("leave", req.Project) + " first")
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
			Notes: []string{"choose the authority: " + config.Command("join", req.Project, "--from <machine>")}}, nil
	}
	at := found[0]
	fp := project.NodeFingerprint(at.Node)
	base := hostURL(at.Host)
	hc := s.client(core.JoinTarget{Via: core.ViaTailnet, URL: base, Authority: at.Machine, Fingerprint: fp}, nil)
	var st api.GitHubLoginStart
	if err := s.call(ctx, core.ViaTailnet, hc, http.MethodPost, base+"/v0/join/github", "", api.GitHubJoinRequest{Project: req.Project, Machine: machine, Area: req.Area, Role: req.Role}, &st); err != nil {
		return nil, err
	}
	j := &ghJoin{key: key, id: st.ID, secret: st.Secret, hc: hc, base: base, fp: fp, owner: at.Owner, via: core.ViaTailnet, project: req.Project, expires: st.Expires,
		view: api.JoinGitHub{Login: ids.New(), UserCode: st.UserCode, VerificationURI: st.VerificationURI, Expires: st.Expires,
			Authority: st.Authority, Owner: at.Owner, For: st.For}}
	if err := s.startGitHub(j, req); err != nil {
		return nil, err
	}
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
func (s *Service) joinGitHubTunnel(ctx context.Context, req api.JoinRequest, key string) (any, error) {
	base, fp, err := parseHome(req.From)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Area) == "" {
		return nil, api.Errorf(api.CodeBadRequest, "a GitHub join over https names one area").WithHint(config.Command("join", req.Project+"/<area>", "--from", req.From))
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
		return nil, api.Errorf(api.CodeConflict, "this machine already joined %s", req.Project).WithHint(config.Command("leave", req.Project) + " first")
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
		return nil, api.Errorf(api.CodeNotFound, "%s is not a %s authority that runs GitHub logins", base, config.Program).
			WithHint("ask the project's owner for an invite: " + config.Command("join", "<code>"))
	}
	var st api.GitHubLoginStart
	if err := s.call(ctx, core.ViaTunnel, hc, http.MethodPost, base+"/v0/join/github", "", api.GitHubJoinRequest{Project: req.Project, Machine: machine, Area: req.Area, Role: req.Role}, &st); err != nil {
		return nil, err
	}
	j := &ghJoin{key: key, id: st.ID, secret: st.Secret, hc: hc, base: base, fp: fp, via: core.ViaTunnel, extra: extra, project: req.Project, expires: st.Expires,
		view: api.JoinGitHub{Login: ids.New(), UserCode: st.UserCode, VerificationURI: st.VerificationURI, Expires: st.Expires,
			Authority: st.Authority, For: st.For}}
	if err := s.startGitHub(j, req); err != nil {
		return nil, err
	}
	v := j.view
	return api.JoinResponse{GitHub: &v, Notes: []string{"approve the GitHub login, then this join goes on"}}, nil
}

// ghJoinKey is what a GitHub join was asked for, with the defaults filled
// in: the project, where (--from, any case), the area, the role (a guest's
// over https), this machine's name there, and the Access token's id. A join
// asked for again with the same key is the same join.
func (s *Service) ghJoinKey(req api.JoinRequest) string {
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	from, role, machine := norm(req.From), norm(req.Role), norm(req.Machine)
	if strings.HasPrefix(from, "http://") || strings.HasPrefix(from, "https://") {
		if base, fp, err := parseHome(req.From); err == nil {
			from = base + "#" + fp
		}
		if role == "" {
			role = api.RoleGuest
		}
	}
	if m, err := s.joinMachine(req); err == nil {
		machine = m
	}
	return strings.Join([]string{strings.TrimSpace(req.Project), from, strings.TrimSpace(req.Area), role, machine,
		strings.TrimSpace(req.AccessID)}, "\x00")
}

// pendingGitHub is the GitHub join this daemon started for key that a rerun
// takes up: its code still live, or it joined; the latest if several, nil if
// none. One that failed after its code expired is not: a rerun starts anew.
func (s *Service) pendingGitHub(key string) *ghJoin {
	now := time.Now()
	var found *ghJoin
	s.ghJoins.Range(func(_, v any) bool {
		j := v.(*ghJoin)
		if j.key == key && (now.Before(j.expires) || j.joined()) && (found == nil || j.expires.After(found.expires)) {
			found = j
		}
		return true
	})
	return found
}

// startGitHub keeps j, started for req, and starts its watch; refused once
// the daemon is stopping.
func (s *Service) startGitHub(j *ghJoin, req api.JoinRequest) error {
	j.done = make(chan struct{})
	j.keepCheckouts(req)
	s.gh.mu.Lock()
	if s.gh.stopped {
		s.gh.mu.Unlock()
		return api.Errorf(api.CodeUnavailable, "this daemon is stopping").WithHint("start again: " + config.Command("join", j.project))
	}
	if s.gh.ctx == nil {
		base := s.rctx
		if base == nil {
			base = context.Background()
		}
		s.gh.ctx, s.gh.stop = context.WithCancel(base)
	}
	ctx := s.gh.ctx
	s.gh.wg.Add(1)
	s.gh.mu.Unlock()
	s.ghJoins.Store(j.view.Login, j)
	go func() {
		defer s.gh.wg.Done()
		s.watchGitHub(ctx, j)
	}()
	return nil
}

// stopGitHubJoins ends the GitHub joins' watches and waits for them; no
// join starts after.
func (s *Service) stopGitHubJoins() {
	s.gh.mu.Lock()
	stop := s.gh.stop
	s.gh.ctx, s.gh.stop, s.gh.stopped = nil, nil, true
	s.gh.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.gh.wg.Wait()
}

// keepCheckouts takes the checkouts a request names: its paths, else its
// cwd when none is known yet or it is in a checkout and the one known is
// not. A poll by handle alone keeps what the first request said.
func (j *ghJoin) keepCheckouts(req api.JoinRequest) {
	inCheckout := func(dir string) bool { return dir != "" && project.GitRoot(project.ExpandPath(dir)) != "" }
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case len(req.Paths) > 0:
		j.paths, j.cwd = req.Paths, req.Cwd
	case len(j.paths) > 0 || req.Cwd == "":
	case j.cwd == "" || (!inCheckout(j.cwd) && inCheckout(req.Cwd)):
		j.cwd = req.Cwd
	}
}

func (j *ghJoin) isFinishing() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finishing && !j.ended()
}

// ended says j's watch is over (res and err are set).
func (j *ghJoin) ended() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// joined says j ended with this machine joined.
func (j *ghJoin) joined() bool {
	if !j.ended() {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err == nil
}

// GitHubPollGap is the least time between two of a watch's polls (a quick
// pending answer, an authority that cannot be reached).
var GitHubPollGap = 2 * time.Second

// watchGitHub waits on the authority for j's outcome until a minute past
// its code's expiry and finishes the join, detached from every client.
func (s *Service) watchGitHub(base context.Context, j *ghJoin) {
	ctx, cancel := context.WithDeadline(base, j.expires.Add(time.Minute))
	defer cancel()
	var res any
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil { // a client's request would have been recovered
				s.d.Log.Error("teamlink: GitHub join", "panic", p)
				res, err = nil, api.Errorf(api.CodeInternal, "the join failed: %v", p)
			}
		}()
		res, err = s.collectGitHub(base, ctx, j)
	}()
	j.mu.Lock()
	j.res, j.err = res, err
	j.mu.Unlock()
	close(j.done)
}

// collectGitHub polls the authority until it answers j's outcome (ctx ends a
// minute past the code's expiry), then finishes the join with the kept
// checkouts (base ends only at shutdown).
func (s *Service) collectGitHub(base, ctx context.Context, j *ghJoin) (any, error) {
	start := func() string { return "start again: " + config.Command("join", j.project) }
	var last error // the authority's own last answer that it did not spend the outcome
	for {
		began := time.Now()
		var out api.GitHubLoginOutcome
		own, err := s.callAt(ctx, j.via, j.hc, http.MethodPost, j.base+"/v0/login/github/"+j.id, "", api.GitHubLoginPoll{Secret: j.secret, WaitMS: 25000}, &out)
		j.mu.Lock()
		if ctx.Err() == nil { // a poll cut by the deadline says nothing of the authority
			j.away = nil
			if !own {
				j.away = err
			}
		}
		away := j.away
		j.mu.Unlock()
		switch {
		case err == nil && out.Join != nil: // admitted, even at the deadline: the authority spent it
			j.mu.Lock()
			j.finishing = true
			req := api.JoinRequest{Project: j.project, Paths: j.paths, Cwd: j.cwd}
			j.mu.Unlock()
			fctx, cancel := context.WithTimeout(base, 2*time.Minute)
			defer cancel()
			return s.finishJoin(fctx, req, *out.Join, j.hc, j.base, j.fp, j.owner, j.via, j.extra)
		case ctx.Err() != nil && time.Now().After(j.expires):
			return nil, api.Errorf(api.CodeForbidden, "the GitHub login expired%s", lastAway(away)).WithHint(start())
		case ctx.Err() != nil:
			return nil, api.Errorf(api.CodeUnavailable, "this daemon stopped before the GitHub login ended").WithHint(start())
		case err == nil && out.Pending:
			last = nil
		case err == nil:
			return nil, api.Errorf(api.CodeInternal, "the authority answered no admission")
		case api.IsCode(err, api.CodeNotFound):
			why := "it restarted, or the code expired"
			if last != nil {
				why = "it answered " + last.Error() + " before" // and spent the outcome with it
			}
			return nil, api.Errorf(api.CodeNotFound, "the authority no longer has this GitHub login (%s)", why).WithHint(start())
		case api.IsCode(err, api.CodeUnavailable) || api.IsCode(err, api.CodeInternal):
			// Unreachable, or not spent (collect again): ask again shortly.
			if own {
				last = err
			}
		default:
			return nil, err
		}
		if gap := GitHubPollGap - time.Since(began); gap > 0 {
			select {
			case <-time.After(gap):
			case <-ctx.Done():
			}
		}
	}
}

// lastAway is ", and the authority did not answer the last poll: <err>" when
// it did not, else "".
func lastAway(err error) string {
	if err == nil {
		return ""
	}
	return ", and the authority did not answer the last poll: " + err.Error()
}

// watchOn answers j to a client: how it ended (once: its handle is then
// spent), else, after wait, the code again.
func (s *Service) watchOn(ctx context.Context, j *ghJoin, wait time.Duration, resumed bool) (any, error) {
	if !j.ended() && wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-j.done:
		case <-t.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !j.ended() {
		v := j.view
		v.Resumed = resumed
		r := api.JoinResponse{GitHub: &v}
		j.mu.Lock()
		switch {
		case j.finishing:
			r.Notes = []string{"approved: this machine is finishing the join"}
		case resumed:
			r.Notes = []string{"the same GitHub code as before: approve it, then this join goes on"}
		}
		if j.away != nil {
			r.Notes = append(r.Notes, "the authority did not answer the last poll ("+j.away.Error()+"); still asking while the code lasts")
		}
		j.mu.Unlock()
		return r, nil
	}
	s.ghJoins.CompareAndDelete(j.view.Login, j)
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.res, j.err
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
	return api.Errorf(api.CodeNotFound, "no %s authority answered on the tailnet for GitHub joins (%s); is Tailscale up? or pass a join code (it contains ':')", config.Program, detail).
		WithHint("ask the project's owner to share their " + config.Program + " node with you in Tailscale, or for an invite: " + config.Command("join", "<code>"))
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
		return api.JoinAuthority{}, "not a " + config.Program + " authority (no GitHub join route)"
	case err != nil:
		return api.JoinAuthority{}, "no answer"
	case !a.Authority || a.Machine != machine || peerNode == "":
		return api.JoinAuthority{}, "not a " + config.Program + " authority"
	case !a.GitHub:
		return api.JoinAuthority{}, "runs no GitHub logins (github:false)"
	}
	if peerOwner == "" {
		peerOwner = "(tagged node)"
	}
	return api.JoinAuthority{Machine: machine, Host: host, Owner: peerOwner, Node: peerNode}, ""
}

// continueGitHub watches a GitHub join this daemon started: still pending
// (the same answer again), refused, or joined.
func (s *Service) continueGitHub(ctx context.Context, req api.JoinRequest) (any, error) {
	v, ok := s.ghJoins.Load(req.Login)
	if !ok {
		return nil, api.NotFound("GitHub join", req.Login).WithHint("start again: " + config.Command("join", "<project>"))
	}
	j := v.(*ghJoin)
	j.keepCheckouts(req)
	wait := 25 * time.Second
	if req.WaitMS > 0 && time.Duration(req.WaitMS)*time.Millisecond < wait {
		wait = time.Duration(req.WaitMS) * time.Millisecond
	}
	return s.watchOn(ctx, j, wait, false)
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
