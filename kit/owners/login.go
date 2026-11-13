package owners

// Logins run by this authority (SPEC 22 "Sign in"; owners-2026-11-01 Q2).
// A client asks to start one; the authority asks GitHub for a device code
// with zm's client id and answers the user code, the verification URL, a
// pending id and a poll secret. While the person approves in a browser,
// the authority polls GitHub, receives the token itself, asks GitHub who
// holds it, keeps only that, and drops the token. It acts (a link) only
// when the client collects the outcome with the secret; an outcome nobody
// collects expires with nothing done. No client ever handles a GitHub
// token, so a token of another app, a personal access token, or one
// captured elsewhere is never accepted, and an outcome cannot be replayed
// at another authority.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

// Login purposes.
const (
	PurposeLink = "link" // link the starting identity to the approving account
	PurposeJoin = "join" // admit the starting tailnet node to a project (join.go)
	PurposeBind = "bind" // bind @owner/name to a project (names.go)
)

// login is one login in flight.
type login struct {
	id       string
	secret   [32]byte // sha256 of the poll secret
	purpose  string
	identity api.Identity // who started it (link)
	from     string       // where it was started, for the mail: "this machine (mini)", "machine x (node n...)"
	local    bool         // started by the machine owner on its socket (its own pool)
	node     string       // join: the tailnet node that started it (only it collects)
	nodeName string       // join: that node's tailnet name, for the person approving
	project  string       // join: the project as the joiner typed it (resolved only at collection)
	machine  string       // join: the node's machine name
	area     string       // join: the area asked for ("" with role "": the strongest grant)
	role     string       // join: the role asked for at most ("" with area: the grant's)
	tunnel   bool         // join: started over the tunnel (its own small pool)
	addr     string       // join over the tunnel: the client address (its own limits)

	// bind: the project (its ULID), the owner's login and the name asked
	// for; then, from GitHub while the token is in hand, the owner and
	// whether the approver is an active member of it (an org), or why not.
	bindProject, bindOwner, bindName string
	bindOwnerAcc                     Account
	bindMember                       bool
	bindErr                          error
	expires                          time.Time // the device code's (a reservation's until GitHub answers)
	done                             chan struct{}
	act                              sync.Mutex // one collector at a time

	// Set once, before done closes: who GitHub says approved, which of the
	// join's candidate orgs it is an active member of, or why not.
	acc       Account
	orgs      map[string]bool
	err       error
	finished  time.Time
	collected bool
}

// SecretBytes is the poll secret's size: 192 bits from crypto/rand (at
// least 128 are required).
const SecretBytes = 24

func hashSecret(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// sweep drops logins past their code's expiry or uncollected past Collect.
// Called with s.mu held.
func (s *Service) sweep(now time.Time) {
	for id, l := range s.logins {
		select {
		case <-l.done:
			if l.collected || now.Sub(l.finished) > s.Collect {
				delete(s.logins, id)
			}
		default:
			if now.After(l.expires.Add(time.Minute)) {
				delete(s.logins, id)
			}
		}
	}
}

func (s *Service) handleStart(r *http.Request) (any, error) {
	var req api.GitHubLoginRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	return s.StartLogin(r.Context(), core.SubjectOf(r.Context()), req)
}

// StartLogin begins a login for sub. The answer carries the poll secret, the
// only copy: the authority keeps its hash.
func (s *Service) StartLogin(ctx context.Context, sub *authz.Subject, req api.GitHubLoginRequest) (api.GitHubLoginStart, error) {
	switch req.Purpose {
	case "", PurposeLink:
		if err := linkable(sub); err != nil {
			return api.GitHubLoginStart{}, err
		}
		if sub.Identity.Kind != api.IdentityHuman {
			return api.GitHubLoginStart{}, api.Errorf(api.CodeForbidden, "only a person links GitHub; identity %s is a %s", sub.Identity.Name, sub.Identity.Kind).
				WithHint("machines and agents keep zm's invites and tokens")
		}
	case PurposeBind:
		if sub == nil || sub.Identity.Kind != api.IdentityHuman {
			return api.GitHubLoginStart{}, api.Errorf(api.CodeForbidden, "only a person binds names: the approver must be the identity's linked GitHub account")
		}
		p, owner, name, err := s.bindTarget(sub, req)
		if err != nil {
			return api.GitHubLoginStart{}, err
		}
		return s.begin(ctx, &login{purpose: PurposeBind, identity: sub.Identity, from: sub.Address,
			local: sub.Via == authz.ViaLocal && sub.TokenID == "", bindProject: p.ID, bindOwner: owner, bindName: name})
	default:
		return api.GitHubLoginStart{}, api.Errorf(api.CodeBadRequest, "unknown login purpose %q", req.Purpose)
	}
	from := "this machine (" + s.d.Config.Machine + ")"
	if sub.Node != "" {
		from = "machine " + sub.Machine + " (tailnet node " + sub.Node + ")"
	}
	return s.begin(ctx, &login{purpose: PurposeLink, identity: sub.Identity, from: from,
		local: sub.Via == authz.ViaLocal && sub.TokenID == ""})
}

// owner keys a login's limits: the identity or the node that started it.
func (l *login) owner() string {
	if l.node != "" {
		return "node:" + l.node
	}
	return "identity:" + l.identity.ID
}

// who names the login's requester for the person approving it.
func (l *login) who() string {
	if l.node != "" {
		who := "node " + l.nodeName + " (" + l.node + ") as machine " + l.machine
		if strings.HasPrefix(l.node, "zmkey:") {
			who = "key " + l.node + " over https as machine " + l.machine
		}
		switch {
		case l.area != "":
			who += ", in area " + l.area + " as " + orGrant(l.role)
		case l.role != "":
			who += ", as " + l.role
		}
		return who
	}
	if l.purpose == PurposeBind {
		return "identity " + l.identity.Name + " to name a project @" + l.bindOwner + "/" + l.bindName
	}
	return "identity " + l.identity.Name
}

// reserve holds a slot for l before GitHub is asked, so concurrent starts
// cannot pass the limits together: MaxPerOwner in flight per owner and
// MaxStarts an hour per owner; MaxLogins in all for remote callers, and a
// separate MaxLocal for the machine owner's socket, which remote starts
// cannot use up.
func (s *Service) reserve(l *login) error {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(now)
	s.pruneStarts(now)
	mine, pool, there := 0, 0, 0
	for _, x := range s.logins {
		if x.owner() == l.owner() {
			mine++
		}
		if x.local == l.local && x.tunnel == l.tunnel {
			pool++
		}
		if l.addr != "" && x.addr == l.addr {
			there++
		}
	}
	recent := withinHour(s.starts[l.owner()], now)
	limit := s.MaxLogins
	switch {
	case l.local:
		limit = s.MaxLocal
	case l.tunnel:
		limit = s.MaxTunnel // unauthenticated callers never use up the tailnet's pool
	}
	var byAddr []time.Time
	if l.addr != "" {
		byAddr = withinHour(s.starts["addr:"+l.addr], now)
	}
	if pool >= limit || mine >= s.MaxPerOwner || len(recent) >= s.MaxStarts ||
		(l.addr != "" && (there >= s.MaxPerAddr || len(byAddr) >= s.MaxAddrStarts)) {
		s.keepStarts(l.owner(), recent)
		if l.addr != "" {
			s.keepStarts("addr:"+l.addr, byAddr)
		}
		return api.Errorf(api.CodeRateLimited, "too many GitHub logins in flight or this hour").WithRetryAfter(60)
	}
	s.starts[l.owner()] = append(recent, now)
	if l.addr != "" {
		s.starts["addr:"+l.addr] = append(byAddr, now)
	}
	l.id, l.expires, l.done = ids.New(), now.Add(time.Minute), make(chan struct{})
	s.logins[l.id] = l
	return nil
}

// withinHour is the times in ts within the last hour.
func withinHour(ts []time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range ts {
		if now.Sub(t) < time.Hour {
			out = append(out, t)
		}
	}
	return out
}

// keepStarts stores recent for owner, or forgets the owner when it is empty
// (keys anyone can make never leave empty entries behind).
func (s *Service) keepStarts(owner string, recent []time.Time) {
	if len(recent) == 0 {
		delete(s.starts, owner)
		return
	}
	s.starts[owner] = recent
}

// pruneStarts drops owners with no start in the last hour, once a minute.
func (s *Service) pruneStarts(now time.Time) {
	if now.Sub(s.startsPruned) < time.Minute {
		return
	}
	s.startsPruned = now
	for o, ts := range s.starts {
		s.keepStarts(o, withinHour(ts, now))
	}
}

// begin asks GitHub for a device code for l (its slot reserved first) and
// starts waiting on it.
func (s *Service) begin(ctx context.Context, l *login) (api.GitHubLoginStart, error) {
	if s.dev == nil {
		return api.GitHubLoginStart{}, api.Errorf(api.CodeUnavailable, "this authority has no GitHub OAuth client id").
			WithHint(`its owner sets "github": {"client_id": "<an OAuth app's, with Enable Device Flow>"} in config.json`)
	}
	if err := s.reserve(l); err != nil {
		return api.GitHubLoginStart{}, err
	}
	release := func() {
		s.mu.Lock()
		delete(s.logins, l.id)
		s.mu.Unlock()
	}
	code, err := s.dev.Start(ctx)
	if err != nil {
		release()
		return api.GitHubLoginStart{}, api.Errorf(api.CodeUnavailable, "could not start a GitHub login: %v", err)
	}
	var b [SecretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		release()
		return api.GitHubLoginStart{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(b[:])
	s.mu.Lock()
	l.secret, l.expires = hashSecret(secret), code.Expires
	s.mu.Unlock()
	go s.run(l, code)
	return api.GitHubLoginStart{ID: l.id, Secret: secret, UserCode: code.UserCode, VerificationURI: code.VerificationURI,
		Expires: code.Expires.UTC(), Authority: s.d.Config.Machine, For: l.who()}, nil
}

// run waits for the person, asks GitHub who approved, and drops the
// token. It acts on nothing: Poll does, when the outcome is collected.
func (s *Service) run(l *login, code DeviceCode) {
	ctx, cancel := context.WithDeadline(s.ctx, l.expires)
	defer cancel()
	var orgs map[string]bool
	acc, err := func() (Account, error) {
		token, err := s.dev.Wait(ctx, code)
		switch {
		case errors.Is(err, ErrDenied):
			return Account{}, api.Errorf(api.CodeForbidden, "the GitHub login was declined")
		case errors.Is(err, ErrExpired), errors.Is(err, context.DeadlineExceeded):
			return Account{}, api.Errorf(api.CodeForbidden, "the GitHub login code expired").WithHint("start again: zm login github")
		case err != nil:
			return Account{}, api.Errorf(api.CodeUnavailable, "GitHub login: %v", err)
		}
		acc, err := s.acc.Verify(ctx, token)
		if err != nil {
			return Account{}, api.Errorf(api.CodeUnavailable, "could not ask GitHub who approved: %v", err)
		}
		if l.purpose == PurposeBind && acc.Kind == KindUser {
			s.bindFacts(ctx, l, acc, token)
		}
		if l.purpose == PurposeJoin {
			// The only thing a join needs the token for, live: which of the
			// project's granted orgs the person is an active member of. Every
			// join then waits to DecideFloor after GitHub's answer, so its
			// timing tells no project or grant from another.
			answered := time.Now()
			if acc.Kind == KindUser {
				orgs = s.memberships(ctx, l.project, token)
			}
			if wait := s.DecideFloor - time.Since(answered); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
				}
			}
		}
		return acc, nil // the token goes no further
	}()
	s.mu.Lock()
	l.acc, l.orgs, l.err, l.finished = acc, orgs, err, time.Now()
	s.mu.Unlock()
	close(l.done)
}

// startedBy says whether the caller is the one that started l: the same
// tailnet node for a join, the same identity otherwise.
func (l *login) startedBy(ctx context.Context, sub *authz.Subject) bool {
	if l.node != "" {
		rm := core.RemoteOf(ctx)
		return rm != nil && rm.NodeID == l.node && !core.IsLocal(ctx)
	}
	return sub != nil && sub.Identity.ID == l.identity.ID
}

func (s *Service) handlePoll(r *http.Request) (any, error) {
	var req api.GitHubLoginPoll
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	return s.Poll(r.Context(), core.SubjectOf(r.Context()), r.PathValue("id"), req)
}

// MaxPollWait bounds one long poll.
const MaxPollWait = 30 * time.Second

// Poll waits up to req.WaitMS for login id's outcome and answers it once.
// An unknown id, a wrong secret, a caller other than the one that started
// it, and an outcome already collected all answer the same 404.
func (s *Service) Poll(ctx context.Context, sub *authz.Subject, id string, req api.GitHubLoginPoll) (api.GitHubLoginOutcome, error) {
	missing := api.NotFound("login", id)
	want := hashSecret(req.Secret)
	s.mu.Lock()
	s.sweep(time.Now())
	l := s.logins[id]
	var have [32]byte
	if l != nil {
		have = l.secret
	}
	s.mu.Unlock()
	if l == nil || subtle.ConstantTimeCompare(have[:], want[:]) != 1 || !l.startedBy(ctx, sub) {
		return api.GitHubLoginOutcome{}, missing
	}
	wait := time.Duration(req.WaitMS) * time.Millisecond
	if wait > MaxPollWait {
		wait = MaxPollWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-l.done:
	case <-t.C:
		return api.GitHubLoginOutcome{Pending: true}, nil
	case <-ctx.Done():
		return api.GitHubLoginOutcome{}, ctx.Err()
	}
	// One collector at a time; the act runs detached from this request, so
	// a client that goes away does not cut it short, and the outcome is
	// spent only once the act committed (or definitively failed).
	l.act.Lock()
	defer l.act.Unlock()
	s.mu.Lock()
	collected, acc, err := l.collected, l.acc, l.err
	s.mu.Unlock()
	if collected {
		return api.GitHubLoginOutcome{}, missing
	}
	out := api.GitHubLoginOutcome{}
	if err == nil {
		out, err = s.act(s.ctx, l, acc)
		if api.IsCode(err, api.CodeUnavailable) || api.IsCode(err, api.CodeInternal) {
			return api.GitHubLoginOutcome{}, err // not spent: collect again
		}
	}
	s.mu.Lock()
	l.collected = true
	s.mu.Unlock()
	return out, err
}

// act does what the login is for, once it is collected.
func (s *Service) act(ctx context.Context, l *login, acc Account) (api.GitHubLoginOutcome, error) {
	if s.actFault != nil {
		if err := s.actFault(); err != nil {
			return api.GitHubLoginOutcome{}, err
		}
	}
	if l.purpose == PurposeBind {
		if acc.Kind != KindUser {
			return api.GitHubLoginOutcome{}, api.Errorf(api.CodeForbidden, "@%s is a GitHub %s; only a person's account binds names", acc.Login, acc.Kind)
		}
		b, err := s.bind(ctx, l, acc)
		if err != nil {
			return api.GitHubLoginOutcome{}, err
		}
		return api.GitHubLoginOutcome{Bind: &b}, nil
	}
	if l.purpose == PurposeJoin {
		join, err := s.admit(ctx, l, acc)
		if err != nil {
			return api.GitHubLoginOutcome{}, err
		}
		return api.GitHubLoginOutcome{Join: &join}, nil
	}
	link, err := s.link(ctx, l.identity, acc, l.from)
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	return api.GitHubLoginOutcome{Link: &link}, nil
}
