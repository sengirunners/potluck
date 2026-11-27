package teamlink

// Machine numbers across machines (SPEC 8 "Machine numbers"): a member
// sends its numbers on their own route after a heartbeat went through, at
// most every MachineEvery per project; an authority records them only for
// a machine whose token reads the project whole (a member's, never an area
// guest's), through the session hooks' Machines when a module sets it, else
// in memory, and serves them on GET /v0/machines.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Tunables (tests change them).
var (
	MachineEvery = 30 * time.Second
	// machineBackoff is how long a member waits before trying an authority
	// that did not know the route (an older one) again.
	machineBackoff = time.Hour
	// MachinePath is the route (a test points it at a missing one to play
	// an older authority).
	MachinePath = "/v0/team/machine"
	// DecodeMachineReport decodes a report, ignoring fields this build does
	// not know: machine numbers only grow, and a newer member's report must
	// not be refused whole (a test plays an authority older than a field).
	DecodeMachineReport = func(r *http.Request, v any) error {
		if err := json.NewDecoder(io.LimitReader(r.Body, api.MaxBody)).Decode(v); err != nil && err != io.EOF {
			return api.Errorf(api.CodeBadRequest, "invalid JSON body: %v", err)
		}
		return nil
	}
)

// machineState is a member's sends per joined project (the last report, an
// older authority's backoff, until when to leave out the zm block, the
// build's update job and release sources and script_runs, which an authority older than them
// refuses) and an authority's reports in memory when no module keeps them.
type machineState struct {
	mu        sync.Mutex
	sent      map[string]time.Time
	off       map[string]time.Time
	noZm      map[string]time.Time
	noJob     map[string]time.Time
	noSources map[string]time.Time
	noRuns    map[string]time.Time
	mem       map[[2]string]core.MachineSeen // by (machine, project)
}

// machineStore is the module that keeps machine numbers, or nil.
func (s *Service) machineStore() core.MachinePresence {
	if h := s.d.SessionHooks(); h != nil {
		return h.Machines
	}
	return nil
}

// localStats are this machine's numbers: the store's, else only the build.
func (s *Service) localStats(ctx context.Context) api.MachineStats {
	if mp := s.machineStore(); mp != nil {
		return mp.MachineStats(ctx)
	}
	st := api.MachineStats{MemoryUsedMB: -1, MemoryTotalMB: -1, SwapUsedMB: -1, SwapTotalMB: -1, DiskFreeMB: -1, DiskTotalMB: -1, At: time.Now().UTC()}
	if s.d.Zm != nil {
		st.Potluck = s.d.Zm()
	}
	return st
}

// buildUnderNewName moves the build a store reported under "zm" to
// "potluck", the field this build sends (an authority from step 1 on reads
// either: MachineStats.Build).
func buildUnderNewName(st *api.MachineStats) {
	if st.Potluck == nil {
		st.Potluck = st.Zm
	}
	st.Zm = nil
}

// withoutBuildField leaves one field out of st's build, under either name:
// has says whether a build carries it, clear leaves it out of a copy. It
// reports whether there was one. The build is copied first: a store may
// hand out a value it keeps.
func withoutBuildField(st *api.MachineStats, has func(*api.MachineZm) bool, clear func(*api.MachineZm)) bool {
	cut := func(z **api.MachineZm) bool {
		if *z == nil || !has(*z) {
			return false
		}
		c := **z
		clear(&c)
		*z = &c
		return true
	}
	a := cut(&st.Potluck)
	b := cut(&st.Zm)
	return a || b
}

// withoutJob leaves the update job out of st's build and reports whether
// there was one.
func withoutJob(st *api.MachineStats) bool {
	return withoutBuildField(st, func(z *api.MachineZm) bool { return z.Job != nil }, func(z *api.MachineZm) { z.Job = nil })
}

// withoutSources leaves the release sources out of st's build and reports
// whether there were any.
func withoutSources(st *api.MachineStats) bool {
	return withoutBuildField(st, func(z *api.MachineZm) bool { return len(z.Sources) > 0 }, func(z *api.MachineZm) { z.Sources = nil })
}

// drop leaves the field an older authority refused out of st, and reports
// whether there was one to leave out (m.mu held). Each field drops at most
// once, so the resend loop ends.
func (m *machineState) drop(f string, st *api.MachineStats, id string, now time.Time) bool {
	switch {
	case f == "script_runs" && st.ScriptRuns:
		m.noRuns[id] = now.Add(machineBackoff)
		st.ScriptRuns = false
	case f == "potluck" && st.Potluck != nil:
		// An authority from before the field's new name: the old name.
		st.Zm, st.Potluck = st.Potluck, nil
	case f == "zm" && st.Zm != nil:
		m.noZm[id] = now.Add(machineBackoff)
		st.Zm = nil
	case f == "job" && withoutJob(st):
		// An authority from before the build's update job (nested in the
		// build, so its decode names only "job"): the rest of the build
		// still goes.
		m.noJob[id] = now.Add(machineBackoff)
	case f == "sources" && withoutSources(st):
		// An authority from before the build's release sources: as for the
		// job, the rest of the build still goes.
		m.noSources[id] = now.Add(machineBackoff)
	case f == "task_slots" && st.TaskSlots != 0:
		st.TaskSlots = 0 // an authority from before the waiting line
	case (f == "sandbox_tasks" || f == "sandbox_slots") && (st.SandboxTasks || st.SandboxSlots != 0):
		// An authority from before sandbox tasks (SPEC 8 "Waiting line"):
		// both go, since slots mean nothing without the flag.
		st.SandboxTasks, st.SandboxSlots = false, 0
	default:
		return false
	}
	return true
}

// leaveOut leaves out of st, before it is sent, each field an older
// authority of project id refused within the back-off (m.mu held).
func (m *machineState) leaveOut(st *api.MachineStats, id string, now time.Time) {
	if now.Before(m.noZm[id]) {
		st.Potluck = nil
	}
	if now.Before(m.noJob[id]) {
		withoutJob(st)
	}
	if now.Before(m.noSources[id]) {
		withoutSources(st)
	}
	if now.Before(m.noRuns[id]) {
		st.ScriptRuns = false
	}
}

// sendNumbers sends st for project id through send: first without the fields
// an older authority refused within the back-off, then, on each refusal that
// names a field, again without it. An authority older than a field decodes
// strictly and refuses the field it names; the field stays out for a while
// (script_runs: SPEC 8 "Script runs"; zm, job and sources: SPEC 2 "Release channel";
// sandbox_tasks/sandbox_slots: SPEC 8 "Waiting line"). It returns the last
// send's error.
func (m *machineState) sendNumbers(st api.MachineStats, id string, now time.Time, send func(api.MachineStats) error) error {
	m.mu.Lock()
	m.leaveOut(&st, id, now)
	m.mu.Unlock()
	err := send(st)
	for err != nil {
		m.mu.Lock()
		dropped := m.drop(refusedField(err), &st, id, now)
		m.mu.Unlock()
		if !dropped {
			break
		}
		err = send(st)
	}
	return err
}

// reportMachine sends this machine's numbers to x's authority when they are
// due (a heartbeat to it just went through).
func (s *Service) reportMachine(ctx context.Context, x *Remote, snap api.Remote) {
	m := &s.numbers
	id := snap.Project.ID
	m.mu.Lock()
	if m.sent == nil {
		m.sent, m.off, m.noZm, m.noJob, m.noSources, m.noRuns = map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}
	}
	now := time.Now()
	if now.Before(m.off[id]) || now.Sub(m.sent[id]) < MachineEvery {
		m.mu.Unlock()
		return
	}
	m.sent[id] = now
	m.mu.Unlock()
	st := s.localStats(ctx)
	buildUnderNewName(&st)
	err := m.sendNumbers(st, id, now, func(st api.MachineStats) error {
		cctx, cancel := context.WithTimeout(ctx, BeatTimeout)
		defer cancel()
		return s.CallRemote(cctx, x, http.MethodPost, MachinePath, api.MachineReport{Project: id, Stats: st}, nil)
	})
	if err == nil {
		return
	}
	if OlderAuthority(err) {
		// An authority older than the route: quiet, and not again for a while.
		m.mu.Lock()
		m.off[id] = now.Add(machineBackoff)
		m.mu.Unlock()
		return
	}
	s.d.Log.Debug("teamlink: machine numbers", "project", snap.Project.Slug, "authority", snap.Authority, "err", err)
}

// refusedField is the field an authority refused as unknown (api.DecodeJSON:
// `json: unknown field "x"`), as one older than the field does; "" for any
// other error.
func refusedField(err error) string {
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeBadRequest {
		return ""
	}
	_, rest, ok := strings.Cut(e.Message, `unknown field "`)
	if !ok {
		return ""
	}
	f, _, _ := strings.Cut(rest, `"`)
	return f
}

// handleMachine is POST /v0/team/machine (the authority's side).
func (s *Service) handleMachine(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	if sub.Node == "" || sub.Machine == "" {
		return nil, api.Errorf(api.CodeForbidden, "machine numbers come from a team replica's daemon (its node-bound token)")
	}
	var req api.MachineReport
	if err := DecodeMachineReport(r, &req); err != nil {
		return nil, err
	}
	p, err := s.d.Projects.Get(sub.Scope, req.Project)
	if err != nil {
		return nil, err
	}
	if !s.d.Store.IsTeam(p.ID) {
		return nil, api.NotFound("project", req.Project)
	}
	mp := s.machineStore()
	m := &s.numbers
	key := [2]string{sub.Machine, p.ID}
	if !sub.Scope.WholeProject(p.ID) {
		// An area guest's machine: not recorded, so never shown (SPEC 8); numbers it sent while it was a member go.
		if mp != nil {
			return struct{}{}, mp.ForgetMachine(ctx, p.ID, sub.Machine)
		}
		m.mu.Lock()
		delete(m.mem, key)
		m.mu.Unlock()
		return struct{}{}, nil
	}
	if mp != nil {
		if err := mp.RecordMachine(ctx, p.ID, sub.Machine, req.Stats); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	}
	m.mu.Lock()
	if m.mem == nil {
		m.mem = map[[2]string]core.MachineSeen{}
	}
	m.mem[key] = core.MachineSeen{Machine: sub.Machine, Project: p.ID, Stats: req.Stats, Seen: time.Now().UTC()}
	m.mu.Unlock()
	return struct{}{}, nil
}

// machineRows are the recorded reports: the store's, else memory's.
func (s *Service) machineRows(ctx context.Context) ([]core.MachineSeen, error) {
	if mp := s.machineStore(); mp != nil {
		return mp.MachineRows(ctx)
	}
	m := &s.numbers
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]core.MachineSeen, 0, len(m.mem))
	for _, r := range m.mem {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out, nil
}

// handleMachines is GET /v0/machines.
func (s *Service) handleMachines(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	out := api.MachineList{Machines: []api.Machine{}}
	if s.seesSelf(sub) {
		out.Machines = append(out.Machines, api.Machine{Machine: s.d.Config.Machine, Stats: s.localStats(ctx)})
	}
	rows, err := s.machineRows(ctx)
	if err != nil {
		return nil, err
	}
	out.Machines = append(out.Machines, s.d.VisibleMachines(ctx, s.d.Config.Machine, sub, rows)...)
	return out, nil
}

// seesSelf: this machine's numbers are its owner's, and a whole-project
// reader's of a team project it is the authority of.
func (s *Service) seesSelf(sub *authz.Subject) bool {
	if sub == nil {
		return false
	}
	if sub.Scope.IsAll() {
		return true
	}
	for _, p := range s.d.Store.TeamProjects() {
		if sub.Scope.WholeProject(p) {
			return true
		}
	}
	return false
}
