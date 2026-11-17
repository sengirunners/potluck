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
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Options are what an issuer needs from its owner.
type Options struct {
	Deliver Deliverer // required: puts the value into the box
	Ledger  Ledger    // required: SQLLedger in the daemon
	ClassOf ClassOf   // required: the box's machine class, asked at every issue and renewal
	Live    Live      // required: whether an assigned box still exists, asked by Start
	Check   Check     // required: the grant check (GrantCheck)
	// BoxTakesSecrets is asked before a credential is minted: an error is
	// why the box's host may not be given one (swap that could put it on
	// disk), so a credential the door would refuse is never minted. nil:
	// not asked.
	BoxTakesSecrets func(ctx context.Context, box Box) error
	// JobHolds is asked before a job's credential is renewed or issued
	// again; nil: never renewed or issued again (it lapses or is ended).
	JobHolds JobHolds
	Events   Publisher // the pool's event log: renewed, revoked and refused (nil: none published)

	// Tokens, BoxExpires and Home are a potluck issuer's (required there):
	// the pool's token mint, each box's lease end, and the authority a box
	// reaches (its tunnel URL and key fingerprint).
	Tokens     TokenMint
	BoxExpires BoxExpires
	Home       Home

	// Secrets, HTTP and Sleep are a github-app issuer's: its App key
	// reference (default secrets.Default()), its HTTP client, and the wait
	// between GitHub retries (tests).
	Secrets  *secrets.Reader
	HTTP     *http.Client
	Sleep    func(context.Context, time.Duration) error
	Now      func() time.Time
	Log      *slog.Logger
	Grace    time.Duration // a renewed-away credential is revoked this long after (default 5 minutes)
	Renewers int           // renewals and revocations Tick runs at once (default 8)
}

// Timing.
const (
	defaultGrace = 5 * time.Minute
	retryEvery   = time.Minute      // after a failed renewal, re-issue or revocation
	retryMax     = 5 * time.Minute  // the longest gap between revocation retries
	lapseMargin  = 10 * time.Minute // a revocation is retried this long past the credential's expiry
	tickCap      = time.Minute      // Tick never answers later than this
	mintTimeout  = 30 * time.Second
)

// Credential is one minted value: what the box gets, when it expires at its
// source, and the hash the ledger names it by. It is held in memory only.
type Credential interface {
	Reveal() string
	Expires() time.Time
	Hashed() string
}

// secretParts is a credential whose Reveal bundles secrets that may also
// appear alone (a box credential's token and key): each is scrubbed too.
type secretParts interface {
	parts() []string
}

// backend is one kind's own part of an issuer: what it checks in a request,
// how it mints and revokes at its source, and its grants' ceilings and
// params. Everything else (admission, holding, renewal, revocation on every
// end, the ledger, restarts) is Minted's, shared by every kind.
type backend interface {
	kind() string
	// check is every refusal decided before the machine class and the
	// grant: what to mint, or why not. err is the kind's source not
	// answering (not a refusal: the caller tries again).
	check(ctx context.Context, req Request) (plan, string, error)
	// mint mints what p names for req, bounding each call to its source
	// (mintTimeout). It may fill in fields of req it looked up (the App's
	// installation); they are kept for renewals. An error that is a
	// publicError answers the caller only its public text.
	mint(ctx context.Context, req *Request, p plan) (Credential, error)
	// revoke revokes c at its source: nil once the source confirms it.
	revoke(ctx context.Context, c Credential) error
	// settle is an earlier run's credential e, minted and never revoked
	// (the authority crashed): true when it was revoked now, false when it
	// can only lapse at its expiry.
	settle(ctx context.Context, e Entry) bool
	ceiling(raw json.RawMessage) (json.RawMessage, error)
	within(ceiling, params json.RawMessage) error
}

// plan is what a request is minted as, decided by the backend's check.
type plan struct {
	perms      map[string]string // what the credential may do (github-token: permissions, metadata read added)
	elevated   []string          // the elevated permissions granted at write, sorted
	ruleReason string            // the config rule's reason
	as         string            // potluck-token: the identity the token acts as
}

// publicError is a mint failure whose detail stays in the ledger: the
// caller is answered public() only.
type publicError interface {
	error
	public() string
}

// unrevokedError is a mint whose source answered with a credential the
// backend refused and could not revoke: Minted retries the revocation.
type unrevokedError struct {
	cred Credential
	err  error
}

func (e *unrevokedError) Error() string { return e.err.Error() }

// Minted is one issuer of a minted kind: per box, a credential its backend
// mints inside the issuer's config ceiling. Credentials are held in memory
// only and never written anywhere; what is written is the assignment (the
// request) and the ledger.
//
// The backend's key is read once, when the issuer is built: rotating it
// takes a daemon restart.
type Minted struct {
	name string // the issuer's name in config
	b    backend
	o    Options
	born time.Time

	mu      sync.Mutex
	closed  bool
	held    map[string]*held // box key \x00 name
	old     []*retired       // renewed-away credentials and revocations to retry
	reissue []*pendingIssue  // assignments Start could not issue yet
}

// held is a box's current credential under one name. A pending one is
// being issued (no credential yet); done closes when it is settled either
// way.
type held struct {
	e        Entry // the ledger fields of this box, name and request
	req      Request
	box      Box
	plan     plan
	tok      Credential
	minted   time.Time
	pending  bool
	done     chan struct{}
	renewing bool
	nextTry  time.Time
}

// retired is a credential to revoke at at.
type retired struct {
	e     Entry
	tok   Credential
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

// reissueTries bounds how often an assignment the source keeps failing for
// is tried again (once a minute): then it is dropped.
const reissueTries = 60

// Load builds the issuer named name from config.json's secrets.issuers.
func Load(ctx context.Context, f config.File, name string, o Options) (*Minted, error) {
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

// New builds an issuer from a validated spec, reading its key.
func New(ctx context.Context, s Spec, o Options) (*Minted, error) {
	if o.Deliver == nil || o.Ledger == nil || o.ClassOf == nil || o.Live == nil || o.Check == nil {
		return nil, errors.New("issuers: Deliver, Ledger, ClassOf, Live and Check are required")
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
	var b backend
	var err error
	switch s.Kind {
	case KindGitHubApp:
		b, err = newGitHubBackend(ctx, s, o)
	case KindPotluck:
		b, err = newPotluckBackend(s, o)
	default:
		return nil, fmt.Errorf("issuers: %s: no issuer of kind %q", s.Name, s.Kind)
	}
	if err != nil {
		return nil, err
	}
	return &Minted{name: s.Name, b: b, o: o, born: o.Now(), held: map[string]*held{}}, nil
}

// Kind is the credential kind the issuer mints.
func (g *Minted) Kind() string { return g.b.kind() }

// Name is the issuer's name in config.
func (g *Minted) Name() string { return g.name }

// Ceiling checks a grant's ceiling when the grant is added and answers it in
// its stored form (grants.Issuer). One not inside the config ceiling is
// refused.
func (g *Minted) Ceiling(raw json.RawMessage) (json.RawMessage, error) { return g.b.ceiling(raw) }

// Within reports whether a request's params sit inside a stored grant
// ceiling (nil: they do; grants.Issuer). It never mints.
func (g *Minted) Within(ceiling, params json.RawMessage) error { return g.b.within(ceiling, params) }

var (
	_ Issuer        = (*Minted)(nil)
	_ grants.Issuer = (*Minted)(nil)
)

func heldKey(b Box, name string) string { return b.key() + "\x00" + name }

// base is the ledger fields of a request.
func (g *Minted) base(req Request) Entry {
	e := Entry{Issuer: g.name, Kind: g.b.kind(), Name: req.Name, Project: req.Box.Project, Area: req.Box.Area, Box: req.Box.ID,
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
func (g *Minted) record(ctx context.Context, e Entry, event, why string) error {
	e.At, e.Event, e.Why = g.o.Now().UTC(), event, why
	var err error
	if event != EventRefused {
		if err = g.o.Ledger.Record(context.WithoutCancel(ctx), e); err != nil {
			g.o.Log.Error("secrets issuer: the ledger refused a row", "issuer", g.name, "event", event, "err", err)
		}
	}
	typ := map[string]string{EventRenewed: EventSecretRenewed, EventRevoked: EventSecretRevoked, EventRefused: EventSecretRefused}[event]
	if typ != "" && g.o.Events != nil {
		if perr := g.publish(ctx, typ, e); perr != nil {
			g.o.Log.Error("secrets issuer: the event log refused an event", "issuer", g.name, "event", typ, "err", perr)
			if err == nil && event == EventRenewed {
				err = perr
			}
		}
	}
	return err
}

func (g *Minted) publish(ctx context.Context, typ string, e Entry) error {
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

func (g *Minted) refusal(why string) error {
	return api.Errorf(api.CodeForbidden, "%s refused: %s", g.b.kind(), why)
}

// errClosed matches (errors.Is) every Issue refused because the issuer
// closed.
var errClosed = errors.New("the issuer is closed")

// closedError is that refusal: an unavailable api error that is errClosed.
type closedError struct{ e *api.Error }

func (c closedError) Error() string        { return c.e.Error() }
func (c closedError) Unwrap() error        { return c.e }
func (c closedError) Is(target error) bool { return target == errClosed }

func (g *Minted) closedErr() error {
	return closedError{api.Errorf(api.CodeUnavailable, "%s: the issuer is closed (the authority is stopping)", g.b.kind())}
}

// unavailable is an api CodeUnavailable error prefixed with the kind.
func (g *Minted) unavailable(format string, a ...any) error {
	return api.Errorf(api.CodeUnavailable, "%s: %s", g.b.kind(), fmt.Sprintf(format, a...))
}

// admit is the backend's check, the machine class and the grant: every
// refusal, decided before the source is called. It sets req's and e's grant
// from the grant check's Decision.
func (g *Minted) admit(ctx context.Context, req *Request, e *Entry) (plan, string, error) {
	req.Grant, e.Grant = "", ""
	p, why, err := g.check(ctx, *req)
	if why != "" || err != nil {
		return plan{}, why, err
	}
	class, err := g.o.ClassOf(ctx, req.Box)
	e.HostClass = class
	if err != nil || class != ClassTrusted {
		return plan{}, "the box's machine is not trusted", nil
	}
	dec, err := g.o.Check(ctx, *req)
	if err != nil {
		return plan{}, "no grant: " + err.Error(), nil
	}
	if dec.Grant == "" {
		return plan{}, "no grant named by the grant check", nil
	}
	req.Grant, e.Grant = dec.Grant, dec.Grant
	return p, "", nil
}

// check is what every kind refuses (the name, the box, the subject), then
// the backend's own check.
func (g *Minted) check(ctx context.Context, req Request) (plan, string, error) {
	if req.Kind != "" && req.Kind != g.b.kind() {
		return plan{}, "a " + req.Kind + " request for a " + g.b.kind() + " issuer", nil
	}
	if !secretNameRE.MatchString(req.Name) {
		return plan{}, "the name must match [A-Z][A-Z0-9_]{0,63}", nil
	}
	if req.Box.Project == "" || req.Box.ID == "" {
		return plan{}, "no box", nil
	}
	if (req.Subject.IdentityID == "") == (req.Subject.Job == nil) {
		return plan{}, "the subject must be one identity or one job", nil
	}
	return g.b.check(ctx, req)
}

// takesSecrets is Options.BoxTakesSecrets, nil when it is not set.
func (g *Minted) takesSecrets(ctx context.Context, box Box) error {
	if g.o.BoxTakesSecrets == nil {
		return nil
	}
	return g.o.BoxTakesSecrets(ctx, box)
}

// Issue mints a credential for req.Box under req.Name, records it, delivers
// it and keeps the assignment. Every refusal is decided before the source is
// called, and every failure after a mint revokes what was minted. The same
// request again answers what the box holds, waiting for one still being
// issued.
func (g *Minted) Issue(ctx context.Context, req Request) (Issued, error) {
	return g.issue(ctx, req, false)
}

// issue is Issue; again is a re-issue after a restart (Start, Tick), which
// a job's credential gets only while JobHolds says the job still holds the
// box.
func (g *Minted) issue(ctx context.Context, req Request, again bool) (Issued, error) {
	if req.Kind == "" { // an assignment kept by an earlier version names none
		req.Kind = g.b.kind()
	}
	e := g.base(req)
	p, why, err := g.admit(ctx, &req, &e)
	if err != nil {
		g.record(ctx, e, EventFailed, err.Error())
		return Issued{}, g.unavailable("%v", err)
	}
	if why != "" {
		g.record(ctx, e, EventRefused, why)
		return Issued{}, g.refusal(why)
	}
	if err := g.takesSecrets(ctx, req.Box); err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.CodeUnavailable {
			// Not known now (right after a restart no host has said anything
			// yet): not a refusal, so a re-issue waits and tries again.
			g.record(ctx, e, EventFailed, err.Error())
			return Issued{}, g.unavailable("%v", err)
		}
		g.record(ctx, e, EventRefused, err.Error())
		return Issued{}, g.refusal(err.Error())
	}
	e.Permissions = permString(p.perms)
	e.Elevated = strings.Join(p.elevated, ",")
	e.RuleReason = p.ruleReason
	k := heldKey(req.Box, req.Name)
	var me *held
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return Issued{}, g.closedErr()
		}
		h := g.held[k]
		if h == nil {
			me = &held{e: e, req: req, box: req.Box, plan: p, pending: true, done: make(chan struct{})}
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
			return Issued{}, api.Errorf(api.CodeConflict, "%s refused: %s", g.b.kind(), why)
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
			return Issued{}, g.unavailable("could not ask whether the job still holds the box: %v", err)
		}
		g.record(ctx, e, EventRefused, why)
		return Issued{}, g.refusal(why)
	}
	tok, err := g.mint(ctx, e, &req, p)
	if err != nil {
		drop()
		var pe publicError
		if errors.As(err, &pe) {
			return Issued{}, g.unavailable("%s", pe.public())
		}
		return Issued{}, g.unavailable("the issuer did not mint: %v", err)
	}
	g.mu.Lock()
	me.req = req // what mint looked up is kept for renewals
	g.mu.Unlock()
	e.ExpiresAt, e.HashedToken = tok.Expires(), tok.Hashed()
	if err := g.record(ctx, e, EventIssued, ""); err != nil {
		drop()
		g.revokeNow(ctx, e, tok, "the ledger refused the issue")
		return Issued{}, g.unavailable("the ledger refused the row; the credential was revoked")
	}
	if err := g.deliver(ctx, req.Box, req.Name, tok); err != nil {
		drop()
		if errors.Is(err, rt.ErrMaybeDelivered) {
			g.record(ctx, e, EventMaybeDelivered, err.Error())
			g.revokeNow(ctx, e, tok, "delivery may have reached the box")
			return Issued{}, fmt.Errorf("%w: %w", g.unavailable(
				"the delivery may have reached the box (the credential was revoked; do not deliver again): %v", err), rt.ErrMaybeDelivered)
		}
		g.revokeNow(ctx, e, tok, "delivery failed")
		return Issued{}, g.unavailable("delivering to the box failed (the credential was revoked): %v", err)
	}
	g.mu.Lock()
	if g.held[k] != me || g.closed { // the box ended, or the issuer closed, while it was being issued
		if g.held[k] == me {
			delete(g.held, k)
		}
		g.mu.Unlock()
		g.revokeNow(ctx, e, tok, "the box ended while it was being issued")
		return Issued{}, api.Errorf(api.CodeConflict, "%s: the box ended while the credential was being issued; it was revoked", g.b.kind())
	}
	me.e, me.tok, me.minted, me.pending = e, tok, g.o.Now(), false
	out := g.issued(me)
	g.mu.Unlock()
	if err := g.o.Ledger.Assign(context.WithoutCancel(ctx), g.name, req); err != nil {
		g.o.Log.Error("secrets issuer: the assignment was not kept; after a restart this box gets no new credential", "issuer", g.name,
			"box", req.Box.key(), "name", req.Name, "err", err)
	}
	return out, nil
}

// stillHolds is why req's box may not get a credential now that its issue
// holds the name ("" it may): its machine is asked again (a demotion between
// the admission and here is seen), and a job's re-issue asks JobHolds. err
// is JobHolds not answering.
func (g *Minted) stillHolds(ctx context.Context, req Request, again bool) (string, error) {
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
func (g *Minted) jobHolds(ctx context.Context, req Request) (string, error) {
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

func (g *Minted) issued(h *held) Issued {
	return Issued{Name: h.e.Name, Repo: h.e.Repo, Permissions: maps.Clone(h.plan.perms), ExpiresAt: h.e.ExpiresAt,
		HashedToken: h.e.HashedToken}
}

// mint mints through the backend and records a failure. A refused answer
// the backend could not revoke goes to the revocation retries.
func (g *Minted) mint(ctx context.Context, e Entry, req *Request, p plan) (Credential, error) {
	tok, err := g.b.mint(ctx, req, p)
	if err == nil {
		return tok, nil
	}
	var ue *unrevokedError
	if errors.As(err, &ue) && ue.cred != nil && ue.cred.Hashed() != "" {
		u := ue.cred
		e.ExpiresAt, e.HashedToken = u.Expires(), u.Hashed()
		g.record(ctx, e, EventFailed, scrub(err.Error(), u))
		g.mu.Lock()
		g.old = append(g.old, &retired{e: e, tok: u, at: g.o.Now().Add(retryEvery), why: "refused answer", tries: 1})
		g.mu.Unlock()
		return nil, errors.New(scrub(err.Error(), u))
	}
	g.record(ctx, e, EventFailed, err.Error())
	return nil, err
}

// deliver hands the value to the Deliverer. Its error, or a panic, is
// returned with the value scrubbed out.
func (g *Minted) deliver(ctx context.Context, box Box, name string, tok Credential) (err error) {
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

func scrub(s string, tok Credential) string {
	if tok == nil {
		return s
	}
	if v := tok.Reveal(); v != "" {
		s = strings.ReplaceAll(s, v, "[secret]")
	}
	if p, ok := tok.(secretParts); ok {
		for _, v := range p.parts() {
			if v != "" {
				s = strings.ReplaceAll(s, v, "[secret]")
			}
		}
	}
	return s
}

// revokeNow revokes tok and records it; a failure is retried by Tick.
func (g *Minted) revokeNow(ctx context.Context, e Entry, tok Credential, why string) bool {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
	err := g.b.revoke(rctx, tok)
	cancel()
	e.ExpiresAt, e.HashedToken = tok.Expires(), tok.Hashed()
	if err == nil {
		g.record(ctx, e, EventRevoked, why)
		return true
	}
	g.record(ctx, e, EventFailed, "revoke failed ("+why+"): "+scrub(err.Error(), tok))
	g.o.Log.Warn("secrets issuer: a revoke failed; retrying", "issuer", g.name, "box", e.Project+"/"+e.Box, "name", e.Name, "err", scrub(err.Error(), tok))
	g.mu.Lock()
	g.old = append(g.old, &retired{e: e, tok: tok, at: g.o.Now().Add(retryEvery), why: why, tries: 1})
	g.mu.Unlock()
	return false
}

// renewAt is two thirds of the credential's life.
func renewAt(h *held) time.Time {
	life := h.tok.Expires().Sub(h.minted)
	return h.minted.Add(life * 2 / 3)
}

// Tick renews every credential at two thirds of its life (through the
// Deliverer, replacing the old value), revokes renewed-away credentials
// after the grace, retries failed revocations and re-issues, and revokes a
// credential that reached its expiry unrenewed. Renewals and revocations run
// concurrently, at most Options.Renewers at once. It answers when it is next
// due, never later than a minute from now, so an owner that sleeps until
// then never misses a credential issued meanwhile.
func (g *Minted) Tick(ctx context.Context) time.Time {
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
		if !now.Before(h.tok.Expires()) {
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
		// The source should no longer take it; revoke anyway, until expiry plus a margin.
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
		g.record(ctx, h.e, EventFailed, "renewal did not succeed before the credential expired; the box has none")
		if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.name, h.box, h.e.Name); err != nil {
			g.o.Log.Error("secrets issuer: an assignment was not removed", "issuer", g.name, "err", err)
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
func (g *Minted) parallel(n int, f func(int)) {
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
func (g *Minted) next() time.Time {
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
		if h.tok.Expires().Before(at) {
			at = h.tok.Expires()
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

func (g *Minted) renew(ctx context.Context, h *held) {
	k := heldKey(h.box, h.e.Name)
	fail := func(why string) {
		g.record(ctx, h.e, EventFailed, "renewal: "+why)
		g.o.Log.Warn("secrets issuer: renewal failed; the old credential stays until it expires", "issuer", g.name,
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
	if err := g.takesSecrets(ctx, h.box); err != nil {
		g.mu.Lock()
		h.renewing = false
		g.mu.Unlock()
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.CodeUnavailable {
			fail("asking whether the box's host may hold a secret: " + err.Error())
			return
		}
		g.End(ctx, h.box, EndSwapUnsafe) // its swap is not safe now: nothing more goes there, and what is there is revoked
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
	// The kind's own check again: what it judged at issue (the identity
	// a token acts as, its role) may have changed since.
	p, why, err := g.check(ctx, req)
	if err != nil {
		fail("asking the kind's check: " + err.Error())
		return
	}
	if why != "" {
		g.mu.Lock()
		h.renewing = false
		g.mu.Unlock()
		g.endOne(ctx, k, "refused at renewal: "+why)
		return
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
	e, req := h.e, h.req
	g.mu.Unlock()
	tok, err := g.mint(ctx, e, &req, p)
	if err != nil {
		fail(err.Error())
		return
	}
	e.HostClass, e.ExpiresAt, e.HashedToken = class, tok.Expires(), tok.Hashed()
	if err := g.record(ctx, e, EventRenewed, ""); err != nil {
		g.revokeNow(ctx, e, tok, "the ledger refused the renewal")
		fail("the ledger refused the row")
		return
	}
	if err := g.deliver(ctx, h.box, h.e.Name, tok); err != nil {
		// A renewal that may have reached the box is retried all the same: the
		// box may now hold the revoked new credential, and only a renewal puts
		// a live one back (a whole-file replace, never a second live value).
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
	h.e, h.req, h.plan, h.tok, h.minted, h.renewing, h.nextTry = e, req, p, tok, g.o.Now(), false, time.Time{}
	g.old = append(g.old, old)
	g.mu.Unlock()
}

// retry revokes a retired credential, or reschedules it. One still
// unrevoked lapseMargin past its expiry is recorded as expired and dropped.
func (g *Minted) retry(ctx context.Context, r *retired) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
	err := g.b.revoke(rctx, r.tok)
	cancel()
	e := r.e
	e.ExpiresAt, e.HashedToken = r.tok.Expires(), r.tok.Hashed()
	if err == nil {
		g.record(ctx, e, EventRevoked, r.why)
		return
	}
	now := g.o.Now()
	if !now.Before(r.tok.Expires().Add(lapseMargin)) {
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

// End revokes every credential issued for box, and any renewed-away one of
// it still in its grace, and forgets the box's assignments. A revocation the
// source does not confirm is retried by Tick until it does or the credential
// is well past its expiry. One being issued is revoked as its issue
// finishes.
func (g *Minted) End(ctx context.Context, box Box, why EndReason) error {
	if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.name, box, ""); err != nil {
		g.o.Log.Error("secrets issuer: an assignment was not removed; Start asks the owner before issuing to it", "issuer", g.name,
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

// EndGrant revokes every credential issued under grant id and forgets those
// assignments (the grant was deleted; the grants package calls it).
func (g *Minted) EndGrant(ctx context.Context, id string) error {
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
	reqs, err := g.o.Ledger.Assigned(ctx, g.name)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if r.Grant == id {
			if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.name, r.Box, r.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// endOne revokes the credential held under k and forgets its assignment.
func (g *Minted) endOne(ctx context.Context, k, why string) {
	g.mu.Lock()
	h := g.held[k]
	if h != nil {
		delete(g.held, k)
	}
	g.mu.Unlock()
	if h == nil || h.pending {
		return
	}
	if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.name, h.box, h.e.Name); err != nil {
		g.o.Log.Error("secrets issuer: an assignment was not removed", "issuer", g.name, "err", err)
	}
	g.revokeNow(ctx, h.e, h.tok, why)
}

// revokeBox revokes box's credentials, keeping its assignments.
func (g *Minted) revokeBox(ctx context.Context, box Box, why string) error {
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
		return g.unavailable("%d revocation(s) for box %s not confirmed; retried until the source confirms or the credential expires", failed, box.key())
	}
	return nil
}

// Sweep ends every box holding a credential that live does not answer true
// for.
func (g *Minted) Sweep(ctx context.Context, live func(Box) bool) {
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

// EndUntrusted ends every box holding a credential, or being issued one,
// that ClassOf no longer answers trusted for (EndHostDemoted). A box being
// issued is ended too: its issue then revokes what it minted. A box whose
// issue starts after this read asks ClassOf itself once it holds the name
// (stillHolds), so a demotion is never missed between the two.
func (g *Minted) EndUntrusted(ctx context.Context) {
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
// credentials are held in memory only): the backend revokes each it can,
// recorded revoked; the rest lapse at their expiry, recorded
// expired_unrevoked. Then it issues again, through the whole of Issue
// (ceiling, class, grant), to every assigned box its owner says still
// lives, delivering the new credential in place of the old. An assignment
// whose box is gone, or that no longer passes, is dropped; one the source
// fails for is tried again by Tick.
func (g *Minted) Start(ctx context.Context) error {
	es, err := g.o.Ledger.Outstanding(ctx, g.name)
	if err != nil {
		return err
	}
	for _, e := range es {
		if !e.At.Before(g.born) {
			continue
		}
		if g.b.settle(ctx, e) {
			if err := g.record(ctx, e, EventRevoked, "the authority restarted: a credential of an earlier run, revoked at start"); err != nil {
				return err
			}
			continue
		}
		g.o.Log.Warn("secrets issuer: a credential of an earlier run could not be revoked; it lapses at its expiry", "issuer", g.name,
			"box", e.Project+"/"+e.Box, "name", e.Name, "repo", e.Repo, "expires", e.ExpiresAt)
		if err := g.record(ctx, e, EventExpiredUnrevoked, "the authority restarted: the credential was held in memory only and lapses at its expiry"); err != nil {
			return err
		}
	}
	reqs, err := g.o.Ledger.Assigned(ctx, g.name)
	if err != nil {
		return err
	}
	for _, req := range reqs {
		g.reissueOne(ctx, &pendingIssue{req: req})
	}
	return nil
}

// reissueOne issues an assignment again if its box lives.
func (g *Minted) reissueOne(ctx context.Context, p *pendingIssue) {
	req := p.req
	unassign := func() {
		if err := g.o.Ledger.Unassign(context.WithoutCancel(ctx), g.name, req.Box, req.Name); err != nil {
			g.o.Log.Error("secrets issuer: an assignment was not removed", "issuer", g.name, "err", err)
		}
	}
	later := func() {
		if p.tries++; p.tries >= reissueTries {
			g.o.Log.Warn("secrets issuer: giving up issuing again after a restart", "issuer", g.name, "box", req.Box.key(), "name", req.Name)
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
		unassign() // never delivered again: the box keeps nothing live (the credential was revoked)
	case errors.As(err, &ae) && ae.Code == api.CodeUnavailable:
		later()
	case errors.As(err, &ae) && ae.Code == api.CodeConflict:
		// the box holds another request under the name now: its row stands
	default:
		unassign()
	}
}

// Close refuses every later Issue and renewal, and revokes every credential
// still held and every renewed-away one (the authority stops: nothing it
// minted should outlive it). It keeps the assignments, so Start issues
// again. One the source does not confirm stays outstanding in the ledger,
// and the next Start settles it.
func (g *Minted) Close(ctx context.Context) error {
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
