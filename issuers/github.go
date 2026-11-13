package issuers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Options are what a GitHubToken needs from its owner.
type Options struct {
	Deliver Deliverer // required: puts the value into the box
	Ledger  Ledger    // required: SQLLedger in the daemon
	ClassOf ClassOf   // required: the box's machine class, asked at every issue and renewal
	Live    Live      // required: whether an assigned box still exists, asked by Start
	Check   Check     // required: the grant check (GrantCheck)
	// JobHolds is asked before a job's credential is renewed or issued
	// again; nil: never renewed or issued again (it lapses or is ended).
	JobHolds JobHolds
	Events   Publisher // the pool's event log: renewed, revoked and refused (nil: none published)

	Secrets  *secrets.Reader // reads the App key reference (default secrets.Default())
	HTTP     *http.Client
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error // between GitHub retries (tests)
	Log      *slog.Logger
	Grace    time.Duration // a renewed-away token is revoked this long after (default 5 minutes)
	Renewers int           // renewals and revocations Tick runs at once (default 8)
}

// Timing.
const (
	defaultGrace = 5 * time.Minute
	retryEvery   = time.Minute      // after a failed renewal, re-issue or revocation
	retryMax     = 5 * time.Minute  // the longest gap between revocation retries
	lapseMargin  = 10 * time.Minute // a revocation is retried this long past the token's expiry
	tickCap      = time.Minute      // Tick never answers later than this
	mintTimeout  = 30 * time.Second
)

// GitHubToken is the github-token kind: per box, an installation token of the
// issuer's App for one repository and the requested permissions, inside the
// issuer's ceiling. Tokens are held in memory only (GitHub's revoke
// authenticates with the token itself) and never written anywhere; what is
// written is the assignment (the request) and the ledger.
//
// The App key is read once, when the issuer is built: rotating it takes a
// daemon restart.
type GitHubToken struct {
	spec Spec
	gh   *ghapp.Client
	o    Options
	born time.Time

	mu      sync.Mutex
	closed  bool
	held    map[string]*held // box key \x00 name
	old     []*retired       // renewed-away tokens and revocations to retry
	reissue []*pendingIssue  // assignments Start could not issue yet
}

// held is a box's current token under one name. A pending one is being
// issued (no token yet); done closes when it is settled either way.
type held struct {
	e        Entry // the ledger fields of this box, name and request
	req      Request
	box      Box
	perms    ghapp.Perms
	tok      ghapp.Token
	minted   time.Time
	pending  bool
	done     chan struct{}
	renewing bool
	nextTry  time.Time
}

// retired is a token to revoke at at.
type retired struct {
	e     Entry
	tok   ghapp.Token
	at    time.Time
	why   string
	tries int
}

// pendingIssue is an assignment to issue again at at.
type pendingIssue struct {
	req   Request
	at    time.Time
	tries int
}

// reissueTries bounds how often an assignment GitHub keeps failing for is
// tried again (once a minute): then it is dropped.
const reissueTries = 60

// Load builds the issuer named name from config.json's secrets.issuers.
func Load(ctx context.Context, f config.File, name string, o Options) (*GitHubToken, error) {
	specs, err := Specs(f)
	if err != nil {
		return nil, err
	}
	s, ok := specs[name]
	if !ok {
		return nil, fmt.Errorf("secrets.issuers has no issuer %q", name)
	}
	return New(ctx, s, o)
}

// New builds a GitHubToken from a validated spec, reading its App key.
func New(ctx context.Context, s Spec, o Options) (*GitHubToken, error) {
	if o.Deliver == nil || o.Ledger == nil || o.ClassOf == nil || o.Live == nil || o.Check == nil {
		return nil, errors.New("issuers: Deliver, Ledger, ClassOf, Live and Check are required")
	}
	if s.Kind != KindGitHubApp {
		return nil, fmt.Errorf("issuers: %s is not a %s issuer", s.Name, KindGitHubApp)
	}
	if o.Secrets == nil {
		o.Secrets = secrets.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Grace <= 0 {
		o.Grace = defaultGrace
	}
	if o.Renewers <= 0 {
		o.Renewers = 8
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	ref, err := secrets.Parse(s.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("secrets.issuers.%s: private_key: %w", s.Name, err)
	}
	key, err := o.Secrets.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("secrets.issuers.%s: private_key: %w", s.Name, err)
	}
	gh, err := ghapp.New(ghapp.Config{AppID: s.AppID, PrivateKeyPEM: []byte(strings.TrimSpace(key.Reveal())), APIURL: s.APIURL,
		HTTP: o.HTTP, Now: o.Now, Sleep: o.Sleep})
	if err != nil {
		return nil, fmt.Errorf("secrets.issuers.%s: %w", s.Name, err)
	}
	return &GitHubToken{spec: s, gh: gh, o: o, born: o.Now(), held: map[string]*held{}}, nil
}

// Kind is KindGitHubToken.
func (g *GitHubToken) Kind() string { return KindGitHubToken }

// Name is the issuer's name in config.
func (g *GitHubToken) Name() string { return g.spec.Name }

var (
	_ Issuer        = (*GitHubToken)(nil)
	_ grants.Issuer = (*GitHubToken)(nil)
)

func heldKey(b Box, name string) string { return b.key() + "\x00" + name }

// base is the ledger fields of a request.
func (g *GitHubToken) base(req Request) Entry {
	e := Entry{Issuer: g.spec.Name, Kind: KindGitHubToken, Name: req.Name, Project: req.Box.Project, Area: req.Box.Area, Box: req.Box.ID,
		Host: req.Box.Host, Repo: req.Repo, Permissions: permString(req.Permissions), Reason: req.Reason, Grant: req.Grant,
		Job: req.Subject.Job}
	switch {
	case req.Subject.Job != nil:
		j := req.Subject.Job
		e.Subject = "job:" + j.Repo + ":" + j.Workflow + "@" + j.Ref + "#" + strconv.FormatInt(j.RunID, 10)
	case req.Subject.IdentityID != "":
		e.Subject = "identity:" + req.Subject.IdentityID
	}
	return e
}

// record writes e to the ledger (a refusal is only published) and publishes
// renewals, revocations and refusals to the pool's event log, audience
// Operate on the box's area. A renewal neither the ledger nor the log takes
// is an error: the caller revokes it.
func (g *GitHubToken) record(ctx context.Context, e Entry, event, why string) error {
	e.At, e.Event, e.Why = g.o.Now().UTC(), event, why
	var err error
	if event != EventRefused {
		if err = g.o.Ledger.Record(context.WithoutCancel(ctx), e); err != nil {
			g.o.Log.Error("secrets issuer: the ledger refused a row", "issuer", g.spec.Name, "event", event, "err", err)
		}
	}
	typ := map[string]string{EventRenewed: EventSecretRenewed, EventRevoked: EventSecretRevoked, EventRefused: EventSecretRefused}[event]
	if typ != "" && g.o.Events != nil {
		if perr := g.publish(ctx, typ, e); perr != nil {
			g.o.Log.Error("secrets issuer: the event log refused an event", "issuer", g.spec.Name, "event", typ, "err", perr)
			if err == nil && event == EventRenewed {
				err = perr
			}
		}
	}
	return err
}

func (g *GitHubToken) publish(ctx context.Context, typ string, e Entry) error {
	d := SecretEvent{By: e.Subject, Job: e.Job, Kind: e.Kind, Issuer: e.Issuer, Names: []string{e.Name}, Box: e.Box, Host: e.Host,
		HostClass: e.HostClass, Grant: e.Grant, Reason: e.Reason, HashedToken: e.HashedToken, Why: e.Why}
	d.Count = len(d.Names)
	d.Params, _ = json.Marshal(map[string]any{"repo": e.Repo, "permissions": e.Permissions})
	if !e.ExpiresAt.IsZero() {
		d.ExpiresAt = e.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if e.Elevated != "" {
		d.Elevated = strings.Split(e.Elevated, ",")
	}
	_, err := g.o.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: typ, Subject: e.Box, Project: e.Project, Area: e.Area,
		Data: d, Audience: grants.LedgerAudience(authz.Operate, computeapi.SecretEventWithheld)})
	return err
}

func refusal(why string) error {
	return api.Errorf(api.CodeForbidden, "github-token refused: %s", why)
}

var errClosed = api.Errorf(api.CodeUnavailable, "github-token: the issuer is closed (the authority is stopping)")

// check is every refusal decided before GitHub is called. It answers the
// permissions to ask for (metadata read added) and the rule that covers them,
// or why not.
func (g *GitHubToken) check(req Request) (ghapp.Perms, Rule, string) {
	if !secretNameRE.MatchString(req.Name) {
		return nil, Rule{}, "the name must match [A-Z][A-Z0-9_]{0,63}"
	}
	if req.Box.Project == "" || req.Box.ID == "" {
		return nil, Rule{}, "no box"
	}
	if (req.Subject.IdentityID == "") == (req.Subject.Job == nil) {
		return nil, Rule{}, "the subject must be one identity or one job"
	}
	if !validRepo(req.Repo) {
		return nil, Rule{}, "the repository must be owner/name"
	}
	if len(req.Permissions) == 0 {
		return nil, Rule{}, "no permissions asked"
	}
	p := ghapp.Perms{"metadata": "read"}
	for k, v := range req.Permissions {
		if !permNameRE.MatchString(k) {
			return nil, Rule{}, "a permission is not a GitHub permission name"
		}
		if permTier(k) == "" {
			return nil, Rule{}, "permission " + k + " is never minted for a box"
		}
		if err := checkLevel(k, v); err != nil {
			return nil, Rule{}, err.Error()
		}
		p[k] = v
	}
	if len(elevatedWrite(p)) > 0 && strings.TrimSpace(req.Reason) == "" {
		return nil, Rule{}, "an elevated permission at write needs the request's reason"
	}
	if j := req.Subject.Job; j != nil {
		if why := jobrule.Check(*j); why != "" {
			return nil, Rule{}, why
		}
		if !strings.EqualFold(j.Repo, req.Repo) {
			return nil, Rule{}, "a job's token is for its own repository only"
		}
	}
	rule, ok := g.spec.match(req.Repo, req.Subject, p)
	if !ok {
		return nil, Rule{}, "outside the issuer's ceiling (repository, permission, event or workflow)"
	}
	return p, rule, ""
}

func validRepo(repo string) bool { return jobrule.ValidRepo(repo) }

// admit is check, the machine class and the grant: every refusal, decided
// before GitHub is called. It sets req's and e's grant from the grant
// check's Decision.
func (g *GitHubToken) admit(ctx context.Context, req *Request, e *Entry) (ghapp.Perms, Rule, string) {
	req.Grant, e.Grant = "", ""
	perms, rule, why := g.check(*req)
	if why != "" {
		return nil, Rule{}, why
	}
	class, err := g.o.ClassOf(ctx, req.Box)
	e.HostClass = class
	if err != nil || class != ClassTrusted {
		return nil, Rule{}, "the box's machine is not trusted"
	}
	dec, err := g.o.Check(ctx, *req)
	if err != nil {
		return nil, Rule{}, "no grant: " + err.Error()
	}
	if dec.Grant == "" {
		return nil, Rule{}, "no grant named by the grant check"
	}
	req.Grant, e.Grant = dec.Grant, dec.Grant
	return perms, rule, ""
}

// Issue mints a token for req.Box under req.Name, checks GitHub's answer,
// records it, delivers it and keeps the assignment. Every refusal is decided
// before GitHub is called, and every failure after a mint revokes what was
// minted. The same request again answers what the box holds, waiting for one
// still being issued.
func (g *GitHubToken) Issue(ctx context.Context, req Request) (Issued, error) {
	return g.issue(ctx, req, false)
}

// issue is Issue; again is a re-issue after a restart (Start, Tick), which
// a job's credential gets only while JobHolds says the job still holds the
// box.
func (g *GitHubToken) issue(ctx context.Context, req Request, again bool) (Issued, error) {
	e := g.base(req)
	perms, rule, why := g.admit(ctx, &req, &e)
	if why != "" {
		g.record(ctx, e, EventRefused, why)
		return Issued{}, refusal(why)
	}
	e.Permissions = permString(perms)
	e.Elevated = strings.Join(elevatedWrite(perms), ",")
	e.RuleReason = rule.Reason
	k := heldKey(req.Box, req.Name)
	var me *held
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return Issued{}, errClosed
		}
		h := g.held[k]
		if h == nil {
			me = &held{e: e, req: req, box: req.Box, perms: perms, pending: true, done: make(chan struct{})}
			g.held[k] = me
			g.mu.Unlock()
			break
		}
		same := strings.EqualFold(h.e.Repo, req.Repo) && h.e.Permissions == e.Permissions && h.e.Subject == e.Subject
		pending, done, out := h.pending, h.done, g.issued(h)
		g.mu.Unlock()
		if !same {
			why := "the box already holds this name (or is being issued it) for another request"
			g.record(ctx, e, EventRefused, why)
			return Issued{}, api.Errorf(api.CodeConflict, "github-token refused: %s", why)
		}
		if !pending {
			return out, nil
		}
		select { // the same request in flight: wait for it, then look again
		case <-done:
		case <-ctx.Done():
			return Issued{}, ctx.Err()
		}
	}
	defer close(me.done)
	drop := func() {
		g.mu.Lock()
		if g.held[k] == me {
			delete(g.held, k)
		}
		g.mu.Unlock()
	}

	// Asked again now that the box is held here: an End (a demotion, the
	// job's completion) from this point on removes this pending entry, and
	// the issue then revokes what it minted; one before it is seen here.
	if why, err := g.stillHolds(ctx, req, again); why != "" || err != nil {
		drop()
		if err != nil {
			g.record(ctx, e, EventFailed, "asking whether the job still holds the box: "+err.Error())
			return Issued{}, api.Errorf(api.CodeUnavailable, "github-token: could not ask whether the job still holds the box: %v", err)
		}
		g.record(ctx, e, EventRefused, why)
		return Issued{}, refusal(why)
	}
	if req.Installation <= 0 {
		mctx, cancel := context.WithTimeout(ctx, mintTimeout)
		id, err := g.gh.RepoInstallation(mctx, req.Repo)
		cancel()
		if err != nil {
			drop()
			g.record(ctx, e, EventFailed, "no installation of the App on the repository's account: "+err.Error())
			return Issued{}, api.Errorf(api.CodeUnavailable, "github-token: no installation of the App on %s's account", owner(req.Repo))
		}
		req.Installation = id
		me.req.Installation = id
	}
	tok, err := g.mint(ctx, e, req.Installation, req.Repo, perms)
	if err != nil {
		drop()
		return Issued{}, api.Errorf(api.CodeUnavailable, "github-token: GitHub did not mint the token: %v", err)
	}
	e.ExpiresAt, e.HashedToken = tok.ExpiresAt, tok.HashedToken()
	if err := g.record(ctx, e, EventIssued, ""); err != nil {
		drop()
		g.revokeNow(ctx, e, tok, "the ledger refused the issue")
		return Issued{}, api.Errorf(api.CodeUnavailable, "github-token: the ledger refused the row; the token was revoked")
	}
	if err := g.deliver(ctx, req.Box, req.Name, tok); err != nil {
		drop()
		if errors.Is(err, rt.ErrMaybeDelivered) {
			g.record(ctx, e, EventMaybeDelivered, err.Error())
			g.revokeNow(ctx, e, tok, "delivery may have reached the box")
			return Issued{}, fmt.Errorf("%w: %w", api.Errorf(api.CodeUnavailable,
				"github-token: the delivery may have reached the box (the token was revoked; do not deliver again): %v", err), rt.ErrMaybeDelivered)
		}
		g.revokeNow(ctx, e, tok, "delivery failed")
		return Issued{}, api.Errorf(api.CodeUnavailable, "github-token: delivering to the box failed (the token was revoked): %v", err)
	}
	g.mu.Lock()
	if g.held[k] != me || g.closed { // the box ended, or the issuer closed, while it was being issued
		if g.held[k] == me {
			delete(g.held, k)
		}
		g.mu.Unlock()
		g.revokeNow(ctx, e, tok, "the box ended while it was being issued")
		return Issued{}, api.Errorf(api.CodeConflict, "github-token: the box ended while the token was being issued; it was revoked")
	}
	me.e, me.tok, me.minted, me.pending = e, tok, g.o.Now(), false
	out := g.issued(me)
	g.mu.Unlock()
	if err := g.o.Ledger.Assign(context.WithoutCancel(ctx), g.spec.Name, req); err != nil {
		g.o.Log.Error("secrets issuer: the assignment was not kept; after a restart this box gets no new token", "issuer", g.spec.Name,
			"box", req.Box.key(), "name", req.Name, "err", err)
	}
	return out, nil
}

func owner(repo string) string { o, _, _ := strings.Cut(repo, "/"); return o }

// stillHolds is why req's box may not get a token now that its issue holds
// the name ("" it may): its machine is asked again (a demotion between the
// admission and here is seen), and a job's re-issue asks JobHolds. err is
// JobHolds not answering.
func (g *GitHubToken) stillHolds(ctx context.Context, req Request, again bool) (string, error) {
	if class, err := g.o.ClassOf(ctx, req.Box); err != nil || class != ClassTrusted {
		return "the box's machine is not trusted", nil
	}
	if !again || req.Subject.Job == nil {
		return "", nil
	}
	return g.jobHolds(ctx, req)
}

// jobHolds is why job request req may not be renewed or issued again (""
// it may), from JobHolds.
func (g *GitHubToken) jobHolds(ctx context.Context, req Request) (string, error) {
	if g.o.JobHolds == nil {
		return "nothing on this machine can say whether the job still holds the box", nil
	}
	ok, err := g.o.JobHolds(ctx, req)
	if err != nil {
		return "", err
	}
	if !ok {
		return "the job no longer holds the box", nil
	}
	return "", nil
}

func (g *GitHubToken) issued(h *held) Issued {
	return Issued{Name: h.e.Name, Repo: h.e.Repo, Permissions: maps.Clone(map[string]string(h.perms)), ExpiresAt: h.e.ExpiresAt,
		HashedToken: h.e.HashedToken}
}

// mint mints and records a failure. A refused answer GitHub would not let us
// revoke goes to the revocation retries.
func (g *GitHubToken) mint(ctx context.Context, e Entry, inst int64, repo string, p ghapp.Perms) (ghapp.Token, error) {
	mctx, cancel := context.WithTimeout(ctx, mintTimeout)
	tok, err := g.gh.MintToken(mctx, inst, repo, p)
	cancel()
	if err == nil {
		return tok, nil
	}
	var ae *ghapp.AnswerError
	if errors.As(err, &ae) && ae.Unrevoked.Reveal() != "" {
		u := ae.Unrevoked
		e.ExpiresAt, e.HashedToken = u.ExpiresAt, u.HashedToken()
		g.record(ctx, e, EventFailed, scrub(err.Error(), u))
		g.mu.Lock()
		g.old = append(g.old, &retired{e: e, tok: u, at: g.o.Now().Add(retryEvery), why: "refused answer", tries: 1})
		g.mu.Unlock()
		return ghapp.Token{}, errors.New(scrub(err.Error(), u))
	}
	g.record(ctx, e, EventFailed, err.Error())
	return ghapp.Token{}, err
}

// deliver hands the value to the Deliverer. Its error, or a panic, is
// returned with the value scrubbed out.
func (g *GitHubToken) deliver(ctx context.Context, box Box, name string, tok ghapp.Token) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the deliverer panicked: %s", scrub(fmt.Sprint(r), tok))
		}
	}()
	if err := g.o.Deliver.Deliver(ctx, box, name, secrets.NewValue(tok.Reveal())); err != nil {
		if errors.Is(err, rt.ErrMaybeDelivered) { // kept: the caller must not deliver again as if nothing arrived
			return fmt.Errorf("%w: %s", rt.ErrMaybeDelivered, scrub(err.Error(), tok))
		}
		return errors.New(scrub(err.Error(), tok))
	}
	return nil
}

func scrub(s string, tok ghapp.Token) string {
	if v := tok.Reveal(); v != "" {
		s = strings.ReplaceAll(s, v, "[secret]")
	}
	return s
}

// revokeNow revokes tok and records it; a failure is retried by Tick.
func (g *GitHubToken) revokeNow(ctx context.Context, e Entry, tok ghapp.Token, why string) bool {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
	err := g.gh.RevokeToken(rctx, tok)
	cancel()
	e.ExpiresAt, e.HashedToken = tok.ExpiresAt, tok.HashedToken()
	if err == nil {
		g.record(ctx, e, EventRevoked, why)
		return true
	}
	g.record(ctx, e, EventFailed, "revoke failed ("+why+"): "+scrub(err.Error(), tok))
	g.o.Log.Warn("secrets issuer: a revoke failed; retrying", "issuer", g.spec.Name, "box", e.Project+"/"+e.Box, "name", e.Name, "err", scrub(err.Error(), tok))
	g.mu.Lock()
	g.old = append(g.old, &retired{e: e, tok: tok, at: g.o.Now().Add(retryEvery), why: why, tries: 1})
	g.mu.Unlock()
	return false
}

// renewAt is two thirds of the token's life.
func renewAt(h *held) time.Time {
	life := h.tok.ExpiresAt.Sub(h.minted)
	return h.minted.Add(life * 2 / 3)
}

// Tick renews every token at two thirds of its life (through the Deliverer,
// replacing the old value), revokes renewed-away tokens after the grace,
// retries failed revocations and re-issues, and revokes a token that reached
// its expiry unrenewed. Renewals and revocations run concurrently, at most
// Options.Renewers at once. It answers when it is next due, never later than
// a minute from now, so an owner that sleeps until then never misses a token
// issued meanwhile.
func (g *GitHubToken) Tick(ctx context.Context) time.Time {
	now := g.o.Now()
	var renew []*held
	var due []*retired
	var again []*pendingIssue
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return now.Add(tickCap)
	}
	var lapsed []*held
	for k, h := range g.held {
		if h.pending || h.renewing {
			continue
		}
		if !now.Before(h.tok.ExpiresAt) {
			delete(g.held, k)
			lapsed = append(lapsed, h)
			continue
		}
		if !now.Before(renewAt(h)) && !now.Before(h.nextTry) {
			h.renewing = true
			renew = append(renew, h)
		}
	}
	for _, h := range lapsed {
		// GitHub should no longer take it; revoke anyway, until expiry plus a margin.
		due = append(due, &retired{e: h.e, tok: h.tok, at: now, why: "lapsed: renewal did not succeed before expiry"})
	}
	keep := g.old[:0]
	for _, r := range g.old {
		if now.Before(r.at) {
			keep = append(keep, r)
		} else {
			due = append(due, r)
		}
	}
	g.old = keep
	keepRe := g.reissue[:0]
	for _, p := range g.reissue {
		if now.Before(p.at) {
			keepRe = append(keepRe, p)
		} else {
			again = append(again, p)
		}
	}
	g.reissue = keepRe
	g.mu.Unlock()
	for _, h := range lapsed {
		g.record(ctx, h.e, EventFailed, "renewal did not succeed before the token expired; the box has no token")
		if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.spec.Name, h.box, h.e.Name); err != nil {
			g.o.Log.Error("secrets issuer: an assignment was not removed", "issuer", g.spec.Name, "err", err)
		}
	}
	g.parallel(len(renew), func(i int) { g.renew(ctx, renew[i]) })
	g.parallel(len(due), func(i int) { g.retry(ctx, due[i]) })
	for _, p := range again {
		g.reissueOne(ctx, p)
	}
	next := g.next()
	if next.IsZero() || next.After(now.Add(tickCap)) {
		next = now.Add(tickCap)
	}
	return next
}

// parallel runs f(0..n-1), at most Options.Renewers at once.
func (g *GitHubToken) parallel(n int, f func(int)) {
	sem := make(chan struct{}, g.o.Renewers)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			f(i)
		}()
	}
	wg.Wait()
}

// next is the earliest time Tick has work (zero: none).
func (g *GitHubToken) next() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	var t time.Time
	early := func(x time.Time) {
		if t.IsZero() || x.Before(t) {
			t = x
		}
	}
	for _, h := range g.held {
		if h.pending {
			continue
		}
		at := renewAt(h)
		if h.nextTry.After(at) {
			at = h.nextTry
		}
		if h.tok.ExpiresAt.Before(at) {
			at = h.tok.ExpiresAt
		}
		early(at)
	}
	for _, r := range g.old {
		early(r.at)
	}
	for _, p := range g.reissue {
		early(p.at)
	}
	return t
}

func (g *GitHubToken) renew(ctx context.Context, h *held) {
	k := heldKey(h.box, h.e.Name)
	fail := func(why string) {
		g.record(ctx, h.e, EventFailed, "renewal: "+why)
		g.o.Log.Warn("secrets issuer: renewal failed; the old token stays until it expires", "issuer", g.spec.Name,
			"box", h.box.key(), "name", h.e.Name, "why", why)
		g.mu.Lock()
		h.renewing, h.nextTry = false, g.o.Now().Add(retryEvery)
		g.mu.Unlock()
	}
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		g.mu.Lock()
		h.renewing = false
		g.mu.Unlock()
		return
	}
	class, err := g.o.ClassOf(ctx, h.box)
	if err != nil || class != ClassTrusted {
		g.mu.Lock()
		h.renewing = false
		g.mu.Unlock()
		g.End(ctx, h.box, EndHostDemoted)
		return
	}
	g.mu.Lock()
	req := h.req
	g.mu.Unlock()
	if req.Subject.Job != nil {
		why, err := g.jobHolds(ctx, req)
		if err != nil {
			fail("asking whether the job still holds the box: " + err.Error())
			return
		}
		if why != "" {
			g.mu.Lock()
			h.renewing = false
			g.mu.Unlock()
			g.End(ctx, h.box, EndJobCompleted)
			return
		}
	}
	dec, err := g.o.Check(ctx, req)
	if err == nil && dec.Grant == "" {
		err = errors.New("no grant named")
	}
	if err != nil {
		g.mu.Lock()
		h.renewing = false
		g.mu.Unlock()
		g.endOne(ctx, k, "no grant at renewal: "+err.Error())
		return
	}
	g.mu.Lock()
	h.req.Grant, h.e.Grant = dec.Grant, dec.Grant // a renewal is under the grant that covers it now
	e := h.e
	g.mu.Unlock()
	tok, err := g.mint(ctx, e, h.req.Installation, h.e.Repo, h.perms)
	if err != nil {
		fail(err.Error())
		return
	}
	e.HostClass, e.ExpiresAt, e.HashedToken = class, tok.ExpiresAt, tok.HashedToken()
	if err := g.record(ctx, e, EventRenewed, ""); err != nil {
		g.revokeNow(ctx, e, tok, "the ledger refused the renewal")
		fail("the ledger refused the row")
		return
	}
	if err := g.deliver(ctx, h.box, h.e.Name, tok); err != nil {
		// A renewal that may have reached the box is retried all the same: the
		// box may now hold the revoked new token, and only a renewal puts a
		// live one back (a whole-file replace, never a second live value).
		if errors.Is(err, rt.ErrMaybeDelivered) {
			g.record(ctx, e, EventMaybeDelivered, "renewal: "+err.Error())
		}
		g.revokeNow(ctx, e, tok, "renewal delivery failed")
		fail("delivery: " + err.Error())
		return
	}
	g.mu.Lock()
	if g.held[k] != h || g.closed { // ended or closed meanwhile: they revoked the old one
		g.mu.Unlock()
		g.revokeNow(ctx, e, tok, "the box ended during renewal")
		return
	}
	old := &retired{e: h.e, tok: h.tok, at: g.o.Now().Add(g.o.Grace), why: "renewed"}
	h.e, h.tok, h.minted, h.renewing, h.nextTry = e, tok, g.o.Now(), false, time.Time{}
	g.old = append(g.old, old)
	g.mu.Unlock()
}

// retry revokes a retired token, or reschedules it. GitHub answering 401
// counts as revoked; one still unrevoked lapseMargin past its expiry is
// recorded as expired and dropped.
func (g *GitHubToken) retry(ctx context.Context, r *retired) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
	err := g.gh.RevokeToken(rctx, r.tok)
	cancel()
	e := r.e
	e.ExpiresAt, e.HashedToken = r.tok.ExpiresAt, r.tok.HashedToken()
	if err == nil {
		g.record(ctx, e, EventRevoked, r.why)
		return
	}
	now := g.o.Now()
	if !now.Before(r.tok.ExpiresAt.Add(lapseMargin)) {
		g.record(ctx, e, EventExpired, "revocation never succeeded ("+r.why+"): "+scrub(err.Error(), r.tok))
		return
	}
	if r.tries == 0 {
		g.record(ctx, e, EventFailed, "revoke failed ("+r.why+"): "+scrub(err.Error(), r.tok))
	}
	r.tries++
	r.at = now.Add(min(retryEvery*time.Duration(r.tries), retryMax))
	g.mu.Lock()
	g.old = append(g.old, r)
	g.mu.Unlock()
}

// End revokes every token issued for box, and any renewed-away token of it
// still in its grace, and forgets the box's assignments. A revocation GitHub
// does not confirm is retried by Tick until it does or the token is well past
// its expiry. One being issued is revoked as its issue finishes.
func (g *GitHubToken) End(ctx context.Context, box Box, why EndReason) error {
	if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.spec.Name, box, ""); err != nil {
		g.o.Log.Error("secrets issuer: an assignment was not removed; Start asks the owner before issuing to it", "issuer", g.spec.Name,
			"box", box.key(), "err", err)
	}
	g.mu.Lock()
	keep := g.reissue[:0]
	for _, p := range g.reissue {
		if p.req.Box.key() != box.key() {
			keep = append(keep, p)
		}
	}
	g.reissue = keep
	g.mu.Unlock()
	return g.revokeBox(ctx, box, string(why))
}

// EndGrant revokes every token issued under grant id and forgets those
// assignments (the grant was deleted; the grants package calls it).
func (g *GitHubToken) EndGrant(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("issuers: EndGrant needs a grant id")
	}
	var ks []string
	g.mu.Lock()
	for k, h := range g.held {
		if h.req.Grant == id {
			ks = append(ks, k)
		}
	}
	keep := g.reissue[:0]
	for _, p := range g.reissue {
		if p.req.Grant != id {
			keep = append(keep, p)
		}
	}
	g.reissue = keep
	g.mu.Unlock()
	for _, k := range ks {
		g.endOne(ctx, k, string(EndGrant))
	}
	reqs, err := g.o.Ledger.Assigned(ctx, g.spec.Name)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if r.Grant == id {
			if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.spec.Name, r.Box, r.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// endOne revokes the token held under k and forgets its assignment.
func (g *GitHubToken) endOne(ctx context.Context, k, why string) {
	g.mu.Lock()
	h := g.held[k]
	if h != nil {
		delete(g.held, k)
	}
	g.mu.Unlock()
	if h == nil || h.pending {
		return
	}
	if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.spec.Name, h.box, h.e.Name); err != nil {
		g.o.Log.Error("secrets issuer: an assignment was not removed", "issuer", g.spec.Name, "err", err)
	}
	g.revokeNow(ctx, h.e, h.tok, why)
}

// revokeBox revokes box's tokens, keeping its assignments.
func (g *GitHubToken) revokeBox(ctx context.Context, box Box, why string) error {
	var hs []*held
	var olds []*retired
	g.mu.Lock()
	for k, h := range g.held {
		if h.box.key() == box.key() {
			delete(g.held, k)
			if !h.pending {
				hs = append(hs, h)
			}
		}
	}
	keep := g.old[:0]
	for _, r := range g.old {
		if r.e.Project == box.Project && r.e.Box == box.ID {
			olds = append(olds, r)
		} else {
			keep = append(keep, r)
		}
	}
	g.old = keep
	g.mu.Unlock()
	failed := 0
	for _, h := range hs {
		if !g.revokeNow(ctx, h.e, h.tok, why) {
			failed++
		}
	}
	for _, r := range olds {
		r.why = why
		g.retry(ctx, r)
	}
	if failed > 0 {
		return api.Errorf(api.CodeUnavailable, "github-token: %d revocation(s) for box %s not confirmed; retried until GitHub confirms or the token expires", failed, box.key())
	}
	return nil
}

// Sweep ends every box holding a token that live does not answer true for.
func (g *GitHubToken) Sweep(ctx context.Context, live func(Box) bool) {
	seen := map[string]Box{}
	g.mu.Lock()
	for _, h := range g.held {
		if !h.pending {
			seen[h.box.key()] = h.box
		}
	}
	g.mu.Unlock()
	for _, b := range seen {
		if !live(b) {
			g.End(ctx, b, EndGone)
		}
	}
}

// EndUntrusted ends every box holding a token, or being issued one, that
// ClassOf no longer answers trusted for (EndHostDemoted). A box being issued
// is ended too: its issue then revokes what it minted. A box whose issue
// starts after this read asks ClassOf itself once it holds the name
// (stillHolds), so a demotion is never missed between the two.
func (g *GitHubToken) EndUntrusted(ctx context.Context) {
	seen := map[string]Box{}
	g.mu.Lock()
	for _, h := range g.held {
		seen[h.box.key()] = h.box
	}
	g.mu.Unlock()
	for _, b := range seen {
		if class, err := g.o.ClassOf(ctx, b); err != nil || class != ClassTrusted {
			g.End(ctx, b, EndHostDemoted)
		}
	}
}

// Start settles what an earlier run minted and never revoked (it crashed:
// tokens are held in memory only, so they cannot be revoked and lapse at
// their expiry, at most GitHub's hour), recording each expired_unrevoked.
// Then it issues again, through the whole of Issue (ceiling, class, grant),
// to every assigned box its owner says still lives, delivering the new token
// in place of the old. An assignment whose box is gone, or that no longer
// passes, is dropped; one GitHub fails for is tried again by Tick.
func (g *GitHubToken) Start(ctx context.Context) error {
	es, err := g.o.Ledger.Outstanding(ctx, g.spec.Name)
	if err != nil {
		return err
	}
	for _, e := range es {
		if !e.At.Before(g.born) {
			continue
		}
		g.o.Log.Warn("secrets issuer: a token of an earlier run could not be revoked; it lapses at its expiry", "issuer", g.spec.Name,
			"box", e.Project+"/"+e.Box, "name", e.Name, "repo", e.Repo, "expires", e.ExpiresAt)
		if err := g.record(ctx, e, EventExpiredUnrevoked, "the authority restarted: the token was held in memory only and lapses at its expiry"); err != nil {
			return err
		}
	}
	reqs, err := g.o.Ledger.Assigned(ctx, g.spec.Name)
	if err != nil {
		return err
	}
	for _, req := range reqs {
		g.reissueOne(ctx, &pendingIssue{req: req})
	}
	return nil
}

// reissueOne issues an assignment again if its box lives.
func (g *GitHubToken) reissueOne(ctx context.Context, p *pendingIssue) {
	req := p.req
	unassign := func() {
		if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.spec.Name, req.Box, req.Name); err != nil {
			g.o.Log.Error("secrets issuer: an assignment was not removed", "issuer", g.spec.Name, "err", err)
		}
	}
	later := func() {
		if p.tries++; p.tries >= reissueTries {
			g.o.Log.Warn("secrets issuer: giving up issuing again after a restart", "issuer", g.spec.Name, "box", req.Box.key(), "name", req.Name)
			unassign()
			return
		}
		p.at = g.o.Now().Add(retryEvery)
		g.mu.Lock()
		g.reissue = append(g.reissue, p)
		g.mu.Unlock()
	}
	live, err := g.o.Live(ctx, req.Box)
	if err != nil {
		later()
		return
	}
	if !live {
		unassign()
		return
	}
	_, err = g.issue(ctx, req, true)
	var ae *api.Error
	switch {
	case err == nil:
	case errors.Is(err, errClosed):
	case errors.Is(err, rt.ErrMaybeDelivered):
		unassign() // never delivered again: the box keeps nothing live (the token was revoked)
	case errors.As(err, &ae) && ae.Code == api.CodeUnavailable:
		later()
	case errors.As(err, &ae) && ae.Code == api.CodeConflict:
		// the box holds another request under the name now: its row stands
	default:
		unassign()
	}
}

// Close refuses every later Issue and renewal, and revokes every token still
// held and every renewed-away one (the authority stops: nothing it minted
// should outlive it). It keeps the assignments, so Start issues again. One
// GitHub does not confirm stays outstanding in the ledger, and the next Start
// records it.
func (g *GitHubToken) Close(ctx context.Context) error {
	boxes := map[string]Box{}
	g.mu.Lock()
	g.closed = true
	for _, h := range g.held {
		boxes[h.box.key()] = h.box
	}
	olds := g.old
	g.old, g.reissue = nil, nil
	g.mu.Unlock()
	var first error
	for _, b := range boxes {
		if err := g.revokeBox(ctx, b, string(EndShutdown)); err != nil && first == nil {
			first = err
		}
	}
	for _, r := range olds {
		g.retry(ctx, r)
	}
	g.mu.Lock()
	g.old = nil // Close does not keep retrying; Start settles the rest
	g.mu.Unlock()
	return first
}
