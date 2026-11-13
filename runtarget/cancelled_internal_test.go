package runtarget

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// eachCancelledAcquiring pages: a pass that cannot settle the first rows (an
// unbound target, a failing driver) still reaches the ones after them.
func TestCancelledAcquiringPages(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, handlesDDL+";"+boxDDL+";"+areaDDL+";"+persistDDL+";"+startedSpendDDL+";"+cancelledDDL+";"+selfRenewDDL); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour).Format(tsLayout)
	const n = 250
	for i := range n {
		rid := fmt.Sprintf("r%03d", i)
		if err := insertAcquiring(ctx, db, "vm", rid, "{}", "box", "human:ana", "", "", "", "", old, false); err != nil {
			t.Fatal(err)
		}
		if err := markCancelled(ctx, db, "vm", rid); err != nil {
			t.Fatal(err)
		}
	}
	// Not cancelled, and too new: never listed.
	if err := insertAcquiring(ctx, db, "vm", "plain", "{}", "box", "human:ana", "", "", "", "", old, false); err != nil {
		t.Fatal(err)
	}
	if err := insertAcquiring(ctx, db, "vm", "new", "{}", "box", "human:ana", "", "", "", "", now.Format(tsLayout), false); err != nil {
		t.Fatal(err)
	}
	if err := markCancelled(ctx, db, "vm", "new"); err != nil {
		t.Fatal(err)
	}
	before := now.Add(-time.Minute).Format(tsLayout)
	var got []string
	// An early stop ends the walk; otherwise it reaches every row, past a page.
	eachCancelledAcquiring(ctx, db, before, cancelledPage, func(rw row) bool {
		got = append(got, rw.RequestID)
		return true
	})
	if len(got) != n {
		t.Fatalf("paged %d rows, want the %d cancelled ones past the cutoff", len(got), n)
	}
	for i, id := range got {
		if want := fmt.Sprintf("r%03d", i); id != want {
			t.Fatalf("row %d is %s, want %s (in order, none twice)", i, id, want)
		}
	}
	stopped := 0
	eachCancelledAcquiring(ctx, db, before, cancelledPage, func(row) bool { stopped++; return stopped < 120 })
	if stopped != 120 {
		t.Errorf("a walk stopped at 120 rows ran %d", stopped)
	}
}
