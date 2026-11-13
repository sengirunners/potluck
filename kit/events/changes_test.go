package events

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Event types the tests publish, as an orchestration module names them:
// the work ones carry workPrefix, so an area move leaves a tombstone.
const (
	evCreated   = workPrefix + "created"
	evCommented = workPrefix + "commented"
	evChanged   = workPrefix + "changed"
	evMailSent  = "dev.zm.mail.sent"
	evDecided   = "dev.zm.decision.recorded"
)

func teamBus(t *testing.T) (*Bus, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := ids.New()
	if _, err := st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTeam(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	b, err := New(ctx, st, "mini")
	if err != nil {
		t.Fatal(err)
	}
	return b, st, p
}

func guestOf(project, area string) *authz.Subject {
	sc := authz.FromTuples([]authz.Tuple{{Subject: "g", Relation: authz.Guest, Object: authz.AreaObject(project, area)}}, nil)
	return &authz.Subject{Scope: sc, Address: "human:victor"}
}

// A guest's changelog: only its area's rows; the cursor only ever moves to
// an id it was sent (never to a hidden one, so gaps reveal nothing); an item
// that leaves its area comes as one tombstone, and nothing after.
func TestChangesGuestCursorAndTombstones(t *testing.T) {
	ctx := context.Background()
	b, _, p := teamBus(t)
	pub := func(typ, subject, area string) api.Event {
		e, err := b.Publish(ctx, Input{Type: typ, Subject: subject, Project: p, Area: area, Data: map[string]string{"id": subject}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e1 := pub(evCreated, "local:aaaaaa", "sprout")
	pub(evCreated, "local:bbbbbb", "billing") // hidden
	pub(evCommented, "local:aaaaaa", "sprout")
	pub(evChanged, "local:aaaaaa", "billing") // moves out: a tombstone
	pub(evChanged, "local:aaaaaa", "billing") // still out: nothing
	last := pub(evCreated, "local:cccccc", "billing")

	g := guestOf(p, "sprout")
	evs, next, more, err := b.Changes(ctx, g, p, "", 0)
	if err != nil || more {
		t.Fatal(err, more)
	}
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
		if e.Area == "billing" {
			t.Fatalf("guest got a billing row: %+v", e)
		}
	}
	want := []string{evCreated, evCommented, EventGone}
	if len(types) != len(want) {
		t.Fatalf("types %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types %v, want %v", types, want)
		}
	}
	if evs[2].Subject != "local:aaaaaa" || next != evs[2].ID || next == last.ID {
		t.Fatalf("tombstone %+v, cursor %s (last hidden %s)", evs[2], next, last.ID)
	}
	// Resuming from the cursor: nothing new, and the cursor stays put (it
	// never jumps to the hidden tail).
	evs, next2, _, err := b.Changes(ctx, g, p, next, 0)
	if err != nil || len(evs) != 0 || next2 != next {
		t.Fatalf("resume: %v %d %s", err, len(evs), next2)
	}
	// LastVisible is a snapshot's cursor: the newest id the guest may read.
	lv, err := b.LastVisible(ctx, g, p)
	if err != nil || lv == last.ID || lv < e1.ID {
		t.Fatalf("last visible %s (hidden last %s): %v", lv, last.ID, err)
	}
	// A whole-project reader gets every row and no tombstones.
	owner := &authz.Subject{Scope: authz.All()}
	evs, next, _, err = b.Changes(ctx, owner, p, "", 0)
	if err != nil || len(evs) != 6 || next != last.ID {
		t.Fatalf("owner: %v %d %s", err, len(evs), next)
	}
	// Paging: limit 2 says more and resumes exactly.
	page, n1, more, err := b.Changes(ctx, owner, p, "", 2)
	if err != nil || len(page) != 2 || !more || n1 != page[1].ID {
		t.Fatalf("page: %v %d %v %s", err, len(page), more, n1)
	}
}

// Pruning records how far it cut, and a cursor below that is 410 resync;
// one at or above it is fine.
func TestChangesResyncAfterPrune(t *testing.T) {
	ctx := context.Background()
	b, st, p := teamBus(t)
	var evs []api.Event
	for i := 0; i < 10; i++ {
		e, err := b.Publish(ctx, Input{Type: evCreated, Subject: "local:x", Project: p})
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	owner := &authz.Subject{Scope: authz.All()}
	if _, _, _, err := b.Changes(ctx, owner, p, evs[0].ID, 0); err != nil {
		t.Fatalf("before prune: %v", err)
	}
	pruneTeam(ctx, st.Writer(p), 4, time.Now().Add(time.Hour))
	if _, _, _, err := b.Changes(ctx, owner, p, evs[0].ID, 0); !api.IsCode(err, api.CodeResync) {
		t.Fatalf("old cursor after prune: %v", err)
	}
	got, _, _, err := b.Changes(ctx, owner, p, evs[5].ID, 0)
	if err != nil || len(got) != 4 {
		t.Fatalf("recent cursor after prune: %v %d", err, len(got))
	}
}

// Restoring an older log loses a replica's cursor. Checking only the current
// tail misses the rollback once newer changes have overtaken the lost id.
func TestChangesResyncAfterRestore(t *testing.T) {
	ctx := context.Background()
	b, st, p := teamBus(t)
	owner := &authz.Subject{Scope: authz.All()}
	pub := func() api.Event {
		t.Helper()
		e, err := b.Publish(ctx, Input{Type: evCreated, Subject: "local:x", Project: p})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	kept, lost := pub(), pub()
	if _, err := st.Writer(p).ExecContext(ctx, `DELETE FROM events WHERE id > ?`, kept.ID); err != nil {
		t.Fatal(err)
	}
	checkLost := func() {
		t.Helper()
		if _, _, _, err := b.Changes(ctx, owner, p, lost.ID, 0); !api.IsCode(err, api.CodeResync) {
			t.Fatalf("lost cursor after restore: %v", err)
		}
	}
	checkLost()
	newer := pub()
	if newer.ID <= lost.ID {
		t.Fatalf("new tail %s did not overtake lost cursor %s", newer.ID, lost.ID)
	}
	checkLost()
	if got, next, _, err := b.Changes(ctx, owner, p, kept.ID, 0); err != nil || len(got) != 1 || next != newer.ID {
		t.Fatalf("retained cursor: %v, %d events, next %s", err, len(got), next)
	}
	if _, err := st.Writer(p).ExecContext(ctx, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	checkLost()
	if got, next, _, err := b.Changes(ctx, owner, p, "", 0); err != nil || len(got) != 0 || next != "" {
		t.Fatalf("empty cursor on empty restored log: %v, %d events, next %s", err, len(got), next)
	}
}

func TestChangesHiddenAndMissingCursorsAnswerAlike(t *testing.T) {
	ctx := context.Background()
	b, _, p := teamBus(t)
	g := guestOf(p, "sprout")
	for _, in := range []Input{
		{Type: evCreated, Subject: "local:hidden", Area: "billing"},
		{Type: evMailSent, Subject: "private", Area: "sprout", Audience: &Audience{Need: authz.Admin}},
	} {
		in.Project = p
		e, err := b.Publish(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{e.ID, ids.New()} {
			if _, _, _, err := b.Changes(ctx, g, p, id, 0); err != ErrResync {
				t.Fatalf("hidden or missing cursor %s: %v", id, err)
			}
		}
	}
	party, err := b.Publish(ctx, Input{Type: evMailSent, Subject: "party", Project: p, Area: "sprout",
		Audience: &Audience{Need: authz.Admin, Parties: []string{g.Address}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, next, _, err := b.Changes(ctx, g, p, party.ID, 0); err != nil || len(got) != 0 || next != party.ID {
		t.Fatalf("audience party cursor: %v, %d events, next %s", err, len(got), next)
	}
}

// Writers racing on a team file past the prune interval (every 500 events)
// never deadlock: the prune runs inside the committing transaction, not on
// the writer under the bus lock (the round C load test found that one).
func TestTeamPruneUnderConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	b, _, p := teamBus(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 250; i++ {
					if _, err := b.Publish(ctx, Input{Type: "dev.zm.test", Subject: "x", Project: p}); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("team writers deadlocked")
	}
}

func TestWriteCursorRequiresVisibleAudienceAndScope(t *testing.T) {
	ctx := context.Background()
	b, _, p := teamBus(t)
	participant := &authz.Subject{Scope: authz.All(), Address: "human:carl"}
	input := Input{Type: evMailSent, Subject: "fixture-private-mail", Project: p, Area: "sprout", Data: map[string]string{"body": "invented private"}, Audience: &Audience{ParticipantsOnly: true, Parties: []string{participant.Address}}}
	ev, err := b.Publish(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*authz.Subject{
		{Scope: authz.All(), Address: "human:unrelated"},
		{Scope: authz.All(), Address: "host-b/1234abcd"},
		{Scope: authz.All(), Address: participant.Address, Node: "old-replica"},
		{Scope: guestOf(p, "billing").Scope, Address: participant.Address},
	} {
		if got, err := b.LastForVisible(ctx, sub, p, input.Subject); err != nil || got != "" {
			t.Fatal("hidden subject cursor", got, err)
		}
		if got, err := b.VisibleEventID(ctx, sub, p, ev.ID); err != nil || got != "" {
			t.Fatal("hidden cached cursor", got, err)
		}
	}
	if got, err := b.LastForVisible(ctx, participant, p, input.Subject); err != nil || got != ev.ID {
		t.Fatal("participant lost cursor", got, err)
	}
	participant.Node, participant.PersonalMail = "upgraded-replica", true
	if got, err := b.VisibleEventID(ctx, participant, p, ev.ID); err != nil || got != ev.ID {
		t.Fatal("upgraded participant lost cursor", got, err)
	}
}
