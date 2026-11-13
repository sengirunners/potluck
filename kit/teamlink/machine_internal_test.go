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
