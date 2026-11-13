package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// A team project's log is its changelog (SPEC 17.6): replicas read it with
// Changes, from a cursor that is always an event id they were sent.

func init() {
	// Per-subject lookups: an item's last status change (a stale base), the
	// event before a move (a guest's tombstone), what a write left behind.
	store.RegisterProject("events/0003_subject", `CREATE INDEX events_subject ON events (subject, id);
CREATE INDEX events_subject_type ON events (subject, type, id);`)
	// pruned_upto: the newest id the team log's pruning removed. A cursor
	// below it may have missed rows (410 resync); one at or above it has not.
	store.RegisterProject("events/0004_meta", `CREATE TABLE events_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`)
}

// EventGone is the tombstone a reader of only some areas gets when a work
// item it could read moved to an area it cannot (SPEC 17.6 "Area
// transitions"): Subject is the item id, ID the id of the event that moved
// it; Data is {"id": ..., "gone": true}. It is never stored.
const EventGone = "dev.zm.sync.gone"

// workPrefix is the type prefix of events about work items, their comments
// and their leases (subject: the item id or lease key).
const workPrefix = "dev.zm.work."

// ErrResync is Changes' answer to a cursor the log can no longer resume.
var ErrResync = api.Errorf(api.CodeResync, "the cursor is no longer in this project's changelog").
	WithHint("take a snapshot (GET /v0/sync/snapshot) and sync from its cursor")

// Changes returns what sub may read of team project's log after the cursor,
// oldest first, at most limit (default 500), redacted for sub, plus the
// cursor to resume from and whether more rows follow. The cursor only ever
// moves to an id sub was sent (gaps reveal nothing): with nothing visible it
// stays where it was. A reader of only some areas also gets EventGone for
// an item whose previous work event was in an area it reads and whose next
// is not. A cursor lost to pruning or an authority restore is ErrResync.
func (b *Bus) Changes(ctx context.Context, sub0 *authz.Subject, project, after string, limit int) (out []api.Event, next string, more bool, err error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if after != "" && !ids.Valid(after) {
		return nil, after, false, api.Errorf(api.CodeBadRequest, "bad cursor %q (want an event id)", after)
	}
	sc := sub0.Scope
	db, err := b.st.Project(sc, authz.Read, project)
	if err != nil {
		return nil, after, false, err
	}
	if after != "" {
		var upto sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT value FROM events_meta WHERE key = 'pruned_upto'`).Scan(&upto); err != nil && err != sql.ErrNoRows {
			return nil, after, false, err
		}
		if upto.Valid && after < upto.String {
			return nil, after, false, ErrResync
		}
		// A restored authority can have lost a cursor the replica already
		// applied. Even when new events have overtaken that cursor, continuing
		// would leave rows from the lost history in the copy forever. The exact
		// pruning boundary is resumable although its own row was removed.
		if !upto.Valid || after != upto.String {
			ok, err := b.hasCursor(ctx, db, sub0, project, after)
			if err != nil {
				return nil, after, false, err
			}
			if !ok {
				return nil, after, false, ErrResync
			}
		}
	}
	whole := sc.Relation(authz.Object{Project: project}).Allows(authz.Read)
	next = after
	scan := after
	const page = 1000
	for len(out) < limit {
		rows, err := db.QueryContext(ctx, `SELECT id, json, mentions, audience, area, type, subject FROM events WHERE id > ? ORDER BY id LIMIT ?`, scan, page)
		if err != nil {
			return nil, after, false, err
		}
		n := 0
		var batch []stored
		type hidden struct {
			id, typ, subject string
		}
		var gone []hidden
		var order []string // ids in scan order: "v<i>" visible, "g<i>" gone
		for rows.Next() {
			n++
			var id, js, ms, au, area, typ, subject string
			if err := rows.Scan(&id, &js, &ms, &au, &area, &typ, &subject); err != nil {
				rows.Close()
				return nil, after, false, err
			}
			scan = id
			if !sc.Can(authz.Read, authz.Object{Project: project, Area: area}) {
				if !whole && strings.HasPrefix(typ, workPrefix) && subject != "" {
					gone = append(gone, hidden{id: id, typ: typ, subject: subject})
					order = append(order, "g")
				}
				continue
			}
			var x stored
			if json.Unmarshal([]byte(js), &x.ev) != nil {
				continue
			}
			_ = json.Unmarshal([]byte(ms), &x.mentions)
			if au != "" {
				x.audience = &Audience{}
				if json.Unmarshal([]byte(au), x.audience) != nil {
					x.audience.Need = authz.Admin
				}
			}
			if stayHome(x) { // never copied to a replica
				continue
			}
			batch = append(batch, x)
			order = append(order, "v")
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, after, false, err
		}
		vi, gi := 0, 0
		for _, k := range order {
			if len(out) >= limit {
				return out, next, true, nil
			}
			if k == "v" {
				x := batch[vi]
				vi++
				if e, ok := b.deliverable(x, sc, sub0.Address, OwnerReader(sub0), Filter{}, authz.PersonalMailReader(sub0)); ok {
					out = append(out, e)
					next = e.ID
				}
				continue
			}
			h := gone[gi]
			gi++
			if b.leftView(ctx, db, sc, project, h.subject, h.id) {
				raw, _ := json.Marshal(map[string]any{"id": h.subject, "gone": true})
				out = append(out, api.Event{SpecVersion: "1.0", ID: h.id, Source: b.source, Type: EventGone, Subject: h.subject,
					DataContentType: "application/json", Data: raw, Project: project})
				next = h.id
			}
		}
		if n < page {
			return out, next, false, nil
		}
	}
	return out, next, true, nil
}

// hasCursor accepts only an event the caller could have received, including
// a guest's tombstone. Hidden and missing ids answer alike, so validation
// cannot be used to test whether a hidden event exists.
func (b *Bus) hasCursor(ctx context.Context, db *sql.DB, sub *authz.Subject, project, id string) (bool, error) {
	var area, audience, typ, subject string
	err := db.QueryRowContext(ctx, `SELECT area, audience, type, subject FROM events WHERE id = ?`, id).
		Scan(&area, &audience, &typ, &subject)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sc := sub.Scope
	o := authz.Object{Project: project, Area: area}
	if sc.Can(authz.Read, o) {
		var a *Audience
		if audience != "" {
			a = &Audience{}
			if json.Unmarshal([]byte(audience), a) != nil {
				return false, nil
			}
		}
		return !stayHomeType(typ, audience) && a.allows(sc, sub.Address, o), nil
	}
	return !sc.Relation(authz.Object{Project: project}).Allows(authz.Read) &&
		strings.HasPrefix(typ, workPrefix) && subject != "" && b.leftView(ctx, db, sc, project, subject, id), nil
}

// leftView reports whether the work event before id about subject was in
// an area sc reads (the item was visible and the event at id moved it out).
func (b *Bus) leftView(ctx context.Context, db *sql.DB, sc authz.Scope, project, subject, id string) bool {
	var area string
	err := db.QueryRowContext(ctx, `SELECT area FROM events WHERE subject = ? AND id < ? AND substr(type, 1, ?) = ? ORDER BY id DESC LIMIT 1`,
		subject, id, len(workPrefix), workPrefix).Scan(&area)
	return err == nil && sc.Can(authz.Read, authz.Object{Project: project, Area: area})
}

// LastVisible is the newest id in team project's log that sub may read
// ("" when none): a snapshot's cursor (SPEC 17.6). Everything committed
// after it has a larger id (team ids are minted in commit order), and
// nothing sub cannot read leaks through it.
func (b *Bus) LastVisible(ctx context.Context, sub0 *authz.Subject, project string) (string, error) {
	sc := sub0.Scope
	db, err := b.st.Project(sc, authz.Read, project)
	if err != nil {
		return "", err
	}
	before := "\x7f"
	for {
		rows, err := db.QueryContext(ctx, `SELECT json, mentions, audience, area FROM events WHERE id < ? ORDER BY id DESC LIMIT 500`, before)
		if err != nil {
			return "", err
		}
		n := 0
		found := ""
		for rows.Next() {
			n++
			var js, ms, au, area string
			if err := rows.Scan(&js, &ms, &au, &area); err != nil {
				rows.Close()
				return "", err
			}
			var x stored
			if json.Unmarshal([]byte(js), &x.ev) != nil {
				continue
			}
			before = x.ev.ID
			if found != "" || !sc.Can(authz.Read, authz.Object{Project: project, Area: area}) {
				continue
			}
			if au != "" {
				x.audience = &Audience{}
				if json.Unmarshal([]byte(au), x.audience) != nil {
					continue
				}
			}
			if stayHome(x) {
				continue
			}
			if (x.audience == nil || !x.audience.ParticipantsOnly || authz.PersonalMailReader(sub0)) && x.audience.allows(sc, sub0.Address, authz.Object{Project: project, Area: area}) {
				found = x.ev.ID
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || found != "" || n < 500 {
			return found, err
		}
	}
}

// LastForVisible returns the newest event about subject deliverable to the caller.
// Item identifiers supplied by a writer do not grant access to event metadata.
func (b *Bus) LastForVisible(ctx context.Context, sub *authz.Subject, project, subject string) (string, error) {
	return b.lastVisibleWhere(ctx, sub, project, "subject = ?", subject)
}

// VisibleEventID rechecks cached cursor metadata under the current reader.
func (b *Bus) VisibleEventID(ctx context.Context, sub *authz.Subject, project, id string) (string, error) {
	return b.lastVisibleWhere(ctx, sub, project, "id = ?", id)
}

func (b *Bus) lastVisibleWhere(ctx context.Context, sub *authz.Subject, project, where, value string) (string, error) {
	db, err := b.st.Project(sub.Scope, authz.Read, project)
	if err != nil {
		return "", err
	}
	rows, err := db.QueryContext(ctx, `SELECT json, mentions, audience FROM events WHERE `+where+` ORDER BY id DESC`, value)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var js, ms, au string
		if err := rows.Scan(&js, &ms, &au); err != nil {
			return "", err
		}
		var x stored
		if json.Unmarshal([]byte(js), &x.ev) != nil {
			continue
		}
		_ = json.Unmarshal([]byte(ms), &x.mentions)
		if au != "" {
			x.audience = &Audience{}
			if json.Unmarshal([]byte(au), x.audience) != nil {
				continue
			}
		}
		if stayHome(x) {
			continue
		}
		if e, ok := b.deliverable(x, sub.Scope, sub.Address, OwnerReader(sub), Filter{}, authz.PersonalMailReader(sub)); ok {
			return e.ID, nil
		}
	}
	return "", rows.Err()
}

// Waiter wakes a long poll when an event of a project that its subscriber
// may read is published. Subscribe before reading the log, then Wait, so
// nothing published in between is missed.
type Waiter struct {
	b       *Bus
	sub     *authz.Subject
	project string
	ch      <-chan api.Event
	cancel  func()
}

// Waiter subscribes sub to project's events (call before reading).
func (b *Bus) Waiter(sub0 *authz.Subject, project string) *Waiter {
	w := &Waiter{b: b, sub: sub0, project: project}
	w.ch, w.cancel = b.Subscribe(sub0, Filter{Project: project, NoAuthorityOnly: true}, 64)
	return w
}

// Wait returns when an event arrived (true), d passed, or ctx ended. Events that
// queued meanwhile are drained (the caller reads the log again anyway). A
// subscription the bus cut off (the waiter fell behind) is renewed, and
// Wait returns at once: "read again" (never a closed channel that would
// make every later Wait return at once).
func (w *Waiter) Wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case _, ok := <-w.ch:
		if !ok {
			w.renew()
			return true
		}
		for {
			select {
			case _, ok := <-w.ch:
				if !ok {
					w.renew()
					return true
				}
			default:
				return true
			}
		}
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

func (w *Waiter) renew() {
	w.cancel()
	w.ch, w.cancel = w.b.Subscribe(w.sub, Filter{Project: w.project, NoAuthorityOnly: true}, 64)
}

// Close ends the subscription.
func (w *Waiter) Close() { w.cancel() }
