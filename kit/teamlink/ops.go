package teamlink

// Machine ops (SPEC 8 "Machine choice"): how a team project's authority
// asks one of its machines to do something and hears back. Nothing
// connects to that machine: the authority queues the op (team_ops); the
// machine takes it in a heartbeat answer or its held-open GET /v0/team/ops
// poll, records it before it runs (team_ops_run, kept across a restart: at
// most once), runs it off the heartbeat, and reports it in the heartbeats
// after (or at once, POST /v0/team/ops/report) until an answer took its
// final word. Each beat also names the ops the machine took (Took): an
// answer carries an op's payload, which may hold stdin or sealed secrets,
// only until its machine says it has it, so a lost answer is made good by
// the next beat and a taken op is never sent again. An op for the
// authority's own machine runs at once.
//
// The queue knows no kind: each kind is registered by the module that owns
// it (RegisterOp) with its handler, and its payload is opaque JSON here.
// The heartbeat itself is beat.go's: it calls PendingOps, ApplyOpReports
// and MarkTook at the authority, OpReports, TookOps, AckOps and TakeOps on
// the machine.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var opMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "team/0005_machine_ops", Script: `
CREATE TABLE team_ops (
	id            TEXT PRIMARY KEY, -- rt-<ulid> for a start; a cancel or read has its own
	project       TEXT NOT NULL,
	machine       TEXT NOT NULL,    -- where it runs
	kind          TEXT NOT NULL,    -- task_start | task_cancel | task_read | session_op
	op            TEXT NOT NULL,    -- JSON api.MachineOp, as the machine gets it
	from_machine  TEXT NOT NULL,    -- the parent's machine
	from_identity TEXT NOT NULL,
	parent        TEXT NOT NULL DEFAULT '',
	request_id    TEXT NOT NULL DEFAULT '',
	request       TEXT NOT NULL DEFAULT '', -- the start's request JSON (a repeated request_id must match)
	state         TEXT NOT NULL,    -- pending | taken | done
	sent          INTEGER NOT NULL DEFAULT 0, -- a heartbeat answer carried it
	task          TEXT NOT NULL DEFAULT '', -- a start's latest JSON api.Task, as the machine reported it
	result        TEXT NOT NULL DEFAULT '', -- a read's JSON api.TaskHistoryPage
	error         TEXT NOT NULL DEFAULT '',
	created       INTEGER NOT NULL,
	updated       INTEGER NOT NULL
);
CREATE INDEX team_ops_pending ON team_ops (project, machine, state);
CREATE UNIQUE INDEX team_ops_request ON team_ops (project, from_machine, parent, request_id) WHERE request_id != '';
CREATE TABLE team_ops_run (
	id      TEXT PRIMARY KEY, -- the authority's op id
	project TEXT NOT NULL,
	kind    TEXT NOT NULL,
	task    TEXT NOT NULL DEFAULT '', -- a start's local task id
	state   TEXT NOT NULL,            -- running | done
	report  TEXT NOT NULL DEFAULT '', -- JSON api.MachineOpReport still to send
	final   INTEGER NOT NULL DEFAULT 0, -- nothing later changes the report
	acked   INTEGER NOT NULL DEFAULT 0, -- an answer took the final report
	created INTEGER NOT NULL
);
CREATE INDEX team_ops_run_unacked ON team_ops_run (project, acked);
CREATE TABLE team_task_refs (
	id      TEXT PRIMARY KEY, -- rt-<ulid>
	project TEXT NOT NULL,
	parent  TEXT NOT NULL,
	created INTEGER NOT NULL
);`},
	// The envelope's payload became opaque (zm-eakh.12.35.7): ops and
	// reports queued in the older shape are dropped (all machines upgrade
	// together). The columns stay: task holds an op's Status, request_id
	// and request its Key and KeyBody.
	{Kind: store.KindRegistry, Name: "team/0007_op_payload", Script: `DELETE FROM team_ops; DELETE FROM team_ops_run;`},
	// took: the op's machine named it in a beat's Took (it has it): no
	// answer carries it again.
	{Kind: store.KindRegistry, Name: "team/0008_op_took", Script: `ALTER TABLE team_ops ADD COLUMN took INTEGER NOT NULL DEFAULT 0;`},
}

// Tunables (tests change them).
var (
	// OpTakeWithin: an op of a kind with no Expiry that no machine took by
	// then is ended.
	OpTakeWithin = 10 * time.Minute
	// OpsPerBeat bounds the ops one heartbeat answer carries.
	OpsPerBeat = 20
	// OpsKeep: how long op records are kept.
	OpsKeep = 30 * 24 * time.Hour
	// OpsWait is how long one GET /v0/team/ops waits at the authority.
	OpsWait = 20 * time.Second
	// opsWaitMax bounds it at the authority.
	opsWaitMax = 25 * time.Second
	// opsScopeEvery is how often a waiting poll looks at its token.
	opsScopeEvery = 500 * time.Millisecond
	// opsIdle is how often a machine that wants no ops looks again (a want
	// wakes it at once).
	opsIdle = 15 * time.Second
	// OpsPollOff stops the poll and the direct report (tests: the heartbeat
	// alone carries ops).
	OpsPollOff atomic.Bool
	// OpsPolls counts the polls this process's authorities answered (tests).
	OpsPolls atomic.Int64
)

const (
	// OpsBeatBytes bounds the op reports one heartbeat carries.
	OpsBeatBytes = 4 << 20
	// maxOpsBody bounds one POST /v0/team/ops/report.
	maxOpsBody = 8 << 20
	// maxOpReports bounds the reports one POST carries.
	maxOpReports = 200
)

// OpHandler is one op kind, registered by the module that owns it. Run is
// required; the rest may be left zero.
type OpHandler struct {
	// Run runs op on this machine (project is the authority's ULID). The
	// payload is the report's; err is its refusal (an *api.Error keeps its
	// code and hint; any other error its text). Both may be set.
	Run func(ctx context.Context, project string, op api.MachineOp) (json.RawMessage, error)
	// Apply takes the machine's report at the authority. nil: the op is
	// done, its Result the report's payload, its Error the refusal.
	Apply func(ctx context.Context, o Op, rep api.MachineOpReport)
	// Args is op as it goes to its machine (it may fill in or seal what the
	// queue does not keep); an error keeps op out of this answer, and ends
	// it with that error while no answer carried it. One an answer carried
	// (an earlier one, or one overlapping this) may have run on its machine:
	// it stays pending until its machine reports it or ExpireOps ends it.
	// Args must not answer op's caller: Refused does.
	Args func(ctx context.Context, project string, op *api.MachineOp) error
	// Refused hears that Args' err ended op, after it did: the one place
	// to answer op's caller a refusal. An op an answer carried is never
	// ended so, and Refused is not called for it.
	Refused func(ctx context.Context, project string, op api.MachineOp, err error)
	// Expiry: an op its machine has not taken by then never goes out and
	// is ended ("not taken in time"). 0: OpTakeWithin.
	Expiry time.Duration
	// ReportWithin: an op an answer carried that its machine has not
	// reported by then is ended ("its machine did not report it"). 0: Expiry.
	ReportWithin time.Duration
	// Expired, when set, ends an expired op instead (it is still pending).
	Expired func(ctx context.Context, o Op, why string)
	// ReportNow posts the report to the authority as soon as it is recorded
	// (else the next heartbeat carries it).
	ReportNow bool
	// BlankOnAck: the report holds a secret. The machine blanks it once an
	// answer took it, or unsent after Keep; the authority blanks a Result
	// not touched for Keep.
	BlankOnAck bool
	Keep       time.Duration
	// Settled: whether a report is final as run (nil: always). A report
	// that is not is sent again with each beat, after Refresh (when set)
	// updated it from Local, until it is.
	Settled func(rep api.MachineOpReport) bool
	Refresh func(ctx context.Context, local string, rep *api.MachineOpReport)
}

// Op is one queued op as the authority keeps it (team_ops).
type Op struct {
	api.MachineOp
	Project, Machine string
	FromMachine      string // the machine that asked
	// Parent is who on FromMachine asked (an address, or none); Key a
	// request id: Project, FromMachine, Parent and Key are unique, and
	// KeyBody is the request it was made for (a repeat must match).
	Parent, Key, KeyBody string
	State                string // pending | taken | done
	Sent                 bool   // an answer carried it
	// Status is a long-running op's latest status (its machine's word);
	// Result its outcome; Error why it failed.
	Status, Result, Error string
	Created, Updated      time.Time
}

// Op states.
const (
	OpPending = "pending"
	OpTaken   = "taken"
	OpDone    = "done"
)

// opReg is a Service's registered kinds and the ops poll's state.
type opReg struct {
	mu   sync.RWMutex
	by   map[string]OpHandler
	wake wakes // per project and machine: an op was queued
	want wantState
	wg   sync.WaitGroup // ops running here
	// running counts, per project, the ops this process is running (until
	// each one's report is recorded), and live holds their ids. A
	// team_ops_run row in state running is not proof: one left by a process
	// that stopped mid-op is never run again and never ends by itself
	// (TookOps ends it once it is older than its kind's ReportWithin).
	running map[string]int
	live    map[string]int // by op id: a resent op counts again
}

// opRunning adds d (1 or -1) to project's count of ops running in this
// process, and likewise op id's (a resent op already running counts
// twice until both end).
func (s *Service) opRunning(project, id string, d int) {
	s.ops.mu.Lock()
	defer s.ops.mu.Unlock()
	if s.ops.running == nil {
		s.ops.running, s.ops.live = map[string]int{}, map[string]int{}
	}
	if s.ops.running[project] += d; s.ops.running[project] <= 0 {
		delete(s.ops.running, project)
	}
	if s.ops.live[id] += d; s.ops.live[id] <= 0 {
		delete(s.ops.live, id)
	}
}

// opLive: this process is running op id.
func (s *Service) opLive(id string) bool {
	s.ops.mu.RLock()
	defer s.ops.mu.RUnlock()
	return s.ops.live[id] > 0
}

// runningOps is how many ops this process is running for project.
func (s *Service) runningOps(project string) int {
	s.ops.mu.RLock()
	defer s.ops.mu.RUnlock()
	return s.ops.running[project]
}

// RegisterOp registers kind's handler. Registering a kind twice panics.
func (s *Service) RegisterOp(kind string, h OpHandler) {
	if kind == "" || h.Run == nil {
		panic("teamlink: an op kind needs a name and Run")
	}
	s.ops.mu.Lock()
	defer s.ops.mu.Unlock()
	if s.ops.by == nil {
		s.ops.by = map[string]OpHandler{}
	}
	if _, dup := s.ops.by[kind]; dup {
		panic("teamlink: op kind " + kind + " registered twice")
	}
	s.ops.by[kind] = h
}

func (s *Service) handler(kind string) (OpHandler, bool) {
	s.ops.mu.RLock()
	defer s.ops.mu.RUnlock()
	h, ok := s.ops.by[kind]
	return h, ok
}

func (s *Service) handlers() map[string]OpHandler {
	s.ops.mu.RLock()
	defer s.ops.mu.RUnlock()
	out := make(map[string]OpHandler, len(s.ops.by))
	for k, h := range s.ops.by {
		out[k] = h
	}
	return out
}

func unknownKind(kind string) string { return "unknown op kind " + kind }

func (h OpHandler) expiry() time.Duration {
	if h.Expiry > 0 {
		return h.Expiry
	}
	return OpTakeWithin
}

func (h OpHandler) reportWithin() time.Duration {
	if h.ReportWithin > 0 {
		return h.ReportWithin
	}
	return h.expiry()
}

// ErrParts is err's message, code and hint (an api error), else its text.
func ErrParts(err error) (msg, code, hint string) {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Message, e.Code, e.Hint
	}
	return err.Error(), "", ""
}

// ---- the authority ----

// QueueOp records o (its ID, Kind, From, Payload, Project, Machine,
// FromMachine and the optional Parent, Key, KeyBody and Status) as pending
// and wakes its machine's poll. It does not run it here (RunHere does).
// A Key already used answers the database's UNIQUE error.
func (s *Service) QueueOp(ctx context.Context, o Op) error {
	if _, ok := s.handler(o.Kind); !ok {
		return api.Errorf(api.CodeBadRequest, "%s", unknownKind(o.Kind))
	}
	js, _ := json.Marshal(o.MachineOp)
	now := store.Millis(time.Now())
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_ops (id, project, machine, kind, op, from_machine, from_identity, parent, request_id, request, state, task, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`, o.ID, o.Project, o.Machine, o.Kind, string(js), o.FromMachine, o.From,
		o.Parent, o.Key, o.KeyBody, o.Status, now, now); err != nil {
		return err
	}
	s.ops.wake.signal(opKey(o.Project, o.Machine))
	return nil
}

func opKey(project, machine string) string { return project + " " + machine }

const opCols = `id, project, machine, kind, op, from_machine, parent, request_id, request, state, sent, task, result, error, created, updated`

func scanOp(sc interface{ Scan(...any) error }) (Op, error) {
	var o Op
	var opJSON string
	var created, updated int64
	err := sc.Scan(&o.ID, &o.Project, &o.Machine, &o.Kind, &opJSON, &o.FromMachine, &o.Parent, &o.Key, &o.KeyBody, &o.State, &o.Sent,
		&o.Status, &o.Result, &o.Error, &created, &updated)
	if err != nil {
		return Op{}, err
	}
	id, kind := o.ID, o.Kind
	_ = json.Unmarshal([]byte(opJSON), &o.MachineOp)
	o.ID, o.Kind = id, kind
	o.Created, o.Updated = store.FromMillis(created), store.FromMillis(updated)
	return o, nil
}

// LoadOp is op id (sql.ErrNoRows when there is none).
func (s *Service) LoadOp(ctx context.Context, id string) (Op, error) {
	return scanOp(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+opCols+` FROM team_ops WHERE id = ?`, id))
}

// FindOp is the op from fromMachine's parent with key in project (kind ""
// for any kind); sql.ErrNoRows when there is none.
func (s *Service) FindOp(ctx context.Context, project, fromMachine, parent, key, kind string) (Op, error) {
	return scanOp(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+opCols+` FROM team_ops
		WHERE project = ? AND from_machine = ? AND parent = ? AND request_id = ? AND (? = '' OR kind = ?)`, project, fromMachine, parent, key, kind, kind))
}

// OpFilter selects ops: each field set must match; Unsent is pending ops
// no answer carried.
type OpFilter struct {
	Project, Machine, Parent, Kind string
	Unsent                         bool
}

// Ops are the ops f selects, oldest first.
func (s *Service) Ops(ctx context.Context, f OpFilter) ([]Op, error) {
	q := `SELECT ` + opCols + ` FROM team_ops WHERE 1=1`
	var args []any
	for _, c := range []struct{ col, v string }{{"project", f.Project}, {"machine", f.Machine}, {"parent", f.Parent}, {"kind", f.Kind}} {
		if c.v != "" {
			q += " AND " + c.col + " = ?"
			args = append(args, c.v)
		}
	}
	if f.Unsent {
		q += " AND state = 'pending' AND sent = 0"
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, q+` ORDER BY created, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OpUpdate is what UpdateOp sets (nil: unchanged) and when: Unsent only
// while no answer carried the op, NotDone only before it is done.
type OpUpdate struct {
	State, Status, Result, Error *string
	Unsent, NotDone              bool
}

// Str is a pointer to v (for OpUpdate).
func Str(v string) *string { return &v }

// UpdateOp applies u to op id; ok when it changed a row.
func (s *Service) UpdateOp(ctx context.Context, id string, u OpUpdate) (bool, error) {
	set, args := "updated = ?", []any{store.Millis(time.Now())}
	for _, c := range []struct {
		col string
		v   *string
	}{{"state", u.State}, {"task", u.Status}, {"result", u.Result}, {"error", u.Error}} {
		if c.v != nil {
			set += ", " + c.col + " = ?"
			args = append(args, *c.v)
		}
	}
	q := `UPDATE team_ops SET ` + set + ` WHERE id = ?`
	args = append(args, id)
	if u.Unsent {
		q += ` AND state = 'pending' AND sent = 0`
	}
	if u.NotDone {
		q += ` AND state != 'done'`
	}
	res, err := s.d.Store.Registry().ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FinishOp marks op id done with result and errMsg.
func (s *Service) FinishOp(ctx context.Context, id, result, errMsg string) {
	_, _ = s.UpdateOp(ctx, id, OpUpdate{State: Str(OpDone), Result: &result, Error: &errMsg})
}

// ExpireOps ends ops their machines did not take (or report) in time, and
// blanks and prunes old records.
func (s *Service) ExpireOps(ctx context.Context) {
	db := s.d.Store.Registry()
	hs := s.handlers()
	now := time.Now()
	shortest := OpTakeWithin
	for kind, h := range hs {
		shortest = min(shortest, h.expiry(), h.reportWithin())
		if h.BlankOnAck && h.Keep > 0 {
			_, _ = db.ExecContext(ctx, `UPDATE team_ops SET result = '' WHERE kind = ? AND result != '' AND updated < ?`, kind, store.Millis(now.Add(-h.Keep)))
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT `+opCols+` FROM team_ops WHERE state = 'pending' AND created < ?`, store.Millis(now.Add(-shortest)))
	if err != nil {
		return
	}
	var xs []Op
	for rows.Next() {
		if o, err := scanOp(rows); err == nil {
			xs = append(xs, o)
		}
	}
	rows.Close()
	for _, o := range xs {
		h, ok := hs[o.Kind]
		if !ok {
			h = OpHandler{}
		}
		why, limit := "not taken in time", h.expiry()
		if o.Sent {
			limit = h.reportWithin()
			if h.ReportWithin > 0 {
				why = "its machine did not report it"
			}
		}
		if now.Sub(o.Created) <= limit {
			continue
		}
		if h.Expired != nil {
			h.Expired(ctx, o, why)
			continue
		}
		_, _ = s.UpdateOp(ctx, o.ID, OpUpdate{State: Str(OpDone), Error: &why, NotDone: true})
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM team_ops WHERE updated < ?`, store.Millis(now.Add(-OpsKeep)))
}

// PendingOps are the ops for machine in project its next heartbeat answer
// carries, oldest first.
func (s *Service) PendingOps(ctx context.Context, project, machine string) []api.MachineOp {
	return s.pendingOps(ctx, project, machine, true)
}

// pendingOps is PendingOps; without sent, only ops no answer carried yet
// (the ops poll: one carried and not reported yet goes again with beats,
// until its machine names it in a beat's Took). An op past its kind's
// Expiry never goes out (ExpireOps ends it).
func (s *Service) pendingOps(ctx context.Context, project, machine string, sent bool) []api.MachineOp {
	now := time.Now()
	cut := `CASE kind`
	var cargs []any
	for kind, h := range s.handlers() {
		cut += ` WHEN ? THEN ?`
		cargs = append(cargs, kind, store.Millis(now.Add(-h.expiry())))
	}
	cut += ` ELSE ? END`
	cargs = append(cargs, store.Millis(now.Add(-OpTakeWithin)))
	args := append([]any{project, machine, sent}, cargs...)
	args = append(args, OpsPerBeat)
	db := s.d.Store.Registry()
	rows, err := db.QueryContext(ctx, `SELECT op FROM team_ops WHERE project = ? AND machine = ? AND state = 'pending' AND took = 0 AND (? OR sent = 0)
		AND created >= `+cut+` ORDER BY created, id LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	var ops []api.MachineOp
	for rows.Next() {
		var js string
		var op api.MachineOp
		if rows.Scan(&js) == nil && json.Unmarshal([]byte(js), &op) == nil {
			ops = append(ops, op)
		}
	}
	rows.Close()
	var out []api.MachineOp
	for _, op := range ops {
		h, ok := s.handler(op.Kind)
		var refusal error
		if !ok {
			refusal = errors.New(unknownKind(op.Kind))
		} else if h.Args != nil {
			refusal = h.Args(ctx, project, &op)
		}
		if refusal == nil {
			out = append(out, op)
			continue
		}
		// Unsent: an op an answer carried (an earlier one, or one
		// overlapping this) may have run on its machine; it is never ended
		// as refused, only not carried again, and its caller hears nothing
		// from here (its machine's report answers it).
		why, _, _ := ErrParts(refusal)
		if ended, err := s.UpdateOp(ctx, op.ID, OpUpdate{State: Str(OpDone), Error: &why, Unsent: true}); err == nil && ended && h.Refused != nil {
			h.Refused(ctx, project, op, refusal)
		}
	}
	// Only an op still pending for this machine goes out: one ended or
	// moved meanwhile (a waiting line placing it elsewhere, a cancel) is
	// neither marked sent nor answered.
	sentOut := out[:0]
	for _, op := range out {
		res, err := db.ExecContext(ctx, `UPDATE team_ops SET sent = 1 WHERE id = ? AND project = ? AND machine = ? AND state = 'pending'`, op.ID, project, machine)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			sentOut = append(sentOut, op)
		}
	}
	return sentOut
}

// MarkTook notes that machine has the ops ids in project (a beat's Took):
// no answer carries them again. An id of another machine's op, or another
// project's, is ignored.
func (s *Service) MarkTook(ctx context.Context, project, machine string, ids []string) {
	if len(ids) == 0 {
		return
	}
	args := []any{project, machine}
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE team_ops SET took = 1 WHERE project = ? AND machine = ? AND took = 0 AND id IN (?` + strings.Repeat(`, ?`, len(ids)-1) + `)`
	_, _ = s.d.Store.Registry().ExecContext(ctx, q, args...)
}

// ApplyOpReports takes machine's reports on its ops in project; a report
// on another machine's op, or another project's, is ignored.
func (s *Service) ApplyOpReports(ctx context.Context, project, machine string, reps []api.MachineOpReport) {
	for _, rep := range reps {
		s.applyReport(ctx, project, machine, rep)
	}
}

func (s *Service) applyReport(ctx context.Context, project, machine string, rep api.MachineOpReport) {
	o, err := s.LoadOp(ctx, rep.ID)
	if err != nil || o.Project != project || o.Machine != machine {
		return
	}
	h, ok := s.handler(o.Kind)
	switch {
	case !ok:
		s.FinishOp(ctx, o.ID, "", unknownKind(o.Kind))
	case h.Apply != nil:
		h.Apply(ctx, o, rep)
	default:
		s.FinishOp(ctx, o.ID, string(rep.Payload), rep.Error)
	}
}

// RunHere runs op for this (the authority's) machine at once and applies
// its report. The op and its report outlive the request that brought it.
func (s *Service) RunHere(ctx context.Context, project string, op api.MachineOp) {
	ctx = context.WithoutCancel(ctx)
	s.applyReport(ctx, project, s.d.Config.Machine, s.runOp(ctx, project, op))
}

// opsRoutes registers the authority's ops poll and report routes.
func (s *Service) opsRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/team/ops", core.Authed, "a replica's node-bound token only; the project visible to it and team; only the ops queued for its own machine", jsonH(s.handleOps))
	r.HandleFunc("POST /v0/team/ops/report", core.Authed, "a replica's node-bound token only; the project visible to it and team; a report counts only for an op of its own machine", jsonH(s.handleOpsReport))
}

// opsProject is the team project sub names by ref, or not_found.
func (s *Service) opsProject(sub *authz.Subject, ref string) (api.Project, error) {
	if strings.TrimSpace(ref) == "" {
		return api.Project{}, api.Errorf(api.CodeBadRequest, "?project= is required")
	}
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return p, err
	}
	if !s.d.Store.IsTeam(p.ID) {
		return api.Project{}, api.NotFound("project", ref)
	}
	return p, nil
}

// handleOps is GET /v0/team/ops?project=&wait_ms=: the ops queued for the
// caller's machine, answered as soon as there is one.
func (s *Service) handleOps(r *http.Request) (any, error) {
	OpsPolls.Add(1)
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	if sub.Node == "" || sub.Machine == "" {
		return nil, api.Errorf(api.CodeForbidden, "machine ops go to a team replica's daemon (its node-bound token)")
	}
	q := r.URL.Query()
	p, err := s.opsProject(sub, q.Get("project"))
	if err != nil {
		return nil, err
	}
	ms, _ := strconv.Atoi(q.Get("wait_ms"))
	deadline := time.Now().Add(min(time.Duration(ms)*time.Millisecond, opsWaitMax))
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.rctx != nil {
		defer context.AfterFunc(s.rctx, cancel)() // shutdown never waits on a poll
	}
	key := opKey(p.ID, sub.Machine)
	for {
		wake := s.ops.wake.ch(key)                         // before reading: an op queued in between wakes this
		ops := s.pendingOps(ctx, p.ID, sub.Machine, false) // one carried already goes again with beats
		left := time.Until(deadline)
		if len(ops) > 0 || left <= 0 || wctx.Err() != nil {
			return api.MachineOpList{Ops: nonNil(ops)}, nil
		}
		select {
		case <-wake:
		case <-time.After(min(left, opsScopeEvery)):
		case <-wctx.Done():
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sub.TokenID != "" && !s.d.Projects.TokenLive(ctx, sub.TokenID) {
			return nil, api.Errorf(api.CodeUnauthenticated, "the token was revoked")
		}
		if core.TeamSubjectOf(ctx) != nil {
			if v, err := s.d.Projects.ScopeVersion(ctx, sub.Identity.ID); err == nil && v != sub.ScopeVersion {
				return nil, api.Errorf(api.CodeRescope, "your access to this project changed").WithHint("the replica refreshes what it holds")
			}
		}
	}
}

// handleOpsReport is POST /v0/team/ops/report.
func (s *Service) handleOpsReport(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	if sub.Node == "" || sub.Machine == "" {
		return nil, api.Errorf(api.CodeForbidden, "op reports come from a team replica's daemon (its node-bound token)")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOpsBody+1))
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "reading the body: %v", err)
	}
	if len(body) > maxOpsBody {
		return nil, api.Errorf(api.CodeTooLarge, "op reports over %d bytes", maxOpsBody)
	}
	var req api.MachineOpReports
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "invalid JSON body: %v", err)
	}
	if len(req.Ops) > maxOpReports {
		return nil, api.Errorf(api.CodeTooLarge, "at most %d reports", maxOpReports)
	}
	p, err := s.opsProject(sub, req.Project)
	if err != nil {
		return nil, err
	}
	s.ApplyOpReports(ctx, p.ID, sub.Machine, req.Ops)
	return struct{}{}, nil
}

// ---- the machine that runs them ----

// runOp runs one op here and answers its report.
func (s *Service) runOp(ctx context.Context, project string, op api.MachineOp) api.MachineOpReport {
	rep := api.MachineOpReport{ID: op.ID, Kind: op.Kind}
	h, ok := s.handler(op.Kind)
	if !ok {
		rep.Error = unknownKind(op.Kind)
		return rep
	}
	payload, err := h.Run(ctx, project, op)
	rep.Payload = payload
	if err != nil {
		rep.Error, rep.Code, rep.Hint = ErrParts(err)
	}
	return rep
}

// TakeOps runs the ops an answer carried that this machine has no record
// of: each is recorded first (never run twice, also across a restart), then
// run off the caller, its report posted at once (ReportNow) or left for
// the next beats (kicked, and fast meanwhile).
func (s *Service) TakeOps(ctx context.Context, project string, ops []api.MachineOp) {
	db := s.d.Store.Registry()
	for _, op := range ops {
		if op.ID == "" {
			continue
		}
		// Counted first, then the remote checked: a leave retires the
		// remote (Remove) before it deletes the join's row, and a forget
		// looks at the count only after that row is gone. So an op whose
		// check passed was counted before the forget looked, and the
		// forget refuses; one whose check fails records nothing, and an
		// answer that outlived a leave and a forget puts no row back
		// (zm-eakh.12.5.10.28). The insert waits for neither lock.
		s.opRunning(project, op.ID, 1)
		s.mu.RLock()
		x := s.remotes[project]
		s.mu.RUnlock()
		if x == nil || Ended(x.Snapshot()) != nil {
			s.opRunning(project, op.ID, -1)
			s.d.Log.Debug("teamlink: an answer's op for a project this machine left; not recorded", "op", op.ID)
			continue
		}
		res, err := db.ExecContext(ctx, `INSERT INTO team_ops_run (id, project, kind, state, created) VALUES (?, ?, ?, 'running', ?) ON CONFLICT (id) DO NOTHING`,
			op.ID, project, op.Kind, store.Millis(time.Now()))
		if err != nil {
			s.d.Log.Warn("teamlink: record a machine op before running it (not run; the next beat tries again)", "op", op.ID, "err", err)
			s.opRunning(project, op.ID, -1)
			continue
		}
		if n, _ := res.RowsAffected(); n != 1 {
			s.opRunning(project, op.ID, -1)
			continue // run (or running) already
		}
		s.ops.wg.Add(1)
		go func(op api.MachineOp) {
			defer s.ops.wg.Done()
			rctx := context.WithoutCancel(ctx)
			rep := s.runOp(rctx, project, op)
			h, _ := s.handler(op.Kind)
			final := h.Settled == nil || h.Settled(rep)
			js, _ := json.Marshal(rep)
			res, err := db.ExecContext(rctx, `UPDATE team_ops_run SET state = 'done', report = ?, final = ? WHERE id = ?`, string(js), final, op.ID)
			s.opRunning(project, op.ID, -1)
			if err != nil {
				s.d.Log.Warn("teamlink: record a machine op's report", "op", op.ID, "err", err)
			} else if n, _ := res.RowsAffected(); n != 1 {
				return // its row went (a forget of the project's key): the report goes nowhere
			} else if h.ReportNow && s.reportNow(rctx, project, rep) {
				return // reported at once: no beat needed
			}
			s.BeatFast() // its report goes with the next beats: soon
			s.KickBeat()
		}(op)
	}
}

// WaitOps waits for the ops running here.
func (s *Service) WaitOps() { s.ops.wg.Wait() }

// Close stops the heartbeat and the ops polls and waits for them and the
// ops running here.
func (s *Service) Close() error {
	s.StopLoops()
	s.WaitOps()
	return nil
}

// OpLocal is what op id made here (SetOpLocal) and its run state; "" while
// none. sql.ErrNoRows when this machine never took it.
func (s *Service) OpLocal(ctx context.Context, id string) (local, state string, err error) {
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT task, state FROM team_ops_run WHERE id = ?`, id).Scan(&local, &state)
	return local, state, err
}

// SetOpLocal records the id of what op id made here (Refresh gets it).
func (s *Service) SetOpLocal(ctx context.Context, id, local string) {
	_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE team_ops_run SET task = ? WHERE id = ?`, local, id)
}

// OpReports are this machine's reports for project's ops the authority has
// not taken the final word of, within OpsBeatBytes; finals are the ids
// whose report is final (AckOps them once an answer took them).
func (s *Service) OpReports(ctx context.Context, project string) (reps []api.MachineOpReport, finals []string) {
	db := s.d.Store.Registry()
	hs := s.handlers()
	for kind, h := range hs {
		if h.BlankOnAck && h.Keep > 0 { // a secret nobody took for long is dropped unsent
			_, _ = db.ExecContext(ctx, `UPDATE team_ops_run SET report = '', acked = 1 WHERE kind = ? AND acked = 0 AND created < ?`, kind, store.Millis(time.Now().Add(-h.Keep)))
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT id, kind, task, report, final FROM team_ops_run WHERE project = ? AND acked = 0 AND state = 'done' ORDER BY created, id LIMIT 200`, project)
	if err != nil {
		return nil, nil
	}
	type x struct {
		id, kind, local, report string
		final                   bool
	}
	var xs []x
	for rows.Next() {
		var v x
		if rows.Scan(&v.id, &v.kind, &v.local, &v.report, &v.final) == nil {
			xs = append(xs, v)
		}
	}
	rows.Close()
	size := 0
	for _, v := range xs {
		var rep api.MachineOpReport
		if json.Unmarshal([]byte(v.report), &rep) != nil {
			continue
		}
		if len(v.report) > OpsBeatBytes { // never sendable: its error instead
			rep = api.MachineOpReport{ID: rep.ID, Kind: rep.Kind, Error: "the report was over the heartbeat's limit", Code: api.CodeInternal}
		} else if size += len(v.report); size > OpsBeatBytes && len(reps) > 0 {
			break // the rest go with the next beats
		}
		if h := hs[v.kind]; !v.final && v.local != "" && h.Refresh != nil {
			h.Refresh(ctx, v.local, &rep)
			if h.Settled == nil || h.Settled(rep) {
				js, _ := json.Marshal(rep)
				_, _ = db.ExecContext(ctx, `UPDATE team_ops_run SET report = ?, final = 1 WHERE id = ?`, string(js), v.id)
				v.final = true
			}
		}
		reps = append(reps, rep)
		if v.final {
			finals = append(finals, v.id)
		}
	}
	return reps, finals
}

// TookOps are the ids of project's ops this machine recorded (TakeOps)
// and has not had a final report taken for, newest first, at most
// maxBeatOps: a beat's Took, so the authority sends none of them again.
// A run record left running by a process that stopped mid-op, and older
// than its kind's ReportWithin (by when the authority has ended the op),
// is ended first (endStuckRuns): it would ride every beat otherwise.
func (s *Service) TookOps(ctx context.Context, project string) []string {
	s.endStuckRuns(ctx, project)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id FROM team_ops_run WHERE project = ? AND acked = 0 ORDER BY created DESC, id DESC LIMIT ?`, project, maxBeatOps)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// endStuckRuns ends project's run records that are still running, are not
// run by this process (it stopped mid-op: the op never runs again and never
// reports) and are older than their kind's ReportWithin, by when the
// authority has ended the op itself. Each is marked done and acked with no
// report: nothing is sent, it leaves every beat's Took, and the record
// stays (acked) until OpsKeep, so an answer that carries the op again does
// not run it a second time. Nothing here touches an op this process runs.
func (s *Service) endStuckRuns(ctx context.Context, project string) {
	db := s.d.Store.Registry()
	now := time.Now()
	hs := s.handlers()
	rows, err := db.QueryContext(ctx, `SELECT id, kind, created FROM team_ops_run WHERE project = ? AND state = 'running' AND acked = 0`, project)
	if err != nil {
		return
	}
	type run struct {
		id, kind string
		created  int64
	}
	var stuck []run
	for rows.Next() {
		var v run
		if rows.Scan(&v.id, &v.kind, &v.created) != nil {
			continue
		}
		limit := OpHandler{}.reportWithin() // an unregistered kind: the default
		if h, ok := hs[v.kind]; ok {
			limit = h.reportWithin()
		}
		if store.Millis(now.Add(-limit)) > v.created {
			stuck = append(stuck, v)
		}
	}
	rows.Close()
	for _, v := range stuck {
		if s.opLive(v.id) {
			continue
		}
		// The state check keeps a record its op just reported; created, one
		// a forget and a new join of the same op id put in its place.
		_, _ = db.ExecContext(ctx, `UPDATE team_ops_run SET state = 'done', report = '', final = 1, acked = 1 WHERE id = ? AND state = 'running' AND created = ?`, v.id, v.created)
	}
}

// AckOps: an answer took these final reports (one holding a secret is
// blanked then).
func (s *Service) AckOps(ctx context.Context, ids []string) {
	db := s.d.Store.Registry()
	for _, id := range ids {
		var kind string
		_ = db.QueryRowContext(ctx, `SELECT kind FROM team_ops_run WHERE id = ?`, id).Scan(&kind)
		if h, _ := s.handler(kind); h.BlankOnAck {
			_, _ = db.ExecContext(ctx, `UPDATE team_ops_run SET acked = 1, report = '' WHERE id = ?`, id)
		} else {
			_, _ = db.ExecContext(ctx, `UPDATE team_ops_run SET acked = 1 WHERE id = ?`, id)
		}
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM team_ops_run WHERE acked = 1 AND created < ?`, store.Millis(time.Now().Add(-OpsKeep)))
}

// reportNow posts rep to project's authority as soon as it is recorded;
// false leaves it to the next heartbeat.
func (s *Service) reportNow(ctx context.Context, project string, rep api.MachineOpReport) bool {
	x := s.ByName(project)
	if x == nil || OpsPollOff.Load() || Ended(x.Snapshot()) != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if err := s.CallRemote(cctx, x, http.MethodPost, "/v0/team/ops/report", api.MachineOpReports{Project: project, Ops: []api.MachineOpReport{rep}}, nil); err != nil {
		s.d.Log.Debug("teamlink: op report (the next heartbeat carries it)", "op", rep.ID, "err", err)
		return false
	}
	s.AckOps(ctx, []string{rep.ID})
	return true
}

// wantState is, per joined project, the reasons this machine wants ops
// without the heartbeat's wait (then it polls), and a wake for when the
// first one comes on.
type wantState struct {
	mu   sync.Mutex
	by   map[string]map[string]bool
	wake wakes
}

// WantsOps turns reason on or off for project: while any reason is on,
// this machine holds GET /v0/team/ops open at project's authority.
func (s *Service) WantsOps(project, reason string, on bool) {
	w := &s.ops.want
	w.mu.Lock()
	if w.by == nil {
		w.by = map[string]map[string]bool{}
	}
	was := len(w.by[project]) > 0
	if on {
		if w.by[project] == nil {
			w.by[project] = map[string]bool{}
		}
		w.by[project][reason] = true
	} else {
		delete(w.by[project], reason)
	}
	now := len(w.by[project]) > 0
	w.mu.Unlock()
	if now && !was {
		w.wake.signal(project)
	}
}

func (s *Service) wantsOps(project string) bool {
	w := &s.ops.want
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.by[project]) > 0
}

// Tunables of the ops poll's retries.
var (
	opsRetryMin = 250 * time.Millisecond
	opsRetryMax = 5 * time.Second
	// opsOlder: an authority without the route is asked again after this
	// (the heartbeat carries ops meanwhile).
	opsOlder = time.Hour
)

// OpsLoop holds x's GET /v0/team/ops open while this machine wants ops
// from x's project (WantsOps), and takes the ops it answers, until ctx
// ends; it idles while x's access is ended (role recovery may restore it).
func (s *Service) OpsLoop(ctx context.Context, x *Remote) {
	backoff := opsRetryMin
	for ctx.Err() == nil {
		snap := x.Snapshot()
		id := snap.Project.ID
		if Ended(snap) != nil || OpsPollOff.Load() || !s.wantsOps(id) {
			select {
			case <-ctx.Done():
			case <-s.ops.want.wake.ch(id):
			case <-time.After(opsIdle):
			}
			continue
		}
		fence := x.RoleReply()
		q := url.Values{"project": {id}, "wait_ms": {strconv.Itoa(int(OpsWait / time.Millisecond))}}
		cctx, cancel := context.WithTimeout(ctx, OpsWait+CallTimeout)
		var out api.MachineOpList
		err := s.CallRemote(cctx, x, http.MethodGet, "/v0/team/ops?"+q.Encode(), nil, &out)
		cancel()
		if err != nil {
			wait := backoff
			if OlderAuthority(err) {
				wait = opsOlder
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			backoff = min(backoff*2, opsRetryMax)
			continue
		}
		backoff = opsRetryMin
		for _, op := range out.Ops {
			if !fence.Current() {
				break // a later role lifetime cannot admit an older answer's operations
			}
			s.TakeOps(ctx, id, []api.MachineOp{op})
		}
	}
}

// wakes are one-shot signals by key: ch is closed by the next signal.
type wakes struct {
	mu sync.Mutex
	by map[string]chan struct{}
}

func (w *wakes) ch(key string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.by == nil {
		w.by = map[string]chan struct{}{}
	}
	c := w.by[key]
	if c == nil {
		c = make(chan struct{})
		w.by[key] = c
	}
	return c
}

func (w *wakes) signal(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c := w.by[key]; c != nil {
		close(c)
		delete(w.by, key)
	}
}

// IsNoOp reports sql.ErrNoRows (no such op).
func IsNoOp(err error) bool { return errors.Is(err, sql.ErrNoRows) }
