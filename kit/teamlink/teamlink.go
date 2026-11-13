// Package teamlink is being joined to another daemon (SPEC 17, KIT layer):
// on a team project's authority, zm project team|solo, invites and their
// redemption (the only unauthenticated route besides health and the receipt
// key), operator classes, the machine-op queue (ops.go: kinds are
// registered by the modules that own them), the heartbeat's answer (beat.go:
// parts are registered too) and machine numbers (machine.go: GET
// /v0/machines); on a member's machine, the heartbeat loop, zm join and zm leave,
// the joined projects (remotes) with their node-bound tokens, the client to
// each authority (every call goes through Send: the token, scope_version,
// the header it answers with, 401 counting, purge and role recovery), and
// GET /v0/people.
//
// On a member's socket, the proxy forwards a call for a joined project to
// its authority (proxy.go, hedge.go) as each route's row says (routes.go);
// teamlink registers the KIT routes' rows, the layer above its own.
//
// It imports only KIT packages. The orchestration layer (internal/team)
// builds the replica copy, its beat parts and its op kinds on top and plugs
// into it through Hooks, RegisterBeatPart, RegisterOp and its route rows.
package teamlink

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/atomicfile"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

var remoteMigrations = []store.Migration{
	// The replica's side: the team projects this machine joined.
	{Kind: store.KindRegistry, Name: "team/0001_remotes", Script: `
CREATE TABLE team_remotes (
	project       TEXT PRIMARY KEY,
	slug          TEXT NOT NULL,
	authority     TEXT NOT NULL,
	url           TEXT NOT NULL,
	fingerprint   TEXT NOT NULL,
	machine       TEXT NOT NULL,
	identity      TEXT NOT NULL,
	cap           TEXT NOT NULL DEFAULT '{}',
	scope_version INTEGER NOT NULL DEFAULT 0,
	header        TEXT NOT NULL DEFAULT '{}',
	paths         TEXT NOT NULL DEFAULT '[]',
	joined        INTEGER NOT NULL,
	state         TEXT NOT NULL DEFAULT 'ok',
	last_error    TEXT NOT NULL DEFAULT ''
);`},
	// SPEC 17.11: a remote reached over the tunnel (url is its https URL,
	// fingerprint its home key's).
	{Kind: store.KindRegistry, Name: "team/0004_via", Script: `ALTER TABLE team_remotes ADD COLUMN via TEXT NOT NULL DEFAULT 'tailnet';`},
}

// DirRemotes holds each joined project's token: remotes/<ULID>.token (0600).
const DirRemotes = "remotes"

// Tunables (tests shorten them).
var (
	// PurgeAfter: a replica that got 401 three times, the first and third at
	// least this far apart, purges its copy (one 401 can be a restarting
	// authority).
	PurgeAfter = 10 * time.Minute
	// CallTimeout bounds one forwarded call.
	CallTimeout = 30 * time.Second
	// DialTimeout bounds connecting to an authority (the dial through this
	// node and the fingerprint check): an offline peer must not stall every
	// merged list on this machine for CallTimeout. Unmeasured on a real
	// tailnet.
	DialTimeout = 2 * time.Second
	// Redemption rate limits: wrong codes per node and overall per window.
	RedeemWindow      = 10 * time.Minute
	RedeemPerNode     = 5
	RedeemPerDaemon   = 50
	DefaultJoinWait   = 10 * time.Second
	unauthorizedLimit = 3
)

// Hooks are how the layer above (internal/team) takes part in a remote's
// life. Each may be nil; set them before Start.
type Hooks struct {
	// Joined runs once a join saved its remote and published it: the copy
	// starts, and resp may get what the member sees (Items, Notes).
	Joined func(ctx context.Context, x *Remote, resp *api.JoinResponse)
	// Removed runs when a remote is forgotten (zm leave): its local state
	// goes; it returns the files it removed.
	Removed func(x *Remote) []string
	// DropCopy drops x's local copy (the caller holds x.ReplicaMu): a
	// revoked or replaced remote never reuses it.
	DropCopy func(x *Remote) error
	// OpenCopy opens x's local copy again after role recovery (the caller
	// holds x.ReplicaMu); x.Copy is non-nil once it is open.
	OpenCopy func(ctx context.Context, x *Remote)
	// ScopeMoved runs when an answer's header moved x's scope (version,
	// role, areas or cap), with x.Mu held: the copy is taken again.
	ScopeMoved func(x *Remote)
	// View adds the layer's state to a remote listed by GET /v0/team/remotes.
	View func(x *Remote, r *api.Remote)
	// MoveLeases moves a project's leases when it turns team (toTeam) or
	// solo and records the mode; nil records the mode only.
	MoveLeases func(ctx context.Context, project string, toTeam bool) (int, error)
	// Machines lists project's machines (with numbers) at its authority;
	// nil: the machines that joined it.
	Machines func(ctx context.Context, project string) []string
	// ClassChanged runs after an operator class was set on machine.
	ClassChanged []func(ctx context.Context, project, machine string, c api.MachineClass)
	// DropForgotten each drop what the layer above keeps for a project id
	// (a host's shares and handle ledger) when this machine's owner forgets
	// the id's authority key and chooses to drop it (project_keys.go); each
	// returns what it dropped. An error keeps the key bound.
	DropForgotten []func(ctx context.Context, project string) ([]string, error)
	// The proxy (proxy.go). FromMember answers a member's call at the
	// authority before the router (live writes): true when it did.
	FromMember func(w http.ResponseWriter, r *http.Request, next http.Handler) bool
	// Copy is x's local copy: ready (it serves while the authority is
	// away), reachable (the authority answered last), the last error. nil:
	// no copy (every call asks the authority).
	Copy func(x *Remote) (ready, reachable bool, lastErr string)
	// ServeCopy may answer r for joined project x from its copy (body nil
	// while unread): true when it did.
	ServeCopy func(w http.ResponseWriter, r *http.Request, x *Remote, area string, body []byte) bool
	// ServeHeld may answer an item this machine answered 404 from x's copy
	// when the copy holds it: true when it did.
	ServeHeld func(w http.ResponseWriter, r *http.Request, x *Remote, body []byte) bool
	// ListShare may give x's share of a merged list without asking the
	// authority (ok true).
	ListShare func(r *http.Request, x *Remote) (theirs map[string]json.RawMessage, ok bool, err error)
	// Relay copies a forwarded answer to the caller (and reads it); nil:
	// copied as it came.
	Relay func(w http.ResponseWriter, r *http.Request, resp *http.Response)
}

// Service is the teamlink module.
type Service struct {
	d *core.Deps
	// Hooks: see Hooks.
	Hooks Hooks

	mu      sync.RWMutex
	remotes map[string]*Remote // by the authority's project ULID
	n       atomic.Int32       // len(remotes), read on every request
	loaded  atomic.Bool        // Load read every recorded join (Loaded)

	keyMu sync.Mutex // a join's bind and save of a project key, and a forget (project_keys.go)

	limMu sync.Mutex
	lims  map[string]*limiter // redemption limits, per transport

	ghJoins sync.Map // GitHub joins this daemon started, by handle (join_github.go)

	ops     opReg        // machine op kinds, the ops poll and the ops running here (ops.go)
	beat    beatReg      // heartbeat parts and loops (beat.go)
	numbers machineState // machine numbers sent and kept (machine.go)

	rctx context.Context // ends at shutdown: role recovery stops with it

	routes routes      // the proxy's route rows (routes.go)
	hedgeB hedgeBudget // the hedged reads' budget and counters (hedgebudget.go)
}

var (
	_ core.Module        = (*Service)(nil)
	_ core.Starter       = (*Service)(nil)
	_ core.Closer        = (*Service)(nil)
	_ core.TeamProxy     = (*Service)(nil)
	_ core.ProxyLister   = (*Service)(nil)
	_ core.HedgeReporter = (*Service)(nil)
)

// ServiceName is teamlink's name on core.Deps (the wiring provides it):
// modules above it (run targets) reach it with From.
const ServiceName = "teamlink"

// From is the teamlink the wiring provided on d, or nil.
func From(d *core.Deps) *Service {
	s, _ := d.Service(ServiceName).(*Service)
	return s
}

// Migrations are teamlink's tables in registry.db (New adds them).
var Migrations = slices.Concat(remoteMigrations, opMigrations, machineClassMigrations, projectKeyMigrations)

// New returns the teamlink module with its own route rows. Remotes load in
// Start.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, remotes: map[string]*Remote{}}
	s.beat.kick = make(chan struct{}, 1)
	s.RegisterRoutes("teamlink", s.kitRows()...)
	if d.Projects != nil {
		d.Projects.OnFreshJoin(forgetClass)
	}
	return s
}

// Start checks the route rows, loads the joined projects and starts the
// heartbeat and each one's ops poll. It never blocks on the network.
func (s *Service) Start(ctx context.Context) error {
	if err := s.CheckRoutes(); err != nil {
		return err
	}
	s.Run(ctx)
	if err := s.Load(ctx); err != nil {
		return err
	}
	s.startLoops(ctx)
	return nil
}

// Run sets the context role recovery stops with (Start does; tests that
// add remotes by hand call it).
func (s *Service) Run(ctx context.Context) { s.rctx = ctx }

// Context ends at shutdown (nil before Start).
func (s *Service) Context() context.Context { return s.rctx }

// Count is how many remotes are recorded (read on every request).
func (s *Service) Count() int32 { return s.n.Load() }

// Add records x as its project's remote (replacing any other).
func (s *Service) Add(x *Remote) {
	id := x.Snapshot().Project.ID
	s.mu.Lock()
	old := s.remotes[id]
	s.remotes[id] = x
	s.n.Store(int32(len(s.remotes)))
	s.mu.Unlock()
	if old != nil && old != x {
		s.unwatch(old)
	}
	s.watch(x)
}

// Forget drops project's remote from the map only (no files, no hooks).
func (s *Service) Forget(project string) {
	s.mu.Lock()
	x := s.remotes[project]
	delete(s.remotes, project)
	s.n.Store(int32(len(s.remotes)))
	s.mu.Unlock()
	if x != nil {
		s.unwatch(x)
	}
}

// Remote is one joined team project, as this machine holds it. Mu guards R
// and the header fields; the layer above keeps its copy in Copy.
type Remote struct {
	Mu            sync.Mutex
	BeatMu        sync.Mutex   // heartbeats to this authority go one at a time, answers in order
	ReplicaMu     sync.Mutex   // serializes copy teardown, reopening and recovery
	PersistMu     sync.Mutex   // terminal leave precedes every later observer save
	saveMu        sync.Mutex   // saves to team_remotes go one at a time (save)
	PublicationMu sync.RWMutex // header transitions exclude old foreground publications
	Left          bool         // terminal even if removing files/registry fails
	Recovery      RoleRecovery
	R             api.Remote
	Token         string
	HC            *http.Client
	FreshHC       *http.Client  // hc without connection reuse (hedge.go)
	Unauth        []time.Time   // 401s since the last answer that was not one
	HeaderRaw     string        // the header as JSON (stored)
	HeaderB64     string        // the last Zm-Team value seen (skips decoding an unchanged one)
	HeaderSeq     uint64        // the sent number of the call headerB64 came with
	Sent          atomic.Uint64 // calls sent to the authority, numbered (observe)
	binds         []project.Binding
	bindsOf       string // the headerRaw and paths binds was made from

	// The layer above's local copy (internal/team: *replica, nil when there
	// is none) and its loops; Copy is guarded by Mu.
	Copy     any
	StopCopy context.CancelFunc
	CopyWG   sync.WaitGroup
	RoleWG   sync.WaitGroup // queued lifecycle work, admitted before closing
}

func (x *Remote) Snapshot() api.Remote {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	r := x.R
	r.Paths = append([]string(nil), x.R.Paths...)
	r.Areas = append([]string(nil), x.R.Areas...)
	return r
}

// bindings are x's cwd rules (SPEC "Which project"): the checkouts recorded
// at join, and the project's and its visible areas' paths and remotes from
// the authority's header, made again only when either changed.
func (x *Remote) Bindings() []project.Binding {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	key := x.HeaderRaw + "\x00" + strings.Join(x.R.Paths, "\x00")
	if x.binds != nil && x.bindsOf == key {
		return x.binds
	}
	var h api.TeamHeader
	_ = json.Unmarshal([]byte(x.HeaderRaw), &h)
	id := x.R.Project.ID
	bs := []project.Binding{{Project: id, RepoRules: api.RepoRules{Paths: x.R.Paths}}, {Project: id, RepoRules: h.Repos}}
	for _, a := range h.AreaRepos {
		bs = append(bs, project.Binding{Project: id, Area: a.Area, RepoRules: a.RepoRules})
	}
	x.binds, x.bindsOf = bs, key
	return bs
}

var (
	_ core.TeamRepos      = (*Service)(nil)
	_ core.TeamReplicas   = (*Service)(nil)
	_ core.JoinedLister   = (*Service)(nil)
	_ core.MachineClasses = (*Service)(nil)
)

// Replicates implements core.TeamReplicas: a remote is recorded for the
// authority project ULID, whatever its state (a revoked one still names an
// authority that is not this machine).
func (s *Service) Replicates(project string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.remotes[strings.ToUpper(project)]
	return ok
}

// JoinedRepos implements core.TeamRepos: each joined project whose access
// has not ended, with the repository rules of its stored header (the same
// rules bindings matches a cwd by). No network call.
func (s *Service) JoinedRepos() []core.JoinedRepos {
	var out []core.JoinedRepos
	for _, x := range s.List() {
		snap := x.Snapshot()
		if Ended(snap) != nil {
			continue
		}
		x.Mu.Lock()
		var h api.TeamHeader
		_ = json.Unmarshal([]byte(x.HeaderRaw), &h)
		x.Mu.Unlock()
		out = append(out, core.JoinedRepos{Project: snap.Project, Repos: h.Repos, AreaRepos: h.AreaRepos})
	}
	return out
}

func (s *Service) Load(ctx context.Context) error {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT project, slug, authority, url, fingerprint, machine, identity, cap,
		scope_version, header, paths, joined, state, last_error, via FROM team_remotes`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]*Remote{}
	for rows.Next() {
		var r api.Remote
		var capJS, header, paths string
		var joined int64
		if err := rows.Scan(&r.Project.ID, &r.Project.Slug, &r.Authority, &r.URL, &r.Fingerprint, &r.Machine, &r.Identity, &capJS,
			&r.ScopeVersion, &header, &paths, &joined, &r.State, &r.LastError, &r.Via); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(capJS), &r.Cap)
		_ = json.Unmarshal([]byte(paths), &r.Paths)
		var h api.TeamHeader
		if json.Unmarshal([]byte(header), &h) == nil {
			r.Areas, r.Role = h.Areas, h.Role
		}
		r.Joined = store.FromMillis(joined)
		r.TokenFile = s.TokenPath(r.Project.ID)
		x := &Remote{R: r, HeaderRaw: header}
		if header == "{}" {
			x.HeaderRaw = ""
		}
		if v, err := secrets.Get(context.Background(), secrets.File(r.TokenFile)); err == nil {
			x.Token = v.Reveal()
		} else if r.State == api.RemoteOK {
			x.R.State, x.R.LastError = api.RemotePurged, "token file: "+err.Error()
		}
		x.HC = s.clientFor(r)
		m[r.Project.ID] = x
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.remotes = m
	s.n.Store(int32(len(m)))
	s.mu.Unlock()
	s.loaded.Store(true)
	return nil
}

// Loaded reports whether a Load has read every join this machine records:
// before it (or after it failed), List may be missing some.
func (s *Service) Loaded() bool { return s.loaded.Load() }

func (s *Service) TokenPath(project string) string {
	return filepath.Join(s.d.Config.Home, DirRemotes, project+".token")
}

// headersPath holds the headers a remote's calls carry (core.TeamTransport
// Extra: a Cloudflare Access service token), "Name: value" lines, 0600.
func (s *Service) headersPath(project string) string {
	return filepath.Join(s.d.Config.Home, DirRemotes, project+".headers")
}

// clientFor is r's client, from its transport.
func (s *Service) clientFor(r api.Remote) *http.Client {
	var extra http.Header
	if v, err := secrets.Get(context.Background(), secrets.File(s.headersPath(r.Project.ID))); err == nil {
		extra = decodeHeaders(v.Reveal())
	}
	return s.client(core.JoinTarget{Via: r.Via, URL: r.URL, Authority: r.Authority, Fingerprint: r.Fingerprint}, extra)
}

func encodeHeaders(h http.Header) []byte {
	var b strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	return []byte(b.String())
}

func decodeHeaders(s string) http.Header {
	h := http.Header{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) != "" {
			h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	if len(h) == 0 {
		return nil
	}
	return h
}

type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

// list is every remote, by slug.
func (s *Service) List() []*Remote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Remote, 0, len(s.remotes))
	for _, x := range s.remotes {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug() < out[j].Slug() })
	return out
}

// JoinedSlugs implements core.JoinedLister: the joined projects that are
// still joined (not purged or revoked).
func (s *Service) JoinedSlugs() []string {
	var out []string
	for _, x := range s.List() {
		if r := x.Snapshot(); r.State == "" || r.State == "ok" {
			out = append(out, x.Slug())
		}
	}
	return out
}

// byName finds a remote by the authority's ULID or by slug.
func (s *Service) ByName(name string) *Remote {
	if name == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if x, ok := s.remotes[strings.ToUpper(name)]; ok {
		return x
	}
	for _, x := range s.remotes {
		if x.Slug() == strings.ToLower(name) {
			return x
		}
	}
	return nil
}

func (x *Remote) Slug() string {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	return x.R.Project.Slug
}

// Save writes x's record to registry.db. Every column is the remote's as
// it is in memory: a re-join that replaces a revoked or purged record
// (finishJoin) has a new URL, key, identity and transport, and a restart
// must load those.
func (s *Service) Save(ctx context.Context, x *Remote) error {
	// One save at a time, reading the remote and its header under one lock:
	// otherwise two saves (or a header taken between the reads) can store an
	// old scope version with a newer header, and after a restart observe
	// keeps the old version until the header changes again.
	x.saveMu.Lock()
	defer x.saveMu.Unlock()
	x.Mu.Lock()
	r, header := x.R, x.HeaderRaw
	capJS, _ := json.Marshal(r.Cap)
	paths, _ := json.Marshal(nonNil(r.Paths))
	x.Mu.Unlock()
	if header == "" {
		header = "{}"
	}
	via := r.Via
	if via == "" {
		via = api.ViaTailnet
	}
	_, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_remotes (project, slug, authority, url, fingerprint, machine, identity,
		cap, scope_version, header, paths, joined, state, last_error, via) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET slug = excluded.slug, authority = excluded.authority, url = excluded.url,
			fingerprint = excluded.fingerprint, machine = excluded.machine, identity = excluded.identity, cap = excluded.cap,
			scope_version = excluded.scope_version, header = excluded.header, paths = excluded.paths, joined = excluded.joined,
			state = excluded.state, last_error = excluded.last_error, via = excluded.via`,
		r.Project.ID, r.Project.Slug, r.Authority, r.URL, r.Fingerprint, r.Machine, r.Identity, string(capJS), r.ScopeVersion,
		header, string(paths), store.Millis(r.Joined), r.State, r.LastError, via)
	return err
}

// Remove forgets a remote: its row, its token file, its client, and
// (Hooks.Removed) what the layer above kept for it.
func (s *Service) Remove(ctx context.Context, x *Remote) []string {
	x.RetireRole() // fail closed, including a failed delete below
	id := x.Snapshot().Project.ID
	var removed []string
	for _, p := range []string{s.TokenPath(id), s.headersPath(id)} {
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		}
	}
	_, _ = s.d.Store.Registry().ExecContext(ctx, `DELETE FROM team_remotes WHERE project = ?`, id)
	s.mu.Lock()
	delete(s.remotes, id)
	s.n.Store(int32(len(s.remotes)))
	s.mu.Unlock()
	s.unwatch(x)
	x.HC.CloseIdleConnections()
	if s.Hooks.Removed != nil {
		removed = append(removed, s.Hooks.Removed(x)...)
	}
	return removed
}

// WriteFileAtomic writes b to path (0600) as atomicfile.WritePrivate does,
// making its directory (0700) first: the remotes directory of a first join.
func WriteFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.WritePrivate(path, b)
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func jsonH(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

func removeFile(p string) error {
	err := os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
