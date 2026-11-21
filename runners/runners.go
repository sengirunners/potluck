// Package runners is the GitHub runner broker (SPEC 20 "GitHub runner
// broker"): on an authority, GitHub Actions jobs for linked installations of
// one GitHub App run in a pool project's boxes, each box starting a
// just-in-time runner the broker minted (SPEC 20 "A GitHub runner is a box").
//
// Nothing about a host or a team is built in: the App, the pool project and
// its sizes are config (config.json "runners"). Boxes are acquired through
// the run-target routes, in-process, as identity zm-runners with a member
// token capped to the linked area, so the run-target module's authz, budget
// and ledger apply unchanged. Registration, deregistration and the orphan
// sweep run here, never on a host.
//
// Brick: runners. Owned by zm-compute-mac (zm-eakh.12.7.3).
package runners

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Identity is the broker's zm identity: boxes are acquired as it.
const Identity = "zm-runners"

const schema = `
CREATE TABLE runners_links (
	installation INTEGER PRIMARY KEY,
	account      TEXT NOT NULL,
	account_type TEXT NOT NULL,
	project      TEXT NOT NULL,
	area         TEXT NOT NULL,
	linked_by    TEXT NOT NULL,
	token_id     TEXT NOT NULL,
	created      INTEGER NOT NULL
);
CREATE TABLE runners_pending (
	state        TEXT PRIMARY KEY,
	kind         TEXT NOT NULL,
	installation INTEGER NOT NULL DEFAULT 0,
	area         TEXT NOT NULL DEFAULT '',
	org          TEXT NOT NULL DEFAULT '',
	replaces     TEXT,
	by           TEXT NOT NULL,
	expires      INTEGER NOT NULL
);
CREATE TABLE runners_inbox (
	delivery TEXT PRIMARY KEY,
	event    TEXT NOT NULL,
	body     BLOB NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_at  INTEGER NOT NULL,
	done     INTEGER NOT NULL DEFAULT 0,
	received INTEGER NOT NULL
);
CREATE INDEX runners_inbox_due ON runners_inbox (done, next_at);
CREATE TABLE runners_jobs (
	job          INTEGER PRIMARY KEY,
	installation INTEGER NOT NULL,
	repo         TEXT NOT NULL,
	run          INTEGER NOT NULL,
	labels       TEXT NOT NULL,
	state        TEXT NOT NULL,
	why          TEXT NOT NULL DEFAULT '',
	target       TEXT NOT NULL DEFAULT '',
	handle       TEXT NOT NULL DEFAULT '',
	runner       TEXT NOT NULL DEFAULT '',
	created      INTEGER NOT NULL,
	updated      INTEGER NOT NULL
);
CREATE INDEX runners_jobs_state ON runners_jobs (state);`

var baseMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "runners/0001_init", Script: schema},
	// lane: queued deliveries (a serve waits for a box) apart from those that
	// end boxes, so those never wait behind a serve.
	{Kind: store.KindRegistry, Name: "runners/0002_lanes", Script: `ALTER TABLE runners_inbox ADD COLUMN lane INTEGER NOT NULL DEFAULT 0;
CREATE INDEX runners_inbox_lane ON runners_inbox (lane, done, next_at);`},
	// name: an App row's name (it rode in area before, which an App row now
	// uses for its first link's area).
	{Kind: store.KindRegistry, Name: "runners/0003_pending_name", Script: `ALTER TABLE runners_pending ADD COLUMN name TEXT NOT NULL DEFAULT '';`},
	// Placement and feedback (zm-eakh.12.23.12): fails counts a delivery's
	// failures in a row (a serve waiting for a box is not one); a job's runner
	// count (mints) names its runners, reserves counts its serves after a
	// takeover or a lost box, started marks a runner a job took, trusted and
	// checked spare GitHub calls on a waiting serve's retries.
	{Kind: store.KindRegistry, Name: "runners/0004_placement", Script: `ALTER TABLE runners_inbox ADD COLUMN fails INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN mints INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN reserves INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN started INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN trusted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN checked INTEGER NOT NULL DEFAULT 0;
UPDATE runners_jobs SET mints = CASE WHEN runner GLOB 'zm-*-*' THEN CAST(substr(runner, instr(substr(runner, 4), '-') + 4) AS INTEGER)
	WHEN runner != '' THEN 1 ELSE 0 END;
UPDATE runners_jobs SET reserves = max(mints - 1, 0);
UPDATE runners_jobs SET started = 1 WHERE state = 'serving';
CREATE INDEX runners_jobs_updated ON runners_jobs (updated);`},
	// A box lost while its job ran (zm-eakh.12.43): runs is the job a served
	// job's runner went to (GitHub gives a runner to any job of the repository
	// with its labels, so it is not always the job it was minted for);
	// runners_lost is one job the broker saw lose its box while GitHub still
	// had it running, to be re-run once GitHub ended it and its run.
	{Kind: store.KindRegistry, Name: "runners/0005_lost", Script: `ALTER TABLE runners_jobs ADD COLUMN runs INTEGER NOT NULL DEFAULT 0;
CREATE TABLE runners_lost (
	job          INTEGER PRIMARY KEY,
	installation INTEGER NOT NULL,
	repo         TEXT NOT NULL,
	run          INTEGER NOT NULL,
	attempt      INTEGER NOT NULL DEFAULT 0,
	state        TEXT NOT NULL,
	why          TEXT NOT NULL,
	updated      INTEGER NOT NULL
);
CREATE INDEX runners_lost_run ON runners_lost (run, state);`},
	// A served job keeps the time limit it was served with (zm-eakh.12.54.11):
	// ttl is the lease its box was acquired with (job_ttl_s then), which its
	// renewals carry whatever job_ttl_s is now; leased is when (just before
	// the acquire), from which its host's lifetime cap counts.
	{Kind: store.KindRegistry, Name: "runners/0006_ttl", Script: `ALTER TABLE runners_jobs ADD COLUMN ttl INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN leased INTEGER NOT NULL DEFAULT 0;`},
	{Kind: store.KindRegistry, Name: "runners/0007_runtime", Script: `ALTER TABLE runners_jobs ADD COLUMN limit_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN limit_source TEXT NOT NULL DEFAULT '';
ALTER TABLE runners_jobs ADD COLUMN started_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN deadline INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners_jobs ADD COLUMN stop_cause TEXT NOT NULL DEFAULT '' CHECK (stop_cause IN ('','broker_timeout','github_timeout'));`},
	// open_guard: the job's runner was minted with a jit_guard of the
	// repository alone (guard.go). A runner minted before the column counts
	// as open, the safe direction: every job that starts on it is decided
	// again, and it is retired when fork serving stops.
	{Kind: store.KindRegistry, Name: "runners/0010_open_guard", Script: `ALTER TABLE runners_jobs ADD COLUMN open_guard INTEGER NOT NULL DEFAULT 1;`},
}

// Job states.
const (
	jobIgnored     = "ignored" // not ours to serve; why says so
	jobRunning     = "running" // a trusted job observed on another job's runner, with no box of its own
	jobServing     = "serving" // a box holds its runner
	jobDone        = "done"    // completed: box released, runner deleted
	jobFailed      = "failed"  // given up on; why says so (a redelivery of queued serves it again)
	jobWaiting     = "waiting" // to be served, retried while GitHub has it queued; why says what it waits for
	jobTaken       = "taken"   // its runner was taken, or its box failed before the job started: box released, to be served again (counts against maxServes)
	jobLost        = "lost"    // its box was taken back or lost while it ran: why says whether it is re-run (lost.go)
	pendingLink    = "link"    // runners_pending.kind: waiting for GitHub's sign-in
	pendingConfirm = "confirm" // signed in and proved; waiting for the person's yes
	pendingApp     = "app"
	pendingInstall = "install" // an install URL's state: GitHub's setup_url return starts a link with it
	installTTL     = time.Hour
	maxPayload     = 5 << 20 // SPEC 20: a delivery at most 5 MiB
	stateTTL       = 10 * time.Minute
	appStateTTL    = 24 * time.Hour
)

// Service is the broker module.
type Service struct {
	d *core.Deps

	// Command is the program a hint, warning or job reason names when it says
	// what to run ("potluck runners allow acme/app"): a program that embeds
	// the broker under another name sets it ("zm"). New sets DefaultCommand;
	// cmd is the one place that spells a command.
	Command string

	// DoctorRead reads GitHub as the machine's owner for the local doctor and
	// setup (doctor_source.go); nil runs gh.
	DoctorRead PersonRead
	Now        func() time.Time
	HTTP       *http.Client
	Secrets    *secrets.Reader
	SweepEvery time.Duration
	Retry      time.Duration // first retry of a delivery that failed; doubles, 6 tries
	OrphanAge  time.Duration // a zm- runner no live job holds is deleted after this
	WaitMax    time.Duration // the longest gap between a waiting serve's tries (default 5 min)
	CheckEvery time.Duration // how often a waiting serve asks GitHub whether the job still waits (default 15 min)
	WatchEvery time.Duration // how often the boxes of jobs not started yet are checked (default 30 s)
	PollEvery  time.Duration // with no public_url, how often GitHub is asked for waiting jobs (check.go; default 15 s)
	CatchUp    time.Duration // with one, how often the check catches up on what webhooks missed (default 5 min)
	PollTick   time.Duration // how often the check looks whether it is due (default 1 s)
	PollBudget int           // counted GitHub calls the check may spend an hour per installation (default 1500)
	Serves     int           // serves at once (each may wait for a box to boot; default 3)
	RefuseCool time.Duration // placement skips a binding this long after it refused a box (default 2 min)
	HostGrace  time.Duration // a served job's host unlisted this long counts as gone (default 5 min)
	// RepoCheckEvery is how often a connected repository's visibility and
	// approval rule for outside contributors are read again (default 1 h).
	RepoCheckEvery time.Duration

	selfCheck doctorState   // the self-check's rotation (doctor_watch.go)
	capacity  capacityStore // hosts' capacity reports (capacity.go)
	mu        sync.Mutex
	cfgMu     sync.RWMutex // guards d.Config.File.Runners against appCreated's write
	linkMu    sync.Mutex   // serializes link changes (doLink)
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	wake      [2]chan struct{} // per lane
	flight    sync.Mutex
	taking    [2]map[string]bool  // per lane: deliveries a worker is applying
	jobsOn    map[int64]bool      // jobs a serve-lane worker is applying a delivery of
	refused   map[string]refusal  // binding -> last box refusal (cooling)
	unlisted  map[int64]time.Time // served job -> since when its host does not list its binding
	renewed   map[int64]time.Time // served job -> when this process last asked to renew its box's lease
	begun     time.Time           // when this process's broker started: no renewal of an earlier one is later
	goneAsk   map[int64]time.Time // served job -> when boxGone may ask GitHub about it again
	app       *ghapp.Client
	appFor    string          // the config the cached client was built from
	public    *publicListener // runners.listen as served now
	lostAsk   map[int64]int   // served job -> times GitHub did not answer boxLost about it
	busy      map[string]bool // runners GitHub listed busy to a retire (listedBusy)
	// targets, when set, stands between the broker and the run-target
	// service (tests: a host that stops listing its bindings).
	targets func(rt.Service) rt.Service

	issuingMu sync.Mutex
	issuing   map[int64]bool // job grant rows this process's issueSecrets holds (jobsecrets.go)

	checksOnce sync.Once
	checks     *checks // the checker's memory (check.go)

	dropsMu sync.Mutex
	drops   map[string]time.Time // why a delivery was dropped -> when that was last logged (accounts.go)
}

// publicListener is the runners.listen socket being served.
type publicListener struct {
	addr string
	stop context.CancelFunc
}

// Migrations are the broker's tables in registry.db (New adds them).
var Migrations = slices.Concat(baseMigrations, repoMigrations, checkMigrations, doctorMigrations, jobSecretMigrations, preselectMigrations)

// New makes the broker; it serves nothing until config.json has runners.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	if link := teamlink.From(d); link != nil {
		link.RegisterRoutes("runners", teamlink.RouteRule{
			Pattern: "GET /v0/runners/pool", Kind: teamlink.KindWrite, Hedge: teamlink.Hedged,
		}, teamlink.RouteRule{
			Pattern: "PUT /v0/runners/capacity/{host}", Kind: teamlink.KindWrite, Hedge: teamlink.Once,
			Why: "it replaces the host's last report and the host publishes again within seconds: no write id to record",
		})
	}
	s := &Service{d: d, Command: DefaultCommand, Now: time.Now, HTTP: &http.Client{Timeout: 15 * time.Second}, Secrets: secrets.Default(),
		SweepEvery: 5 * time.Minute, Retry: 30 * time.Second, OrphanAge: 10 * time.Minute, WaitMax: 5 * time.Minute,
		CheckEvery: 15 * time.Minute, WatchEvery: 30 * time.Second, Serves: 3, RefuseCool: 2 * time.Minute, HostGrace: 5 * time.Minute, RepoCheckEvery: time.Hour,
		refused: map[string]refusal{}, unlisted: map[int64]time.Time{}, renewed: map[int64]time.Time{}, goneAsk: map[int64]time.Time{},
		wake:   [2]chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)},
		taking: [2]map[string]bool{{}, {}}, jobsOn: map[int64]bool{}, lostAsk: map[int64]int{}, busy: map[string]bool{}}
	if d.Projects != nil {
		d.Projects.OnRemove(s.refuseRemove)
	}
	issuers.ProvideJobHolds(d, s.jobHolds) // job grants: a job's credential is renewed only while it holds its box
	issuers.ProvideJobBox(d, s.jobBox)     // a potluck-token rule of another project serves a job only in its box here
	return s
}

func (s *Service) now() time.Time { return s.Now().UTC() }
func (s *Service) db() *sql.DB    { return s.d.Store.Registry() }

// conf is config.json's runners with defaults.
func (s *Service) conf() config.Runners {
	s.cfgMu.RLock()
	c := s.d.Config.File.Runners
	c.Pool.Sizes = maps.Clone(c.Pool.Sizes)
	c.Accounts = slices.Clone(c.Accounts)
	s.cfgMu.RUnlock()
	if c.Label == "" {
		c.Label = c.Pool.Project // a pool's jobs ask for it by name: runs-on: <pool>
	}
	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = "https://api.github.com"
	}
	if c.GitHub.WebURL == "" {
		c.GitHub.WebURL = "https://github.com"
	}
	c.GitHub.APIURL = strings.TrimRight(c.GitHub.APIURL, "/")
	c.GitHub.WebURL = strings.TrimRight(c.GitHub.WebURL, "/")
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	return c
}

// secret reads a secret reference from config.
func (s *Service) secret(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", s.off()
	}
	r, err := secrets.Parse(ref)
	if err != nil {
		return "", err
	}
	v, err := s.Secrets.Get(ctx, r)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v.Reveal()), nil
}

// client is the App's API client, rebuilt when its config changes.
func (s *Service) client(ctx context.Context) (*ghapp.Client, error) {
	c := s.conf()
	if c.GitHub.AppID == 0 {
		return nil, s.off()
	}
	key := c.GitHub.APIURL + "|" + c.GitHub.PrivateKey + "|" + itoa(c.GitHub.AppID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app != nil && s.appFor == key {
		return s.app, nil
	}
	pem, err := s.secret(ctx, c.GitHub.PrivateKey)
	if err != nil {
		return nil, err
	}
	cl, err := ghapp.New(ghapp.Config{AppID: c.GitHub.AppID, PrivateKeyPEM: []byte(pem), APIURL: c.GitHub.APIURL, HTTP: s.HTTP, Now: s.Now})
	if err != nil {
		return nil, err
	}
	s.app, s.appFor = cl, key
	return cl, nil
}

// pool resolves the pool project from config.
func (s *Service) pool() (api.ProjectRef, error) {
	c := s.conf()
	if c.Pool.Project == "" {
		return api.ProjectRef{}, s.off()
	}
	p, ok := s.d.Projects.Lookup(c.Pool.Project)
	if !ok {
		return api.ProjectRef{}, api.Errorf(api.CodeUnavailable, "the runner pool project %q is not on this machine", c.Pool.Project)
	}
	return p, nil
}

// Routes: SPEC 20 "GitHub runner broker".
func (s *Service) Routes(r *core.Router) {
	r.HandleFunc("GET /v0/runners/doctor", core.Local, "local owner: read-only repository diagnostics using the owner's gh sign-in", s.handle(s.doctorHTTP))
	r = r.For("runners")
	r.HandleFunc("POST /v0/runners/hook", core.Public, "the App's webhook secret signs the body (X-Hub-Signature-256); answers nothing scoped", s.hook)
	r.HandleFunc("POST /v0/runners/links", core.Authed, "admin of the area in the runner pool project", s.handle(s.linkStart))
	r.HandleFunc("GET /v0/runners/links", core.Authed, "links in the caller's readable areas of the pool project", s.handle(s.links))
	r.HandleFunc("DELETE /v0/runners/links/{installation}", core.Authed, "admin of the link's area", s.handle(s.unlink))
	r.HandleFunc("GET /v0/runners/jobs", core.Authed, "jobs of links in the caller's readable areas of the pool project", s.handle(s.jobs))
	r.HandleFunc("GET /v0/runners/oauth", core.Public, "a single-use state minted by POST /v0/runners/links or GET /v0/runners/app/installed; answers a confirmation page, nothing scoped; without one, an install return (installation_id or setup_action) is GET /v0/runners/app/installed", getOnly(s.oauth))
	r.HandleFunc("POST /v0/runners/oauth", core.Public, "a single-use state from the confirmation page (the signed-in person's yes); answers nothing scoped", s.confirm)
	r.HandleFunc("POST /v0/runners/app", core.Authed, "owner of the runner pool project", s.handle(s.appStart))
	r.HandleFunc("GET /v0/runners/app/new", core.Public, "a single-use state minted by POST /v0/runners/app", getOnly(s.appPage))
	r.HandleFunc("GET /v0/runners/app/created", core.Public, "a single-use state minted by POST /v0/runners/app; answers nothing scoped", getOnly(s.appCreated))
	r.HandleFunc("GET /v0/runners/app/installed", core.Public, "a single-use install state (POST /v0/runners/links without installation_id, or the App's creation); GitHub's installation_id is only a claim: it goes on to /v0/runners/oauth's sign-in and admin check; answers a redirect to GitHub or a page, nothing scoped", getOnly(s.installed))
	r.HandleFunc("POST /v0/runners/setup", core.Local, "local: the machine's owner, also owner of the pool project; writes config.json runners (pool, public_url, listen), opens the listener live, answers the App's creation or install URL", s.handle(s.setup))
	r.HandleFunc("POST /v0/runners/settings", core.Local, "local: the machine's owner, also owner of the pool project; writes one of config.json runners label, job_ttl_s, served from the next job on; answers the setup", s.handle(s.set))
	r.HandleFunc("GET /v0/runners/pool", core.Authed, "readers of the pool project; placement inputs for RunsOn (no App or listener details)", s.handle(s.poolStatus))
	r.HandleFunc("POST /v0/runners/repos", core.Local, "local: the machine's owner, also owner of the pool project; connects a repository of a linked installation (a public one with its owner's yes); answers its record", s.handle(s.allow))
	r.HandleFunc("DELETE /v0/runners/repos/{owner}/{name}", core.Local, "local: the machine's owner, also owner of the pool project; disconnects a repository", s.handle(s.disallow))
	r.HandleFunc("GET /v0/runners/repos", core.Authed, "connected repositories of links in the caller's readable areas of the pool project", s.handle(s.repos))
	r.HandleFunc("PUT /v0/runners/capacity/{host}", core.Authed, "the named host's own machine (its node-bound token, a reader of the pool project) or the authority's owner on its socket; replaces the host's capacity report", s.handle(s.capacityPut))
	r.HandleFunc("GET /v0/runners/capacity/{host}", core.Public, "none: a host's capacity report as it published it, for routers on cloud runners; nothing scoped", s.capacityGet)
	r.HandleFunc("GET /v0/runners/setup", core.Local, "local: the machine's owner; the broker's config and listener (secret references only, never values)", s.handle(s.setupStatus))
}

// getOnly refuses HEAD on a page that consumes a one-time state (a link
// preview's HEAD would burn it); Go's mux routes HEAD to GET patterns.
func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func (s *Service) handle(f func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := f(r)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

// Start runs the delivery workers (Serves in the serve lane, one for ends),
// the sweep, the box watch and the checking of GitHub for waiting jobs.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s.begun = s.now()
	for _, n := range s.notices() {
		s.logf("runners: %s", n)
	}
	if addr := s.conf().Listen; addr != "" {
		// A taken or bad address leaves the broker running without its
		// public listener: zm runners setup then picks a free one.
		if err := s.listen(ctx, addr); err != nil {
			s.logf("runners: listener not opened (%s picks a free port): %v", s.cmd("runners setup"), err)
		}
	}
	if !s.conf().AllowForkPRs { // read at start: switched off by hand while the daemon was down
		s.retireOpen(ctx, "", retireOff)
	}
	s.wg.Add(2)
	go s.checkLoop(ctx)
	go s.awakeLoop(ctx)

	s.ctx, s.cancel = ctx, cancel
	n := min(max(s.Serves, 1), 16)
	s.wg.Add(n + 4)
	for range n {
		go s.work(s.ctx, laneServe)
	}
	go s.work(s.ctx, laneEnds)
	go s.sweeper(s.ctx)
	go s.watcher(s.ctx)
	go s.doctorWatch(s.ctx)
	return nil
}

// PublicHandler serves only the broker's public routes (SPEC 20: the
// webhook and the browser returns), for runners.listen: whatever proxy
// fronts public_url reaches these and nothing else of the daemon.
func (s *Service) PublicHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /v0/runners/hook", s.hook)
	m.HandleFunc("GET /v0/runners/oauth", getOnly(s.oauth))
	m.HandleFunc("POST /v0/runners/oauth", s.confirm)
	m.HandleFunc("GET /v0/runners/app/new", getOnly(s.appPage))
	m.HandleFunc("GET /v0/runners/app/created", getOnly(s.appCreated))
	m.HandleFunc("GET /v0/runners/app/installed", getOnly(s.installed))
	m.HandleFunc("GET /v0/runners/capacity/{host}", s.capacityGet)
	return m
}

// listen opens runners.listen: loopback only, the public routes only.
// Callers hold s.mu.
func (s *Service) listen(ctx context.Context, addr string) error {
	ln, err := openListen(addr)
	if err != nil {
		return err
	}
	s.servePublic(ctx, ln)
	return nil
}

// openListen opens a loopback socket for runners.listen.
func openListen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("runners.listen %q: a loopback address and port (127.0.0.1:PORT), fronted by public_url's proxy", addr)
	}
	return net.Listen("tcp", addr)
}

// servePublic serves the public routes on ln until ctx ends or it is replaced,
// as s.public. Callers hold s.mu.
func (s *Service) servePublic(ctx context.Context, ln net.Listener) {
	ctx, stop := context.WithCancel(ctx)
	srv := &http.Server{Handler: http.MaxBytesHandler(s.PublicHandler(), maxPayload+4096), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
	s.wg.Add(2)
	go func() { defer s.wg.Done(); _ = srv.Serve(ln) }()
	go func() { defer s.wg.Done(); <-ctx.Done(); _ = srv.Close() }()
	if s.public != nil {
		s.public.stop()
	}
	s.public = &publicListener{addr: ln.Addr().String(), stop: stop}
}

func (s *Service) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	s.revokeTokens()
	return nil
}

// revokeTokens ends the broker's cached installation tokens at GitHub, so
// none outlives the daemon by up to an hour. A failure is logged; the
// tokens lapse at their expiry.
func (s *Service) revokeTokens() {
	s.mu.Lock()
	app := s.app
	s.mu.Unlock()
	if app == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if n, err := app.RevokeCached(ctx); err != nil {
		s.logf("runners: revoked %d installation token(s) at stop; the rest lapse within the hour: %v", n, err)
	}
}

func (s *Service) kick(lane int) {
	select {
	case s.wake[lane] <- struct{}{}:
	default:
	}
}

// newState is a single-use state token.
func newState() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// takeState consumes a pending state of kind: it answers the row once.
func (s *Service) takeState(ctx context.Context, state, kind string) (pending, error) {
	var p pending
	if len(state) != 48 {
		return p, errBadState
	}
	var replaces sql.NullString
	var pre string
	err := s.db().QueryRowContext(ctx, `DELETE FROM runners_pending WHERE state = ? AND kind = ? AND expires > ?
		RETURNING installation, area, org, replaces, by, name, preselect`, state, kind, s.now().Unix()).Scan(&p.installation, &p.area, &p.org, &replaces, &p.by, &p.name, &pre)
	p.preselect = decodePreselect(pre)
	p.replaces, p.mayReplace = replaces.String, replaces.Valid
	if kind == pendingApp && p.name == "" { // a row from before 0003: the name rode in area
		p.name, p.area = p.area, ""
	}
	if errors.Is(err, sql.ErrNoRows) {
		return p, errBadState
	}
	return p, err
}

type pending struct {
	installation int64
	area, org    string
	name         string // an App row's App name
	by           string
	replaces     string // the area of an existing link this one may replace
	mayReplace   bool
	preselect    *Preselect // an App row's: the repositories its install URL pre-selects
}

var errBadState = api.Errorf(api.CodeNotFound, "this link expired or was used; start again")

func writeSecret(path, v string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(v+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
