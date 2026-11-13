package runtarget

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// run_handles is this brick's table (project.db): one row per acquire
// request, keyed by binding and request_id, with the handle once the driver
// answered. A db's url is never stored.
var handleMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "runtarget/0001_handles", Script: handlesDDL},
	{Kind: store.KindProject, Name: "runtarget/0002_box", Script: boxDDL},
	{Kind: store.KindProject, Name: "runtarget/0003_area", Script: areaDDL},
	{Kind: store.KindProject, Name: "runtarget/0004_ended", Script: `CREATE INDEX run_handles_ended ON run_handles (ended) WHERE ended<>'';
` + tsNormalize},
	{Kind: store.KindProject, Name: "runtarget/0005_renew", Script: `ALTER TABLE run_handles ADD COLUMN last_renew TEXT NOT NULL DEFAULT '';`},
	// A box acquired with persist, and when it last started after a stop (persist.go).
	{Kind: store.KindProject, Name: "runtarget/0006_persist", Script: persistDDL},
	// The driver's spend figure at that start (persist.go).
	{Kind: store.KindProject, Name: "runtarget/0008_started_spend", Script: startedSpendDDL},
	// A cancel of the acquire stood (acquires.go).
	{Kind: store.KindProject, Name: "runtarget/0010_cancelled", Script: cancelledDDL},
	// A box's request id is looked up across bindings (requests.go).
	{Kind: store.KindProject, Name: "runtarget/0011_request_index", Script: `CREATE INDEX run_handles_request ON run_handles (request_id, kind);`},
	// A handle whose client renews it itself: the sweep leaves it (sweep.go renewHeld).
	{Kind: store.KindProject, Name: "runtarget/0012_self_renew", Script: selfRenewDDL},
}

// markCancelled records that a cancel of an acquire with no handle yet
// stood (the driver answered it cancelled): it holds nothing it hands over.
func markCancelled(ctx context.Context, db *sql.DB, target, requestID string) error {
	_, err := db.ExecContext(context.WithoutCancel(ctx), `UPDATE run_handles SET cancelled=1 WHERE target=? AND request_id=? AND handle IS NULL`, target, requestID)
	return err
}

// setCancelled records whether what a cancelled acquire made was ended
// (on), or could not be and is held as a plain handle (off).
func setCancelled(ctx context.Context, db *sql.DB, target, requestID string, on bool) error {
	_, err := db.ExecContext(ctx, `UPDATE run_handles SET cancelled=? WHERE target=? AND request_id=?`, on, target, requestID)
	return err
}

// cancelledAcquiring are up to limit of the rows whose cancel stood while
// they had no handle, created before the time before: what their acquire
// made, if anything, is still to be found (sweep.go). They come in
// (target, request_id) order, those after the cursor (target, request_id)
// only; "" "" starts at the first. A caller pages to the end, so rows a
// pass cannot settle (an unbound target, a failing driver) never hide later ones.
func cancelledAcquiring(ctx context.Context, db *sql.DB, before, afterTarget, afterRequestID string, limit int) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+handleCols+` FROM run_handles WHERE cancelled=1 AND state=? AND handle IS NULL AND created<?
		AND (target>? OR (target=? AND request_id>?)) ORDER BY target, request_id LIMIT ?`,
		stateAcquiring, before, afterTarget, afterTarget, afterRequestID, limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// setRenewed records when zm last renewed a handle (renewal is throttled).
func setRenewed(ctx context.Context, db *sql.DB, target, requestID, at string) error {
	_, err := db.ExecContext(ctx, `UPDATE run_handles SET last_renew=? WHERE target=? AND request_id=?`, at, target, requestID)
	return err
}

// lastRenew is when zm last renewed a handle ("" never).
func lastRenew(ctx context.Context, db *sql.DB, target, requestID string) string {
	var at string
	_ = db.QueryRowContext(ctx, `SELECT last_renew FROM run_handles WHERE target=? AND request_id=?`, target, requestID).Scan(&at)
	return at
}

// tsLayout is every time zm writes in run_handles: fixed width in UTC, so
// string order is time order (RFC3339Nano trims trailing zeros: "05Z" would
// sort after "05.5Z").
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// tsNormalize rewrites rows written before tsLayout (RFC3339Nano, UTC).
const tsNormalize = `UPDATE run_handles SET created = substr(created,1,19) || '.' || substr(rtrim(substr(created,21),'Z') || '000000000', 1, 9) || 'Z' WHERE created<>'' AND length(created)<>30 AND substr(created,-1)='Z';
UPDATE run_handles SET ended = substr(ended,1,19) || '.' || substr(rtrim(substr(ended,21),'Z') || '000000000', 1, 9) || 'Z' WHERE ended<>'' AND length(ended)<>30 AND substr(ended,-1)='Z';
UPDATE run_handles SET acquired = substr(acquired,1,19) || '.' || substr(rtrim(substr(acquired,21),'Z') || '000000000', 1, 9) || 'Z' WHERE acquired<>'' AND length(acquired)<>30 AND substr(acquired,-1)='Z';`

// persistDDL adds whether a box keeps its disk, and when it last started.
const persistDDL = `ALTER TABLE run_handles ADD COLUMN persist INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_handles ADD COLUMN started TEXT NOT NULL DEFAULT '';`

// startedSpendDDL adds the spend figure as of a box's last start.
const startedSpendDDL = `ALTER TABLE run_handles ADD COLUMN started_spend REAL;`

// cancelledDDL marks an acquire whose cancel stood (acquires.go).
const cancelledDDL = `ALTER TABLE run_handles ADD COLUMN cancelled INTEGER NOT NULL DEFAULT 0;`

// selfRenewDDL adds whether the handle's client renews it itself (an
// acquire's self_renew): the sweep never renews such a handle.
const selfRenewDDL = `ALTER TABLE run_handles ADD COLUMN self_renew INTEGER NOT NULL DEFAULT 0;`

// areaDDL adds the area the acquire was made in: a pool team is an area, and
// a handle is seen only by those who may read its area.
const areaDDL = `ALTER TABLE run_handles ADD COLUMN area TEXT NOT NULL DEFAULT '';`

// boxDDL adds a box's size and booted image digest, and when acquire
// answered (a box's billing clock starts there, not at the ask).
const boxDDL = `ALTER TABLE run_handles ADD COLUMN size TEXT NOT NULL DEFAULT '';
ALTER TABLE run_handles ADD COLUMN image_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE run_handles ADD COLUMN acquired TEXT NOT NULL DEFAULT '';`

const handlesDDL = `
CREATE TABLE run_handles (
  target       TEXT NOT NULL,
  request_id   TEXT NOT NULL,
  request      TEXT NOT NULL,
  handle       TEXT UNIQUE,
  kind         TEXT NOT NULL,
  actor        TEXT NOT NULL,
  state        TEXT NOT NULL,
  ref          TEXT NOT NULL DEFAULT '',
  commit_sha   TEXT NOT NULL DEFAULT '',
  script       TEXT NOT NULL DEFAULT '',
  exit_code    INTEGER,
  expires_at   TEXT NOT NULL DEFAULT '',
  ttl_s        INTEGER NOT NULL DEFAULT 0,
  log          TEXT NOT NULL DEFAULT '',
  spend_unit   TEXT NOT NULL DEFAULT '',
  spend_amount REAL,
  billed       INTEGER NOT NULL DEFAULT 1,
  created      TEXT NOT NULL,
  ended        TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (target, request_id)
);
CREATE INDEX run_handles_live ON run_handles (target, actor) WHERE ended = '';
`

// States (SPEC 20) plus zm's own "acquiring": asked, no answer yet.
const (
	stateAcquiring = "acquiring"
	stateReady     = "ready"
	stateRunning   = "running"
	stateEnded     = "ended"
	stateExpired   = "expired"
	stateReleased  = "released"
	stateReclaimed = "reclaimed"
	stateLost      = "lost"
)

func terminal(state string) bool {
	switch state {
	case stateEnded, stateExpired, stateReleased, stateReclaimed, stateLost:
		return true
	}
	return false
}

const handleCols = `target, request_id, request, COALESCE(handle,''), kind, actor, state, ref, commit_sha, script,
  exit_code, expires_at, ttl_s, log, spend_unit, spend_amount, created, ended, size, image_digest, acquired, area, persist, started, started_spend, cancelled, self_renew`

type row struct {
	computeapi.RunTargetHandle
	request   string
	cancelled bool // a cancel of it stood (setCancelled)
}

func scanRow(sc interface{ Scan(...any) error }) (row, error) {
	var r row
	var exit sql.NullInt64
	var amount, startedAmount sql.NullFloat64
	var unit string
	err := sc.Scan(&r.Target, &r.RequestID, &r.request, &r.Handle, &r.Kind, &r.Actor, &r.State, &r.Ref, &r.Commit,
		&r.Script, &exit, &r.ExpiresAt, &r.TTLS, &r.Log, &unit, &amount, &r.Created, &r.Ended, &r.Size, &r.ImageDigest, &r.Acquired, &r.Area,
		&r.Persist, &r.Started, &startedAmount, &r.cancelled, &r.SelfRenew)
	if err != nil {
		return r, err
	}
	if exit.Valid {
		n := int(exit.Int64)
		r.Exit = &n
	}
	if amount.Valid {
		r.Spend = &computeapi.RunTargetSpend{Unit: unit, Amount: amount.Float64}
	}
	if startedAmount.Valid && r.Started != "" {
		r.StartedSpend = &computeapi.RunTargetSpend{Unit: unit, Amount: startedAmount.Float64}
	}
	return r, nil
}

func byRequest(ctx context.Context, db *sql.DB, target, requestID string) (row, bool, error) {
	r, err := scanRow(db.QueryRowContext(ctx, `SELECT `+handleCols+` FROM run_handles WHERE target=? AND request_id=?`, target, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// usedElsewhere are the rows of other bindings of kind holding requestID,
// newest first (requests.go).
func usedElsewhere(ctx context.Context, db *sql.DB, target, kind, requestID string) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+handleCols+` FROM run_handles WHERE request_id=? AND target<>? AND kind=? ORDER BY created DESC`, requestID, target, kind)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func byHandle(ctx context.Context, db *sql.DB, handle string) (row, bool, error) {
	r, err := scanRow(db.QueryRowContext(ctx, `SELECT `+handleCols+` FROM run_handles WHERE handle=?`, handle))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// listRows are target's handles whose area passes areaSQL (sc.AreaSQL).
func listRows(ctx context.Context, db *sql.DB, target string, liveOnly bool, areaSQL string, areaArgs []any) ([]row, error) {
	q := `SELECT ` + handleCols + ` FROM run_handles WHERE target=? AND (` + areaSQL + `)`
	if liveOnly {
		q += ` AND ended=''`
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY created DESC LIMIT 500`, append([]any{target}, areaArgs...)...)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// liveRows are every binding's live handles in one project.
func liveRows(ctx context.Context, db *sql.DB) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+handleCols+` FROM run_handles WHERE ended='' AND handle IS NOT NULL LIMIT 2000`)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// scanRows reads every row of rows and closes it.
func scanRows(rows *sql.Rows) ([]row, error) {
	defer rows.Close()
	var out []row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// liveBoxes are every binding's live boxes in area of one project, all of
// them (a limit must not miss one).
func liveBoxes(ctx context.Context, db *sql.DB, area string) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+handleCols+` FROM run_handles WHERE ended='' AND handle IS NOT NULL AND kind=? AND area=?`,
		computeapi.RunTargetKindBox, area)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// endedRows are handles matching only that ended at or after since, oldest
// end first.
func endedRows(ctx context.Context, db *sql.DB, only rt.EndedOnly, since string, limit int) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+handleCols+` FROM run_handles WHERE ended<>'' AND ended>=? AND handle IS NOT NULL AND (?='' OR area=?) AND (?='' OR kind=?)
ORDER BY ended, rowid LIMIT ?`, since, only.Area, only.Area, only.Kind, only.Kind, limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func liveCount(ctx context.Context, db *sql.DB, target, actor string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_handles WHERE target=? AND actor=? AND ended='' AND state<>?`,
		target, actor, stateAcquiring).Scan(&n)
	return n, err
}

// periodSpend sums billed spend of target's handles created since start.
func periodSpend(ctx context.Context, db *sql.DB, target, unit string, start time.Time) (float64, error) {
	var sum sql.NullFloat64
	err := db.QueryRowContext(ctx, `SELECT SUM(spend_amount) FROM run_handles WHERE target=? AND spend_unit=? AND billed=1 AND created>=?`,
		target, unit, start.UTC().Format(tsLayout)).Scan(&sum)
	return sum.Float64, err
}

func insertAcquiring(ctx context.Context, db *sql.DB, target, requestID, request, kind, actor, ref, commit, script, area, now string, selfRenew bool) error {
	_, err := db.ExecContext(ctx, `INSERT INTO run_handles (target, request_id, request, kind, actor, state, ref, commit_sha, script, area, created, self_renew)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, target, requestID, request, kind, actor, stateAcquiring, ref, commit, script, area, now, selfRenew)
	return err
}

// update writes what a driver answered. Spend never falls: zm keeps the
// highest figure seen. A terminal state is kept once reached, except that
// a later answer may fill in exit. endedNow is true only for the one write
// that ended the row (the end hook's trigger), judged inside a transaction.
func update(ctx context.Context, db *sql.DB, target, requestID string, u upd, now string) (endedNow bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var before string
	if err := tx.QueryRowContext(ctx, `SELECT ended FROM run_handles WHERE target=? AND request_id=?`, target, requestID).Scan(&before); err != nil {
		return false, err
	}
	ended := ""
	if terminal(u.state) {
		ended = now
	}
	var exit any
	if u.exit != nil {
		exit = *u.exit
	}
	var amount any
	unit := ""
	if u.spend != nil {
		amount, unit = u.spend.Amount, u.spend.Unit
	}
	billed := 1
	if u.state == stateReclaimed {
		billed = 0
	}
	_, err = tx.ExecContext(ctx, `UPDATE run_handles SET
  handle = COALESCE(NULLIF(?,''), handle),
  acquired = CASE WHEN ?<>'' AND acquired='' THEN ? ELSE acquired END,
  state = CASE WHEN ended<>'' AND state<>? THEN state WHEN ?='' THEN state ELSE ? END,
  commit_sha = CASE WHEN ?<>'' THEN ? ELSE commit_sha END,
  exit_code = COALESCE(exit_code, ?),
  expires_at = CASE WHEN ?<>'' AND ended='' THEN ? ELSE expires_at END,
  ttl_s = CASE WHEN ?>0 THEN ? ELSE ttl_s END,
  log = CASE WHEN ?<>'' THEN ? ELSE log END,
  spend_unit = CASE WHEN ? IS NOT NULL AND (spend_amount IS NULL OR ?>spend_amount) THEN ? ELSE spend_unit END,
  spend_amount = CASE WHEN ? IS NOT NULL AND (spend_amount IS NULL OR ?>spend_amount) THEN ? ELSE spend_amount END,
  billed = CASE WHEN ?=0 AND ended='' THEN 0 ELSE billed END,
  ended = CASE WHEN ended='' THEN ? ELSE ended END
WHERE target=? AND request_id=?`,
		u.handle, u.acquired, u.acquired, stateAcquiring, u.state, u.state,
		u.commit, u.commit,
		exit,
		u.expiresAt, u.expiresAt,
		u.ttl, u.ttl,
		u.log, u.log,
		amount, amount, unit,
		amount, amount, amount,
		billed,
		ended,
		target, requestID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return before == "" && ended != "", nil
}

type upd struct {
	handle, state, commit, expiresAt, log string
	acquired                              string
	ttl                                   int
	exit                                  *int
	spend                                 *computeapi.RunTargetSpend
}

// setBox records a box's size, booted image digest and whether it keeps its disk.
func setBox(ctx context.Context, db *sql.DB, target, requestID, size, digest string, persist bool) error {
	_, err := db.ExecContext(ctx, `UPDATE run_handles SET size=?, image_digest=?, persist=? WHERE target=? AND request_id=?`, size, digest, persist, target, requestID)
	return err
}

func deleteAcquiring(ctx context.Context, db *sql.DB, target, requestID string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM run_handles WHERE target=? AND request_id=? AND state=?`, target, requestID, stateAcquiring)
	return err
}
