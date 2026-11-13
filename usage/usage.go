// Package usage is the sandbox usage ledger (SPEC 20 "Boxes", "Usage"): one immutable row
// per box handle, written when run targets report the handle's end
// (rt.Service.OnEnd, after zm's final status), and read back scoped
// to the areas the caller reads. It never calls a driver and never stores
// anything a box was sent. Owned by sandboxes (zm-eakh.12.5).
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Migrations are the usage module's tables (New adds them).
var Migrations = []store.Migration{
	{Kind: store.KindProject, Name: "usage/0001_box_usage", Script: ddl},
}

// box_usage is this brick's table (project.db). handle is the key, so a
// repeated end report writes nothing.
const ddl = `
CREATE TABLE box_usage (
  handle           TEXT PRIMARY KEY,
  target           TEXT NOT NULL,
  area             TEXT NOT NULL DEFAULT '',
  actor            TEXT NOT NULL,
  size             TEXT NOT NULL DEFAULT '',
  image_digest     TEXT NOT NULL DEFAULT '',
  end_state        TEXT NOT NULL,
  acquired         TEXT NOT NULL DEFAULT '',
  ended            TEXT NOT NULL,
  lease_s          INTEGER NOT NULL,
  driver_unit      TEXT NOT NULL DEFAULT '',
  driver_amount    REAL,
  billable_minutes INTEGER NOT NULL,
  billed           INTEGER NOT NULL,
  recorded         TEXT NOT NULL
);
CREATE INDEX box_usage_ended ON box_usage (ended);
`

// Service is the usage module.
type Service struct {
	d    *core.Deps
	now  func() time.Time
	stop context.CancelFunc
	wg   sync.WaitGroup
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
)

// reconcileEvery is how often Start reconciles after its first, full pass;
// reconcileLookback is how far before the newest row a periodic pass reads
// again. A restart re-reads every project's whole record once, so a row
// lost before a long outage still comes back.
const (
	reconcileEvery    = 10 * time.Minute
	reconcileLookback = 24 * time.Hour
)

// Start reconciles every project's whole record at once, then every
// reconcileEvery from reconcileLookback before the newest row, so a row lost
// to a failed write or a stop inside Record's retries comes back from run
// targets' record of ended handles (rt.Service.EndedSince). Close
// stops it and waits.
func (s *Service) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(reconcileEvery)
		defer t.Stop()
		for full := true; ; full = false {
			s.reconcileSafely(ctx, full)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Close stops the reconcile loop and waits for a pass in flight.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
	return nil
}

func (s *Service) reconcileSafely(ctx context.Context, full bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("usage: reconcile panicked", "panic", r)
		}
	}()
	if _, err := s.reconcileAll(ctx, full); err != nil && ctx.Err() == nil {
		slog.Warn("usage: reconcile", "err", err)
	}
}

// Reconcile writes any missing box row for every project with a box binding,
// from reconcileLookback before each project's newest row. It answers how
// many rows it wrote; a project that fails is reported and the rest go on.
func (s *Service) Reconcile(ctx context.Context) (int, error) { return s.reconcileAll(ctx, false) }

func (s *Service) reconcileAll(ctx context.Context, full bool) (int, error) {
	wrote := 0
	var errs []error
	for _, p := range s.d.Projects.List(s.d.System().Scope) {
		if err := s.dropGoneLimits(ctx, p.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: sandbox minute limits: %w", p.Slug, err))
		}
		boxes := false
		for _, b := range rt.ProjectTargets(s.d.Projects, p.ID) {
			boxes = boxes || b.Kind == computeapi.RunTargetKindBox
		}
		if !boxes || ctx.Err() != nil {
			continue
		}
		n, err := s.reconcile(ctx, p.ID, full)
		wrote += n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Slug, err))
		}
	}
	return wrote, errors.Join(errs...)
}

// reconcile reads project's ended handles page by page and inserts the box
// rows it lacks, one transaction per page. Times on both sides are
// fixed-width UTC, so text order is time order.
func (s *Service) reconcile(ctx context.Context, project string, full bool) (int, error) {
	svc := rt.From(s.d)
	if svc == nil {
		return 0, nil
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err != nil {
		return 0, err
	}
	var since time.Time
	if !full {
		var newest sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT MAX(ended) FROM box_usage`).Scan(&newest); err != nil {
			return 0, err
		}
		if t, err := time.Parse(time.RFC3339Nano, newest.String); err == nil {
			since = t.Add(-reconcileLookback)
		}
	}
	have, err := handlesSince(ctx, db, "", since)
	if err != nil {
		return 0, err
	}
	wrote := 0
	complete, err := endedPages(ctx, svc, project, rt.EndedOnly{}, since, 1000, func(hs []computeapi.RunTargetHandle) error {
		var missing []computeapi.BoxUsage
		for _, h := range hs {
			if h.Kind == computeapi.RunTargetKindBox && h.Ended != "" && !have[h.Handle] {
				missing = append(missing, Compute(h))
				have[h.Handle] = true
			}
		}
		n, err := s.insertAll(ctx, db, missing)
		wrote += n
		return err
	})
	if err == nil && !complete {
		slog.Error("usage: reconcile stopped before the newest end; later rows unread", "project", project)
	}
	return wrote, err
}

// endedPages hands f project's ended handles matching only from since
// (inclusive), oldest first, a page of 1000 at a time, for at most pages pages. complete is
// false when it stopped before the newest end: the page bound, or 1000
// handles that ended in one instant, past which no reader can page.
func endedPages(ctx context.Context, svc rt.Service, project string, only rt.EndedOnly, since time.Time, pages int, f func([]computeapi.RunTargetHandle) error) (bool, error) {
	for range pages {
		hs, err := svc.EndedSince(ctx, project, only, since, 1000)
		if err != nil {
			return false, err
		}
		if err := f(hs); err != nil {
			return false, err
		}
		if len(hs) < 1000 {
			return true, nil
		}
		last, err := time.Parse(time.RFC3339Nano, hs[len(hs)-1].Ended)
		if err != nil || !last.After(since) {
			return false, nil
		}
		since = last
	}
	return false, nil
}

// handlesSince is the set of rows already written in area ("": every area)
// that ended at or after t.
func handlesSince(ctx context.Context, db queryer, area string, t time.Time) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT handle FROM box_usage WHERE ended >= ? AND (?='' OR area=?)`, t.UTC().Format(wireTime), area, area)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		have[h] = true
	}
	return have, rows.Err()
}

// insertAll writes rows in one transaction and answers how many were new.
func (s *Service) insertAll(ctx context.Context, db *sql.DB, us []computeapi.BoxUsage) (int, error) {
	if len(us) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, u := range us {
		res, err := tx.ExecContext(ctx, insertSQL, s.insertArgs(u)...)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// New returns the usage module, subscribed to run-target ends.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, now: time.Now}
	if svc := rt.From(d); svc != nil { // wired before this module (internal/wiring)
		svc.OnEnd(s.Record)
		svc.OnBoxAcquire(s.allowBox) // limits.go
	}
	// On a member's machine GET /v0/usage goes to the authority of the
	// project named (or the cwd's), which answers as it answers that member;
	// a pure read there, so it is hedged.
	if link := teamlink.From(d); link != nil {
		link.RegisterRoutes("usage",
			teamlink.RouteRule{Pattern: "GET /v0/usage", Kind: teamlink.KindWrite, Hedge: teamlink.Hedged},
			teamlink.RouteRule{Pattern: "GET /v0/usage/limits", Kind: teamlink.KindWrite, Hedge: teamlink.Hedged},
			teamlink.RouteRule{Pattern: "PUT /v0/usage/limits/{area}", Kind: teamlink.KindWrite, Hedge: teamlink.Once,
				Why: "an admin's edit, sent once"})
	}
	return s
}

// Routes registers GET /v0/usage.
func (s *Service) Routes(r *core.Router) {
	r = r.For("usage")
	r.HandleFunc("GET /v0/usage", core.Authed, "read on the resolved project; only rows in areas the caller reads", s.list)
	r.HandleFunc("GET /v0/usage/limits", core.Authed, "read on the resolved project", s.limitsList)
	r.HandleFunc("PUT /v0/usage/limits/{area}", core.Authed, "admin on the whole resolved project", s.limitSet)
}

// maxRows is the most rows one read answers (newest first).
const maxRows = 1000

// minutesPer converts a driver's spend unit to minutes of wall time. A unit
// not listed (cpu_s, money) says nothing about time, so the lease alone counts.
var minutesPer = map[string]float64{"slot_min": 1, "vm_min": 1, "min": 1, "s": 1.0 / 60}

// The lease rule, shared by what a box is billed (Compute) and what a live
// one has run so far (soFar), so a limit counts as the ledger will bill.

// leaseSpan is how long a box's lease ran from start (an RFC 3339 time) to
// end, never past expiresAt (the host ends the box there; zm may notice
// later). It is negative when end is before start; ok is false when start
// is no time (a box that never answered acquire).
func leaseSpan(start string, end time.Time, expiresAt string) (time.Duration, bool) {
	from, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return 0, false
	}
	if exp, err := time.Parse(time.RFC3339Nano, expiresAt); err == nil && exp.Before(end) {
		end = exp
	}
	return end.Sub(from), true
}

// ranMinutes is the wall time a driver's spend figure says a box ran (ok is
// false for a unit that says nothing about time).
func ranMinutes(unit string, amount float64) (float64, bool) {
	per, ok := minutesPer[unit]
	return math.Max(0, amount*per), ok
}

// wholeMinutes bills a part of a minute as a minute.
func wholeMinutes(m float64) int { return int(math.Ceil(m - 1e-9)) }

// wireTime is how rows keep times: fixed-width UTC, so text order is time
// order (RFC3339Nano drops trailing zeros: "10:00:00.5Z" < "10:00:00Z").
const wireTime = "2006-01-02T15:04:05.000000000Z"

func fixed(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(wireTime)
}

// Compute is one box's row from its ended handle (exported for tests).
// The clock runs from acquire's answer to the end zm recorded, never past
// expires_at (the host ends the box there; zm may notice later); the
// driver's own figure can lower the billable minutes, never raise them.
// Reclaimed boxes are never billed; lost ones are held unbilled; a box that
// never answered acquire, or whose end is not after its start, bills nothing.
func Compute(h computeapi.RunTargetHandle) computeapi.BoxUsage {
	u := computeapi.BoxUsage{Handle: h.Handle, Target: h.Target, Area: h.Area, Actor: h.Actor, Size: h.Size,
		ImageDigest: h.ImageDigest, EndState: h.State, Acquired: fixed(h.Acquired), Ended: fixed(h.Ended)}
	if h.Spend != nil {
		u.DriverUnit, u.DriverAmount = h.Spend.Unit, &h.Spend.Amount
	}
	end, err := time.Parse(time.RFC3339Nano, h.Ended)
	d, ok := leaseSpan(h.Acquired, end, h.ExpiresAt)
	if err != nil || !ok || d <= 0 {
		return u
	}
	u.LeaseS = int(d / time.Second)
	lease := d.Minutes()
	minutes := lease
	if u.DriverAmount != nil {
		if ran, ok := ranMinutes(u.DriverUnit, *u.DriverAmount); ok {
			minutes = min(lease, ran)
		}
	}
	u.BillableMinutes = wholeMinutes(minutes)
	u.Billed = (h.State == "released" || h.State == "expired") && u.BillableMinutes > 0
	return u
}

// Record writes h's row if h is a box, retrying a failed write (the end
// hook fires once per handle). It never fails the end that called it. A
// daemon that stops inside the retries, or a lasting failure, leaves the row
// to Reconcile (Start runs it at once and every reconcileEvery).
func (s *Service) Record(ctx context.Context, project string, h computeapi.RunTargetHandle) {
	if h.Kind != "box" || h.Handle == "" || h.Ended == "" {
		return
	}
	u := Compute(h)
	var err error
	for try, wait := 0, 50*time.Millisecond; try < 6; try, wait = try+1, wait*3 {
		if err = s.insert(ctx, project, u); err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(wait) // a busy database; at most about 18 s in all
	}
	if err != nil {
		slog.Error("usage: box row not written", "project", project, "handle", h.Handle, "err", err)
	}
}

func (s *Service) insert(ctx context.Context, project string, u computeapi.BoxUsage) error {
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, insertSQL, s.insertArgs(u)...)
	return err
}

const insertSQL = `INSERT INTO box_usage (handle, target, area, actor, size, image_digest,
  end_state, acquired, ended, lease_s, driver_unit, driver_amount, billable_minutes, billed, recorded)
  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(handle) DO NOTHING`

func (s *Service) insertArgs(u computeapi.BoxUsage) []any {
	return []any{u.Handle, u.Target, u.Area, u.Actor, u.Size, u.ImageDigest, u.EndState, u.Acquired, u.Ended, u.LeaseS,
		u.DriverUnit, u.DriverAmount, u.BillableMinutes, u.Billed, s.now().UTC().Format(wireTime)}
}

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// billedMinutes is the one sum of billed minutes: rows of box_usage that
// pass where (SQL over its columns) and were billed. GET /v0/usage and the
// per-area limit (limits.go) both read it.
func billedMinutes(ctx context.Context, q queryer, where string, args ...any) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(billable_minutes), 0) FROM box_usage WHERE billed=1 AND (`+where+`)`, args...).Scan(&n)
	return n, err
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	out, err := s.read(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) read(r *http.Request) (computeapi.UsageList, error) {
	t, err := s.d.WriteTarget(r.Context(), "", authz.Read)
	if err != nil {
		return computeapi.UsageList{}, err
	}
	since := r.URL.Query().Get("since")
	if since != "" {
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return computeapi.UsageList{}, api.Errorf(api.CodeBadRequest, "since: want RFC 3339: %v", err)
		}
		since = at.UTC().Format(wireTime)
	}
	sc, err := s.d.ReadScope(r.Context(), t.Project.ID)
	if err != nil {
		return computeapi.UsageList{}, err
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Read, t.Project.ID)
	if err != nil {
		return computeapi.UsageList{}, err
	}
	where, args := sc.AreaSQL(t.Project.ID, "area", authz.Read)
	if since != "" {
		where += ` AND ended >= ?`
		args = append(args, since)
	}
	out := computeapi.UsageList{Project: t.Project.Slug, Boxes: []computeapi.BoxUsage{}}
	// One read, so the count, the sum and the rows agree.
	tx, err := db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return computeapi.UsageList{}, err
	}
	defer tx.Rollback()
	var total int
	if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM box_usage WHERE `+where, args...).Scan(&total); err != nil {
		return computeapi.UsageList{}, err
	}
	if out.BilledMinutes, err = billedMinutes(r.Context(), tx, where, args...); err != nil {
		return computeapi.UsageList{}, err
	}
	out.Truncated = total > maxRows
	rows, err := tx.QueryContext(r.Context(), `SELECT handle, target, area, actor, size, image_digest, end_state, acquired,
  ended, lease_s, driver_unit, driver_amount, billable_minutes, billed FROM box_usage WHERE `+where+
		` ORDER BY ended DESC, handle LIMIT `+strconv.Itoa(maxRows), args...)
	if err != nil {
		return computeapi.UsageList{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var u computeapi.BoxUsage
		var amount sql.NullFloat64
		if err := rows.Scan(&u.Handle, &u.Target, &u.Area, &u.Actor, &u.Size, &u.ImageDigest, &u.EndState, &u.Acquired,
			&u.Ended, &u.LeaseS, &u.DriverUnit, &amount, &u.BillableMinutes, &u.Billed); err != nil {
			return computeapi.UsageList{}, err
		}
		if amount.Valid {
			u.DriverAmount = &amount.Float64
		}
		out.Boxes = append(out.Boxes, u)
	}
	return out, rows.Err()
}
