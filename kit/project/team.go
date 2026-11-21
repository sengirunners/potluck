package project

// Teams (SPEC 17.3, 17.4): which projects are team projects (this machine is
// their authority), invites and their redemption into an identity, a grant
// and a node-bound token in one transaction, the machine name each replica
// node holds, and every identity's scope_version.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/kit/config"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	store.RegisterRegistry("project/0002_team", `
ALTER TABLE projects ADD COLUMN team INTEGER NOT NULL DEFAULT 0;
ALTER TABLE identities ADD COLUMN scope_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE tokens ADD COLUMN node TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN machine TEXT NOT NULL DEFAULT '';
CREATE INDEX tokens_node ON tokens (project, node);
CREATE INDEX tokens_machine ON tokens (machine);
CREATE TABLE invites (
	id             TEXT PRIMARY KEY,
	project        TEXT NOT NULL,
	area           TEXT NOT NULL DEFAULT '',
	identity       TEXT NOT NULL,
	role           TEXT NOT NULL,
	code_hash      TEXT NOT NULL UNIQUE,
	allow_tagged   INTEGER NOT NULL DEFAULT 0,
	created        INTEGER NOT NULL,
	expires        INTEGER NOT NULL,
	created_by     TEXT NOT NULL DEFAULT '',
	attempts       INTEGER NOT NULL DEFAULT 0,
	node           TEXT NOT NULL DEFAULT '',
	machine        TEXT NOT NULL DEFAULT '',
	token          TEXT NOT NULL DEFAULT '',
	pending_secret TEXT NOT NULL DEFAULT '',
	redeemed       INTEGER
);
CREATE INDEX invites_project ON invites (project);
CREATE TABLE team_nodes (
	node     TEXT PRIMARY KEY,
	machine  TEXT NOT NULL UNIQUE,
	identity TEXT NOT NULL,
	created  INTEGER NOT NULL
);`)
	// An invite's operator class (SPEC 8, zm-eakh.12.23.13) travels to the
	// node-bound token its redemption mints, in the same transaction: the
	// token is what the class holds for (revoked or replaced, the default
	// applies again), so removing a redeemed invite keeps it.
	store.RegisterRegistry("project/0003_invite_class", `
ALTER TABLE invites ADD COLUMN class TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN class TEXT NOT NULL DEFAULT '';`)
	// lowered_by is dropped again by project/0005; grant_ceilings
	// (ceiling.go) holds lowerings.
	store.RegisterRegistry("project/0004_grant_lowered_by", `
ALTER TABLE grants ADD COLUMN lowered_by TEXT NOT NULL DEFAULT '';`)
}

// Invite limits (SPEC 17.4).
const (
	DefaultInviteTTL = 24 * time.Hour
	MaxInviteTTL     = 7 * 24 * time.Hour
	MaxLiveInvites   = 10 // per project
	// MaxInviteAttempts redemptions of one invite's code (right code, any
	// node, retries included) before it stops answering.
	MaxInviteAttempts = 10
)

// ErrBadCode is the one answer for a wrong, used, expired or removed join
// code, and for a tagged node using an invite without --allow-tagged: the
// caller learns nothing about which.
//
// ErrBadCode carries no hint (the hint names a command of the running
// program, so it is made when the error is: BadCode). Compare with IsBadCode.
var ErrBadCode = api.Errorf(api.CodeForbidden, "invalid, used or expired join code")

// BadCode is ErrBadCode with its hint.
func BadCode() *api.Error {
	return ErrBadCode.WithHint("ask the project's owner for a new invite: " + config.Command("invite", "<project> --as <you> --role member"))
}

// IsBadCode reports whether err is the answer BadCode makes.
func IsBadCode(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == ErrBadCode.Code && e.Message == ErrBadCode.Message
}

// crockford is Crockford's base32 (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewJoinSecret is 8 Crockford base32 characters (40 bits), as XXXX-XXXX.
func NewJoinSecret() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, 0, 9)
	for i, x := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, crockford[x&31])
	}
	return string(out)
}

// NewTunnelSecret is a tunnel invite's secret (SPEC 17.11): 25 Crockford
// base32 characters (125 bits), as XXXXX-XXXXX-XXXXX-XXXXX-XXXXX. Only an
// HMAC proof keyed by its hash crosses the tunnel, and a proof captured by a
// hijacked hostname must not be guessable offline (40 bits would be).
func NewTunnelSecret() string {
	var b [25]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, 0, 29)
	for i, x := range b {
		if i > 0 && i%5 == 0 {
			out = append(out, '-')
		}
		out = append(out, crockford[x&31])
	}
	return string(out)
}

// NormalizeJoinSecret is the canonical form of a typed secret: upper case,
// dashes and spaces dropped, I/L read as 1 and O as 0. "" if it cannot be
// one (8 characters, or a tunnel invite's 25).
func NormalizeJoinSecret(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "I", "1", "L", "1", "O", "0").Replace(strings.TrimSpace(s)))
	if len(s) != 8 && len(s) != 25 {
		return ""
	}
	for _, c := range s {
		if !strings.ContainsRune(crockford, c) {
			return ""
		}
	}
	return s
}

func joinHash(secret string) string {
	h := sha256.Sum256([]byte("zm-invite/1\n" + secret))
	return hex.EncodeToString(h[:])
}

var fpEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NodeFingerprint is what a join code pins: 16 base32 characters of
// sha256 over the authority node's stable node ID. A replica checks it on
// every connection to its authority (WhoIs on the dialled peer).
func NodeFingerprint(nodeID string) string {
	h := sha256.Sum256([]byte("zm-node/1\n" + nodeID))
	return fpEnc.EncodeToString(h[:10])
}

// JoinCode is zm-<machine>:<fingerprint>:XXXX-XXXX (the tailnet), or
// <https URL>#<key fingerprint>:<secret> (the tunnel, SPEC 17.11).
type JoinCode struct {
	Machine string // the authority's machine name (tailnet codes)
	// Host is the authority node's full MagicDNS name when the code
	// carries it (zm-host-c.example.ts.net): a node shared in from
	// another tailnet resolves only by it. "" in older codes: zm-<machine>.
	Host string
	// NodePrefix is the node name's prefix in the code: "zm-" (or "" in a
	// code made by hand), or "potluck-" in a later daemon's code.
	NodePrefix  string
	URL         string // the home's public URL (tunnel codes)
	Fingerprint string // the node's (16) or the home key's (26)
	Secret      string // normalized, no dash
}

// KeyFingerprintLen is a home key fingerprint's length in a tunnel code.
const KeyFingerprintLen = 26

// MagicDNSSuffix ends every tailnet name a join code may carry (Tailscale's
// MagicDNS: <tailnet>.ts.net), so a code can never make a replica dial an
// internet host.
const MagicDNSSuffix = ".ts.net"

// validDNSName is a MagicDNS suffix after the node's first label: DNS
// labels of a-z 0-9 - ending in .ts.net (the tailnet's own name).
func validDNSName(s string) bool {
	if s == "" || len(s) > 200 || !strings.HasSuffix("."+s, MagicDNSSuffix) || "."+s == MagicDNSSuffix {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// AuthorityHost is the host a replica dials for c: its full MagicDNS name
// when the code carries it, else zm-<machine> (the code's own prefix).
func (c JoinCode) AuthorityHost() string {
	if c.Host != "" {
		return c.Host
	}
	if c.NodePrefix != "" {
		return c.NodePrefix + c.Machine
	}
	return api.NodePrefix + c.Machine
}

// LooksLikeJoinCode reports whether s is shaped like a join code rather
// than a project name (zm join takes either): a ':' or the zm- prefix (a
// potluck-<machine> code has its ':' separators; potluck-foo is a project), a
// secret's XXXX-XXXX shape, anything a project slug cannot be (upper case,
// too long, other characters). A malformed code is refused as a code and
// never starts a GitHub join; only a valid slug is a project name.
func LooksLikeJoinCode(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Contains(s, ":") || strings.HasPrefix(strings.ToLower(s), api.NodePrefix) || !api.ValidSlug(s) {
		return true
	}
	return len(s) == 9 && s[4] == '-' && NormalizeJoinSecret(s) != ""
}

// ParseJoinCode reads a join code, or the code in an invite link
// (JoinCodeOf).
func ParseJoinCode(s string) (JoinCode, error) {
	s, err := JoinCodeOf(s)
	if err != nil {
		return JoinCode{}, err
	}
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return parseTunnelCode(s)
	}
	bad := api.Errorf(api.CodeBadRequest, "a join code looks like potluck-<machine>:<fingerprint>:XXXX-XXXX (or zm-<machine>:...) or https://<host>#<fingerprint>:<secret>")
	parts := strings.Split(s, ":")
	if len(parts) != 3 || !(strings.HasPrefix(parts[0], api.NodePrefix) || strings.HasPrefix(parts[0], api.NodePrefixNew)) {
		return JoinCode{}, bad
	}
	node := strings.ToLower(strings.TrimSuffix(parts[0], "."))
	prefix, rest, _ := api.NodeMachine(node)
	machine, suffix, full := strings.Cut(rest, ".")
	c := JoinCode{Machine: machine, NodePrefix: prefix, Fingerprint: strings.ToLower(parts[1]), Secret: NormalizeJoinSecret(parts[2])}
	if full {
		if !validDNSName(suffix) {
			return JoinCode{}, bad
		}
		c.Host = node
	}
	if !api.ValidName(c.Machine) || len(c.Fingerprint) != 16 || len(c.Secret) != 8 {
		return JoinCode{}, bad
	}
	return c, nil
}

func parseTunnelCode(s string) (JoinCode, error) {
	bad := api.Errorf(api.CodeBadRequest, "a tunnel join code looks like https://<host>#<fingerprint>:<secret>")
	base, frag, ok := strings.Cut(s, "#")
	fp, secret, ok2 := strings.Cut(frag, ":")
	if !ok || !ok2 {
		return JoinCode{}, bad
	}
	u, err := TunnelURL(base)
	if err != nil {
		return JoinCode{}, err
	}
	c := JoinCode{URL: u, Fingerprint: strings.ToLower(fp), Secret: NormalizeJoinSecret(secret)}
	if len(c.Fingerprint) != KeyFingerprintLen || len(c.Secret) != 25 {
		return JoinCode{}, bad
	}
	return c, nil
}

// TunnelURL checks a home's public URL (SPEC 17.11) and returns it without
// a trailing slash: https, or http only to a loopback host (a rehearsal on
// one machine); no path (the API is served at /v0), query or user info.
func TunnelURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return "", api.Errorf(api.CodeBadRequest, "tunnel url %q: want https://<host>[:<port>], no path", s)
	}
	u.Path = ""
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		ip, err := netip.ParseAddr(h)
		if !strings.EqualFold(h, "localhost") && (err != nil || !ip.IsLoopback()) {
			return "", api.Errorf(api.CodeBadRequest, "tunnel url %q: http only to a loopback host; use https", s)
		}
	default:
		return "", api.Errorf(api.CodeBadRequest, "tunnel url %q: want https://", s)
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil && strings.Contains(u.Host, ":") && !strings.HasPrefix(u.Host, "[") {
		return "", api.Errorf(api.CodeBadRequest, "tunnel url %q: bad host", s)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// RedeemProof is what a replica sends to redeem a tunnel invite (SPEC
// 17.11): HMAC-SHA256 keyed by the code's stored hash over the home's key
// fingerprint, the replica's key id and its machine name. The secret never
// crosses the tunnel, and the proof is good only for that key.
func RedeemProof(secret, homeFP, keyID, machine string) string {
	return proof(joinHash(NormalizeJoinSecret(secret)), homeFP, keyID, machine)
}

func proof(hash, homeFP, keyID, machine string) string {
	m := hmac.New(sha256.New, []byte(hash))
	m.Write([]byte("zm-redeem/1\n" + homeFP + "\n" + keyID + "\n" + machine))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// MatchProof returns the code hash of the invite p proves (a live one, or
// one keyID already redeemed), or "" for none. Every candidate is tried,
// so the time does not say which failed.
func (r *Registry) MatchProof(ctx context.Context, p, homeFP, keyID, machine string) string {
	got, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || len(got) != sha256.Size || keyID == "" {
		return ""
	}
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT code_hash FROM invites WHERE (node = '' AND expires > ?) OR node = ?`,
		store.Millis(time.Now()), keyID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	found := ""
	for rows.Next() {
		var h string
		if rows.Scan(&h) != nil {
			continue
		}
		want, _ := base64.RawURLEncoding.DecodeString(proof(h, homeFP, keyID, machine))
		if hmac.Equal(got, want) && found == "" {
			found = h
		}
	}
	return found
}

// ---- team projects ----

// IsTeam reports whether project id is a team project whose authority is
// this machine.
func (r *Registry) IsTeam(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	return ok && e.team
}

// HeldToken is a live node-bound token of a registry.db that a home holds
// the secret of (HeldTokensIn): whose it is, and the node and machine HQ
// bound it to.
type HeldToken struct {
	Identity api.Identity
	Node     string
	Machine  string
}

// HeldTokensIn reads db, a registry.db (TuplesIn), for the tokens whose
// secrets are held: each a plain (never limited) token with that secret's
// hash, bound to a node, not a box's, unrevoked and unexpired. A secret
// with no such row is left out.
func HeldTokensIn(ctx context.Context, db *sql.DB, held []string) ([]HeldToken, error) {
	now := store.Millis(time.Now())
	var out []HeldToken
	for _, secret := range held {
		if !authz.LooksLikeToken(secret) {
			continue
		}
		if _, limited := authz.TokenLimit(secret); limited {
			continue
		}
		var h HeldToken
		err := db.QueryRowContext(ctx, `SELECT t.node, t.machine, i.id, i.name FROM tokens t JOIN identities i ON i.id = t.identity
			WHERE t.hash = ? AND t.node != '' AND t.box_key = '' AND t.route_limit = '' AND t.revoked IS NULL AND (t.expires IS NULL OR t.expires > ?)`,
			authz.HashToken(secret), now).Scan(&h.Node, &h.Machine, &h.Identity.ID, &h.Identity.Name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// TeamProjectsIn lists the team projects db, a registry.db, holds, by slug
// (TuplesIn): what IsTeam says of each project of a live registry.
func TeamProjectsIn(ctx context.Context, db *sql.DB) ([]api.ProjectRef, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, slug FROM projects WHERE team = 1 ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.ProjectRef
	for rows.Next() {
		var p api.ProjectRef
		if err := rows.Scan(&p.ID, &p.Slug); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetTeam records project id as team (or solo) and switches its file's pool
// (one writer, four readers). Moving the leases is the caller's (the work
// module's MoveLeases); it calls this at the commit point of the move.
func (r *Registry) SetTeam(ctx context.Context, id string, team bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return api.NotFound("project", id)
	}
	// The registry row first (the commit point of a move): a failed UPDATE
	// leaves the pool as it was; a pool that will not switch puts the row
	// back, so memory and the database never disagree.
	if _, err := r.st.Registry().ExecContext(ctx, `UPDATE projects SET team = ? WHERE id = ?`, team, id); err != nil {
		return err
	}
	if err := r.st.SetTeam(ctx, id, team); err != nil {
		if _, rerr := r.st.Registry().ExecContext(context.WithoutCancel(ctx), `UPDATE projects SET team = ? WHERE id = ?`, e.team, id); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	e.team = team
	return nil
}

// JoinedNodes counts the live node-bound tokens of project id: replicas
// still joined (zm project solo refuses while there are any).
func (r *Registry) JoinedNodes(ctx context.Context, id string) (int, error) {
	var n int
	err := r.st.Registry().QueryRowContext(ctx, `SELECT count(*) FROM tokens WHERE project = ? AND node != '' AND revoked IS NULL`, id).Scan(&n)
	return n, err
}

// bumpScope moves an identity's scope_version on: every grant change
// (revoke, role or area) makes its replicas' next call answer 409 rescope.
func bumpScope(ctx context.Context, tx *sql.Tx, identityID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE identities SET scope_version = scope_version + 1 WHERE id = ?`, identityID)
	return err
}

// ScopeVersion is an identity's current scope_version.
func (r *Registry) ScopeVersion(ctx context.Context, identityID string) (int64, error) {
	var v int64
	err := r.st.Registry().QueryRowContext(ctx, `SELECT scope_version FROM identities WHERE id = ?`, identityID).Scan(&v)
	return v, err
}

func (r *Registry) identityVersion(ctx context.Context, id string) (api.Identity, int64, error) {
	who, err := r.identity(ctx, id)
	if err != nil {
		return who, 0, err
	}
	v, err := r.ScopeVersion(ctx, who.ID)
	return who, v, err
}

// TeamHeader is the project header for a node-bound token's subject: its
// project as the member sees it, its cap and scope_version (SPEC 17.3).
func (r *Registry) TeamHeader(sub *authz.Subject) (api.TeamHeader, bool) {
	if sub == nil || sub.Node == "" || sub.Cap == nil || sub.Cap.Project == "" {
		return api.TeamHeader{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[sub.Cap.Project]
	if !ok {
		return api.TeamHeader{}, false
	}
	h := api.TeamHeader{Project: api.ProjectRef{ID: sub.Cap.Project, Slug: e.file.Slug}, Role: string(sub.Scope.Role(sub.Cap.Project)),
		Cap:          api.TeamCap{Project: sub.Cap.Project, Slug: e.file.Slug, Area: sub.Cap.Area, Role: string(sub.Cap.Role)},
		ScopeVersion: sub.ScopeVersion, Identity: sub.Identity.Name, Machine: sub.Machine}
	whole, areas := sub.Scope.Areas(sub.Cap.Project)
	vis := map[string]bool{}
	for _, a := range areas {
		vis[a] = true
	}
	for _, a := range e.file.Areas {
		if whole || vis[a.Name] {
			h.Areas = append(h.Areas, a.Name)
			h.Remotes = append(h.Remotes, a.Remotes...)
			if len(a.Paths)+len(a.Remotes) > 0 {
				h.AreaRepos = append(h.AreaRepos, api.NamedRepos{Area: a.Name, RepoRules: a.RepoRules})
			}
		}
	}
	if whole {
		h.Remotes = append(h.Remotes, e.file.Repos.Remotes...)
		h.Repos = e.file.Repos
	}
	return h, true
}

// ---- invites ----

// InviteInput is what CreateInvite needs beyond the request: the
// authority's machine name and node fingerprint, which go into the code;
// for a tunnel invite (SPEC 17.11), the home's public URL and its key's
// fingerprint instead.
type InviteInput struct {
	api.InviteRequest
	Authority string
	Host      string // the authority node's full MagicDNS name (the code carries it); "" for <Node>
	// Node is the authority's node name: potluck-<Authority> for a node made
	// from the rename's step 2 on; "" (or a name of another machine) for
	// zm-<Authority>, an older node's.
	Node        string
	Fingerprint string
	URL         string
}

// CreateInvite stores an invite for a team project and returns its join
// code, once. Admin on the project/area, or an inviter's guest invite
// (inviteTarget); at most MaxLiveInvites live per project. No identity,
// grant or token exists until redemption.
func (r *Registry) CreateInvite(ctx context.Context, sub *authz.Subject, in InviteInput) (api.InviteResponse, error) {
	rel, err := authz.ParseRelation(in.Role)
	if err != nil {
		return api.InviteResponse{}, err
	}
	if rel == authz.Owner {
		return api.InviteResponse{}, api.Errorf(api.CodeBadRequest, "an invite's role is member or guest").
			WithHint("owners are the authority's; grant owner with " + config.Command("grant") + " after they join")
	}
	pid, area, inviter, err := r.inviteTarget(ctx, sub, in, rel)
	if err != nil {
		return api.InviteResponse{}, err
	}
	if !r.IsTeam(pid) {
		return api.InviteResponse{}, api.Errorf(api.CodeConflict, "project %s is solo", r.Slug(pid)).
			WithHint("make this machine its authority first: " + config.Command("project team", r.Slug(pid)))
	}
	name := strings.TrimSpace(in.As)
	if !api.ValidName(name) || name != strings.ToLower(name) {
		return api.InviteResponse{}, api.Errorf(api.CodeBadRequest, "bad identity name %q", in.As)
	}
	if name == r.local.Name {
		return api.InviteResponse{}, api.Errorf(api.CodeBadRequest, "%s is this machine's owner", name)
	}
	ttl := DefaultInviteTTL
	if in.TTLSeconds < 0 {
		return api.InviteResponse{}, api.Errorf(api.CodeBadRequest, "ttl must be positive")
	} else if in.TTLSeconds > 0 {
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	if ttl > MaxInviteTTL {
		return api.InviteResponse{}, api.Errorf(api.CodeBadRequest, "ttl is over the maximum (7 days)")
	}
	class := strings.TrimSpace(in.Class)
	if class != "" && class != api.MachineTrusted && class != api.MachinePublic {
		return api.InviteResponse{}, api.Errorf(api.CodeBadRequest, "an invite's class is trusted, public or none (the default of its role)")
	}
	secret := NewJoinSecret()
	if in.URL != "" {
		if len(in.Fingerprint) != KeyFingerprintLen {
			return api.InviteResponse{}, api.Errorf(api.CodeUnavailable, "this machine's tunnel key is not readable")
		}
		secret = NewTunnelSecret()
	} else if !api.ValidName(in.Authority) || len(in.Fingerprint) != 16 {
		return api.InviteResponse{}, api.Errorf(api.CodeUnavailable, "this machine's tailnet node is not running").
			WithHint("a join code names the authority's tailnet node: run " + config.Command("tailnet up") + " first (or set tunnel.url and use --via tunnel)")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	inv := api.Invite{ID: ids.New(), Project: r.Slug(pid), ProjectID: pid, Area: area, As: name, Role: string(rel),
		AllowTagged: in.AllowTagged, Class: class, CreatedBy: sub.Address, Created: now, Expires: now.Add(ttl), State: api.InviteLive}
	err = store.Tx(ctx, r.st.Registry(), func(tx *sql.Tx) error {
		var live int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invites WHERE project = ? AND node = '' AND expires > ?`,
			pid, store.Millis(now)).Scan(&live); err != nil {
			return err
		}
		if inviter != "" {
			if err := inviterInviteTx(ctx, tx, inviter, pid, area, name); err != nil {
				return err
			}
		}
		if live >= MaxLiveInvites {
			return api.Errorf(api.CodeConflict, "project %s already has %d live invites", inv.Project, live).
				WithHint("remove one first: " + config.Command("invite ls") + ", " + config.Command("invite rm", "<id>"))
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO invites (id, project, area, identity, role, code_hash, allow_tagged, class, created, expires, created_by, inviter)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, inv.ID, pid, area, name, inv.Role, joinHash(NormalizeJoinSecret(secret)),
			in.AllowTagged, class, store.Millis(now), store.Millis(inv.Expires), sub.Address, inviter)
		return err
	})
	if err != nil {
		return api.InviteResponse{}, err
	}
	if in.URL != "" {
		return api.InviteResponse{Invite: inv, Code: in.URL + "#" + in.Fingerprint + ":" + secret}, nil
	}
	host := api.NodePrefix + in.Authority
	node := in.Node
	if node == "" {
		node, _, _ = strings.Cut(in.Host, ".")
	}
	if _, m, ok := api.NodeMachine(node); ok && m == in.Authority {
		host = strings.ToLower(strings.TrimSpace(node))
	}
	// A node shared in from another tailnet resolves only by its full name;
	// a name a code may not carry (a Headscale tailnet's own domain) leaves
	// the short code, which reaches the node through the pin-picked fallback.
	if h := strings.ToLower(strings.TrimSuffix(in.Host, ".")); strings.HasPrefix(h, host+".") && validDNSName(strings.TrimPrefix(h, host+".")) {
		host = h
	}
	return api.InviteResponse{Invite: inv, Code: host + ":" + in.Fingerprint + ":" + secret}, nil
}

const inviteCols = `id, project, area, identity, role, allow_tagged, class, created_by, created, expires, node, machine, token, pending_secret, redeemed`

func (r *Registry) scanInvite(sc interface{ Scan(...any) error }, now time.Time) (api.Invite, string, error) {
	var inv api.Invite
	var created, expires int64
	var token, pending string
	var redeemed sql.NullInt64
	if err := sc.Scan(&inv.ID, &inv.ProjectID, &inv.Area, &inv.As, &inv.Role, &inv.AllowTagged, &inv.Class, &inv.CreatedBy, &created, &expires,
		&inv.Node, &inv.Machine, &token, &pending, &redeemed); err != nil {
		return inv, "", err
	}
	inv.Project, inv.Created, inv.Expires, inv.Redeemed = r.Slug(inv.ProjectID), store.FromMillis(created), store.FromMillis(expires), store.NullMillis(redeemed)
	switch {
	case inv.Node != "":
		inv.State, inv.Used = api.InviteRedeemed, pending == ""
	case !inv.Expires.After(now):
		inv.State = api.InviteExpired
	default:
		inv.State = api.InviteLive
	}
	return inv, token, nil
}

// Invites lists every invite, newest first (local callers only).
func (r *Registry) Invites(ctx context.Context) ([]api.Invite, error) {
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT `+inviteCols+` FROM invites ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Invite{}
	now := time.Now()
	for rows.Next() {
		inv, _, err := r.scanInvite(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ended, err := r.endedInvites(ctx, now)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].State == api.InviteRedeemed && ended[out[i].ID] {
			out[i].Revoked, out[i].Used = true, false
		}
	}
	return out, nil
}

// endedInvites are the ids of redeemed invites whose machine (node) holds
// no live token in the invite's project now: it left, or its token was
// revoked or expired. A machine that joined again since keeps its invite in
// use.
func (r *Registry) endedInvites(ctx context.Context, now time.Time) (map[string]bool, error) {
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT i.id FROM invites i WHERE i.node != '' AND NOT EXISTS (
		SELECT 1 FROM tokens t WHERE t.node = i.node AND t.project = i.project AND t.revoked IS NULL AND (t.expires IS NULL OR t.expires > ?))`,
		store.Millis(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RemoveInvite deletes an invite (local callers only). A redeemed invite's
// token stays: revoke it with zm token revoke.
func (r *Registry) RemoveInvite(ctx context.Context, id string) (api.Invite, error) {
	inv, _, err := r.scanInvite(r.st.Registry().QueryRowContext(ctx, `DELETE FROM invites WHERE id = ? RETURNING `+inviteCols,
		strings.ToUpper(strings.TrimSpace(id))), time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return api.Invite{}, api.NotFound("invite", id)
	}
	return inv, err
}

// RedeemInput is one redemption, after the network checks (SPEC 17.4).
type RedeemInput struct {
	Secret  string // normalized
	Hash    string // instead of Secret: the code hash a proof matched (MatchProof)
	Node    string // the caller's stable node ID, or its key id (tunnel)
	Tagged  bool
	Machine string
}

// Redeem turns an invite into an identity (if new), a grant and a
// node-bound token, in one transaction. Until that node's first
// authenticated call with the token, the same code from the same node gets
// the same token again (a lost answer never burns the code); after it, and
// from any other node, ErrBadCode. A wrong, used, expired or removed code is
// ErrBadCode too. A machine name another node holds is a conflict (the
// code is not spent).
func (r *Registry) Redeem(ctx context.Context, in RedeemInput) (api.RedeemResponse, error) {
	if (in.Secret == "" && in.Hash == "") || in.Node == "" {
		return api.RedeemResponse{}, BadCode()
	}
	machine := strings.TrimSpace(in.Machine)
	if !api.ValidName(machine) || machine != strings.ToLower(machine) {
		return api.RedeemResponse{}, api.Errorf(api.CodeBadRequest, "bad machine name %q", in.Machine)
	}
	hash := in.Hash
	if hash == "" {
		hash = joinHash(in.Secret)
	}
	db := r.st.Registry()
	// The identity guard (approvals' lookalike rule) reads other tables:
	// check before the transaction, for the name this invite would create.
	var name string
	if err := db.QueryRowContext(ctx, `SELECT identity FROM invites WHERE code_hash = ?`, hash).Scan(&name); err != nil {
		return api.RedeemResponse{}, BadCode()
	}
	if _, err := r.identity(ctx, name); api.IsCode(err, api.CodeNotFound) {
		r.mu.RLock()
		guard := r.guard
		r.mu.RUnlock()
		if guard != nil {
			if err := guard(ctx, name); err != nil {
				return api.RedeemResponse{}, err
			}
		}
	}
	var out api.RedeemResponse
	now := time.Now().UTC().Truncate(time.Millisecond)
	err := store.Tx(ctx, db, func(tx *sql.Tx) error {
		var id, pid, area, ident, role, class, node, token, pending, inviter string
		var tagged bool
		var expires int64
		var attempts int
		err := tx.QueryRowContext(ctx, `SELECT id, project, area, identity, role, class, allow_tagged, expires, node, token, pending_secret, attempts, inviter
			FROM invites WHERE code_hash = ?`, hash).Scan(&id, &pid, &area, &ident, &role, &class, &tagged, &expires, &node, &token, &pending, &attempts, &inviter)
		if err != nil {
			return BadCode()
		}
		// A retry from the node that redeemed it is not an attempt: until
		// the token's first call it gets the same token, however often a
		// lost answer makes it ask (checked below).
		if retry := node != "" && node == in.Node; !retry {
			if _, err := tx.ExecContext(ctx, `UPDATE invites SET attempts = attempts + 1 WHERE id = ?`, id); err != nil {
				return err
			}
			if attempts >= MaxInviteAttempts {
				return errSpent
			}
		}
		if in.Tagged && !tagged {
			return errSpent
		}
		slug := r.Slug(pid)
		if slug == "" || !r.IsTeam(pid) {
			return errSpent
		}
		out = api.RedeemResponse{Cap: api.TeamCap{Project: pid, Slug: slug, Area: area, Role: role}, Machine: machine,
			Identity: ident, Authority: r.cfg.Machine}
		if node != "" {
			// Idempotent per (code, node) until the token's first use.
			var revoked sql.NullInt64
			var tokMachine string
			if node != in.Node || pending == "" ||
				tx.QueryRowContext(ctx, `SELECT revoked, machine FROM tokens WHERE id = ?`, token).Scan(&revoked, &tokMachine) != nil || revoked.Valid {
				return errSpent
			}
			out.Token, out.Machine = pending, tokMachine
			return tx.QueryRowContext(ctx, `SELECT scope_version FROM identities WHERE name = ?`, ident).Scan(&out.ScopeVersion)
		}
		if expires <= store.Millis(now) {
			return errSpent
		}
		if inviter != "" {
			if ok, err := r.inviterRedeemable(ctx, tx, id, inviter, pid, area, ident); err != nil {
				return err
			} else if !ok {
				return errSpent
			}
		}
		if err := r.freshJoin(ctx, tx, pid, machine, in.Node, now); err != nil {
			return err
		}
		who, tid, secret, err := r.admitTx(ctx, tx, now, admission{node: in.Node, machine: machine, identity: ident,
			project: pid, area: area, role: role, class: class, by: "invite:" + id, invite: true})
		if err != nil {
			return err
		}
		if inviter != "" {
			if err := markGuestOf(ctx, tx, who, inviter); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET node = ?, machine = ?, token = ?, pending_secret = ?, redeemed = ? WHERE id = ?`,
			in.Node, machine, tid, secret, store.Millis(now), id); err != nil {
			return err
		}
		out.Token = secret
		return tx.QueryRowContext(ctx, `SELECT scope_version FROM identities WHERE id = ?`, who).Scan(&out.ScopeVersion)
	})
	r.forgetMachines()
	if errors.Is(err, errSpent) {
		// The attempt counter must stick, so the refusal commits: answer
		// ErrBadCode after recording the attempt.
		_, _ = db.ExecContext(ctx, `UPDATE invites SET attempts = attempts + 1 WHERE code_hash = ?`, hash)
		return api.RedeemResponse{}, BadCode()
	}
	if err != nil {
		return api.RedeemResponse{}, err
	}
	return out, nil
}

// nodeLive reports whether node holds a live (not revoked) node-bound
// token in any project: while it does, it keeps its machine name.
func nodeLive(ctx context.Context, tx *sql.Tx, node string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens WHERE node = ? AND revoked IS NULL`, node).Scan(&n)
	return n > 0, err
}

// errSpent rolls a redemption back; Redeem answers ErrBadCode for it.
var errSpent = errors.New("join code spent")

// ensureIdentityTx finds or creates (kind human) identity name inside tx.
func ensureIdentityTx(ctx context.Context, tx *sql.Tx, name string, now time.Time) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM identities WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = ids.New()
	_, err = tx.ExecContext(ctx, `INSERT INTO identities (id, name, kind, created) VALUES (?, ?, ?, ?)`, id, name, api.IdentityHuman, store.Millis(now))
	return id, err
}

// RevokeOwn revokes the node-bound token sub authenticated with (a replica
// leaving its team project).
func (r *Registry) RevokeOwn(ctx context.Context, sub *authz.Subject) error {
	if sub == nil || sub.Node == "" || sub.TokenID == "" {
		return api.Errorf(api.CodeForbidden, "only a replica's node-bound token can leave this way")
	}
	_, err := r.st.Registry().ExecContext(ctx, `UPDATE tokens SET revoked = coalesce(revoked, ?) WHERE id = ? AND node = ?`,
		store.Millis(time.Now()), sub.TokenID, sub.Node)
	r.forgetMachines()
	return err
}

// machineQueries counts MachineProjects' database reads (tests).
var machineQueries atomic.Int64

// machineEntry is one machine's cached projects; until (UnixMilli, 0 =
// never) is its first token's expiry, when the entry is read again.
type machineEntry struct {
	projects []string
	until    int64
}

// MachineProjects lists the team projects a replica machine name holds a
// live (unrevoked, unexpired) node-bound token for. Cached per machine until
// the next redemption or revocation, or the first of its tokens' expiry.
func (r *Registry) MachineProjects(ctx context.Context, machine string) []string {
	r.machinesMu.Lock()
	defer r.machinesMu.Unlock()
	now := store.Millis(time.Now())
	if e, ok := r.machines[machine]; ok && (e.until == 0 || now < e.until) {
		return e.projects
	}
	machineQueries.Add(1)
	// A token whose identity has no grant left in the project (zm revoke
	// took the last one) is as good as revoked: its replica can neither
	// read nor heartbeat, so its sessions must not linger in the
	// authority's lists (dogfood: a revoked guest's session showed for an
	// hour, holding a key it had released). An expired one is the same.
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT t.project, min(t.expires) FROM tokens t WHERE t.machine = ? AND t.node != '' AND t.revoked IS NULL
		AND (t.expires IS NULL OR t.expires > ?)
		AND EXISTS (SELECT 1 FROM grants g WHERE g.subject = t.identity AND g.project = t.project) GROUP BY t.project`, machine, now)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	var until int64
	for rows.Next() {
		var p string
		var exp sql.NullInt64
		if rows.Scan(&p, &exp) == nil {
			out = append(out, p)
			if exp.Valid && (until == 0 || exp.Int64 < until) {
				until = exp.Int64
			}
		}
	}
	if rows.Err() != nil {
		return out
	}
	if r.machines == nil {
		r.machines = map[string]machineEntry{}
	}
	r.machines[machine] = machineEntry{projects: out, until: until}
	return out
}

// forgetMachines clears the MachineProjects cache (a node-bound token was
// added or revoked).
func (r *Registry) forgetMachines() {
	r.machinesMu.Lock()
	r.machines = nil
	r.machinesMu.Unlock()
}

// RepoFileProject is what a .zm/project.toml in cwd or a parent up to the
// git root names (slug, area), for a replica deciding whether a write goes
// to a team project it joined.
func RepoFileProject(cwd string) (slug, area string, ok bool) {
	cwd = ExpandPath(cwd)
	if cwd == "" {
		return "", "", false
	}
	f, _, found := findRepoFile(cwd, GitRoot(cwd))
	if !found || f.err != nil {
		return "", "", false
	}
	return f.file.Project, f.file.Area, true
}

// TokenLive reports whether token id is neither revoked nor expired (a
// waiting sync call ends when its replica's token stops being one). A
// limited token is never live here: no sync call is its.
func (r *Registry) TokenLive(ctx context.Context, id string) bool {
	var revoked, expires sql.NullInt64
	if err := r.st.Registry().QueryRowContext(ctx, `SELECT revoked, expires FROM tokens WHERE id = ? AND route_limit = ''`, id).Scan(&revoked, &expires); err != nil {
		return false
	}
	return !revoked.Valid && (!expires.Valid || expires.Int64 > store.Millis(time.Now()))
}

// TokenScope is what token id reads now (its identity's grants, narrowed to
// the token's cap), or false once it is revoked, expired or gone: a scope
// recorded at a heartbeat must not outlive a downgrade. A limited token has
// no scope here (false): this is read with no route to check its limit.
func (r *Registry) TokenScope(ctx context.Context, id string) (authz.Scope, bool) {
	var identityID, pid, area, role string
	var revoked, expires sql.NullInt64
	if err := r.st.Registry().QueryRowContext(ctx, `SELECT identity, project, area, role, revoked, expires FROM tokens WHERE id = ? AND route_limit = ''`, id).
		Scan(&identityID, &pid, &area, &role, &revoked, &expires); err != nil {
		return authz.Scope{}, false
	}
	if revoked.Valid || (expires.Valid && expires.Int64 <= store.Millis(time.Now())) {
		return authz.Scope{}, false
	}
	tuples, err := r.Tuples(ctx, identityID)
	if err != nil {
		return authz.Scope{}, false
	}
	return authz.FromTuples(tuples, &authz.Cap{Project: pid, Area: area, Role: authz.Relation(role)}), true
}

// Member is an identity's grant in a project (GET /v0/people).
type Member struct {
	Identity api.Identity // ID, Name and Kind
	Project  string       // ULID
	Area     string       // "" for a whole-project grant
	Relation authz.Relation
}

// Members lists the grants in projects with their identities, ignoring
// scope (daemon internal: the caller keeps what its reader shares).
func (r *Registry) Members(ctx context.Context, projects []string) ([]Member, error) {
	return MembersIn(ctx, r.st.Registry(), projects)
}

// MembersIn is Members read from db, a registry.db (TuplesIn).
func MembersIn(ctx context.Context, db *sql.DB, projects []string) ([]Member, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	js, err := json.Marshal(projects)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT i.id, i.name, i.kind, g.project, g.area, g.relation FROM grants g JOIN identities i ON i.id = g.subject
		WHERE g.project IN (SELECT value FROM json_each(?)) ORDER BY i.name, g.project, g.area`, string(js))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Identity.ID, &m.Identity.Name, &m.Identity.Kind, &m.Project, &m.Area, &m.Relation); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Joined is how a machine joined a project (MachineJoin).
type Joined struct {
	Nodes  []string // the nodes of its live node-bound tokens there
	Member bool     // one of those tokens is for the whole project and counts for member or more now (tokenStanding)
	// Class is the operator class the invites those tokens came from carried:
	// public when any did (the stricter wins), else trusted when any did,
	// else "" (none did).
	Class string
}

// liveJoinWhere selects the tokens by which a machine is joined to a project
// (arguments: machine, project, now in ms): node-bound, unrevoked, unexpired,
// their identity still holding a grant in the project. MachineJoin and
// Admit's fresh-join test both stand on it.
const liveJoinWhere = `t.machine = ? AND t.project = ? AND t.node != ''
		AND t.revoked IS NULL AND (t.expires IS NULL OR t.expires > ?)
		AND EXISTS (SELECT 1 FROM grants g WHERE g.subject = t.identity AND g.project = t.project)`

// heldGrant is the SQL column of the relation a token's identity holds now on
// the grant the token is for (the same project and area), else empty. Grants are
// keyed by subject and object, so there is at most one. Read it with
// tokenStanding.
const heldGrant = `COALESCE((SELECT g.relation FROM grants g WHERE g.subject = t.identity AND g.project = t.project AND g.area = t.area), '')`

// tokenStanding is what a join token counts for now: the weaker of its own
// role and held, the relation its grant has now (heldGrant), so lowering a
// member to guest makes their token a guest's at once. own reports that the
// grant still allows all the token's role did, the condition for the class
// its invite carried to loosen the machine. A token whose grant is gone, or
// whose role is no known relation, counts for nothing.
func tokenStanding(role, held string) (now authz.Relation, own bool) {
	tok, err := authz.ParseRelation(role)
	if err != nil {
		return authz.None, false
	}
	h, err := authz.ParseRelation(held)
	if err != nil {
		return authz.None, false
	}
	now = authz.Min(tok, h)
	return now, now == tok
}

// MachineJoin is how machine joined project, at its authority: the nodes of
// its live node-bound tokens there (unrevoked, unexpired, their identity
// still holding a grant in the project, as MachineProjects counts them),
// whether any of those tokens is itself for the whole project (no area) with
// a role of member or more, and the class their invites carried. It is the
// default of a machine's operator class (SPEC 8 "Operator class"): the class
// the owner put on the invite, else a machine the owner let in as a member is
// trusted; one let in as a guest (a lender, a stranger), or to one area, is
// public, whatever else its identity holds. The token's own role decides, not
// the identity's other grants, so a guest invite for a member's spare machine
// stays a guest's; and only as far as the grant the token is for still
// allows: lowering a member to guest makes their machine a guest's at once,
// with no rejoin (zm-eakh.12.5.10.8).
func (r *Registry) MachineJoin(ctx context.Context, project, machine string) (Joined, error) {
	var j Joined
	// What loosens a machine (a trusted class, a member's role) counts only
	// while its identity still holds the grant the token is for (the same
	// area) at the token's role or more: revoking a host's hosts grant, or a
	// member's, ends it though another grant keeps the token live, and so
	// does lowering it. A public class counts while the token is live: losing
	// or lowering a grant never makes a machine more trusted.
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT t.node, t.role, t.area, t.class,
		`+heldGrant+`
		FROM tokens t WHERE `+liveJoinWhere+` ORDER BY t.node`,
		machine, project, store.Millis(time.Now()))
	if err != nil {
		return j, err
	}
	defer rows.Close()
	for rows.Next() {
		var node, role, area, class, held string
		if err := rows.Scan(&node, &role, &area, &class, &held); err != nil {
			return Joined{}, err
		}
		if len(j.Nodes) == 0 || j.Nodes[len(j.Nodes)-1] != node {
			j.Nodes = append(j.Nodes, node)
		}
		now, own := tokenStanding(role, held)
		if area == "" && now.Allows(authz.Operate) {
			j.Member = true
		}
		switch {
		case class == api.MachinePublic:
			j.Class = api.MachinePublic
		case own && class == api.MachineTrusted && j.Class == "":
			j.Class = api.MachineTrusted
		}
	}
	return j, rows.Err()
}

// InvitedMachines are the machine names in project holding a live
// node-bound token whose invite carried a class (an area guest's machine
// sends no numbers, so a pool's host is listed this way).
func (r *Registry) InvitedMachines(ctx context.Context, project string) ([]string, error) {
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT DISTINCT t.machine FROM tokens t WHERE t.project = ? AND t.node != '' AND t.class != ''
		AND t.revoked IS NULL AND (t.expires IS NULL OR t.expires > ?)
		AND EXISTS (SELECT 1 FROM grants g WHERE g.subject = t.identity AND g.project = t.project AND g.area = t.area) ORDER BY t.machine`,
		project, store.Millis(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
