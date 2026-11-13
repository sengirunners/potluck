package grants

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/protocol"
)

// secretName is the file-name rule for a secret in a box.
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// valueMax is the most bytes a value grant allows (and the default's
// ceiling): one exec's 64 KiB of names and values.
const valueMax = 64 << 10

// callerHeld is whether the caller holds kind's secret (no issuer mints
// it): value, and a jit_config the runner broker minted itself.
func callerHeld(kind string) bool {
	return kind == computeapi.SecretKindValue || kind == computeapi.SecretKindGitHubJIT
}

// Request is one ask to put secrets in a box, checked at the authority
// before anything is sent to a driver or a host.
type Request struct {
	Project string // the pool project's ULID
	Area    string // the box's area
	// Subject is who asks. Job, when set, is a GitHub job the runner
	// broker serves (then only job grants match).
	Subject *authz.Subject
	Job     *jobrule.Job
	Kind    string // computeapi.SecretKind*
	Scope   string // computeapi.SecretScope*
	Verb    string // exec, spawn, secrets, acquire (for the ledger)
	Target  string // the run target as listed (<binding> or <binding>@<machine>)
	Box     string // the handle ("" before acquire answers)
	Host    string // the machine serving the box ("" = this one)
	// HostClass is api.MachineTrusted or api.MachinePublic, as the
	// authority sees the host; anything else counts as public.
	HostClass string
	// Sizes maps each name to its value's size in bytes (kind value). The
	// grants package never sees a value.
	Sizes map[string]int
	// Name is any other kind's one file name (github-jit: SecretNameJIT);
	// Params is what a minted kind's request asks for, checked by its issuer
	// against the ceiling.
	Name   string
	Params json.RawMessage
	Reason string
}

// Decision is a passed check. Grant is the grant that allowed a one-name
// request (a minted kind's: what the caller hands its issuer with Params);
// Event.Grants has one per name.
type Decision struct {
	Project, Area string
	Grant         string
	Params        json.RawMessage
	// Identity is who asked, by identity id ("" for a job).
	Identity string
	Event    computeapi.SecretEvent
	mentions []string
}

// who is the asker as the policy needs it.
type who struct {
	identityID string
	// rel is the role role grants are judged by (a member machine's link by
	// its identity's grants, core.AreaRelation); operate is whether the
	// caller's own scope, cap included, may operate in the area: an
	// identity grant needs it, so a narrowed token never inherits its
	// identity's grant.
	rel     authz.Relation
	operate bool
	// since is, per area ("" = the whole pool), when the identity's current
	// membership reaching it began (absent or zero: none): an identity
	// grant made before it went stale.
	since map[string]time.Time
	job   *jobrule.Job
}

// Check is the grant check: the caller's request against the pool's grants
// and the defaults. It answers a Decision, or a refusal that names no
// secret (a public host's, or forbidden), recording the refusal in the
// ledger.
func Check(ctx context.Context, d *core.Deps, req Request) (Decision, error) {
	if err := shape(req); err != nil {
		return Decision{}, err
	}
	dec := decision(req)
	db, err := d.Store.Project(d.System().Scope, authz.Read, req.Project)
	if err != nil {
		return Decision{}, err
	}
	rows, on, err := load(ctx, db, `1=1`)
	if err != nil {
		return Decision{}, err
	}
	if on {
		rows = append(rows, Defaults()...)
	}
	var iss Issuer
	if !callerHeld(req.Kind) {
		if iss = IssuerOf(d, req.Kind); iss == nil {
			refused(ctx, d, dec, "no issuer for the kind")
			return Decision{}, api.Errorf(api.CodeUnavailable, "this pool's authority mints no %s secrets", req.Kind)
		}
	}
	w, err := whoOf(ctx, d, req, rows)
	if err != nil {
		return Decision{}, err
	}
	grants, why := decide(rows, req, w, iss)
	if why != "" {
		refused(ctx, d, dec, why)
		if why == whyPublic {
			host := req.Host
			if host == "" {
				host = "this machine"
			}
			return Decision{}, api.Errorf(api.CodeForbidden, "%s is a public host: it is given no secrets, only secret-free work", host)
		}
		return Decision{}, refusal()
	}
	dec.Event.Grants = grants
	if req.Name != "" {
		dec.Grant = grants[req.Name]
	}
	return dec, nil
}

// whoOf reads what the policy needs about the asker.
func whoOf(ctx context.Context, d *core.Deps, req Request, rows []computeapi.SecretGrant) (who, error) {
	if req.Job != nil {
		return who{job: req.Job}, nil
	}
	sub := req.Subject
	if sub == nil {
		return who{}, nil
	}
	w := who{identityID: sub.Identity.ID, operate: sub.Can(authz.Operate, authz.Object{Project: req.Project, Area: req.Area})}
	var err error
	if w.rel, err = d.AreaRelation(ctx, sub, req.Project, req.Area); err != nil {
		return w, err
	}
	if w.identityID != "" && d.Projects != nil {
		w.since = map[string]time.Time{}
		for _, g := range rows {
			if _, done := w.since[g.Area]; done || g.To.IdentityID != w.identityID {
				continue
			}
			since, ok, err := d.Projects.MemberSince(ctx, w.identityID, req.Project, g.Area)
			if err != nil {
				return w, err
			}
			if !ok {
				since = time.Time{}
			}
			w.since[g.Area] = since
		}
	}
	return w, nil
}

// shape refuses a request no grant could match, naming nothing it carries.
func shape(req Request) error {
	switch {
	case req.Project == "":
		return api.Errorf(api.CodeBadRequest, "a secret request names no project")
	case req.Kind == computeapi.SecretKindValue && (len(req.Sizes) == 0 || req.Name != "" || len(req.Params) > 0):
		return api.Errorf(api.CodeBadRequest, "a value request carries names and sizes only")
	case req.Kind != computeapi.SecretKindValue && (len(req.Sizes) > 0 || req.Name == ""):
		return api.Errorf(api.CodeBadRequest, "a %s request names one secret", req.Kind)
	case req.Kind == computeapi.SecretKindGitHubJIT && (req.Name != computeapi.SecretNameJIT || len(req.Params) > 0):
		return api.Errorf(api.CodeBadRequest, "a github-jit request is the jit_config alone")
	case !slices.Contains([]string{computeapi.SecretScopeExec, computeapi.SecretScopeBox}, req.Scope):
		return api.Errorf(api.CodeBadRequest, "a secret's scope is exec or box")
	}
	for n := range req.Sizes {
		if !secretName.MatchString(n) {
			return api.Errorf(api.CodeBadRequest, "a secret name is not [A-Z][A-Z0-9_]{0,63}")
		}
	}
	if req.Name != "" && !secretName.MatchString(req.Name) {
		return api.Errorf(api.CodeBadRequest, "a secret name is not [A-Z][A-Z0-9_]{0,63}")
	}
	return nil
}

// refusal is the grant check's answer when no grant covers a name (and a
// reserved name's, the same). No command named: zm and potluck both
// compile this check.
func refusal() error {
	return api.Errorf(api.CodeForbidden, "no secret grant lets you put these secrets in this box").
		WithHint("an admin of the area adds a secret grant that covers each one (GET /v0/secret_grants lists them)")
}

// whyUncovered is decide's answer when a name has no grant.
const whyUncovered = "no grant covers a name"

// whyPublic is decide's answer for a host that is not trusted.
const whyPublic = "the host is not trusted"

// decide is the policy itself, with no I/O: for each name, the first grant
// (rows in order, the defaults last) that covers it. Any name no grant
// covers refuses the whole request; why says why, in words that name no
// secret. iss is the kind's issuer (nil for a caller-held kind).
func decide(rows []computeapi.SecretGrant, req Request, w who, iss Issuer) (map[string]string, string) {
	if req.HostClass != api.MachineTrusted { // v1: a public host gets nothing, whatever a row says
		return nil, whyPublic
	}
	if req.Subject == nil && req.Job == nil {
		return nil, "no subject"
	}
	names := map[string]int{}
	if req.Kind == computeapi.SecretKindValue {
		names = req.Sizes
	} else {
		names[req.Name] = 0
	}
	out := map[string]string{}
	for name, size := range names {
		for _, g := range rows {
			if covers(g, req, w, name, size, iss) {
				out[name] = g.ID
				break
			}
		}
		if out[name] == "" {
			return nil, whyUncovered
		}
	}
	return out, ""
}

// covers is whether grant g lets req put name (size bytes) in its box.
// Anything malformed in g matches nothing.
func covers(g computeapi.SecretGrant, req Request, w who, name string, size int, iss Issuer) bool {
	switch {
	case !inForce(g),
		g.Kind != req.Kind,
		!slices.Contains(g.Scopes, req.Scope),
		g.Area != "" && g.Area != req.Area,
		len(g.Targets) > 0 && !slices.Contains(g.Targets, req.Target),
		g.Name != name && !(g.Name == "*" && g.Kind == computeapi.SecretKindValue),
		!subjectMatches(g, req, w):
		return false
	}
	switch req.Kind {
	case computeapi.SecretKindValue:
		max, ok := valueCeiling(g.Ceiling)
		return ok && size <= max
	case computeapi.SecretKindGitHubJIT:
		return emptyCeiling(g.Ceiling)
	}
	return iss != nil && iss.Within(g.Ceiling, req.Params) == nil
}

// inForce is whether g can grant anything now: on, readable, current, and
// for trusted hosts (the only class a grant may name today).
func inForce(g computeapi.SecretGrant) bool {
	return !g.Off && !g.Corrupt && !g.Stale && g.HostClass == api.MachineTrusted
}

// subjectMatches is g.To against who asks. A job grant matches only a job
// (from the broker); a person's grant only a person.
func subjectMatches(g computeapi.SecretGrant, req Request, w who) bool {
	to := g.To
	set := 0
	for _, b := range []bool{to.IdentityID != "", to.Role != "", to.Job != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		return false
	}
	if run := req.Job; run != nil {
		return JobMatches(g, *run)
	}
	switch {
	case req.Subject == nil || to.Job != nil:
		return false
	case to.IdentityID != "":
		return w.identityID != "" && w.identityID == to.IdentityID && w.operate && !staleFor(g, w.since[g.Area])
	}
	want, err := authz.ParseRelation(to.Role)
	if err != nil || want == authz.Guest {
		return false
	}
	return w.rel != authz.None && authz.Max(w.rel, want) == w.rel
}

// JobMatches reports whether g is a job grant that names run: the run
// passes jobrule's rules (its own repository, the default branch, an event
// it serves) and is of the grant's repository, ref, workflow and events.
// It looks at nothing else of g (kind, area, scope, host class: Check's).
func JobMatches(g computeapi.SecretGrant, run jobrule.Job) bool {
	j := g.To.Job
	return j != nil && g.To.IdentityID == "" && g.To.Role == "" && jobrule.Check(run) == "" && strings.EqualFold(j.Repo, run.Repo) &&
		j.Ref == run.Ref && jobrule.Serves(j.Events, []string{j.Workflow}, run)
}

// JobGrants is project's grants in force of a minted kind (an issuer's, not value or
// github-jit) to jobs of repo, for a box of area, as stored. The runner
// broker asks it before asking GitHub about a job's run, then keeps those
// that name the run (JobMatches); each is still decided by Check when the
// issuer mints it.
func JobGrants(ctx context.Context, d *core.Deps, project, area, repo string) ([]computeapi.SecretGrant, error) {
	db, err := d.Store.Project(d.System().Scope, authz.Read, project)
	if err != nil {
		return nil, err
	}
	rows, _, err := load(ctx, db, `1=1`)
	if err != nil {
		return nil, err
	}
	var out []computeapi.SecretGrant
	for _, g := range rows {
		// A grant Check would refuse whatever the run (off, stale, not for a
		// box, or naming targets, which a job's request never has) names no job.
		if inForce(g) && slices.Contains(g.Scopes, computeapi.SecretScopeBox) && len(g.Targets) == 0 &&
			!callerHeld(g.Kind) && g.To.Job != nil && strings.EqualFold(g.To.Job.Repo, repo) && (g.Area == "" || g.Area == area) {
			out = append(out, g)
		}
	}
	return out, nil
}

// staleFor is whether an identity grant made at g.Created predates the
// identity's current membership of its area (or the identity holds none).
func staleFor(g computeapi.SecretGrant, since time.Time) bool {
	made, err := time.Parse(time.RFC3339Nano, g.Created)
	return since.IsZero() || err != nil || since.After(made)
}

// valueCeiling reads {"max_bytes": n}; anything else matches nothing.
func valueCeiling(raw json.RawMessage) (int, bool) {
	var c struct {
		MaxBytes int `json:"max_bytes"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.MaxBytes < 1 || c.MaxBytes > valueMax {
		return 0, false
	}
	return c.MaxBytes, true
}

// emptyCeiling is a github-jit grant's ceiling: {} (a jit_config has no
// parameters a grant could cap).
func emptyCeiling(raw json.RawMessage) bool {
	var m map[string]any
	return json.Unmarshal(raw, &m) == nil && m != nil && len(m) == 0
}

// Defaults are the built-in grants, one switch for both. They keep today's
// behaviour: anyone who may operate in the box's area (a member) may send
// value secrets of any name, up to 64 KiB, on exec and spawn, and a
// jit_config on acquire, to a box on a trusted host. exec and spawn already
// require the box's holder, acquire operate on the area.
func Defaults() []computeapi.SecretGrant {
	member := computeapi.GrantTo{Role: api.RoleMember}
	return []computeapi.SecretGrant{
		{ID: computeapi.GrantDefaultID, Builtin: true, To: member, Kind: computeapi.SecretKindValue, Name: "*",
			Ceiling: json.RawMessage(fmt.Sprintf(`{"max_bytes":%d}`, valueMax)), HostClass: api.MachineTrusted,
			Scopes: []string{computeapi.SecretScopeExec}},
		{ID: computeapi.GrantDefaultJITID, Builtin: true, To: member, Kind: computeapi.SecretKindGitHubJIT,
			Name: computeapi.SecretNameJIT, Ceiling: json.RawMessage(`{}`), HostClass: api.MachineTrusted,
			Scopes: []string{computeapi.SecretScopeBox}},
	}
}

func decision(req Request) Decision {
	e := computeapi.SecretEvent{Kind: req.Kind, Scope: req.Scope, Verb: req.Verb, Target: req.Target, Box: req.Box,
		Host: req.Host, HostClass: req.HostClass, Params: req.Params, Reason: req.Reason}
	if e.HostClass != api.MachineTrusted {
		e.HostClass = api.MachinePublic
	}
	dec := Decision{Project: req.Project, Area: req.Area, Params: req.Params}
	if sub := req.Subject; sub != nil {
		e.By, e.Identity, dec.Identity = sub.Address, sub.Identity.ID, sub.Identity.ID
		if e.By == "" {
			e.By = sub.Identity.Name
		}
		if sub.Address != "" {
			dec.mentions = append(dec.mentions, sub.Address) // redacted for a reader who may not see it
		}
	}
	if j := req.Job; j != nil {
		run := *j
		e.Job = &run
	}
	if req.Kind == computeapi.SecretKindValue {
		e.Sizes = map[string]int{}
		for n, s := range req.Sizes {
			e.Names = append(e.Names, n)
			e.Sizes[n] = s
		}
		sort.Strings(e.Names)
	} else {
		e.Names = []string{req.Name}
	}
	dec.Event = e
	return dec
}

func publish(ctx context.Context, d *core.Deps, typ string, dec Decision) error {
	if d.Events == nil {
		return api.Errorf(api.CodeUnavailable, "the secrets ledger is not running")
	}
	dec.Event.Count = len(dec.Event.Names)
	_, err := d.Events.Publish(ctx, events.Input{Type: typ, Subject: dec.Event.Box, Project: dec.Project, Area: dec.Area,
		Data: dec.Event, Mentions: dec.mentions, Audience: LedgerAudience(authz.Operate, computeapi.SecretEventWithheld)})
	return err
}

func init() {
	// Enforced by type at read time too, whatever a row's stored audience
	// says (kit/events): rows written before this rule stay home as well.
	events.RegisterAuthorityOnly(events.TypeRule{Prefix: "dev.zm.secret.", Withheld: computeapi.SecretEventWithheld, CountOf: "names"})
	events.RegisterAuthorityOnly(events.TypeRule{Prefix: "dev.zm.secret_grant.", Withheld: computeapi.SecretGrantWithheld})
}

// LedgerAudience is the audience of every secret.* and secret_grant.*
// event: readers with need on the area, on the pool's machine only (never
// copied to a replica), and over a tunnel without the keys withheld (the
// names).
func LedgerAudience(need authz.Action, withheld []string) *events.Audience {
	return &events.Audience{Need: need, AuthorityOnly: true, NotOverTunnel: withheld}
}

func refused(ctx context.Context, d *core.Deps, dec Decision, why string) {
	dec.Event.Why = why
	if err := publish(ctx, d, computeapi.EventSecretRefused, dec); err != nil && d.Log != nil {
		d.Log.Warn("grants: the ledger did not take a refusal", "err", err)
	}
}

// Granted writes the ledger entry for caller-held secrets about to be
// handed to the transport. Call it after every other check and just before
// the call that carries them: if the ledger cannot take it (or a daemon has
// no event log), nothing is sent. For a kind an issuer mints it does
// nothing: the issuer writes its own entries.
func Granted(ctx context.Context, d *core.Deps, dec Decision) error {
	if !callerHeld(dec.Event.Kind) {
		return nil
	}
	if err := publish(ctx, d, computeapi.EventSecretGranted, dec); err != nil {
		return api.Errorf(api.CodeUnavailable, "the secrets ledger did not take the entry, so nothing was sent")
	}
	return nil
}

// Delivered writes the ledger entry for the call that carried caller-held
// secrets, once it answered (failed: it returned an error, so they may or
// may not have reached the box). A failure to write it is logged. Nothing
// for an issuer's kind.
func Delivered(ctx context.Context, d *core.Deps, dec Decision, failed bool) {
	if !callerHeld(dec.Event.Kind) {
		return
	}
	dec.Event.Failed = failed
	if err := publish(context.WithoutCancel(ctx), d, computeapi.EventSecretDelivered, dec); err != nil && d.Log != nil {
		d.Log.Warn("grants: the ledger did not take a delivery", "err", err)
	}
}

// reserves is whether g reserves its name in the boxes it covers: a grant
// of an issuer's kind naming one.
func reserves(g computeapi.SecretGrant) bool { return !callerHeld(g.Kind) && g.Name != "" }

// Reserved is the set of names the authority mints into a box of area in
// project itself: every name a grant of an issuer's kind names in that area
// or for every area, the jit_config's, and the door's ready marker (a
// runner box's job-started hook lets its job start on it). A box holder's own secrets call
// may neither set nor unset one, so a minted credential cannot be
// overwritten or removed from inside the door. A grant of another area
// reserves nothing here.
func Reserved(ctx context.Context, d *core.Deps, project, area string) (map[string]bool, error) {
	db, err := d.Store.Project(d.System().Scope, authz.Read, project)
	if err != nil {
		return nil, err
	}
	rows, _, err := load(ctx, db, `1=1`)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{computeapi.SecretNameJIT: true, protocol.SecretNameReady: true}
	for _, g := range rows {
		if reserves(g) && (g.Area == "" || g.Area == area) {
			out[g.Name] = true
		}
	}
	return out, nil
}

// CheckReserved refuses req (a box-scope call setting req.Sizes' names and
// unsetting unset) when it names one Reserved for its box, with exactly the
// grant check's refusal and ledger entry, so the answer does not tell a
// member which names an issuer's grant holds.
func CheckReserved(ctx context.Context, d *core.Deps, req Request, unset []string) error {
	reserved, err := Reserved(ctx, d, req.Project, req.Area)
	if err != nil {
		return err
	}
	hit := false
	for n := range req.Sizes {
		hit = hit || reserved[n]
	}
	for _, n := range unset {
		hit = hit || reserved[n]
	}
	if !hit {
		return nil
	}
	dec := decision(req)
	if len(dec.Event.Names) == 0 || dec.Event.Names[0] == "" {
		dec.Event.Names = append([]string(nil), unset...)
	}
	refused(ctx, d, dec, whyUncovered)
	return refusal()
}
