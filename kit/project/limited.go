package project

import (
	"context"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// A limited token carries a route limit (tokens.route_limit, authz.Cap.Limit):
// it reaches only the routes that admit that limit by name (core.Router.Admit)
// and works only from the one tailnet device named at mint (pin_node and
// pin_key, its stable node ID and node key; pin_name is how the minter named
// it, for lists). Its secret has its own shape and its stored hash is
// authz.HashLimited, so no lookup by authz.HashToken (every build before
// limits, and every other reader here) ever finds the row.
//
// Every reader of the tokens table decides what a limited row is to it
// (TestTokenReaders lists them): the subject builders other than
// FromTokenCaller refuse it, and the node-bound readers never see one,
// because a limited row has no node, machine or box key.
func init() {
	store.RegisterRegistry("project/0008_token_route_limit", `
ALTER TABLE tokens ADD COLUMN route_limit TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN pin_node TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN pin_key TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN pin_name TEXT NOT NULL DEFAULT '';`)
}

// Caller is what the listener knows of a token's caller (core.Authenticate
// fills it from the request).
type Caller struct {
	// Remote is false on the Unix socket.
	Remote bool
	// NodeID is the WhoIs stable node ID over the tailnet, or over the
	// tunnel the id of the key that signed the request; "" when the
	// listener identified nobody.
	NodeID string
	// NodeKey is the WhoIs node key; "" over the tunnel and on a listener
	// without WhoIs.
	NodeKey string
	// Via is "" for the tailnet (WhoIs), else the listener that carried the
	// call (core.ViaTunnel).
	Via string
	// Self is true for this daemon's own node or a caller on this host.
	Self bool
}

// Pin is the tailnet device a limited token works from: its stable node ID
// and node key, which every call must come from, and the name the minter
// gave it.
type Pin struct {
	NodeID  string
	NodeKey string
	Name    string
}

// LimitedMint is what MintLimited needs besides the request: the device
// (resolved from the tailnet by the caller), the TTL ceiling (a setting),
// and which limits some route admits.
type LimitedMint struct {
	Pin      Pin
	MaxTTL   time.Duration
	Admitted func(limit string) bool
}

// limitedRole is the role a limited token is stored with: its route limit,
// not its role, says what it reaches.
const limitedRole = authz.Owner

// MintLimited mints a token limited to req.Limit for the caller's own
// identity, pinned to one device. Nothing is written until every rule
// holds, and no grant or identity is ever added: the caller (the machine's
// owner on its socket, not a session acting through Zm-As, by its own
// scope) must already hold a project-wide
// grant of at least the stored role by its identity's own tuples. The
// secret is returned once; only its HashLimited is stored.
func (r *Registry) MintLimited(ctx context.Context, sub *authz.Subject, req api.MintTokenRequest, m LimitedMint) (api.MintTokenResponse, error) {
	bad := func(format string, a ...any) (api.MintTokenResponse, error) {
		return api.MintTokenResponse{}, api.Errorf(api.CodeBadRequest, format, a...)
	}
	if sub == nil || sub.Via != authz.ViaLocal || sub.Remote || sub.TokenID != "" || sub.Session != "" || sub.Identity.ID == "" {
		return api.MintTokenResponse{}, api.Errorf(api.CodeForbidden, "a limited token is minted only by the machine's owner on its own socket, not by a session")
	}
	switch {
	case !authz.ValidLimit(req.Limit):
		return bad("limit %q is not a limit name (lowercase letters, at most %d)", req.Limit, authz.MaxLimitLen)
	case m.Admitted == nil || !m.Admitted(req.Limit):
		return bad("no route admits limit %q", req.Limit)
	case req.As != "":
		return bad("a limited token is for your own identity: leave as empty")
	case req.BoxKey != "":
		return bad("a limited token is never a box's")
	case req.Project == "":
		return bad("a limited token needs a project")
	case req.Area != "":
		return bad("a limited token is for the whole project, not an area")
	case req.TTLSeconds <= 0:
		return bad("a limited token needs a ttl")
	case m.MaxTTL < time.Second || req.TTLSeconds > int64(m.MaxTTL/time.Second):
		return bad("a limited token's ttl is at most %s", m.MaxTTL)
	case m.Pin.NodeID == "" || m.Pin.NodeKey == "":
		return bad("a limited token needs the device it works from")
	}
	if req.Role != "" {
		rel, err := authz.ParseRelation(req.Role)
		if err != nil {
			return api.MintTokenResponse{}, err
		}
		if rel != limitedRole {
			return bad("a limited token's role is %s", limitedRole)
		}
	}
	r.mu.RLock()
	pid := r.idOf(req.Project)
	r.mu.RUnlock()
	if pid == "" || !sub.Scope.SeesProject(pid) {
		return api.MintTokenResponse{}, api.NotFound("project", req.Project)
	}
	// The caller's own scope (a session may be capped) and its identity's
	// own grant: the socket's scope All alone says nothing about the
	// identity, and the token is read later through the identity's tuples.
	if !atLeast(sub.Scope.Relation(authz.Object{Project: pid}), limitedRole) {
		return api.MintTokenResponse{}, api.Errorf(api.CodeForbidden, "your role cannot mint a limited token in project %s", r.Slug(pid))
	}
	ok, err := r.holdsWhole(ctx, sub.Identity.ID, pid, limitedRole)
	if err != nil {
		return api.MintTokenResponse{}, err
	}
	if !ok {
		return api.MintTokenResponse{}, api.Errorf(api.CodeForbidden, "identity %s holds no %s grant on the whole project %s", sub.Identity.Name, limitedRole, r.Slug(pid)).
			WithHint("a limited token adds no grant: it works only for an identity that already owns the project")
	}
	secret := authz.NewLimitedSecret(req.Limit)
	now := time.Now().UTC()
	exp := store.FromMillis(store.Millis(now.Add(time.Duration(req.TTLSeconds) * time.Second)))
	t := api.Token{ID: ids.New(), Identity: sub.Identity.Name, Project: r.Slug(pid), Role: string(limitedRole), Note: req.Note,
		Created: store.FromMillis(store.Millis(now)), Expires: &exp, Limit: req.Limit, Device: m.Pin.Name}
	if _, err := r.st.Registry().ExecContext(ctx, `INSERT INTO tokens (id, hash, identity, project, area, role, note, created, expires, route_limit, pin_node, pin_key, pin_name)
		VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, authz.HashLimited(req.Limit, secret), sub.Identity.ID, pid, t.Role, t.Note,
		store.Millis(now), store.Millis(exp), req.Limit, m.Pin.NodeID, m.Pin.NodeKey, m.Pin.Name); err != nil {
		return api.MintTokenResponse{}, err
	}
	return api.MintTokenResponse{Token: t, Secret: secret}, nil
}

// limitedRow is a limited token's row as FromTokenCaller read it.
type limitedRow struct {
	expires                       bool // a limited row always has an expiry
	identity, project, area, role string
	node, boxKey                  string // never set on a limited row
	pinNode, pinKey               string
}

// limitedAdmits reports whether a limited token's row may authenticate c
// now: only over a listener with WhoIs, from exactly its pinned device (node
// ID and key: a re-keyed device needs a new token), never from this host,
// and only while its identity still holds a project-wide grant of at least
// the stored role by its own tuples. A row without an expiry, a node, a box
// key, a project or with an area is no limited token MintLimited wrote, and
// is refused. It reads, never writes.
func (r *Registry) limitedAdmits(ctx context.Context, row limitedRow, c Caller) (bool, error) {
	if !c.Remote || c.Via != "" || c.Self || c.NodeID == "" || c.NodeKey == "" ||
		row.pinNode == "" || row.pinKey == "" || c.NodeID != row.pinNode || c.NodeKey != row.pinKey {
		return false, nil
	}
	if !row.expires || row.node != "" || row.boxKey != "" || row.project == "" || row.area != "" {
		return false, nil
	}
	role, err := authz.ParseRelation(row.role)
	if err != nil {
		return false, nil
	}
	return r.holdsWhole(ctx, row.identity, row.project, role)
}

// holdsWhole reports whether identity holds a project-wide grant on project
// of at least role, by its own tuples.
func (r *Registry) holdsWhole(ctx context.Context, identity, project string, role authz.Relation) (bool, error) {
	tuples, err := r.Tuples(ctx, identity)
	if err != nil {
		return false, err
	}
	return atLeast(authz.FromTuples(tuples, nil).Relation(authz.Object{Project: project}), role), nil
}

// atLeast reports whether have is role or stronger (None never is).
func atLeast(have, role authz.Relation) bool {
	return have != authz.None && authz.Max(have, role) == have
}
