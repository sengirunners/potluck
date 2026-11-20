package runtarget

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/runtarget/rt"
	_ "modernc.org/sqlite"
)

// Spend never falls, a reclaimed handle is never billed, terminal states
// stick, and the budget sums the calendar period's billed spend (SPEC 20).
func TestSpendBookkeeping(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, handlesDDL+";"+boxDDL+";"+areaDDL+";"+persistDDL+";"+startedSpendDDL+";"+cancelledDDL+";"+selfRenewDDL+";"+publishDDL); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC) // a Friday
	at := func(d time.Duration) string { return now.Add(d).Format(tsLayout) }
	sp := func(n float64) *computeapi.RunTargetSpend {
		return &computeapi.RunTargetSpend{Unit: "cpu_s", Amount: n}
	}
	for _, k := range []string{"a", "b", "old"} {
		created := at(0)
		if k == "old" {
			created = at(-48 * time.Hour)
		}
		if err := insertAcquiring(ctx, db, "vm", k, "{}", "run", "human:ana", "main", "c", "probe.sh", "", created, false); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		key string
		u   upd
	}{
		{"a", upd{handle: "run:vm/a", state: stateRunning, spend: sp(5)}},
		{"a", upd{state: stateRunning, spend: sp(2)}}, // lower: ignored
		{"a", upd{state: stateEnded, exit: new(int), spend: sp(7)}},
		{"a", upd{state: stateRunning}}, // after the end: ignored
		{"b", upd{handle: "run:vm/b", state: stateRunning, spend: sp(4)}},
		{"b", upd{state: stateReclaimed, spend: sp(6)}},
		{"old", upd{handle: "run:vm/old", state: stateEnded, spend: sp(50)}},
	}
	for _, s := range steps {
		if _, err := update(ctx, db, "vm", s.key, s.u, at(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	a, _, _ := byRequest(ctx, db, "vm", "a")
	if a.State != stateEnded || a.Spend == nil || a.Spend.Amount != 7 || a.Exit == nil || a.Ended == "" {
		t.Errorf("a: %+v", a.RunTargetHandle)
	}
	b, _, _ := byRequest(ctx, db, "vm", "b")
	if b.State != stateReclaimed || b.Spend.Amount != 6 {
		t.Errorf("b: %+v", b.RunTargetHandle)
	}
	day, err := periodSpend(ctx, db, "vm", "cpu_s", rt.PeriodStart("day", now))
	if err != nil || day != 7 {
		t.Errorf("today's billed spend: %v %v (want 7: b was reclaimed, old is yesterday's)", day, err)
	}
	week, _ := periodSpend(ctx, db, "vm", "cpu_s", rt.PeriodStart("week", now))
	if week != 57 {
		t.Errorf("this week's billed spend: %v, want 57", week)
	}
	if n, _ := liveCount(ctx, db, "vm", "human:ana"); n != 0 {
		t.Errorf("live handles: %d", n)
	}
	for p, want := range map[string]string{"day": "2026-11-06", "week": "2026-11-02", "month": "2026-11-01"} {
		if got := rt.PeriodStart(p, now).Format("2006-01-02"); got != want {
			t.Errorf("PeriodStart(%s) = %s, want %s", p, got, want)
		}
	}
}

// A box booted off every pin is refused exec and swept; a pinned one is not.
func TestOffPin(t *testing.T) {
	pin := "img@sha256:" + strings.Repeat("a", 64)
	b := computeapi.RunTarget{Kind: computeapi.RunTargetKindBox, Images: []string{pin}}
	box := func(d string) row { var r row; r.ImageDigest = d; return r }
	if offPin(b, box("sha256:"+strings.Repeat("a", 64))) || offPin(b, box("")) {
		t.Error("a pinned (or not yet booted) box counted off its pin")
	}
	if !offPin(b, box("sha256:"+strings.Repeat("b", 64))) {
		t.Error("a drifted box counted as pinned")
	}
	if offPin(computeapi.RunTarget{Kind: computeapi.RunTargetKindRun}, box("sha256:x")) {
		t.Error("a run counted as a box")
	}
}

// endedRows answers ended handles at or after since, oldest end first.
func TestEndedRows(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, handlesDDL+";"+boxDDL+";"+areaDDL+";"+persistDDL+";"+startedSpendDDL+";"+cancelledDDL+";"+selfRenewDDL+";"+publishDDL); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	at := func(m int) string { return base.Add(time.Duration(m) * time.Minute).Format(tsLayout) }
	for _, k := range []string{"late", "early", "live", "old"} {
		area := ""
		if k == "late" {
			area = "team1"
		}
		if err := insertAcquiring(ctx, db, "box", k, "{}", "box", "human:ana", "", "", "", area, at(0), false); err != nil {
			t.Fatal(err)
		}
		u := upd{handle: "box:box/" + k, state: stateReady}
		if _, err := update(ctx, db, "box", k, u, at(0)); err != nil {
			t.Fatal(err)
		}
		if k == "live" {
			continue
		}
		end := map[string]int{"late": 30, "early": 10, "old": -60}[k]
		if _, err := update(ctx, db, "box", k, upd{state: stateReleased}, at(end)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := endedRows(ctx, db, rt.EndedOnly{}, at(0), 10)
	if err != nil || len(rows) != 2 || rows[0].Handle != "box:box/early" || rows[1].Handle != "box:box/late" {
		t.Fatalf("ended since: %+v %v", rows, err)
	}
	if rows, _ := endedRows(ctx, db, rt.EndedOnly{}, at(-120), 1); len(rows) != 1 || rows[0].Handle != "box:box/old" {
		t.Errorf("limit and order: %+v", rows)
	}
	if rows, err := endedRows(ctx, db, rt.EndedOnly{Area: "team1", Kind: "box"}, at(-120), 10); err != nil || len(rows) != 1 || rows[0].Handle != "box:box/late" {
		t.Errorf("one area's ends: %+v %v", rows, err)
	}
}

// Fixed-width times keep string order equal to time order within a second,
// and the migration's normalize rewrites RFC3339Nano rows to match.
func TestEndedOrderWithinASecond(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, handlesDDL+";"+boxDDL+";"+areaDDL+";"+persistDDL+";"+startedSpendDDL+";"+cancelledDDL+";"+selfRenewDDL+";"+publishDDL); err != nil {
		t.Fatal(err)
	}
	sec := time.Date(2026, 11, 6, 12, 0, 5, 0, time.UTC)
	for k, frac := range map[string]time.Duration{"a": 120 * time.Millisecond, "b": 500 * time.Millisecond, "c": 100 * time.Millisecond, "z": 0} {
		if err := insertAcquiring(ctx, db, "box", k, "{}", "box", "human:ana", "", "", "", "", sec.Format(tsLayout), false); err != nil {
			t.Fatal(err)
		}
		if _, err := update(ctx, db, "box", k, upd{handle: "box:box/" + k, state: stateReady}, sec.Format(tsLayout)); err != nil {
			t.Fatal(err)
		}
		if _, err := update(ctx, db, "box", k, upd{state: stateReleased}, sec.Add(frac).Format(tsLayout)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := endedRows(ctx, db, rt.EndedOnly{}, sec.Format(tsLayout), 10)
	var got []string
	for _, r := range rows {
		got = append(got, r.Handle)
	}
	if err != nil || strings.Join(got, ",") != "box:box/z,box:box/c,box:box/a,box:box/b" {
		t.Errorf("order within a second from a whole-second since: %v %v", got, err)
	}
	// A row written the old way (RFC3339Nano, trimmed) is rewritten to the layout.
	if _, err := db.ExecContext(ctx, `UPDATE run_handles SET ended=? WHERE request_id='b'`, sec.Add(500*time.Millisecond).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, tsNormalize); err != nil {
		t.Fatal(err)
	}
	var ended string
	_ = db.QueryRowContext(ctx, `SELECT ended FROM run_handles WHERE request_id='b'`).Scan(&ended)
	if ended != sec.Add(500*time.Millisecond).Format(tsLayout) {
		t.Errorf("normalized: %q", ended)
	}
}

// A publish is kept once, from the update that ends the row as ended: not
// from one that leaves it running or ends it any other way, and never
// replaced by a later one.
func TestPublishIsKeptOnceAtTheEnd(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, handlesDDL+";"+boxDDL+";"+areaDDL+";"+persistDDL+";"+startedSpendDDL+";"+cancelledDDL+";"+selfRenewDDL+";"+publishDDL); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC).Format(tsLayout)
	for _, k := range []string{"ended", "released", "twice"} {
		if err := insertAcquiring(ctx, db, "vm", k, "{}", "run", "human:ana", "main", "c", "probe.sh", "", at, false); err != nil {
			t.Fatal(err)
		}
	}
	pub := func(s string) json.RawMessage { return json.RawMessage(s) }
	for _, s := range []struct {
		key string
		u   upd
	}{
		{"ended", upd{handle: "run:vm/ended", state: stateRunning, publish: pub(`{"early":1}`)}},
		{"ended", upd{state: stateEnded, publish: pub(`{"a":1}`)}},
		{"released", upd{handle: "run:vm/released", state: stateReleased, publish: pub(`{"b":1}`)}},
		{"twice", upd{handle: "run:vm/twice", state: stateEnded, publish: pub(`{"c":1}`)}},
		{"twice", upd{state: stateEnded, publish: pub(`{"c":2}`)}},
	} {
		if _, err := update(ctx, db, "vm", s.key, s.u, at); err != nil {
			t.Fatal(err)
		}
	}
	for k, want := range map[string]string{"ended": `{"a":1}`, "released": "", "twice": `{"c":1}`} {
		r, _, err := byRequest(ctx, db, "vm", k)
		if err != nil || string(r.Publish) != want {
			t.Errorf("%s: publish %q (%v), want %q", k, r.Publish, err, want)
		}
	}
}
