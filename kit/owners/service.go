package owners

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/kit/config"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

var linkMigrations = []store.Migration{
	// owner_links maps a zm identity to its provider account. It holds no
	// token: none is ever stored. 0005 lets one account own several
	// identities (one person, many names: zm-mpb3r.10).
	{Kind: store.KindRegistry, Name: "owners/0001_links", Script: `
CREATE TABLE owner_links (
	identity TEXT PRIMARY KEY,
	account  TEXT NOT NULL UNIQUE,
	login    TEXT NOT NULL,
	linked   INTEGER NOT NULL,
	seen     INTEGER NOT NULL
);`},
	// Every link is also a fact in project's ceiling_accounts (linkTx), so an
	// owner's removal of one identity reaches the others the same account
	// is or was (project.RememberAccount). This fills it for links made
	// before linkTx recorded them; it is named for the table it fills so it
	// runs after project/0006 creates it.
	{Kind: store.KindRegistry, Name: "project/0007_linked_accounts", Script: `
INSERT INTO ceiling_accounts (subject, account) SELECT identity, account FROM owner_links WHERE identity != '' AND account != ''
	ON CONFLICT DO NOTHING;`},
	// Drops UNIQUE(account): SQLite drops a constraint only by rebuilding
	// the table. Every row comes over as it was.
	{Kind: store.KindRegistry, Name: "owners/0005_many_per_account", Script: `
CREATE TABLE owner_links_new (
	identity TEXT PRIMARY KEY,
	account  TEXT NOT NULL,
	login    TEXT NOT NULL,
	linked   INTEGER NOT NULL,
	seen     INTEGER NOT NULL
);
INSERT INTO owner_links_new (identity, account, login, linked, seen) SELECT identity, account, login, linked, seen FROM owner_links;
DROP TABLE owner_links;
ALTER TABLE owner_links_new RENAME TO owner_links;
CREATE INDEX owner_links_account ON owner_links (account, linked);`},
}

// Service is the owners module: GitHub logins run by this authority, and
// identity links.
type Service struct {
	d   *core.Deps
	acc Accounts
	dev Device // nil: no github.client_id

	// Limits on logins in flight; Collect is how long an outcome waits to
	// be collected once known. Set before serving (tests shorten them).
	MaxLogins   int // remote starts in flight, in all
	MaxLocal    int // the machine owner's own pool (its socket)
	MaxPerOwner int // in flight per identity or node
	MaxStarts   int // starts an hour per identity or node
	// Tunnel callers are unauthenticated (anyone can make a key): their own
	// pool, and limits per client address too.
	MaxTunnel     int // tunnel starts in flight, in all
	MaxPerAddr    int // in flight per client address
	MaxAddrStarts int // starts an hour per client address
	Collect       time.Duration
	// RecheckEvery is how often org admissions are re-checked (default 1
	// hour); DecideFloor is the least time between GitHub's answer and a
	// join's outcome, so how long the authority took tells nothing.
	RecheckEvery time.Duration
	DecideFloor  time.Duration
	// OrgFailGrace is how long org membership checks may fail before new
	// joins through org grants stop (default 24 hours; nobody is revoked).
	OrgFailGrace time.Duration
	// TTL is how long a cached owner (login -> id, an id's login and
	// whether GitHub still answers for it) is trusted before GitHub is
	// asked again (default 1 hour). Clock is the time (tests move it).
	TTL   time.Duration
	Clock func() time.Time
	// NegTTL caches a login GitHub does not know (default 10 minutes);
	// MaxLookups is how many uncached name lookups a caller makes a minute
	// (default 30).
	NegTTL     time.Duration
	MaxLookups int
	// A grant GitHub's rate limit refused is retried by the queue
	// (grantqueue.go): it looks for due ones every QueueEvery (default 30
	// seconds), tries again QueueSlack after the reset GitHub named, after
	// QueueBackoff when GitHub failed some other way (default 5 minutes),
	// and gives up, telling the granter, QueueGiveUp after it was asked
	// (default 24 hours).
	QueueEvery   time.Duration
	QueueSlack   time.Duration
	QueueBackoff time.Duration
	QueueGiveUp  time.Duration

	// orgUntold holds the projects whose failing org checks were only
	// logged (no notifier): logged once until the checks run again.
	orgUntold sync.Map

	actFault    func() error // tests: fail an act before it does anything (export_test.go)
	withinFault func() error // tests: fail a join after its identity is made, before the link

	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	logins       map[string]*login
	starts       map[string][]time.Time // owner (or "addr:<client address>") -> start times in the last hour
	startsPruned time.Time
	keySeq       uint64                 // the last key login's start order (keylogin.go)
	lookups      map[string][]time.Time // caller -> uncached name lookups in the last minute
	queueWake    chan struct{}          // a grant was queued: the worker looks now
	queueRun     sync.Mutex             // one pass over the grant queue at a time
}

// Migrations are the owners module's tables in registry.db (New adds them).
var Migrations = slices.Concat(linkMigrations, grantMigrations, admissionMigrations, nameMigrations, grantQueueMigrations)

// New is the module over acc and dev (GitHub's, in the daemon); dev nil
// turns logins off until github.client_id is set.
func New(d *core.Deps, acc Accounts, dev Device) *Service {
	d.AddMigrations(Migrations...)
	if d.Projects != nil {
		d.Projects.OnRemove(dropRemoved)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{d: d, acc: acc, dev: dev, MaxLogins: 64, MaxLocal: 4, MaxPerOwner: 4, MaxStarts: 20, MaxTunnel: 8, MaxPerAddr: 4, MaxAddrStarts: 20, Collect: 2 * time.Minute,
		RecheckEvery: time.Hour, DecideFloor: 1500 * time.Millisecond, OrgFailGrace: 24 * time.Hour,
		TTL: time.Hour, Clock: time.Now, NegTTL: 10 * time.Minute, MaxLookups: 30,
		QueueEvery: 30 * time.Second, QueueSlack: 2 * time.Second, QueueBackoff: 5 * time.Minute, QueueGiveUp: 24 * time.Hour,
		ctx: ctx, cancel: cancel, logins: map[string]*login{}, starts: map[string][]time.Time{}, lookups: map[string][]time.Time{}, queueWake: make(chan struct{}, 1)}
}

// Close stops every login in flight.
func (s *Service) Close() error {
	s.cancel()
	return nil
}

// Routes registers the owners routes (API.md "Owners").
func (s *Service) Routes(r *core.Router) {
	r = r.For("owners")
	r.HandleFunc("POST /v0/login/github/start", core.Authed, "link: the caller's own human identity, by the socket owner or a replica's node-bound token only (never a minted token or a tailnet login); at most 4 logins in flight per identity; bind: admin on the project, the approver must be the identity's linked account; key: a person's own identity as for link, read on ?project=, and core.Deps.KeyLogin's check (else unknown purpose); invite: a replica's node-bound token from its own node (a key-bound one over the tunnel), a person's identity, the team project the token is capped to; collecting links the identity and admits the node at its GitHub grants, if any", jsonH(s.handleStart))
	r.HandleFunc("GET /v0/owner", core.Authed, "read on the project named by ?project= (else not_found); answers the caller's own owner right (core.Deps.ProjectAdmin) and GitHub link", jsonH(s.handleOwner))
	s.grantRoutes(r)
	s.joinRoutes(r)
	r.HandleFunc("GET /v0/projects/{project}/names", core.Authed, "read on the project", jsonH(s.handleNames))
	r.HandleFunc("GET /v0/names/{owner}/{name}", core.Authed, "read on the project the name reaches, else answered like a missing name", jsonH(s.handleResolve))
	r.HandleFunc("DELETE /v0/identities/{identity}/github", core.Authed, "the machine owner on the socket, the identity's own replica (node-bound token), or an admin of a project it holds a grant in; never a minted token or a tailnet login; anyone else gets the 404 of a missing identity", jsonH(s.handleUnlink))
	r.HandleFunc("POST /v0/login/github/{id}", core.Public, "its own: the login's single-use poll secret, and the identity that started it; one answer (404) for an unknown id, a wrong secret, another caller and a collected outcome", jsonH(s.handlePoll))
}

// jsonH adapts fn to a handler writing 200 + JSON, or the error.
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

// linkable says whether sub may link its identity: only a credential that
// stands for the whole person. A minted token is capped (an agent's, a
// CI's) and must not become the identity's GitHub login, which joins with
// every grant the identity has; a tailnet login is ambient.
func linkable(sub *authz.Subject) error {
	switch {
	case sub == nil:
		return api.Errorf(api.CodeUnauthenticated, "authentication required")
	case sub.Via == authz.ViaLocal && sub.TokenID == "":
		return nil // the machine's owner on its socket (and its sessions)
	case sub.Via == authz.ViaToken && sub.Node != "":
		return nil // a team replica's node-bound token: the member who joined
	}
	return api.Errorf(api.CodeForbidden, "only you can link your identity: run %s on your own machine", config.Command("login github")).
		WithHint("a minted token or a tailnet login cannot link an identity to GitHub")
}

// link records acc as identity's account. Linking is explicit and once: an
// identity linked to another account is a conflict; the same pair again
// refreshes the login. One account may own several identities (the same
// person under several names); a new one is told to the identities the
// account already owns, so a link they did not make is seen.
func (s *Service) link(ctx context.Context, identity api.Identity, acc Account, from string) (api.GitHubIdentity, error) {
	var out api.GitHubIdentity
	var first bool
	var others []api.Identity
	err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var err error
		if out, first, err = s.linkTx(ctx, tx, identity, acc); err != nil || !first {
			return err
		}
		others, err = linkedOthers(ctx, tx, acc.ID, identity.ID)
		return err
	})
	if err != nil {
		return api.GitHubIdentity{}, err
	}
	if first {
		s.tellLinked(ctx, identity, acc, from)
		for _, o := range others {
			s.tell(ctx, o, nil, fmt.Sprintf("GitHub @%s (%s), linked to your identity %s, was also linked to identity %s on %s, from %s: "+
				"both are now one person. If that was not you: %s (the machine owner or a project admin). "+
				"The account stays on record against %s even after the unlink, so an owner removing or lowering %s's grants lowers %s's too.",
				acc.Login, acc.ID, o.Name, identity.Name, s.d.Config.Machine, from, config.Command("identity unlink", "github", identity.Name, "--revoke"), identity.Name, identity.Name, o.Name))
		}
	}
	return out, nil
}

// linkedOthers is the identities other than except that account is linked to.
func linkedOthers(ctx context.Context, tx *sql.Tx, account, except string) ([]api.Identity, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.id, i.name, i.kind FROM owner_links l JOIN identities i ON i.id = l.identity
		WHERE l.account = ? AND l.identity != ? ORDER BY l.linked, i.name`, account, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Identity
	for rows.Next() {
		var id api.Identity
		if err := rows.Scan(&id.ID, &id.Name, &id.Kind); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// tellLinked is the mail of an account's first link to an identity.
func (s *Service) tellLinked(ctx context.Context, identity api.Identity, acc Account, from string) {
	s.tell(ctx, identity, nil, fmt.Sprintf("GitHub @%s (%s) was linked to identity %s on %s, from %s. If that was not you: "+
		"%s (the machine owner or a project admin), then link again with %s.",
		acc.Login, acc.ID, identity.Name, s.d.Config.Machine, from,
		config.Command("identity unlink", "github", identity.Name, "--revoke"), config.Command("login github")))
}

// linkTx is link inside tx; first is a new link (not a refresh).
func (s *Service) linkTx(ctx context.Context, tx *sql.Tx, identity api.Identity, acc Account) (out api.GitHubIdentity, first bool, err error) {
	if acc.Kind != KindUser {
		return out, false, api.Errorf(api.CodeForbidden, "@%s is a GitHub %s; only a person's account signs in", acc.Login, acc.Kind)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	out = api.GitHubIdentity{Identity: identity.Name, Account: acc.ID, Login: acc.Login}
	var mine string
	var linked int64
	switch err := tx.QueryRowContext(ctx, `SELECT account, linked FROM owner_links WHERE identity = ?`, identity.ID).Scan(&mine, &linked); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return out, false, err
	case mine != acc.ID:
		return out, false, api.Errorf(api.CodeConflict, "identity %s is linked to another GitHub account", identity.Name).
			WithHint("a link is made once and never moved")
	}
	if mine == "" {
		linked, first = store.Millis(now), true
	}
	out.Linked = store.FromMillis(linked)
	if _, err = tx.ExecContext(ctx, `INSERT INTO owner_links (identity, account, login, linked, seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (identity) DO UPDATE SET login = excluded.login, seen = excluded.seen`,
		identity.ID, acc.ID, acc.Login, linked, store.Millis(now)); err != nil {
		return out, false, err
	}
	return out, first, project.RememberAccount(ctx, tx, identity.ID, acc.ID)
}

// tell tells body to identity and to the owners (core.Deps.Notify: zm
// mails it), in each of projects (nil: every project identity holds a grant
// in). It is how owners see a link or a join they did not expect
// (device-code phishing, SPEC 22).
func (s *Service) tell(ctx context.Context, identity api.Identity, projects []string, body string) {
	if projects == nil {
		tuples, err := s.d.Projects.Tuples(ctx, identity.ID)
		if err != nil {
			s.d.Log.Warn("owners: who to tell", "err", err)
			return
		}
		seen := map[string]bool{}
		for _, t := range tuples {
			if p, _, err := authz.ParseObject(t.Object); err == nil && !seen[p] {
				seen[p] = true
				projects = append(projects, p)
			}
		}
	}
	for _, pid := range projects {
		ref := api.ProjectRef{ID: pid, Slug: s.d.Projects.Slug(pid)}
		for _, to := range []string{"human:" + identity.Name, "owners"} {
			if err := s.d.Notify(ctx, core.Notice{Project: ref, To: to, Body: body}); err != nil && !errors.Is(err, core.ErrNoNotifier) {
				s.d.Log.Warn("owners: mail", "to", to, "project", ref.Slug, "err", err)
			}
		}
	}
}

// providerDown is the answer when GitHub could not be asked.
func providerDown(err error) error {
	return api.Errorf(api.CodeUnavailable, "could not ask GitHub: %v", err)
}

func (s *Service) now() time.Time { return s.Clock().UTC().Truncate(time.Millisecond) }
