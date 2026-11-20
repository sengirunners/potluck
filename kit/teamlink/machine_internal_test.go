package teamlink

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// An authority takes a newer replica's machine numbers with fields it does
// not know (they only grow), instead of refusing the whole report.
func TestMachineReportIgnoresNewerFields(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v0/team/machine", strings.NewReader(`{"project":"p1","stats":{"memory_total_mb":8,"from_the_future":true},"also_new":1}`))
	var rep api.MachineReport
	if err := DecodeMachineReport(r, &rep); err != nil || rep.Project != "p1" || rep.Stats.MemoryTotalMB != 8 {
		t.Fatalf("report %+v: %v", rep, err)
	}
	r, _ = http.NewRequest(http.MethodPost, "/v0/team/machine", strings.NewReader(`{"project":`))
	if err := DecodeMachineReport(r, &rep); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("broken JSON: %v", err)
	}
}

// Only the field an older authority names is left out: an unrelated refusal
// drops nothing.
func TestRefusedField(t *testing.T) {
	for msg, want := range map[string]string{
		`invalid JSON body: json: unknown field "script_runs"`:   "script_runs",
		`invalid JSON body: json: unknown field "zm"`:            "zm",
		`invalid JSON body: json: unknown field "task_slots"`:    "task_slots",
		`invalid JSON body: json: unknown field "sandbox_tasks"`: "sandbox_tasks",
		`invalid JSON body: unexpected EOF`:                      "",
	} {
		if got := refusedField(api.Errorf(api.CodeBadRequest, "%s", msg)); got != want {
			t.Errorf("%q: %q, want %q", msg, got, want)
		}
	}
	if got := refusedField(api.Errorf(api.CodeForbidden, `unknown field "zm"`)); got != "" {
		t.Errorf("a forbidden answer named %q", got)
	}
}

// Rename step 2: the heartbeat names the build under "potluck", whatever
// field the machine store filled (zm's fills Zm); a step 1 authority
// decodes it strictly (its MachineStats has both fields) and reads it with
// Build.
func TestHeartbeatBuildUnderNewName(t *testing.T) {
	st := api.MachineStats{Zm: &api.MachineZm{Version: "v9"}}
	buildUnderNewName(&st)
	if st.Zm != nil || st.Potluck == nil || st.Potluck.Version != "v9" {
		t.Fatalf("%+v", st)
	}
	b, _ := json.Marshal(api.MachineReport{Project: "p", Stats: st})
	if strings.Contains(string(b), `"zm"`) {
		t.Fatalf("sent: %s", b)
	}
	var got api.MachineReport
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil || got.Stats.Build() == nil || got.Stats.Build().Version != "v9" {
		t.Fatalf("step 1 authority's decode: %+v %v", got, err)
	}
	st = api.MachineStats{Potluck: &api.MachineZm{Version: "a"}, Zm: &api.MachineZm{Version: "b"}}
	buildUnderNewName(&st)
	if st.Zm != nil || st.Potluck.Version != "a" {
		t.Fatalf("both set: %+v", st)
	}
}

// An authority from before sandbox tasks decodes strictly and refuses
// sandbox_tasks or sandbox_slots: the report goes again without both, and
// the rest of the numbers still arrive.
func TestMachineReportDropsSandboxForOlderAuthority(t *testing.T) {
	// An authority before sandbox tasks: its strict decode names the first
	// field its MachineStats lacks.
	older := func(st api.MachineStats) error {
		b, _ := json.Marshal(st)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		for _, f := range []string{"sandbox_slots", "sandbox_tasks"} {
			if _, ok := got[f]; ok {
				return api.Errorf(api.CodeBadRequest, "invalid JSON body: json: unknown field %q", f)
			}
		}
		return nil
	}
	m := &machineState{noZm: map[string]time.Time{}, noRuns: map[string]time.Time{}}
	st := api.MachineStats{MemoryTotalMB: 8, RemoteTasks: true, TaskSlots: 2, SandboxTasks: true, SandboxSlots: 2}
	err := older(st)
	for tries := 0; err != nil; tries++ {
		if tries > 2 || !m.drop(refusedField(err), &st, "p1", time.Now()) {
			t.Fatalf("not dropped after %d tries: %v (%+v)", tries, err, st)
		}
		err = older(st)
	}
	if st.SandboxTasks || st.SandboxSlots != 0 || st.TaskSlots != 2 || !st.RemoteTasks || st.MemoryTotalMB != 8 {
		t.Fatalf("after the back-off: %+v", st)
	}
	// Each field drops once; slots alone (no flag) go too.
	if m.drop("sandbox_tasks", &st, "p1", time.Now()) {
		t.Fatal("dropped twice")
	}
	st.SandboxSlots = 3
	if !m.drop("sandbox_slots", &st, "p1", time.Now()) || st.SandboxSlots != 0 {
		t.Fatalf("slots alone: %+v", st)
	}
}

// olderBuild is the build block as an authority from before the update job
// reads it: every field but job.
type olderBuild struct {
	Version   string           `json:"version"`
	Commit    string           `json:"commit"`
	Started   time.Time        `json:"started"`
	Release   string           `json:"release,omitempty"`
	Installed time.Time        `json:"installed,omitzero"`
	Held      string           `json:"held,omitempty"`
	HeldSince time.Time        `json:"held_since,omitzero"`
	Last      *api.UpdateCheck `json:"last,omitempty"`
	Stable    []string         `json:"stable,omitempty"`
	Failed    []string         `json:"failed,omitempty"`
}

// olderAuthority decodes a report's build as an authority from before the
// update job does, strictly (its decode error names the nested field alone,
// as encoding/json does) or not; knowsPotluck is false for one from before
// the build's new name, which refuses "potluck" first.
func olderAuthority(st api.MachineStats, strict, knowsPotluck bool) (map[string]olderBuild, error) {
	b, _ := json.Marshal(api.MachineReport{Project: "p1", Stats: st})
	var rep struct {
		Stats map[string]json.RawMessage `json:"stats"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, err
	}
	if _, ok := rep.Stats["potluck"]; ok && strict && !knowsPotluck {
		return nil, api.Errorf(api.CodeBadRequest, "invalid JSON body: json: unknown field %q", "potluck")
	}
	got := map[string]olderBuild{}
	for _, f := range []string{"potluck", "zm"} {
		raw, ok := rep.Stats[f]
		if !ok {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		if strict {
			dec.DisallowUnknownFields()
		}
		var ob olderBuild
		if err := dec.Decode(&ob); err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "invalid JSON body: %v", err)
		}
		got[f] = ob
	}
	return got, nil
}

// An authority from before the build's update job: a strict one refuses
// "job", and the report goes again without it, so the machine's other
// numbers (its build included) still arrive, and the job stays out for the
// back-off; a tolerant one takes the report with the job in it as it is.
func TestMachineReportDropsJobForOlderAuthority(t *testing.T) {
	zero := int64(0)
	kept := &api.MachineZm{Version: "v9", Held: "testing", Job: &api.UpdateJob{MinAgeS: &zero}}
	fresh := func() api.MachineStats {
		return api.MachineStats{MemoryTotalMB: 8, ScriptRuns: true, Potluck: kept}
	}

	// Tolerant: nothing to drop.
	if got, err := olderAuthority(fresh(), false, true); err != nil || got["potluck"].Version != "v9" {
		t.Fatalf("tolerant authority: %+v %v", got, err)
	}

	for _, knowsPotluck := range []bool{true, false} {
		m := &machineState{noZm: map[string]time.Time{}, noJob: map[string]time.Time{}, noRuns: map[string]time.Time{}}
		st := fresh()
		now := time.Now()
		_, err := olderAuthority(st, true, knowsPotluck)
		if err == nil {
			t.Fatal("a strict older authority took the job")
		}
		var got map[string]olderBuild
		for tries := 0; err != nil; tries++ {
			if tries > 2 || !m.drop(refusedField(err), &st, "p1", now) {
				t.Fatalf("knows potluck %v: not dropped after %d tries: %v (%+v)", knowsPotluck, tries, err, st)
			}
			got, err = olderAuthority(st, true, knowsPotluck)
		}
		b := st.Build()
		if b == nil || b.Job != nil || st.MemoryTotalMB != 8 || !st.ScriptRuns {
			t.Fatalf("knows potluck %v: after the back-off: %+v %+v", knowsPotluck, st, b)
		}
		name := "potluck"
		if !knowsPotluck {
			name = "zm"
		}
		if ob, ok := got[name]; !ok || ob.Version != "v9" || ob.Held != "testing" {
			t.Fatalf("knows potluck %v: the authority read %+v", knowsPotluck, got)
		}
		if !now.Before(m.noJob["p1"]) || now.Before(m.noZm["p1"]) {
			t.Fatalf("knows potluck %v: back-off job %v zm %v", knowsPotluck, m.noJob["p1"], m.noZm["p1"])
		}
		if m.drop("job", &st, "p1", now) {
			t.Fatalf("knows potluck %v: dropped twice", knowsPotluck)
		}
		// The next send within the hour leaves the job out before it goes,
		// so the authority takes it at once; after the hour the job goes
		// again.
		next := fresh()
		buildUnderNewName(&next)
		m.leaveOut(&next, "p1", now.Add(time.Minute))
		if knowsPotluck {
			if _, err := olderAuthority(next, true, true); err != nil || next.Build().Job != nil || next.Build().Version != "v9" || !next.ScriptRuns {
				t.Fatalf("the next send: %+v %v", next.Build(), err)
			}
		} else if next.Build() == nil || next.Build().Job != nil {
			t.Fatalf("knows potluck %v: the next send carries %+v", knowsPotluck, next.Build())
		}
		later := fresh()
		m.leaveOut(&later, "p1", now.Add(machineBackoff+time.Minute))
		if later.Build().Job == nil {
			t.Fatalf("knows potluck %v: the job stays out after the back-off", knowsPotluck)
		}
	}
	// The store's own value is never changed.
	if kept.Job == nil {
		t.Fatal("the dropped job was cut from the store's value")
	}
}

// An older strict authority through the real send loop: the first send costs
// one refusal and arrives without the job; sends within the hour go out
// without it at once (one request each); after the hour the job goes again.
func TestSendNumbersLeavesJobOutWithinBackoff(t *testing.T) {
	min := int64(600)
	kept := &api.MachineZm{Version: "v9", Job: &api.UpdateJob{MinAgeS: &min}}
	fresh := func() api.MachineStats {
		return api.MachineStats{MemoryTotalMB: 8, ScriptRuns: true, Potluck: kept}
	}
	for _, knowsPotluck := range []bool{true, false} {
		m := &machineState{noZm: map[string]time.Time{}, noJob: map[string]time.Time{}, noRuns: map[string]time.Time{}}
		requests := 0
		var last map[string]olderBuild
		send := func(st api.MachineStats) error {
			requests++
			var err error
			last, err = olderAuthority(st, true, knowsPotluck)
			return err
		}
		now := time.Now()
		st := fresh()
		buildUnderNewName(&st)
		if err := m.sendNumbers(st, "p1", now, send); err != nil {
			t.Fatalf("knows potluck %v: first send: %v", knowsPotluck, err)
		}
		first := requests
		if first < 2 {
			t.Fatalf("knows potluck %v: the first send made %d requests, want a refusal then a resend", knowsPotluck, first)
		}
		for _, ob := range last {
			if ob.Version != "v9" {
				t.Fatalf("knows potluck %v: the authority read %+v", knowsPotluck, last)
			}
		}
		// Within the hour: no refusal for the job. One request, or two for an
		// authority from before the build's new name, which refuses "potluck"
		// on every send (there is no back-off for the rename).
		want := 1
		if !knowsPotluck {
			want = 2
		}
		requests = 0
		next := fresh()
		buildUnderNewName(&next)
		if err := m.sendNumbers(next, "p1", now.Add(time.Minute), send); err != nil || requests != want {
			t.Fatalf("knows potluck %v: the next send made %d requests: %v", knowsPotluck, requests, err)
		}
		// After the hour the job goes out again, and is refused again.
		requests = 0
		later := fresh()
		buildUnderNewName(&later)
		if err := m.sendNumbers(later, "p1", now.Add(machineBackoff+time.Minute), send); err != nil || requests <= want {
			t.Fatalf("knows potluck %v: after the back-off %d requests: %v", knowsPotluck, requests, err)
		}
		if kept.Job == nil {
			t.Fatal("the dropped job was cut from the store's value")
		}
	}
}
