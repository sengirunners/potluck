package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

func cacheDB(t *testing.T, conns int) *sql.DB {
	t.Helper()
	db, err := openDB(context.Background(), t.TempDir()+"/c.db", base(KindProject), conns, soloBusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t (v) VALUES ('a'), ('b'), ('c');`); err != nil {
		t.Fatal(err) // a script: runs uncached
	}
	return db
}

func rawConn(t *testing.T, c *sql.Conn) *cachedConn {
	t.Helper()
	var cc *cachedConn
	if err := c.Raw(func(dc any) error { cc = dc.(*cachedConn); return nil }); err != nil {
		t.Fatal(err)
	}
	return cc
}

func TestStmtCacheReuses(t *testing.T) {
	ctx := context.Background()
	db := cacheDB(t, 1)
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cc := rawConn(t, c)
	hits, misses := cc.hits, cc.misses
	for i := range 5 {
		var v string
		if err := c.QueryRowContext(ctx, `SELECT v FROM t WHERE id = ?`, i%3+1).Scan(&v); err != nil || v != string(rune('a'+i%3)) {
			t.Fatalf("row %d: %q %v", i, v, err)
		}
	}
	if cc.misses-misses != 1 || cc.hits-hits != 4 {
		t.Fatalf("misses %d hits %d, want 1 and 4", cc.misses-misses, cc.hits-hits)
	}
}

// The same SQL run again while its rows are open (a nested loop) must not
// reset the outer statement.
func TestStmtCacheNestedSameQuery(t *testing.T) {
	ctx := context.Background()
	db := cacheDB(t, 1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	q := `SELECT id, v FROM t WHERE id >= ? ORDER BY id`
	outer, err := tx.QueryContext(ctx, q, 1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for outer.Next() {
		var id int
		var v string
		if err := outer.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		inner, err := tx.QueryContext(ctx, q, id)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for inner.Next() {
			n++
		}
		inner.Close()
		got = append(got, fmt.Sprintf("%s%d", v, n))
	}
	if err := outer.Err(); err != nil {
		t.Fatal(err)
	}
	outer.Close()
	if fmt.Sprint(got) != "[a3 b2 c1]" {
		t.Fatalf("nested: %v", got)
	}
}

// Closed rows release their read snapshot: a write on another connection is
// seen by the next run of the cached statement; writes through cached
// statements commit.
func TestStmtCacheSeesWrites(t *testing.T) {
	ctx := context.Background()
	db := cacheDB(t, 2)
	reader, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count := func() int {
		var n int
		if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 3 {
		t.Fatal("start")
	}
	for i := range 3 {
		if _, err := db.ExecContext(ctx, `INSERT INTO t (v) VALUES (?)`, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != 4+i {
			t.Fatalf("after insert %d: count %d", i, n)
		}
	}
}

// More distinct statements than the cache holds: the oldest are finalized,
// every query still answers.
func TestStmtCacheEvicts(t *testing.T) {
	ctx := context.Background()
	db := cacheDB(t, 1)
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range stmtCacheSize + 50 {
		var n int
		if err := c.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) + %d FROM t`, i)).Scan(&n); err != nil || n != 3+i {
			t.Fatalf("query %d: %d %v", i, n, err)
		}
	}
	if cc := rawConn(t, c); len(cc.stmts) != stmtCacheSize || cc.lru.Len() != stmtCacheSize {
		t.Fatalf("cache holds %d (%d), want %d", len(cc.stmts), cc.lru.Len(), stmtCacheSize)
	}
	if cacheable(`SELECT 1; SELECT 2`) || !cacheable(`SELECT 1;`) {
		t.Fatal("cacheable")
	}
}

// A cached statement's rows must report the same column metadata as the
// driver's own (the wrapper once hid modernc's optional interfaces, so
// rows.ColumnTypes() came back empty).
func TestStmtCacheColumnTypes(t *testing.T) {
	ctx := context.Background()
	db := cacheDB(t, 1)
	const q = `SELECT id, v FROM t WHERE id = ?`
	types := func() []*sql.ColumnType {
		rows, err := db.QueryContext(ctx, q, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		ct, err := rows.ColumnTypes()
		if err != nil {
			t.Fatal(err)
		}
		return ct
	}
	types() // first run prepares; the second is served from the cache
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hits := rawConn(t, c).hits
	c.Close()
	ct := types()
	c, err = db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rawConn(t, c).hits == hits {
		t.Fatal("second query was not served from the cache")
	}
	c.Close()
	want := map[string]string{"id": "INTEGER", "v": "TEXT"}
	for _, col := range ct {
		if got := col.DatabaseTypeName(); got != want[col.Name()] {
			t.Errorf("column %s: DatabaseTypeName %q, want %q", col.Name(), got, want[col.Name()])
		}
		if col.ScanType() == reflect.TypeFor[any]() {
			t.Errorf("column %s: scan type is the any fallback, the driver's was hidden", col.Name())
		}
		if _, ok := col.Nullable(); !ok {
			t.Errorf("column %s: Nullable not reported", col.Name())
		}
	}
}
