package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/runtarget/rt"
	"github.com/sengirunners/potluck/vmhost"
)

// potluck host jobs reads the jobs of the box sizes in this machine's
// bindings file from their state directories, with no daemon; a relayed
// job's pool is said to be unnamed while no daemon answers.
func TestHostJobsReadsStateDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pl := filepath.Join(home, ".potluck")
	t.Setenv(EnvHome, pl)
	t.Setenv("ZM_MACHINE", "jobs-test")
	if err := os.MkdirAll(pl, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	box := computeapi.HostRunTarget{RunTarget: computeapi.RunTarget{Name: "small", Driver: "potluck-driver-tart", Kind: computeapi.RunTargetKindBox, Isolation: "ephemeral",
		Images: []string{"ghcr.io/o/a@sha256:" + strings.Repeat("ab", 32)}, Config: map[string]string{"state_dir": state}}, Share: []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}
	other := box // a second size on the same state directory: its jobs are listed once
	other.Name = "large"
	if err := os.WriteFile(filepath.Join(pl, "run_targets.toml"), []byte(rt.EncodeHostRunTargets([]computeapi.HostRunTarget{box, other})), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	var lines []byte
	for _, j := range []vmhost.Job{
		{Handle: "box:small/a", Binding: "small", Kind: "box", Size: "small", CPU: 2, Start: start, End: start.Add(30 * time.Minute), UpS: 1800, State: "released"},
		{Handle: "box:small/b", Binding: "small", Kind: "box", Size: "small", CPU: 2, Start: start.Add(time.Minute), End: start.Add(2 * time.Minute), UpS: 60, State: "expired"},
	} {
		b, _ := json.Marshal(j)
		lines = append(append(lines, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(state, "jobs.jsonl"), lines, 0o600); err != nil {
		t.Fatal(err)
	}
	led := `{"version":2,"handles":{"box:small/a":{"project":"01ARZ3NDEKTSV4RRFFQ69G5FAV","at":"` + start.Format(time.RFC3339) + `"}}}`
	if err := os.WriteFile(filepath.Join(pl, "host_handles.json"), []byte(led), 0o600); err != nil {
		t.Fatal(err)
	}

	var o, er bytes.Buffer
	if code := Run(context.Background(), []string{"--json", "host", "jobs"}, strings.NewReader(""), &o, &er); code != 0 {
		t.Fatalf("host jobs: exit %d\n%s", code, er.String())
	}
	var v menubar.JobsView
	if err := json.Unmarshal(o.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Total != 2 || v.Last12h != 2 || v.Running != 0 || v.Energy.TotalWh != 2*0.5*menubar.WattsPerCore+2*(60.0/3600)*menubar.WattsPerCore {
		t.Fatalf("view %+v", v)
	}
	if v.Jobs[0].Handle != "box:small/b" || v.Jobs[0].Pool != "this Mac" || v.Jobs[0].Ended != "reached its time limit" ||
		!strings.Contains(v.Jobs[1].Pool, "could not be read") || !strings.Contains(v.Note, "not running") {
		t.Errorf("jobs %+v", v.Jobs)
	}

	o.Reset()
	if code := Run(context.Background(), []string{"host", "jobs"}, strings.NewReader(""), &o, &er); code != 0 {
		t.Fatalf("host jobs: exit %d\n%s", code, er.String())
	}
	for _, want := range []string{"2 started in the last 12 hours", "Electricity, estimated", "STARTED", "reached its time limit", "Today"} {
		if !strings.Contains(o.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, o.String())
		}
	}
}
