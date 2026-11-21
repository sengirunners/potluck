// Package core is what every module gets from the daemon: config, the store,
// the project registry, the event bus, a logger, the authenticated caller,
// project resolution, a guarded router, and narrow interfaces to the other
// modules. Owned by the integrator.
//
// Modules never import each other. The base asks the modules built on it
// through hooks they register (hooks.go: health parts, notifiers, session
// hooks) and the service registry (services.go); the server wires them
// before any request is served. Prefer events (d.Events.Subscribe) for
// reactions and these for synchronous questions.
//
// Deps fields added since 2026-11-01, with their owners
// (older fields are not on it yet):
//
//	tr        team transports (transport.go): drivers registered by
//	          wiring.NewKit (wiring: tailnet, tunnel); the zm-tunnel lane owns the seam
//	schema    integrator (schema.go): migration lists the store does not
//	          register (issues.db), named in the build manifest
//	Zm        integrator (kit/server: the release channel state in
//	          machine numbers, SPEC 2 "Release channel"; zm-8ww5)
//	services  integrator (services.go): Provide/Service, a registry of
//	          services a module publishes by name (zm-eakh.12.35 G4; run
//	          targets publish "runtarget", reached through runtarget/rt)
//	holders   integrator (services.go): AddHolderCheck/HolderLive, whether
//	          an address holding something is live (the session module
//	          answers for sessions; run targets renew boxes it holds)
//
// Every service method that returns data to a caller takes the caller's
// *authz.Subject (never optional). Daemon-internal calls pass
// authz.System(machine). Out-of-scope objects are api.NotFound, identical to
// missing ones (see authz.Subject.Check).
package core

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// Deps is shared by every module. Fields are set once by the server before
// serving and never reassigned; read them at call time, not in constructors.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Projects *project.Registry
	Events   *events.Bus
	Log      *slog.Logger
	Started  time.Time
	// Parked: this daemon's home does not serve (kit/hometerm), so Store,
	// Projects and Events are nil and only its listeners run (kit/server
	// term.go). Set by server.New alone.
	Parked bool
	// Closing is closed when the daemon starts shutting down: long streams end.
	Closing <-chan struct{}

	// Handler is the daemon's full API handler (with middleware). The tailnet
	// module serves it on its listener, with WithRemote (WhoIs) on each
	// request. Requests reaching it are remote unless marked WithLocal, which
	// only the Unix listener does: never call WithLocal from a module. On a
	// serving daemon that named a successor it holds every call but health
	// and the term until its first ask of the successor has its answer,
	// which comes after every module's Start: a Start must not wait on it.
	Handler http.Handler

	// Net is the tailnet module (teams dial their authority through it);
	// Team is the team module's proxy in front of the router on a replica.
	// Either may be nil.
	Net  NetService
	Team TeamProxy
	// Zm reports this machine's zm build and release channel state for its
	// machine numbers (SPEC 2 "Release channel"); the daemon sets it, and it
	// may be nil (a module's tests).
	Zm func() *api.MachineZm
	// UpdateJob is the update job this daemon can start on request, or nil
	// when it knows none; the daemon calls it each time it builds its
	// machine numbers (MachineZm.Job). The program sets it; nil reports no
	// job.
	UpdateJob func() *api.UpdateJob
	// GitHubLogin is the GitHub login an identity (by ID) is linked to, or
	// "" (the owners brick sets it; nil in a module's tests).
	GitHubLogin func(ctx context.Context, identity string) string
	// LinkedAccount is the verified provider account an identity (by ID) is
	// linked to ("github:<numeric id>") and its login, or "", "" (the owners
	// brick sets it; nil in a module's tests).
	LinkedAccount func(ctx context.Context, identity string) (account, login string)
	// KeyLogin binds signing keys through GitHub logins of purpose "key"
	// (keylogin.go; the approvals brick sets it; nil in a module's tests).
	KeyLogin KeyLogin
	// Aliases is an identity's exact inbox aliases here, sorted (the
	// approvals brick sets it; nil in a module's tests).
	Aliases func(ctx context.Context, identity string) []string

	hosts  hostNames  // OwnTailnetHost's cache of the node's MagicDNS names
	tr     transports // team transport drivers (transport.go)
	term   termLock   // ApplyTerm's (term.go)
	schema schemas    // AddSchema (schema.go)
	hooks  hooks      // health parts, notifiers, session hooks (hooks.go)

	svcMu    sync.Mutex     // guards services and holders
	services map[string]any // Provide/Service (services.go)
	holders  []HolderCheck  // AddHolderCheck (services.go)

	scrubMu   sync.Mutex
	scrubbers []Scrubber
	// backupScrubbers blank secrets in a staged registry copy (scrub.go).
	backupScrubbers []BackupScrubber
}

// NetService is the tailnet module's in-process API (SPEC 17.4): a replica
// dials its authority through its own node, never the host's Tailscale, and
// checks who answered.
type NetService interface {
	// Dial connects to addr (e.g. "zm-mini:7433") through this daemon's node.
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	// Peer identifies the tailnet node at remoteAddr (one this node dialled
	// or accepted): its stable node ID, key, login and tags.
	Peer(ctx context.Context, remoteAddr string) (Remote, error)
	// Self describes this daemon's node.
	Self(ctx context.Context) (NetSelf, error)
	// Up starts the node like zm tailnet up (saving config) and waits up to
	// wait for it to serve or ask for a browser login.
	Up(ctx context.Context, authKey string, wait time.Duration) (api.TailnetStatus, error)
}

// NetSelf is this daemon's tailnet node.
type NetSelf struct {
	Running bool
	// Enabled: this run joins the tailnet (it came up from config or
	// --tailnet, or by tailnet up; not since tailnet down), whether or not
	// the node runs yet. A restart that keeps the daemon's listeners passes
	// --tailnet when it is set.
	Enabled  bool
	NodeID   string // stable node ID ("" until logged in)
	Hostname string // the hostname it asked for (zm-<machine>)
	DNSName  string // the name the tailnet gave it (a taken name gets a suffix)
	HTTPS    bool   // serving HTTPS on :443 at DNSName (a Tailscale certificate)
	AuthURL  string // waiting for a browser login
	Peers    []string
	// PeerList is every peer: its self-reported host name, its full
	// MagicDNS name (zm-mini.example.ts.net: a node shared in from
	// another tailnet resolves only by it) and its stable node ID. Two
	// peers may report the same host name; the node ID tells them apart.
	PeerList []NetPeer
}

// NetPeer is one tailnet peer as this node sees it.
type NetPeer struct {
	Host    string // its own host name (self-reported: not unique)
	DNSName string // its full MagicDNS name, without the trailing dot (unique)
	NodeID  string // its stable node ID
	NodeKey string // its node public key ("nodekey:..."), as WhoIs reports it
	Login   string // its owner's login ("" for a tagged node)
	OS      string // its self-reported OS
	Tagged  bool   // a tagged node (server, CI)
	// Relays is true when the node may carry other hosts' traffic: it
	// serves subnet routes or is an exit node.
	Relays bool
	// Shared is true for a node shared in from another tailnet.
	Shared bool
}

// HedgeReporter is the team module's hedged-read counters (SPEC 17.3), for
// GET /v0/health on the socket.
type HedgeReporter interface {
	HedgeStats() api.HedgeStats
}

// JoinedLister is the team module's list of the team projects this machine
// joined (their slugs), for the swap manifest (SPEC 2, "Swaps").
type JoinedLister interface {
	JoinedSlugs() []string
}

// TeamProxy is the hook in front of the router on a replica (SPEC 17.3,
// round B; teamlink's proxy, d.Team): a joined team project's calls from
// this machine's socket go to its authority.
type TeamProxy interface {
	// Proxy serves r (forwarding it to an authority, or merging the
	// authority's answer with next's for a list) and reports true, or
	// reports false and leaves r to next (the router).
	Proxy(w http.ResponseWriter, r *http.Request, next http.Handler) bool
}

// Team round B2 (SPEC 17.3, cross-machine mail and presence). Optional
// interfaces next to the modules' services, so fakes need no change:

// MachinePresence is implemented by the session module (owner): this
// machine's numbers for a heartbeat (MachineStats, sampled at most every
// 30 s), and an authority's record of a replica's (RecordMachine, from
// its report for project; teamlink calls it, and keeps them in memory when
// no module implements this). SPEC 8 "Machine numbers".
type MachinePresence interface {
	MachineStats(ctx context.Context) api.MachineStats
	RecordMachine(ctx context.Context, project, machine string, st api.MachineStats) error
	// ForgetMachine drops a machine's numbers for a project (it reports as
	// an area guest now: what it sent as a member must not linger).
	ForgetMachine(ctx context.Context, project, machine string) error
	// MachineRows are every recorded (machine, project) report, newest
	// first per machine (GET /v0/machines filters them per caller).
	MachineRows(ctx context.Context) ([]MachineSeen, error)
}

// MachineSeen is one machine's last report for one project.
type MachineSeen struct {
	Machine, Project string
	Stats            api.MachineStats
	Seen             time.Time
}

// ReapPresence asks the session module to end presence rows whose replica
// left (after a node-bound token was revoked). No-op without one.
func (d *Deps) ReapPresence(ctx context.Context) {
	if h := d.SessionHooks(); h != nil && h.Reap != nil {
		h.Reap(context.WithoutCancel(ctx))
	}
}

// TeamReplicas is implemented by the team module on a replica (d.Team).
// Replicates reports whether this machine holds a joined (replica) copy of
// the project with this authority ULID, in any state: its authority is
// elsewhere, so nothing here may act for it with a local credential. No
// network.
type TeamReplicas interface {
	Replicates(project string) bool
}

// TeamRepos is implemented by the team module on a replica (d.Team). It
// touches no network: each joined project's repository rules as its
// authority's last answer sent them (the Zm-Team header, already narrowed
// to what this member sees), so a read that only needs a project's mapping
// (GET /v0/github/repos) answers here, and GitHub is read with this
// machine's own login, as for a local project.
type TeamRepos interface {
	JoinedRepos() []JoinedRepos
}

// JoinedRepos is one joined project's repository rules: Repos the
// project's own (whole-project members only), AreaRepos each visible
// area's.
type JoinedRepos struct {
	Project   api.ProjectRef
	Repos     api.RepoRules
	AreaRepos []api.NamedRepos
}

// WriteID is the replica's id for the write r carries (Zm-Write-Id), read
// only from a node-bound token's call and only when it is a ULID: the work
// and mail modules store a comment or mail under it, so a replayed write
// never duplicates (SPEC 17.6). "" otherwise.
func WriteID(r *http.Request) string {
	id := strings.TrimSpace(api.GetHeader(r.Header, api.HeaderWriteID))
	if id == "" || TeamSubjectOf(r.Context()) == nil || !ids.Valid(id) {
		return ""
	}
	return strings.ToUpper(id)
}

// BaseOf is a team replica's base for a field write (Zm-Base: a change id
// it had seen for the item), read only from a node-bound token's call and
// only when it is a ULID. The work module refuses the write when the field
// changed after it, in the transaction that would change it (SPEC 17.6).
func BaseOf(r *http.Request) string {
	b := strings.TrimSpace(api.GetHeader(r.Header, api.HeaderBase))
	if b == "" || TeamSubjectOf(r.Context()) == nil || !ids.Valid(b) {
		return ""
	}
	return strings.ToUpper(b)
}

type statusSinkKey struct{}

// WithStatusSink returns ctx asking the work module to note in *p the id of
// any status_changed event a write it carries mints (the team write path:
// a later queued status write of the same item takes it as seen).
func WithStatusSink(ctx context.Context, p *string) context.Context {
	return context.WithValue(ctx, statusSinkKey{}, p)
}

// NoteStatusEvent records id in ctx's status sink, if any.
func NoteStatusEvent(ctx context.Context, id string) {
	if p, ok := ctx.Value(statusSinkKey{}).(*string); ok && p != nil && id > *p {
		*p = id
	}
}

// System is the daemon acting on its own behalf.
func (d *Deps) System() *authz.Subject { return authz.System(d.Config.Machine) }

// SubscriptionSubject rebuilds a push subscriber's current scope without
// letting another brick read the registry's tokens table.
func (d *Deps) SubscriptionSubject(ctx context.Context, tokenID string) (*authz.Subject, error) {
	return d.Projects.SubscriptionSubject(ctx, tokenID)
}

// Module is what every module's New returns.
type Module interface {
	Routes(r *Router)
}

// Starter is implemented by modules with background work (lease ticker, PTY
// reapers, tailnet). Start is called once after the socket listens; it must
// not block. The context ends at shutdown.
type Starter interface {
	Start(ctx context.Context) error
}

// Closer is implemented by modules that hold resources (PTYs, tsnet).
// Close is called at shutdown, in reverse wiring order.
type Closer interface {
	Close() error
}

// MachineClasses is implemented by the team module: at a team project's
// authority, a machine's operator class there (SPEC 8 "Operator class"),
// decided by the authority, never by the machine's own report. Anything that
// places work on a project's machines (a sandbox pool, a run target's host)
// reads it here: secrets and credentialed work go only to "trusted".
type MachineClasses interface {
	MachineClass(ctx context.Context, project, machine string) (api.MachineClass, error)
}

// Visible reports whether a reader with scope sc may see address. Session
// addresses ask the session module; other addresses are visible.
func (d *Deps) Visible(ctx context.Context, sc authz.Scope, address string) bool {
	if sc.IsAll() || address == "" {
		return true
	}
	a, err := api.ParseAddress(address)
	if err != nil {
		return false
	}
	if a.Kind != api.KindSession {
		return true
	}
	if h := d.SessionHooks(); h != nil && h.Visible != nil && h.Visible(ctx, sc, a.String()) {
		return true
	}
	if a.Machine == d.Config.Machine || d.Projects == nil {
		return false
	}
	// A session on a team replica (SPEC 17.3) with no presence row yet (the
	// session module answers for those with one, by their area): whole-
	// project readers of a project its machine joined see it; area-only
	// readers never do.
	for _, p := range d.Projects.MachineProjects(ctx, a.Machine) {
		if sc.WholeProject(p) {
			return true
		}
	}
	return false
}

// RedactFor is Redact that also shows a caller its own address.
func (d *Deps) RedactFor(ctx context.Context, sub *authz.Subject, address string) string {
	if sub != nil && address != "" && strings.EqualFold(address, sub.Address) {
		return address
	}
	return d.Redact(ctx, sub, address)
}

// Redact returns address, or "someone" when sub may not see it. Use it on
// every holder / acked_by / owner / via field returned to a caller.
func (d *Deps) Redact(ctx context.Context, sub *authz.Subject, address string) string {
	if d.Visible(ctx, sub.Scope, address) {
		return address
	}
	return events.Someone
}
