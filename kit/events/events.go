// Package events is the daemon's event log and stream: CloudEvents
// envelopes, appended to one log per project (project.db) plus a machine log
// (registry.db) for project "none", fanned out in-process, and served over
// SSE at GET /v0/events with per-subscriber authz filtering. Owned by the
// integrator.
//
// Modules publish AFTER their transaction commits, never inside store.Tx:
//
//	d.Events.Publish(ctx, events.Input{Type: api.EventMailSent, Subject: m.ID,
//		Project: m.Project, Area: m.Area, Data: m, Mentions: []string{m.From, m.Recipient}})
//
// Event ids are ULIDs from kit/ids, minted under the bus lock, so they
// increase in publish order across every log: one id is a complete resume
// cursor (Last-Event-ID) and there is no sequence number to leak counts.
//
// Mentions lists addresses inside Data or Subject that a subscriber might
// not be allowed to see (session addresses, lease holders, ackers). For such
// a subscriber every occurrence is replaced by "someone" before delivery.
package events

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

const schema = `
CREATE TABLE events (
	id       TEXT PRIMARY KEY,
	type     TEXT NOT NULL,
	subject  TEXT NOT NULL DEFAULT '',
	area     TEXT NOT NULL DEFAULT '',
	time     INTEGER NOT NULL,
	mentions TEXT NOT NULL DEFAULT '[]',
	json     TEXT NOT NULL
);
CREATE INDEX events_type ON events (type, id);`

// audience narrows who may read an event below "read on its area" (JSON of
// Audience; ” = everyone who may read the area).
const audienceCol = `ALTER TABLE events ADD COLUMN audience TEXT NOT NULL DEFAULT '';`

func init() {
	store.RegisterRegistry("events/0001_init", schema)
	store.RegisterProject("events/0001_init", schema)
	store.RegisterRegistry("events/0002_audience", audienceCol)
	store.RegisterProject("events/0002_audience", audienceCol)
	const deliveries = `CREATE TABLE event_deliveries (delivery_id TEXT PRIMARY KEY, payload_sha TEXT NOT NULL, event_id TEXT NOT NULL);`
	store.RegisterRegistry("events/0003_deliveries", deliveries)
	store.RegisterProject("events/0003_deliveries", deliveries)
}

// Keep bounds each log: older events are pruned.
const Keep = 20000

// A team project's log keeps TeamKeep rows and TeamKeepFor, whichever is
// longer (SPEC 17.8).
const (
	TeamKeep    = 200000
	TeamKeepFor = 30 * 24 * time.Hour
)

// A team project's authority-only rows (registered types, authority.go:
// the secrets ledger) have their own retention, since they neither count
// toward TeamKeep nor move pruned_upto: at most LedgerKeep rows per
// registered prefix, and none older than LedgerKeepFor, whichever removes
// more. A pool that mostly runs secret commands stays bounded, and the
// ledger's bound does not depend on other traffic. (A solo log keeps Keep
// rows of every kind together.)
const (
	LedgerKeep    = 50000
	LedgerKeepFor = 90 * 24 * time.Hour
)

// Someone replaces an address the reader may not see.
const Someone = "someone"

// Input is one event to publish.
type Input struct {
	DeliveryID string // internal durable effect identity; never used as event ordering cursor
	Type       string
	Subject    string
	// Project is the project ULID; "" or "none" = the machine log.
	Project string
	Area    string
	Data    any
	// Mentions are addresses in Data/Subject to redact for readers who cannot see them.
	Mentions []string
	// Audience, when set, narrows who receives the event (nil = every
	// reader of its project/area).
	Audience *Audience
	// OwnerOnly names top-level keys of Data that only the machine's owner
	// (scope all: the socket without a token, the daemon itself) receives;
	// every other reader, live or replayed, gets the event without them (a
	// shell session's terminal title, SPEC 8).
	OwnerOnly []string
	// MembersOnly names top-level keys of Data that only readers of the
	// whole project receive (members; the owner too): an area guest gets the
	// event without them (a shared session's last prompt, SPEC 17.10).
	MembersOnly []string
	// Live: delivered to live subscribers only, never stored, so never
	// replayed (Since, catch-up, sync): a high-rate, superseded-at-once
	// change such as a session's activity (SPEC 12). Its id is minted under
	// the bus lock like any other, so it is a valid resume cursor.
	Live bool
}

// Audience narrows an event to readers with Need on its project/area, plus
// the Parties (exact caller addresses), who need only read. Mail to the
// owners mailbox and needs=human mail use it: a guest of the area does not
// get them unless it sent or received them.
type Audience struct {
	ParticipantsOnly bool         `json:"participants_only,omitempty"`
	Need             authz.Action `json:"need"`
	Parties          []string     `json:"parties,omitempty"`
	// OwnerOnly is Input.OwnerOnly, stored with the audience (Need "" then
	// lets every reader of the area in).
	OwnerOnly []string `json:"owner_only,omitempty"`
	// MembersOnly is Input.MembersOnly, stored the same way.
	MembersOnly []string `json:"members_only,omitempty"`
	// AuthorityOnly keeps the event on the machine that holds the log: it
	// is never copied to a replica (Changes, LastVisible and the cursor
	// checks a replica's sync uses skip it), so it is read only where it
	// was written, by GET /v0/events there.
	AuthorityOnly bool `json:"authority_only,omitempty"`
	// NotOverTunnel names top-level keys of Data a reader whose request
	// came over a tunnel (Filter.OverTunnel) never receives: the tunnel's
	// provider reads answers.
	NotOverTunnel []string `json:"not_over_tunnel,omitempty"`
}

// authorityOnly is whether a stays on the machine that holds the log.
func (a *Audience) authorityOnly() bool { return a != nil && a.AuthorityOnly }

// allows reports whether a reader with scope sc and address addr is in a.
func (a *Audience) allows(sc authz.Scope, addr string, o authz.Object) bool {
	if a == nil || (!a.ParticipantsOnly && (a.Need == "" || sc.Can(a.Need, o))) {
		return true
	}
	if addr == "" {
		return false
	}
	for _, p := range a.Parties {
		if p == addr {
			return true
		}
	}
	return false
}

// Filter selects events. Zero value matches everything the subscriber may see.
type Filter struct {
	Types   []string // type prefixes; empty = all
	Subject string   // exact subject; empty = all
	Project string   // project ULID; empty = all visible
	// OverTunnel: the reader's request came over a tunnel; an event's
	// Audience.NotOverTunnel keys (and its type rule's) are withheld from it.
	OverTunnel bool
	// NoAuthorityOnly skips events that stay on the machine holding the
	// log (a replica's long-poll wake: Waiter).
	NoAuthorityOnly bool
}

// ParseTypes splits a comma list of type prefixes.
func ParseTypes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (f Filter) match(e api.Event) bool {
	if f.Subject != "" && e.Subject != f.Subject {
		return false
	}
	if f.Project != "" && e.Project != f.Project {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, p := range f.Types {
		if strings.HasPrefix(e.Type, p) {
			return true
		}
	}
	return false
}

// Visibility reports whether a reader with scope sc may see address. The
// server wires one that asks the session module; the default hides every
// session address from restricted readers.
type Visibility func(ctx context.Context, sc authz.Scope, address string) bool

func defaultVisibility(_ context.Context, sc authz.Scope, address string) bool {
	if sc.IsAll() {
		return true
	}
	a, err := api.ParseAddress(address)
	return err == nil && a.Kind != api.KindSession
}

// Bus persists and fans out events. Safe for concurrent use.
type Bus struct {
	st     *store.Store
	source string

	vmu     sync.RWMutex
	visible Visibility

	mu     sync.Mutex
	subs   map[*sub]struct{}
	counts map[string]int
}

type stored struct {
	replay   bool
	ev       api.Event
	mentions []string
	audience *Audience
}

type sub struct {
	f        Filter
	sc       authz.Scope
	addr     string // the subscriber's address (Audience.Parties)
	personal bool
	owner    bool // OwnerReader: gets owner-only keys
	ch       chan stored
	lag      chan struct{} // closed when the subscriber fell behind
	o        sync.Once
}

// lagged marks s as fallen behind. The caller holds b.mu or calls
// b.drop(s) itself.
func (s *sub) lagged() { s.o.Do(func() { close(s.lag) }) }

func (b *Bus) drop(s *sub) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// New returns a bus over st with source "zm://<machine>". It raises the id
// floor above every stored event id.
func New(ctx context.Context, st *store.Store, machine string) (*Bus, error) {
	b := &Bus{st: st, source: "zm://" + machine, visible: defaultVisibility, subs: map[*sub]struct{}{}, counts: map[string]int{}}
	for _, db := range b.logs(authz.All(), "") {
		var max sql.NullString
		if err := db.db.QueryRowContext(ctx, `SELECT max(id) FROM events`).Scan(&max); err != nil {
			return nil, err
		}
		ids.Floor(max.String)
		w := db.db
		if wr := st.Writer(db.project); wr != nil {
			w = wr
		}
		if _, err := sealStored(ctx, w); err != nil { // rows written before their type's rule
			return nil, err
		}
	}
	return b, nil
}

// SetVisibility installs the address-visibility check (server wiring).
func (b *Bus) SetVisibility(v Visibility) {
	b.vmu.Lock()
	b.visible = v
	b.vmu.Unlock()
}

// Source is this bus's CloudEvents source.
func (b *Bus) Source() string { return b.source }

func machineLevel(p string) bool { return p == "" || p == api.ProjectNone }

// Publish stores an event in its project's log and delivers it to
// subscribers. It never blocks on a subscriber: one whose buffer is full is
// cut off (its channel closes, see Subscribe) and must resume from the store.
//
// A team project's event (SPEC 17.3) is minted inside a write transaction on
// that file's writer connection and committed before anyone else can mint
// one there, so its log's id order is its commit order (TeamTx).
func (b *Bus) Publish(ctx context.Context, in Input) (api.Event, error) {
	project := in.Project
	if machineLevel(project) {
		project = api.ProjectNone
	}
	if in.Live {
		// Minted and delivered under the bus lock, which every stored
		// publish (team transactions included) holds from minting to
		// delivery: ids stay in delivery order, so a client resuming from a
		// live event's id misses no stored one.
		b.mu.Lock()
		defer b.mu.Unlock()
		x, err := b.build(project, in)
		if err != nil {
			return api.Event{}, err
		}
		b.deliver(x)
		return x.ev, nil
	}
	if project != api.ProjectNone && b.st.IsTeam(project) {
		var e api.Event
		err := b.TeamTx(ctx, project, func(_ *sql.Tx, emit func(Input)) error {
			emit(in)
			return nil
		}, func(evs []api.Event) {
			if len(evs) == 1 {
				e = evs[0]
			}
		})
		return e, err
	}
	var db *sql.DB
	var err error
	if project == api.ProjectNone {
		db = b.st.Registry()
	} else if db, err = b.st.Project(authz.All(), authz.Read, project); err != nil {
		return api.Event{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var x stored
	if in.DeliveryID == "" {
		x, err = b.insert(ctx, db, project, in)
	} else {
		err = store.Tx(ctx, db, func(tx *sql.Tx) error { var e error; x, e = b.insert(ctx, tx, project, in); return e })
	}
	if err != nil {
		return api.Event{}, err
	}
	if !x.replay {
		b.fanout(ctx, db, project, x)
	}
	return x.ev, nil
}

// TeamTx runs fn in one write transaction on team project's writer
// connection (BEGIN IMMEDIATE: the file's write lock), then mints an id for
// every event fn emitted, stores them in the same transaction and commits,
// all under the bus lock; subscribers get them after the commit. So a
// change, its events and their ids commit together, and id order, commit
// order and publish order are the same for that file (a replica resuming at
// any id never misses an earlier one that committed later). done, if not
// nil, gets the published events. fn must use tx only (the writer has one
// connection). Lock order is always the file's write lock, then the bus
// lock, so solo publishes (bus lock, then another file) cannot deadlock
// with it.
func (b *Bus) TeamTx(ctx context.Context, project string, fn func(tx *sql.Tx, emit func(Input)) error, done func([]api.Event)) error {
	db := b.st.Writer(project)
	if db == nil {
		return api.NotFound("project", project)
	}
	var queued []Input
	emit := func(in Input) {
		in.Project = project
		queued = append(queued, in)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx, emit); err != nil {
		_ = tx.Rollback()
		return err
	}
	if len(queued) == 0 {
		return tx.Commit()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []stored
	for _, in := range queued {
		x, err := b.insert(ctx, tx, project, in)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		out = append(out, x)
	}
	// The log is pruned now and then inside this transaction: it already
	// holds the writer's one connection. Pruning on the writer from fanout,
	// under the bus lock, deadlocked against a TeamTx that held the writer
	// and waited for the bus lock (found by the round C load test).
	if b.counts[project] += len(out); b.counts[project] >= 500 {
		b.counts[project] = 0
		if err := pruneTeamTx(ctx, tx, TeamKeep, time.Now().Add(-TeamKeepFor)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	evs := make([]api.Event, len(out))
	for i, x := range out {
		if !x.replay {
			b.fanout(ctx, db, project, x)
		}
		evs[i] = x.ev
	}
	if done != nil {
		done(evs)
	}
	return nil
}

type execer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insert mints the event's id and stores it. The caller holds b.mu.
func (b *Bus) insert(ctx context.Context, db execer, project string, in Input) (stored, error) {
	var payloadSHA string
	if in.DeliveryID != "" {
		payload, _ := json.Marshal(in)
		h := sha256.Sum256(payload)
		payloadSHA = hex.EncodeToString(h[:])
		var old, ev string
		err := db.QueryRowContext(ctx, `SELECT payload_sha,event_id FROM event_deliveries WHERE delivery_id=?`, in.DeliveryID).Scan(&old, &ev)
		if err == nil {
			if old != payloadSHA {
				return stored{}, api.Errorf(api.CodeConflict, "event delivery payload changed")
			}
			e := api.Event{ID: ev, Project: project, Type: in.Type, Subject: in.Subject}
			var js string
			if err := db.QueryRowContext(ctx, `SELECT json FROM events WHERE id=?`, ev).Scan(&js); err == nil {
				if err = json.Unmarshal([]byte(js), &e); err != nil {
					return stored{}, err
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return stored{}, err
			}
			return stored{ev: e, replay: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return stored{}, err
		}
	}
	x, err := b.build(project, in)
	if err != nil {
		return stored{}, err
	}
	mentions, _ := json.Marshal(x.mentions)
	audience := ""
	if x.audience != nil {
		js, _ := json.Marshal(x.audience)
		audience = string(js)
	}
	e := x.ev
	js, _ := json.Marshal(e)
	if _, err := db.ExecContext(ctx, `INSERT INTO events (id, type, subject, area, time, mentions, json, audience) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Type, e.Subject, e.Area, store.Millis(e.Time), string(mentions), string(js), audience); err != nil {
		return stored{}, err
	}
	if in.DeliveryID != "" {
		if _, err := db.ExecContext(ctx, `INSERT INTO event_deliveries (delivery_id,payload_sha,event_id) VALUES (?,?,?)`, in.DeliveryID, payloadSHA, e.ID); err != nil {
			return stored{}, err
		}
	}
	return x, nil
}

// build mints the event's id and envelope. The caller holds b.mu.
func (b *Bus) build(project string, in Input) (stored, error) {
	raw, err := json.Marshal(in.Data)
	if err != nil {
		return stored{}, err
	}
	if len(in.OwnerOnly) > 0 || len(in.MembersOnly) > 0 {
		au := Audience{}
		if in.Audience != nil {
			au = *in.Audience
		}
		au.OwnerOnly, au.MembersOnly = in.OwnerOnly, in.MembersOnly
		in.Audience = &au
	}
	e := api.Event{SpecVersion: "1.0", ID: ids.New(), Source: b.source, Type: in.Type, Subject: in.Subject,
		Time: time.Now().UTC().Truncate(time.Millisecond), DataContentType: "application/json", Data: raw,
		Project: project, Area: in.Area}
	return stored{ev: e, mentions: nonEmpty(in.Mentions), audience: in.Audience}, nil
}

// deliver hands x to every live subscriber (one behind by its buffer is
// cut off). The caller holds b.mu.
func (b *Bus) deliver(x stored) {
	for s := range b.subs {
		select {
		case s.ch <- x:
		default:
			s.lagged()
			delete(b.subs, s)
		}
	}
}

// fanout delivers x to every subscriber and prunes the log now and then.
// The caller holds b.mu.
func (b *Bus) fanout(ctx context.Context, db *sql.DB, project string, x stored) {
	b.deliver(x)
	if project != api.ProjectNone && b.st.IsTeam(project) {
		// A team project's log is its changelog (SPEC 17.8): kept for 30
		// days and TeamKeep rows, whichever is longer, and pruned inside
		// TeamTx (never here, under the bus lock, on the writer).
		return
	}
	if b.counts[project]++; b.counts[project] >= 500 {
		b.counts[project] = 0
		_, _ = db.ExecContext(ctx, `DELETE FROM events WHERE id < (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET ?)`, Keep-1)
	}
}

// PruneTeam cuts team project's log back to keep rows or the rows since
// cutoff, whichever is more (the bus does this every 500 events with
// TeamKeep and TeamKeepFor; tests call it to force a 410 resync).
func (b *Bus) PruneTeam(ctx context.Context, project string, keep int, cutoff time.Time) {
	if db := b.st.Writer(project); db != nil {
		pruneTeam(ctx, db, keep, cutoff)
	}
}

// pruneTeam cuts a team log back to keep rows or the rows since cutoff,
// whichever is more, and records the newest id it removed (events_meta
// pruned_upto), so a replica cursor below it is answered 410 resync.
func pruneTeam(ctx context.Context, db *sql.DB, keep int, cutoff time.Time) {
	_ = store.Tx(ctx, db, func(tx *sql.Tx) error { return pruneTeamTx(ctx, tx, keep, cutoff) })
}

func pruneTeamTx(ctx context.Context, tx *sql.Tx, keep int, cutoff time.Time) error {
	if err := pruneLedgerTx(ctx, tx, LedgerKeep, time.Now().Add(-LedgerKeepFor)); err != nil {
		return err
	}
	// Nothing is pruned before the oldest row is past cutoff (rows are kept
	// TeamKeepFor, whichever is longer): one indexed lookup, instead of the
	// OFFSET walk below over up to keep rows, which ran inside the writer's
	// transaction every 500 events and stalled every write as the log grew
	// (the load test's tail latencies).
	var oldest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT time FROM events ORDER BY id LIMIT 1`).Scan(&oldest); err != nil && err != sql.ErrNoRows {
		return err
	}
	if !oldest.Valid || oldest.Int64 >= store.Millis(cutoff) {
		return nil
	}
	// keep counts the rows a replica copies: events that stay on this
	// machine (authoritySQL) neither count nor move pruned_upto past an idle
	// replica's cursor sooner.
	home, hargs := authoritySQL()
	var upto sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT max(id) FROM events WHERE id < (SELECT id FROM events WHERE NOT `+home+` ORDER BY id DESC LIMIT 1 OFFSET ?) AND time < ? AND NOT `+home,
		append(append(append([]any{}, hargs...), keep-1, store.Millis(cutoff)), hargs...)...).Scan(&upto); err != nil || !upto.Valid {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id <= ?`, upto.String); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events_meta (key, value) VALUES ('pruned_upto', ?)
		ON CONFLICT (key) DO UPDATE SET value = max(value, excluded.value)`, upto.String)
	return err
}

func nonEmpty(xs []string) []string {
	out := []string{}
	for _, x := range xs {
		if x != "" && x != Someone {
			out = append(out, x)
		}
	}
	return out
}

// MinBuffer is the smallest Subscribe buffer (tests lower it).
var MinBuffer = 16

// Subscribe returns live events that sub may read, matching f, already
// redacted for sub, and a cancel func. In-process consumers use
// authz.System(machine) to see everything unredacted.
//
// Events are never dropped silently: if the consumer falls more than buf
// events behind, the subscription is cut off and the channel is closed. A
// channel that closes before the consumer called cancel means "you missed
// events": resubscribe, then catch up with Since(<last id you handled>) (the
// SSE handler ends the stream with an ": overflow" comment so remote clients
// reconnect with Last-Event-ID).
func (b *Bus) Subscribe(sub0 *authz.Subject, f Filter, buf int) (<-chan api.Event, func()) {
	n := max(buf, MinBuffer)
	s := &sub{f: f, sc: sub0.Scope, addr: sub0.Address, owner: OwnerReader(sub0), personal: authz.PersonalMailReader(sub0), ch: make(chan stored, n), lag: make(chan struct{})}
	out := make(chan api.Event, n)
	done := make(chan struct{})
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case <-s.lag:
				return
			case x := <-s.ch:
				e, ok := b.deliverable(x, s.sc, s.addr, s.owner, f, s.personal)
				if !ok {
					continue
				}
				select {
				case out <- e:
				case <-done:
					return
				default: // the consumer is behind: cut it off, it resumes from its cursor
					b.drop(s)
					s.lagged()
					return
				}
			}
		}
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() {
			b.drop(s)
			close(done)
		})
	}
}

// deliverable applies the read check, the audience, the filter and redaction.
func (b *Bus) deliverable(x stored, sc authz.Scope, addr string, owner bool, f Filter, personal ...bool) (api.Event, bool) {
	if x.audience != nil && x.audience.ParticipantsOnly && len(personal) > 0 && !personal[0] {
		return api.Event{}, false
	}
	if f.NoAuthorityOnly && stayHome(x) {
		return api.Event{}, false
	}
	e := x.ev
	o := authz.Object{Project: e.Project, Area: e.Area}
	if !sc.Can(authz.Read, o) || !x.audience.allows(sc, addr, o) || !f.match(e) {
		return api.Event{}, false
	}
	if !owner && x.audience != nil && len(x.audience.OwnerOnly) > 0 {
		e = withoutKeys(e, x.audience.OwnerOnly)
	}
	if !owner && x.audience != nil && len(x.audience.MembersOnly) > 0 && !sc.WholeProject(e.Project) {
		e = withoutKeys(e, x.audience.MembersOnly)
	}
	if f.OverTunnel {
		e = overTunnel(x, e)
	}
	if sc.IsAll() || len(x.mentions) == 0 {
		return e, true
	}
	b.vmu.RLock()
	vis := b.visible
	b.vmu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var hide []string
	for _, m := range x.mentions {
		if !vis(ctx, sc, m) {
			hide = append(hide, m)
		}
	}
	if len(hide) > 0 {
		e = Redact(e, hide)
	}
	return e, true
}

// Redact replaces every occurrence of each hidden address in the event's
// subject and in every string inside its data with "someone".
func Redact(e api.Event, hidden []string) api.Event {
	// Longest first, so "mini/abcd1234" is not half-replaced by a shorter mention.
	sort.Slice(hidden, func(i, j int) bool { return len(hidden[i]) > len(hidden[j]) })
	repl := func(s string) string {
		for _, h := range hidden {
			s = strings.ReplaceAll(s, h, Someone)
		}
		return s
	}
	e.Subject = repl(e.Subject)
	if len(e.Data) > 0 {
		var v any
		if json.Unmarshal(e.Data, &v) == nil {
			v = walk(v, repl)
			e.Data, _ = json.Marshal(v)
		} else {
			e.Data = nil
		}
	}
	return e
}

// OwnerReader reports a reader that gets owner-only keys: the machine's
// owner in person, on the socket without a token (scope all, via local).
// The daemon's own followers (authz.System: webhooks, which POST events to
// any URL, presence, sync) are not one, though they read everything.
func OwnerReader(sub *authz.Subject) bool {
	return sub != nil && sub.Scope.IsAll() && sub.Via == authz.ViaLocal
}

// ScrubOwnerOnly removes owner-only keys from every stored event in db (a
// copy of a log leaving the daemon: zm project export).
func ScrubOwnerOnly(ctx context.Context, db *sql.DB) error {
	return scrub(ctx, db, "owner_only", func(a Audience) []string { return a.OwnerOnly })
}

// ScrubMembersOnly removes members-only keys from every stored event in db
// (a copy of a log leaving the daemon: zm project export, whose reader may
// not be a member).
func ScrubMembersOnly(ctx context.Context, db *sql.DB) error {
	return scrub(ctx, db, "members_only", func(a Audience) []string { return a.MembersOnly })
}

// scrub removes from every stored event whose audience names field the keys
// keysOf gives.
func scrub(ctx context.Context, db *sql.DB, field string, keysOf func(Audience) []string) error {
	return store.Tx(ctx, db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, json, audience FROM events WHERE audience LIKE ?`, `%"`+field+`"%`)
		if err != nil {
			return err
		}
		type fix struct{ id, js string }
		var fixes []fix
		for rows.Next() {
			var id, js, au string
			if err := rows.Scan(&id, &js, &au); err != nil {
				rows.Close()
				return err
			}
			var a Audience
			var e api.Event
			if json.Unmarshal([]byte(au), &a) != nil || json.Unmarshal([]byte(js), &e) != nil {
				continue
			}
			e = withoutKeys(e, keysOf(a))
			b, _ := json.Marshal(e)
			fixes = append(fixes, fix{id, string(b)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, f := range fixes {
			if _, err := tx.ExecContext(ctx, `UPDATE events SET json = ? WHERE id = ?`, f.js, f.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// withoutKeys removes top-level keys from the event's data (an object).
func withoutKeys(e api.Event, keys []string) api.Event {
	var m map[string]json.RawMessage
	if json.Unmarshal(e.Data, &m) != nil {
		e.Data = nil // not an object: fail closed
		return e
	}
	for _, k := range keys {
		delete(m, k)
	}
	e.Data, _ = json.Marshal(m)
	return e
}

func walk(v any, f func(string) string) any {
	switch x := v.(type) {
	case string:
		return f(x)
	case []any:
		for i := range x {
			x[i] = walk(x[i], f)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[f(k)] = walk(val, f)
		}
		return out
	}
	return v
}

type logDB struct {
	project string
	db      *sql.DB
}

// logs are the event logs sc may read, optionally just one project.
func (b *Bus) logs(sc authz.Scope, project string) []logDB {
	var out []logDB
	if (project == "" || project == api.ProjectNone) && sc.Can(authz.Read, authz.Object{Project: api.ProjectNone}) {
		out = append(out, logDB{project: api.ProjectNone, db: b.st.Registry()})
	}
	for _, p := range b.st.Projects(sc, authz.Read) {
		if project == "" || project == p.ID {
			out = append(out, logDB{project: p.ID, db: p.DB})
		}
	}
	return out
}

// Since returns stored events after the cursor (an event id; "" = from the
// start) that sub may read and f matches, oldest first, at most limit
// (default 500), redacted for sub.
func (b *Bus) Since(ctx context.Context, sub0 *authz.Subject, after string, f Filter, limit int) ([]api.Event, error) {
	if limit <= 0 {
		limit = 500
	}
	if after != "" && !ids.Valid(after) {
		return nil, api.Errorf(api.CodeBadRequest, "bad event cursor %q (want an event id)", after)
	}
	sc := sub0.Scope
	var all []stored
	for _, l := range b.logs(sc, f.Project) {
		q := `SELECT json, mentions, audience FROM events WHERE id > ?`
		args := []any{after}
		if l.project != api.ProjectNone {
			w, a := sc.AreaSQL(l.project, "area", authz.Read)
			q += " AND " + w
			args = append(args, a...)
		}
		if f.Subject != "" {
			q += " AND subject = ?"
			args = append(args, f.Subject)
		}
		if len(f.Types) > 0 {
			var or []string
			for _, t := range f.Types {
				or = append(or, "substr(type, 1, ?) = ?")
				args = append(args, len(t), t)
			}
			q += " AND (" + strings.Join(or, " OR ") + ")"
		}
		q += " ORDER BY id LIMIT ?"
		args = append(args, limit)
		rows, err := l.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var js, ms, au string
			if err := rows.Scan(&js, &ms, &au); err != nil {
				rows.Close()
				return nil, err
			}
			var x stored
			if json.Unmarshal([]byte(js), &x.ev) != nil {
				continue
			}
			_ = json.Unmarshal([]byte(ms), &x.mentions)
			if au != "" {
				x.audience = &Audience{}
				if json.Unmarshal([]byte(au), x.audience) != nil {
					x.audience.Need = authz.Admin // unreadable: fail closed
				}
			}
			all = append(all, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ev.ID < all[j].ev.ID })
	out := make([]api.Event, 0, min(len(all), limit))
	for _, x := range all {
		if len(out) == limit {
			break
		}
		if e, ok := b.deliverable(x, sc, sub0.Address, OwnerReader(sub0), f, authz.PersonalMailReader(sub0)); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// ErrStop can be returned from a ReadSSE callback to end reading without error.
var ErrStop = errors.New("stop")

// ErrLagged matches (errors.Is) the *LagError ReadSSE returns when the server
// cut the stream off because the reader fell behind.
var ErrLagged = errors.New("event stream overflowed: reconnect with Last-Event-ID")

// LagError is an ": overflow <id>" end of stream. Reconnect with
// Last-Event-ID = the last event handled, or Last when none was; the replay
// fills the gap.
type LagError struct {
	Last string // the server's cursor at the cut (may be "")
}

func (e *LagError) Error() string        { return ErrLagged.Error() + " (last " + e.Last + ")" }
func (e *LagError) Is(target error) bool { return target == ErrLagged }

// Scrub overwrites texts in project's stored events (a redaction, SPEC
// "Native issues"): events filed under one of subjects, for any text, and
// any event carrying a text at least minUnscoped long. Ids, types and order
// do not change; only the JSON's text does. Under the subjects only string
// values equal to a text change (scrubJSON, core.ScrubJSON), so a short
// text cannot mangle keys or other values; elsewhere a text of minUnscoped
// bytes or more is replaced wherever it occurs (scrub, core.ScrubText).
func (b *Bus) Scrub(ctx context.Context, project string, subjects, texts []string, minUnscoped int,
	scrub func(s string, texts []string, minLen int) string, scrubJSON func(js string, texts []string) (string, bool)) error {
	db := b.st.Writer(project)
	if db == nil {
		return nil
	}
	fix := func(q string, args []any, apply func(string) string) error {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		type ev struct{ id, js string }
		var evs []ev
		for rows.Next() {
			var e ev
			if err := rows.Scan(&e.id, &e.js); err != nil {
				rows.Close()
				return err
			}
			evs = append(evs, e)
		}
		rows.Close()
		for _, e := range evs {
			if nj := apply(e.js); nj != e.js {
				if _, err := db.ExecContext(ctx, `UPDATE events SET json = ? WHERE id = ?`, nj, e.id); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, sub := range subjects {
		// Filed under the redacted item: only string values equal to a text.
		exact := func(js string) string { nj, _ := scrubJSON(js, texts); return nj }
		if err := fix(`SELECT id, json FROM events WHERE subject = ?`, []any{sub}, exact); err != nil {
			return err
		}
	}
	for _, t := range texts {
		if len(t) < minUnscoped {
			continue
		}
		enc, _ := json.Marshal(t)
		sub := func(js string) string { return scrub(js, texts, minUnscoped) }
		if err := fix(`SELECT id, json FROM events WHERE instr(json, ?) > 0 OR instr(json, ?) > 0`, []any{t, string(enc[1 : len(enc)-1])}, sub); err != nil {
			return err
		}
	}
	return nil
}
