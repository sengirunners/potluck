package store

import (
	"container/list"
	"context"
	"database/sql/driver"
	"reflect"
	"strings"
)

// Every query a module runs goes through a per-connection prepared-statement
// cache, so the hot paths (session lookups, inbox, notice, leases, auth) parse
// their SQL once per connection instead of on every call (a prepare is about
// two thirds of a small query's cost in modernc sqlite). It sits under
// database/sql, so modules keep calling db.QueryContext/ExecContext with
// plain SQL; scope SQL repeats per scope shape, so it caches too.
//
// database/sql never uses one driver.Conn from two goroutines at once (it
// holds the conn's lock around every call, Rows.Next and Rows.Close
// included), so the cache needs no lock of its own. A statement whose rows
// are still open is busy: running the same SQL again on that connection (a
// nested query in a loop) prepares a one-off statement instead of resetting
// the open one.

// stmtCacheSize bounds the statements kept per connection (least recently
// used are finalized first).
const stmtCacheSize = 128

type cachingConnector struct{ driver.Connector }

func (c cachingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return newCachedConn(conn), nil
}

// innerConn is what modernc's connection implements and database/sql uses.
type innerConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type cachedStmt struct {
	query string
	stmt  driver.Stmt
	busy  bool // its rows are open
	el    *list.Element
}

type cachedConn struct {
	innerConn
	stmts map[string]*cachedStmt
	lru   *list.List // front = most recently used
	// hits and misses are for tests.
	hits, misses int
}

func newCachedConn(c driver.Conn) driver.Conn {
	ic, ok := c.(innerConn)
	if !ok {
		return c // a driver without the interfaces: no cache
	}
	return &cachedConn{innerConn: ic, stmts: map[string]*cachedStmt{}, lru: list.New()}
}

// cacheable: one statement (a script is re-parsed per run anyway).
func cacheable(q string) bool {
	return len(q) <= 8<<10 && !strings.Contains(strings.TrimRight(q, "; \t\r\n"), ";")
}

// get returns a cached, idle statement for q (preparing it if new), or nil
// when q must run uncached.
func (c *cachedConn) get(ctx context.Context, q string) (*cachedStmt, error) {
	if st, ok := c.stmts[q]; ok {
		if st.busy {
			return nil, nil
		}
		c.hits++
		c.lru.MoveToFront(st.el)
		return st, nil
	}
	if !cacheable(q) {
		return nil, nil
	}
	s, err := c.innerConn.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	c.misses++
	st := &cachedStmt{query: q, stmt: s}
	st.el = c.lru.PushFront(st)
	c.stmts[q] = st
	for el := c.lru.Back(); c.lru.Len() > stmtCacheSize && el != nil; {
		prev := el.Prev()
		if old := el.Value.(*cachedStmt); !old.busy {
			c.lru.Remove(el)
			delete(c.stmts, old.query)
			_ = old.stmt.Close()
		}
		el = prev
	}
	return st, nil
}

func (c *cachedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	st, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return c.innerConn.QueryContext(ctx, q, args)
	}
	rows, err := st.stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	st.busy = true
	return &cachedRows{Rows: rows, st: st}, nil
}

func (c *cachedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	st, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return c.innerConn.ExecContext(ctx, q, args)
	}
	return st.stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (c *cachedConn) Close() error {
	for _, st := range c.stmts {
		_ = st.stmt.Close()
	}
	c.stmts, c.lru = nil, list.New()
	return c.innerConn.Close()
}

// cachedRows wraps a cached statement's rows. Embedding driver.Rows alone
// would hide the driver's optional interfaces from database/sql, so the
// column-type ones are forwarded (rows.ColumnTypes() sees what modernc
// reports); a driver without one gets database/sql's own fallback.
// modernc's rows have no further result sets (driver.RowsNextResultSet), and
// neither do these: a cached statement is a single statement.
type cachedRows struct {
	driver.Rows
	st *cachedStmt
}

var (
	_ driver.RowsColumnTypeDatabaseTypeName = (*cachedRows)(nil)
	_ driver.RowsColumnTypeLength           = (*cachedRows)(nil)
	_ driver.RowsColumnTypeNullable         = (*cachedRows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*cachedRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*cachedRows)(nil)
)

func (r *cachedRows) ColumnTypeDatabaseTypeName(i int) string {
	if x, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return x.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

func (r *cachedRows) ColumnTypeLength(i int) (int64, bool) {
	if x, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return x.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *cachedRows) ColumnTypeNullable(i int) (nullable, ok bool) {
	if x, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return x.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *cachedRows) ColumnTypePrecisionScale(i int) (precision, scale int64, ok bool) {
	if x, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return x.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

func (r *cachedRows) ColumnTypeScanType(i int) reflect.Type {
	if x, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return x.ColumnTypeScanType(i)
	}
	return reflect.TypeFor[any]()
}

func (r *cachedRows) Close() error {
	err := r.Rows.Close() // resets the statement for its next use
	r.st.busy = false
	return err
}
