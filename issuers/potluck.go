package issuers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// KindPotluckToken is the credential kind a potluck issuer mints.
const KindPotluckToken = computeapi.SecretKindPotluckToken

// TokenMint is the pool's own token mint (kit/project's Registry): the
// tokens table every caller of this daemon authenticates against.
type TokenMint interface {
	Lookup(ref string) (api.ProjectRef, bool)
	HasArea(project, area string) bool
	Identity(ctx context.Context, ref string) (api.Identity, error)
	Tuples(ctx context.Context, identityID string) ([]authz.Tuple, error)
	Local(address string) *authz.Subject
	Mint(ctx context.Context, sub *authz.Subject, req api.MintTokenRequest) (api.MintTokenResponse, error)
	RevokeToken(ctx context.Context, id string) (api.Token, error)
	RevokeTokenHash(ctx context.Context, hash string) (bool, error)
	// TokenExpiry is when token id expires (zero: never); an error when it
	// is revoked, expired or unknown.
	TokenExpiry(ctx context.Context, id string) (time.Time, error)
}

// BoxExpires is when box's lease ends, as the run targets record it (zero:
// it has no end).
type BoxExpires func(ctx context.Context, box Box) (time.Time, error)

// Home is the authority a box's credential names: this daemon's tunnel URL
// and key fingerprint (kit/tunnel's Service.Home). An error is why a box
// could not reach it (no tunnel URL).
type Home func() (url, fingerprint string, err error)

// potluckLife is the longest a potluck-token lives: it is renewed at two
// thirds of it while its box lives, and never outlives the box's lease.
const potluckLife = time.Hour

// minLease is the least lease a box must have left to be issued a token.
const minLease = time.Minute

// boxRoles are the roles a token for a box may carry: owner never.
var boxRoles = []authz.Relation{authz.Guest, authz.Member}

// potluckRole reads a role a box's token may carry.
func potluckRole(s string) (authz.Relation, error) {
	r, err := authz.ParseRelation(s)
	if err != nil {
		return authz.None, fmt.Errorf("role %q: guest or member", s)
	}
	for _, ok := range boxRoles {
		if r == ok {
			return r, nil
		}
	}
	return authz.None, fmt.Errorf("role %s is never minted for a box (guest or member)", r)
}

// roleWithin reports whether asked is at most top.
func roleWithin(asked, top authz.Relation) bool { return authz.Min(asked, top) == asked }

// potluckSpec validates a potluck issuer: its identity for jobs and its
// ceiling rules (project, area, role, optionally workflows and events).
func potluckSpec(name string, is config.SecretsIssuer) (Spec, error) {
	bad := func(format string, a ...any) (Spec, error) {
		return Spec{}, fmt.Errorf("secrets.issuers.%s: %s", name, fmt.Sprintf(format, a...))
	}
	if is.Use != "" || is.AppID != 0 || is.PrivateKey != "" || is.APIURL != "" {
		return bad("use, app_id, private_key and api_url are a %q issuer's", KindGitHubApp)
	}
	if is.As != "" && !api.ValidName(is.As) {
		return bad("as %q is not an identity name", is.As)
	}
	s := Spec{Name: name, Kind: KindPotluck, As: is.As}
	for i, r := range is.Ceiling {
		rule, err := checkPotluckRule(r)
		if err != nil {
			return bad("ceiling[%d]: %v", i, err)
		}
		s.Ceiling = append(s.Ceiling, rule)
	}
	return s, nil
}

func checkPotluckRule(r config.SecretsCeiling) (Rule, error) {
	if r.Repo != "" || len(r.Permissions) > 0 || len(r.Elevated) > 0 || r.Reason != "" {
		return Rule{}, fmt.Errorf("repo, permissions, elevated and reason are a %q issuer's", KindGitHubApp)
	}
	if r.Project == "" || !(api.ValidSlug(strings.ToLower(r.Project)) || ids.Valid(strings.ToUpper(r.Project))) {
		return Rule{}, fmt.Errorf("project %q: a project's slug or ULID", r.Project)
	}
	if r.Area != "" && !api.ValidSlug(r.Area) {
		return Rule{}, fmt.Errorf("area %q is not an area name", r.Area)
	}
	if _, err := potluckRole(r.Role); err != nil {
		return Rule{}, err
	}
	var wfs []string
	for _, w := range r.Workflows {
		if !jobrule.ValidWorkflow(w) {
			return Rule{}, fmt.Errorf("workflow %q: a file under .github/workflows/ (.yml or .yaml)", w)
		}
		wfs = append(wfs, w)
	}
	events, err := jobrule.RuleEvents(r.Events)
	if err != nil {
		return Rule{}, err
	}
	return Rule{Project: r.Project, Area: r.Area, Role: strings.ToLower(r.Role), Workflows: wfs, Events: events}, nil
}

// potluckBackend is the potluck-token kind: per box, a token of this pool's
// own (the tokens every caller authenticates with), capped to one role on
// the box's project and area, living at most an hour and never past the
// box's lease. A person's token acts as that person; a GitHub job's acts as
// the issuer's identity (Spec.As). The identity must already hold that role
// there: the token only ever narrows what it holds.
//
// The token is bound to a key made for this one mint (a per-box key): the
// tunnel takes it only on requests that key signed. What lands in the box
// is one file (tunnel.BoxCredential): the token, the key, and the
// authority's tunnel URL and key fingerprint. A renewal mints a new token
// with a new key and replaces the file whole. The authority keeps the key
// only inside the held credential, in memory, like the token.
type potluckBackend struct {
	spec    Spec
	tokens  TokenMint
	expires BoxExpires
	home    Home
	now     func() time.Time
}

// potluckToken is a minted token as a Credential: Reveal is the box
// credential file, Hashed the token's hash (the ledger's and the tokens
// table's).
type potluckToken struct {
	id, secret string
	cred       string // the box credential file (tunnel.BoxCredential)
	seed       string // the per-box key's seed, hex, as the file holds it
	exp        time.Time
}

func (t potluckToken) Reveal() string     { return t.cred }
func (t potluckToken) parts() []string    { return []string{t.secret, t.seed} }
func (t potluckToken) Expires() time.Time { return t.exp }
func (t potluckToken) Hashed() string     { return authz.HashToken(t.secret) }
func (t potluckToken) String() string     { return "potluck-token " + t.id }
func (t potluckToken) GoString() string   { return t.String() }

func newPotluckBackend(s Spec, o Options) (*potluckBackend, error) {
	if o.Tokens == nil || o.BoxExpires == nil || o.Home == nil {
		return nil, fmt.Errorf("secrets.issuers.%s: a %s issuer needs the token mint, the boxes' leases and the authority's tunnel", s.Name, KindPotluck)
	}
	return &potluckBackend{spec: s, tokens: o.Tokens, expires: o.BoxExpires, home: o.Home, now: o.Now}, nil
}

func (b *potluckBackend) kind() string { return KindPotluckToken }

// rule finds a config rule covering role on box for sub.
func (b *potluckBackend) rule(box Box, sub Subject, role authz.Relation) (Rule, bool) {
	for _, r := range b.spec.Ceiling {
		p, ok := b.tokens.Lookup(r.Project)
		if !ok || p.ID != box.Project || (r.Area != "" && r.Area != box.Area) {
			continue
		}
		if top, _ := potluckRole(r.Role); !roleWithin(role, top) {
			continue
		}
		if sub.Job != nil && !jobrule.Serves(r.Events, r.Workflows, *sub.Job) {
			continue
		}
		if sub.Job == nil && len(r.Workflows) > 0 { // a workflow rule serves only jobs
			continue
		}
		return r, true
	}
	return Rule{}, false
}

func (b *potluckBackend) check(ctx context.Context, req Request) (plan, string, error) {
	if req.Repo != "" || len(req.Permissions) > 0 {
		return plan{}, "a potluck-token names a role, not a repository or permissions", nil
	}
	role, err := potluckRole(req.Role)
	if err != nil {
		return plan{}, err.Error(), nil
	}
	if _, _, err := b.home(); err != nil {
		return plan{}, "a box could not reach this authority: " + err.Error(), nil
	}
	if req.Box.Area != "" && !b.tokens.HasArea(req.Box.Project, req.Box.Area) {
		return plan{}, "the box's area is not an area of its project", nil
	}
	if j := req.Subject.Job; j != nil {
		if why := jobrule.Check(*j); why != "" {
			return plan{}, why, nil
		}
	}
	if _, ok := b.rule(req.Box, req.Subject, role); !ok {
		return plan{}, "outside the issuer's ceiling (project, area, role, event or workflow)", nil
	}
	as := req.Subject.IdentityID
	if req.Subject.Job != nil {
		if b.spec.As == "" {
			return plan{}, "the issuer names no identity for a job's token (as)", nil
		}
		as = b.spec.As
	}
	if c := req.Caller; c != nil && c.TokenID != "" {
		if _, err := b.tokens.TokenExpiry(ctx, c.TokenID); err != nil {
			if code(err) != api.CodeUnauthenticated {
				return plan{}, "", fmt.Errorf("reading the caller's token: %w", err)
			}
			return plan{}, "the token the caller authenticated with is revoked or expired", nil
		}
	}
	who, err := b.tokens.Identity(ctx, as)
	if err != nil {
		if code(err) != api.CodeNotFound {
			return plan{}, "", fmt.Errorf("reading the identity: %w", err)
		}
		return plan{}, "the identity the token would act as does not exist", nil
	}
	tuples, err := b.tokens.Tuples(ctx, who.ID)
	if err != nil {
		return plan{}, "", fmt.Errorf("reading the identity's grants: %w", err)
	}
	held := authz.FromTuples(tuples, &authz.Cap{Project: req.Box.Project, Area: req.Box.Area, Role: role}).
		Relation(authz.Object{Project: req.Box.Project, Area: req.Box.Area})
	if held != role {
		return plan{}, "identity " + who.Name + " does not hold " + string(role) + " on the box's project and area", nil
	}
	return plan{perms: map[string]string{"role": string(role)}, as: who.Name}, "", nil
}

// mint mints a token as p.as, capped to the box's project, area and role,
// living until the earliest of an hour from now, the box's lease end and the
// expiry of the token the caller authenticated with.
func (b *potluckBackend) mint(ctx context.Context, req *Request, p plan) (Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	now := b.now()
	exp := now.Add(potluckLife)
	lease, err := b.expires(ctx, req.Box)
	if err != nil {
		return nil, fmt.Errorf("reading the box's lease: %w", err)
	}
	if !lease.IsZero() && lease.Before(exp) {
		exp = lease
	}
	// A caller that authenticated with a token (perhaps another box's
	// potluck-token) gets nothing that outlives that token.
	if c := req.Caller; c != nil && c.TokenID != "" {
		ce, err := b.tokens.TokenExpiry(ctx, c.TokenID)
		if err != nil {
			return nil, fmt.Errorf("reading the caller's token: %w", err)
		}
		if !ce.IsZero() && ce.Before(exp) {
			exp = ce
		}
	}
	if exp.Sub(now) < minLease {
		return nil, errors.New("the box's lease (or the caller's token) ends within a minute")
	}
	// Whole seconds, less one: the registry stamps the expiry from its own
	// clock a moment later.
	url, fp, err := b.home()
	if err != nil {
		return nil, err
	}
	key, err := tunnel.NewBoxKey()
	if err != nil {
		return nil, err
	}
	limit := exp
	ttl := int64(exp.Sub(now)/time.Second) - 1
	exp = now.Add(time.Duration(ttl) * time.Second)
	out, err := b.tokens.Mint(ctx, b.tokens.Local(""), api.MintTokenRequest{As: p.as, Project: req.Box.Project, Area: req.Box.Area,
		Role: p.perms["role"], TTLSeconds: ttl, NoGrant: true, Note: "potluck-token " + req.Name + " for box " + req.Box.ID,
		BoxKey: tunnel.BoxKeyID(key)})
	if err != nil {
		return nil, err
	}
	tok := potluckToken{id: out.Token.ID, secret: out.Secret, seed: hex.EncodeToString(key.Seed()), exp: exp}
	cred, cerr := tunnel.BoxCredential{Token: out.Secret, Key: key, URL: url, Home: fp}.Encode()
	if cerr != nil {
		if err := b.revoke(context.WithoutCancel(ctx), tok); err != nil {
			return nil, &unrevokedError{cred: tok, err: errors.New("the box credential could not be made, and revoking its token failed")}
		}
		return nil, fmt.Errorf("the box credential could not be made (its token was revoked): %w", cerr)
	}
	tok.cred = cred
	if out.Token.Expires == nil || out.Token.Expires.After(limit) {
		// The registry stamped its expiry from its own clock, later: a mint
		// that stalled past the margin would outlive the cap, so it is
		// revoked and refused.
		if err := b.revoke(context.WithoutCancel(ctx), tok); err != nil {
			return nil, &unrevokedError{cred: tok, err: errors.New("the registry stamped an expiry past the cap, and revoking it failed")}
		}
		return nil, errors.New("the registry stamped an expiry past the cap (the mint stalled); it was revoked")
	}
	return tok, nil
}

func (b *potluckBackend) revoke(ctx context.Context, c Credential) error {
	t, ok := c.(potluckToken)
	if !ok {
		return api.Errorf(api.CodeInternal, "potluck-token: not a potluck token")
	}
	_, err := b.tokens.RevokeToken(ctx, t.id)
	var ae *api.Error
	if errors.As(err, &ae) && ae.Code == api.CodeNotFound {
		return nil // no such token: nothing can authenticate with it
	}
	return err
}

// settle revokes an earlier run's token by its hash: the tokens table
// outlives the run, so nothing is left to lapse.
func (b *potluckBackend) settle(ctx context.Context, e Entry) bool {
	if e.HashedToken == "" {
		return false
	}
	_, err := b.tokens.RevokeTokenHash(ctx, e.HashedToken)
	return err == nil
}

// PotluckCeiling is a potluck-token grant's ceiling: {"role": "member"}.
type PotluckCeiling struct {
	Role string `json:"role"`
}

// PotluckParams is what a potluck-token request asks for: Request's role.
type PotluckParams struct {
	Role string `json:"role"`
}

func parsePotluckCeiling(raw json.RawMessage) (PotluckCeiling, authz.Relation, error) {
	var c PotluckCeiling
	if err := strict(raw, &c); err != nil {
		return PotluckCeiling{}, authz.None, fmt.Errorf("potluck-token ceiling: %w", err)
	}
	r, err := potluckRole(c.Role)
	if err != nil {
		return PotluckCeiling{}, authz.None, fmt.Errorf("potluck-token ceiling: %w", err)
	}
	return PotluckCeiling{Role: string(r)}, r, nil
}

// ceiling refuses a grant ceiling whose role no config rule reaches.
func (b *potluckBackend) ceiling(raw json.RawMessage) (json.RawMessage, error) {
	c, r, err := parsePotluckCeiling(raw)
	if err != nil {
		return nil, err
	}
	for _, rule := range b.spec.Ceiling {
		if top, _ := potluckRole(rule.Role); roleWithin(r, top) {
			return json.Marshal(c)
		}
	}
	return nil, fmt.Errorf("potluck-token ceiling: role %s is not inside issuer %s's config ceiling", r, b.spec.Name)
}

func (b *potluckBackend) within(ceiling, params json.RawMessage) error {
	_, top, err := parsePotluckCeiling(ceiling)
	if err != nil {
		return err
	}
	var p PotluckParams
	if err := strict(params, &p); err != nil {
		return fmt.Errorf("potluck-token params: %w", err)
	}
	r, err := potluckRole(p.Role)
	if err != nil {
		return fmt.Errorf("potluck-token params: %w", err)
	}
	if !roleWithin(r, top) {
		return errors.New("potluck-token params: outside the grant's ceiling")
	}
	return nil
}

// code is err's api code ("" for another error).
func code(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}
