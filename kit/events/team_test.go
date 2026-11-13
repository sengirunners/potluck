package events

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// A team project's log (SPEC 17.3): an id is minted while the file's write
// lock is held and committed before the next one is minted there, so a
// reader resuming at any id it has seen never meets an earlier id later.
// Writers race on the team file (plain publishes and TeamTx changes) while
// a reader keeps resuming from the last id it saw.
func TestTeamEventIDsInCommitOrder(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team, solo := ids.New(), ids.New()
	for _, p := range []string{team, solo} {
		if _, err := st.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetTeam(ctx, team, true); err != nil {
		t.Fatal(err)
	}
	bus, err := New(ctx, st, "mini")
	if err != nil {
		t.Fatal(err)
	}
	owner := &authz.Subject{Scope: authz.All()}
	const writers, each = 8, 60
	var wg sync.WaitGroup
	var done atomic.Bool
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				var err error
				switch i % 3 {
				case 0:
					_, err = bus.Publish(ctx, Input{Type: "dev.zm.test", Subject: fmt.Sprint(w, i), Project: team})
				case 1:
					err = bus.TeamTx(ctx, team, func(tx *sql.Tx, emit func(Input)) error {
						emit(Input{Type: "dev.zm.test", Subject: fmt.Sprint(w, i, "a")})
						emit(Input{Type: "dev.zm.test", Subject: fmt.Sprint(w, i, "b")})
						return nil
					}, nil)
				default: // another file's publishes interleave
					_, err = bus.Publish(ctx, Input{Type: "dev.zm.test", Subject: fmt.Sprint(w, i), Project: solo})
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); done.Store(true) }()
	seen := map[string]bool{}
	last := ""
	for {
		fin := done.Load()
		evs, err := bus.Since(ctx, owner, last, Filter{Project: team}, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.ID <= last || seen[e.ID] {
				t.Fatalf("event %s after %s", e.ID, last)
			}
			seen[e.ID], last = true, e.ID
		}
		if fin && len(evs) == 0 {
			break
		}
	}
	var n int
	if err := st.Writer(team).QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// Every committed event was seen exactly once, resuming each time from
	// the last id seen: none committed behind the cursor.
	if want := writers * (each / 3) * 3; len(seen) != n || n != want {
		t.Fatalf("saw %d of %d events (want %d)", len(seen), n, want)
	}
}
