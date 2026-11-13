package sqlbackup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func seed(t *testing.T, path string, rows int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO t (body) VALUES (?)`, strings.Repeat("row ", 200)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func rowsIn(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A copy holds what was committed, is owner-only, and works from a path
// with spaces (iCloud Drive's is "Mobile Documents").
func TestSnapshotAndCheck(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "Mobile Documents", "a~b")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := seed(t, filepath.Join(dir, "live.db"), 50)
	dst := filepath.Join(dir, "a-1.db")
	info, err := Snapshot(ctx, db, dst, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 || info.Bytes != st.Size() || info.Pages == 0 {
		t.Fatalf("copy %v, info %+v", st.Mode(), info)
	}
	if n := rowsIn(t, dst); n != 50 {
		t.Fatalf("rows %d", n)
	}
	if _, err := os.Stat(dst + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a leftover: %v", err)
	}
	if _, err := Snapshot(ctx, db, dst, 0); err == nil {
		t.Fatal("a second copy under one name replaced the first")
	}
}

// Check refuses a truncated file, a corrupted one and one that is not a
// database, and a failed copy is never published.
func TestCheckRefusesDamage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := seed(t, filepath.Join(dir, "live.db"), 200)
	good := filepath.Join(dir, "good.db")
	if _, err := Snapshot(ctx, db, good, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"truncated": b[:len(b)/2],
		"garbage":   []byte(strings.Repeat("not a database ", 1000)),
		"empty":     {},
	}
	corrupt := append([]byte(nil), b...)
	for i := len(corrupt) / 2; i < len(corrupt)/2+8192; i++ {
		corrupt[i] = 0xff
	}
	cases["corrupted"] = corrupt
	for name, data := range cases {
		p := filepath.Join(dir, name+".db")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Check(ctx, p); err == nil {
			t.Errorf("%s passed Check", name)
		}
	}
	if _, err := Check(ctx, filepath.Join(dir, "missing.db")); err == nil {
		t.Error("a missing file passed Check")
	}
	if _, err := Check(ctx, good); err != nil {
		t.Errorf("the good copy: %v", err)
	}
}

func TestPlausible(t *testing.T) {
	for _, c := range []struct {
		n, prev int64
		ok      bool
	}{{0, 0, false}, {1, 0, true}, {100, 100, true}, {25, 100, true}, {24, 100, false}, {0, 100, false}, {1000, 100, true}} {
		if err := Plausible(c.n, c.prev); (err == nil) != c.ok {
			t.Errorf("Plausible(%d, %d) = %v", c.n, c.prev, err)
		}
	}
}

// A copy far smaller than the last good one is refused before it has a name,
// so a store emptied by mistake cannot rotate good copies out.
func TestSnapshotRefusesAShrink(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := seed(t, filepath.Join(dir, "live.db"), 10)
	dst := filepath.Join(dir, "a-1.db")
	if _, err := Snapshot(ctx, db, dst, 1<<40); err == nil {
		t.Fatal("no error")
	}
	names, _ := os.ReadDir(dir)
	for _, n := range names {
		if strings.HasPrefix(n.Name(), "a-1") {
			t.Fatalf("left %s", n.Name())
		}
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

func touch(t *testing.T, dir string, ns ...string) {
	t.Helper()
	for _, n := range ns {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Retention keeps the newest keep, and keep 0 still keeps the newest.
func TestPruneKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "a-1.db", "a-2.db", "a-3.db", "a-4.db", "a-5.db.tmp", "b-1.db", "a-1.other")
	if err := Prune(dir, "a-", ".db", 2); err != nil {
		t.Fatal(err)
	}
	want := []string{"a-1.other", "a-3.db", "a-4.db", "a-5.db.tmp", "b-1.db"}
	if got := names(t, dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("after keep 2: %v", got)
	}
	if err := Prune(dir, "a-", ".db", 0); err != nil {
		t.Fatal(err)
	}
	want = []string{"a-1.other", "a-4.db", "a-5.db.tmp", "b-1.db"}
	if got := names(t, dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("after keep 0: %v", got)
	}
	if err := Prune(filepath.Join(dir, "gone"), "a-", ".db", 1); err != nil {
		t.Fatal(err)
	}
}

// KeepOnly refuses to run without the copy it keeps, so it can never end
// with none, and it removes a copy dated after the one it keeps (a clock
// that went back must not keep a stale copy past a redaction).
func TestKeepOnly(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "a-1.db", "a-2.db", "a-9.db")
	if err := KeepOnly(dir, "a-", ".db", "a-5.db"); err == nil {
		t.Fatal("KeepOnly ran without the copy to keep")
	}
	if got := names(t, dir); len(got) != 3 {
		t.Fatalf("removed files: %v", got)
	}
	if err := KeepOnly(dir, "a-", ".db", "a-2.db"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !reflect.DeepEqual(got, []string{"a-2.db"}) {
		t.Fatalf("left %v", got)
	}
}

func TestSweepRemovesOnlyLeftovers(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "a-1.db", "a-2.db.tmp", "a-2.db.gz.tmp", "b-1.db.tmp")
	if err := Sweep(dir, "a-"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !reflect.DeepEqual(got, []string{"a-1.db", "b-1.db.tmp"}) {
		t.Fatalf("left %v", got)
	}
}

// VACUUM INTO must not block the source's writer: with no busy timeout at
// all, every write during a copy commits at once.
func TestSnapshotDoesNotTakeTheWriter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	copier := seed(t, src, 4000)
	writer, err := sql.Open("sqlite", "file:"+src+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	done := make(chan error, 1)
	go func() {
		_, err := Snapshot(ctx, copier, filepath.Join(dir, "a-1.db"), 0)
		done <- err
	}()
	writes := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if writes == 0 {
				t.Fatal("the copy ended before any write: the test proves nothing")
			}
			return
		default:
		}
		if _, err := writer.Exec(`INSERT INTO t (body) VALUES ('during')`); err != nil {
			t.Fatalf("write %d during the copy: %v", writes, err)
		}
		writes++
	}
}

// spyDB checks the output file as VACUUM INTO is about to write it.
type spyDB struct {
	*sql.DB
	before func(path string) error
}

func (s spyDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if err := s.before(args[0].(string)); err != nil {
		return nil, err
	}
	return s.DB.ExecContext(ctx, q, args...)
}

// A whole store is never on disk with the default mode.
func TestCopyIsOwnerOnlyWhileItIsWritten(t *testing.T) {
	dir := t.TempDir()
	saw := false
	db := spyDB{seed(t, filepath.Join(dir, "live.db"), 5), func(path string) error {
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != 0o600 || st.Size() != 0 {
			t.Errorf("before the copy: %v, %d bytes", st.Mode(), st.Size())
		}
		saw = true
		return nil
	}}
	if _, err := Snapshot(context.Background(), db, filepath.Join(dir, "a-1.db"), 0); err != nil {
		t.Fatal(err)
	}
	if !saw {
		t.Fatal("the spy did not run")
	}
}

// A disk that fills during the copy leaves nothing that looks like a backup,
// and the older copy alone.
func TestDiskFullLeavesNoCopy(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "a-1.db")
	db := spyDB{seed(t, filepath.Join(dir, "live.db"), 5), func(path string) error {
		if err := os.WriteFile(path, []byte("half a database"), 0o600); err != nil {
			t.Fatal(err)
		}
		return syscall.ENOSPC
	}}
	_, err := Snapshot(context.Background(), db, filepath.Join(dir, "a-2.db"), 0)
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err %v", err)
	}
	if got, _ := List(dir, "a-", ".db"); !reflect.DeepEqual(got, []string{"a-1.db"}) {
		t.Fatalf("copies %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "a-2.db.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial file stayed: %v", err)
	}
}

// Too little room is refused before anything is written, with 1.1x the source
// as the bar.
func TestFreeSpaceCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := seed(t, filepath.Join(dir, "live.db"), 200)
	var pages, size int64
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	store := uint64(pages * size)
	old := freeBytes
	t.Cleanup(func() { freeBytes = old })
	freeBytes = func(string) (uint64, error) { return store, nil } // the store's size is not enough
	_, err := Snapshot(ctx, db, filepath.Join(dir, "a-1.db"), 0)
	if !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("err %v", err)
	}
	for _, n := range names(t, dir) {
		if strings.HasPrefix(n, "a-1") {
			t.Fatalf("left %s", n)
		}
	}
	freeBytes = func(string) (uint64, error) { return store * 2, nil }
	if _, err := Snapshot(ctx, db, filepath.Join(dir, "a-1.db"), 0); err != nil {
		t.Fatal(err)
	}
	freeBytes = func(string) (uint64, error) { return 0, errors.New("no statfs here") }
	if _, err := Snapshot(ctx, db, filepath.Join(dir, "a-2.db"), 0); err != nil {
		t.Fatalf("a filesystem that will not say: %v", err)
	}
}
