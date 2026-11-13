package events

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// An authority-only event is never handed to a replica's sync (Changes,
// LastVisible, the cursor checks) though readers of the log see it; its
// NotOverTunnel keys are withheld from a reader over a tunnel.
func TestAuthorityOnlyAndNotOverTunnel(t *testing.T) {
	ctx := context.Background()
	b, _, p := teamBus(t)
	member := &authz.Subject{Address: "human:ana",
		Scope: authz.FromTuples([]authz.Tuple{{Subject: "a", Relation: authz.Member, Object: authz.ProjectObject(p)}}, nil)}
	plain, err := b.Publish(ctx, Input{Type: "dev.zm.test.plain", Subject: "x", Project: p, Data: map[string]string{"id": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := b.Publish(ctx, Input{Type: "dev.zm.secret.granted", Subject: "box", Project: p,
		Data:     map[string]any{"names": []string{"SECRET_NAME_ZZQ"}, "count": 1},
		Audience: &Audience{Need: authz.Operate, AuthorityOnly: true, NotOverTunnel: []string{"names"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs, next, _, err := b.Changes(ctx, member, p, "", 100)
	if err != nil || len(evs) != 1 || evs[0].ID != plain.ID || next != plain.ID {
		t.Fatalf("a replica's changes: %+v %s %v", evs, next, err)
	}
	if last, err := b.LastVisible(ctx, member, p); err != nil || last != plain.ID {
		t.Errorf("LastVisible: %q %v, want the plain event", last, err)
	}
	if id, _ := b.VisibleEventID(ctx, member, p, secret.ID); id != "" {
		t.Errorf("VisibleEventID answered the authority-only event")
	}
	if _, _, _, err := b.Changes(ctx, member, p, secret.ID, 100); err != ErrResync {
		t.Errorf("a cursor at the authority-only event: %v, want resync (never one a replica was sent)", err)
	}
	// The log's own readers see it: whole on the socket or the tailnet,
	// without the withheld keys over a tunnel.
	read := func(f Filter) string {
		got, err := b.Since(ctx, member, "", f, 10)
		if err != nil || len(got) != 1 {
			t.Fatalf("since %+v: %+v %v", f, got, err)
		}
		return string(got[0].Data)
	}
	f := Filter{Types: []string{"dev.zm.secret."}}
	if d := read(f); !strings.Contains(d, "SECRET_NAME_ZZQ") {
		t.Errorf("a reader of the log: %s", d)
	}
	f.OverTunnel = true
	if d := read(f); strings.Contains(d, "SECRET_NAME_ZZQ") || !strings.Contains(d, `"count":1`) {
		t.Errorf("a reader over a tunnel: %s", d)
	}
}

func init() {
	RegisterAuthorityOnly(TypeRule{Prefix: "dev.zm.testsecret.", Withheld: []string{"names"}, CountOf: "names"})
}

// A row of a registered type written with an old audience (none: before
// the rule) stays home anyway, by its type: a replica's bootstrap and sync
// never see it, a waiter is not woken by it, a tunnel reader gets a count
// and no names, an export drops it, pruning does not count it, and a new
// bus rewrites its stored audience once.
func TestAuthorityOnlyByType(t *testing.T) {
	ctx := context.Background()
	b, st, p := teamBus(t)
	member := &authz.Subject{Address: "human:ana",
		Scope: authz.FromTuples([]authz.Tuple{{Subject: "a", Relation: authz.Member, Object: authz.ProjectObject(p)}}, nil)}
	w := b.Waiter(member, p)
	defer w.Close()
	all, cancel := b.Subscribe(member, Filter{Project: p}, 16)
	defer cancel()
	old, err := b.Publish(ctx, Input{Type: "dev.zm.testsecret.granted", Subject: "box", Project: p,
		Data: map[string]any{"names": []string{"OLD_NAME_ZZQ", "TWO"}}})
	if err != nil {
		t.Fatal(err)
	}
	if w.Wait(ctx, 200*time.Millisecond) {
		t.Error("a replica's waiter woke for an authority-only event")
	}
	select {
	case e := <-all:
		if e.ID != old.ID {
			t.Errorf("subscribe: %s", e.ID)
		}
	case <-time.After(2 * time.Second):
		t.Error("a reader on the authority was not sent it live")
	}
	plain, err := b.Publish(ctx, Input{Type: "dev.zm.test.plain", Subject: "x", Project: p, Data: map[string]string{"id": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !w.Wait(ctx, 2*time.Second) {
		t.Error("the waiter did not wake for a plain event")
	}
	// Bootstrap (a full resync) and sync.
	if last, _ := b.LastVisible(ctx, member, p); last != plain.ID {
		t.Errorf("LastVisible %q, want the plain event", last)
	}
	if evs, _, _, _ := b.Changes(ctx, member, p, "", 100); len(evs) != 1 || evs[0].ID != plain.ID {
		t.Errorf("a full resync: %+v", evs)
	}
	if id, _ := b.LastForVisible(ctx, member, p, "box"); id != "" {
		t.Errorf("LastForVisible answered it: %s", id)
	}
	// A tunnel reader: a count in place of the names.
	got, err := b.Since(ctx, member, "", Filter{Types: []string{"dev.zm.testsecret."}, OverTunnel: true}, 10)
	if err != nil || len(got) != 1 || strings.Contains(string(got[0].Data), "OLD_NAME_ZZQ") || !strings.Contains(string(got[0].Data), `"count":2`) {
		t.Fatalf("over a tunnel: %+v %v", got, err)
	}
	// A new bus rewrites the stored audience, once.
	if _, err := New(ctx, st, "mini"); err != nil {
		t.Fatal(err)
	}
	var au string
	if err := st.Writer(p).QueryRowContext(ctx, `SELECT audience FROM events WHERE id = ?`, old.ID).Scan(&au); err != nil ||
		!strings.Contains(au, `"authority_only":true`) || !strings.Contains(au, `"names"`) {
		t.Fatalf("stored audience after start: %q %v", au, err)
	}
	if _, err := New(ctx, st, "mini"); err != nil { // idempotent
		t.Fatal(err)
	}
	// An export copy drops it.
	if err := store.FilterExport(ctx, st.Writer(p), "human:ana"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Writer(p).QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id = ?`, old.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("an export kept it: %d %v", n, err)
	}
}

// Pruning keeps the newest keep rows a replica copies: authority-only rows
// do not count, so they never push pruned_upto past a replica's cursor.
func TestPruneDoesNotCountAuthorityOnly(t *testing.T) {
	ctx := context.Background()
	b, st, p := teamBus(t)
	var plain []api.Event
	for i := range 4 {
		e, err := b.Publish(ctx, Input{Type: evCreated, Subject: "local:x", Project: p})
		if err != nil {
			t.Fatal(err)
		}
		plain = append(plain, e)
		for range 3 {
			if _, err := b.Publish(ctx, Input{Type: "dev.zm.testsecret.granted", Subject: fmt.Sprint(i), Project: p, Data: map[string]any{"names": []string{"N"}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	owner := &authz.Subject{Scope: authz.All()}
	pruneTeam(ctx, st.Writer(p), 3, time.Now().Add(time.Hour))
	if _, _, _, err := b.Changes(ctx, owner, p, plain[1].ID, 0); err != nil {
		t.Errorf("a cursor at the newest three replicable rows' first: %v (authority-only rows counted)", err)
	}
}

// The ledger's own retention: a ledger-only log is bounded by its row cap
// and its age cap (whichever removes more), and in a mixed log it removes
// only ledger rows and never moves pruned_upto.
func TestLedgerRetention(t *testing.T) {
	ctx := context.Background()
	b, st, p := teamBus(t)
	db := st.Writer(p)
	pub := func(typ string, n int) []api.Event {
		var out []api.Event
		for i := range n {
			e, err := b.Publish(ctx, Input{Type: typ, Subject: fmt.Sprint(i), Project: p, Data: map[string]any{"names": []string{"N"}}})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
		return out
	}
	count := func(where string) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	prune := func(keep int, cutoff time.Time) {
		if err := store.Tx(ctx, db, func(tx *sql.Tx) error { return pruneLedgerTx(ctx, tx, keep, cutoff) }); err != nil {
			t.Fatal(err)
		}
	}
	// Ledger only: the row cap.
	led := pub("dev.zm.testsecret.granted", 12)
	prune(5, time.Now().Add(-time.Hour))
	if n := count(`type LIKE 'dev.zm.testsecret.%'`); n != 5 {
		t.Fatalf("after the row cap: %d ledger rows, want 5", n)
	}
	var oldest string
	_ = db.QueryRowContext(ctx, `SELECT min(id) FROM events`).Scan(&oldest)
	if oldest != led[7].ID {
		t.Errorf("the oldest kept is %s, want the 8th (the newest five stay)", oldest)
	}
	// The age cap removes more than the row cap would.
	if _, err := db.ExecContext(ctx, `UPDATE events SET time = ? WHERE id <= ?`, store.Millis(time.Now().Add(-100*24*time.Hour)), led[9].ID); err != nil {
		t.Fatal(err)
	}
	prune(5, time.Now().Add(-90*24*time.Hour))
	if n := count(`type LIKE 'dev.zm.testsecret.%'`); n != 2 {
		t.Fatalf("after the age cap: %d ledger rows, want 2", n)
	}
	// Mixed: plain rows untouched, pruned_upto not moved.
	plain := pub(evCreated, 3)
	pub("dev.zm.testsecret.granted", 4)
	prune(1, time.Now().Add(-time.Hour))
	if n := count(`type LIKE 'dev.zm.testsecret.%'`); n != 1 {
		t.Errorf("mixed: %d ledger rows, want 1", n)
	}
	if n := count(`type = '` + evCreated + `'`); n != 3 {
		t.Errorf("mixed: %d plain rows, want 3", n)
	}
	var upto sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT value FROM events_meta WHERE key = 'pruned_upto'`).Scan(&upto)
	if upto.Valid {
		t.Errorf("the ledger's retention moved pruned_upto to %s", upto.String)
	}
	owner := &authz.Subject{Scope: authz.All()}
	if _, _, _, err := b.Changes(ctx, owner, p, plain[0].ID, 0); err != nil {
		t.Errorf("a replica's cursor after the ledger's retention: %v", err)
	}
}

// The start-up rewrite: old unflagged rows are rewritten once, in one go;
// a second start changes nothing.
func TestSealStoredOnce(t *testing.T) {
	ctx := context.Background()
	b, st, p := teamBus(t)
	for i := range 3 {
		if _, err := b.Publish(ctx, Input{Type: "dev.zm.testsecret.granted", Subject: fmt.Sprint(i), Project: p, Data: map[string]any{"names": []string{"N"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Publish(ctx, Input{Type: "dev.zm.test.plain", Project: p}); err != nil {
		t.Fatal(err)
	}
	if n, err := sealStored(ctx, st.Writer(p)); err != nil || n != 3 {
		t.Fatalf("first start: %d rewritten %v, want 3", n, err)
	}
	if n, err := sealStored(ctx, st.Writer(p)); err != nil || n != 0 {
		t.Fatalf("second start: %d rewritten %v, want 0", n, err)
	}
}
