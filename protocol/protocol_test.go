package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProtocolNames(t *testing.T) {
	if !SameProtocol("potluck/1", Protocol) || !SameProtocol("zm/v0.2", "potluck/1") || SameProtocol("potluck/2", Protocol) {
		t.Fatal("SameProtocol")
	}
	if ProtocolMajor("potluck/1") == ProtocolMajor(ProtocolOld) || Protocol != "potluck/1" || ProtocolOld != "zm/v0" || ProtocolNext != Protocol {
		t.Fatal("protocol names")
	}
	var r Request
	if err := json.Unmarshal([]byte(`{"potluck":"potluck/1","verb":"describe"}`), &r); err != nil || r.Protocol() != "potluck/1" {
		t.Fatalf("new key: %+v %v", r, err)
	}
	r = Request{}
	if err := json.Unmarshal([]byte(`{"zm":"zm/v0","verb":"describe"}`), &r); err != nil || r.Protocol() != "zm/v0" {
		t.Fatalf("old key: %+v %v", r, err)
	}
	// What a daemon sends (rename step 2): both keys. A step 1 driver reads
	// "zm" (or nothing); one of this build reads "potluck" first.
	b, _ := json.Marshal(NewRequest("describe", "", nil, nil))
	if string(b) != `{"zm":"zm/v0","potluck":"potluck/1","verb":"describe"}` {
		t.Fatalf("sent %s", b)
	}
	var old struct {
		Spec string `json:"zm"`
	}
	if json.Unmarshal(b, &old) != nil || old.Spec != "zm/v0" || json.Unmarshal(b, &r) != nil || r.Protocol() != "potluck/1" {
		t.Fatalf("read back: %+v %+v", old, r)
	}
}

func writeExe(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A driver is found under its new name first, else its old one.
func TestLookDriver(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	old := writeExe(t, dir, "zm-run-x")
	if p, err := LookDriver("x"); err != nil || p != old {
		t.Fatalf("old name only: %q %v", p, err)
	}
	nw := writeExe(t, dir, "potluck-driver-x")
	if p, err := LookDriver("x"); err != nil || p != nw {
		t.Fatalf("both names: %q %v", p, err)
	}
	if p, err := LookDriver("zm-run-x"); err != nil || p != old {
		t.Fatalf("a program name: %q %v", p, err)
	}
	if _, err := LookDriver("y"); err == nil {
		t.Fatal("no driver found one")
	}
	for in, want := range map[string]string{"zm-run-tart": "tart", "potluck-driver-tart": "tart", "tart": "", "zm-e2b": ""} {
		if got := DriverProgram(in); got != want {
			t.Errorf("DriverProgram(%q) = %q", in, got)
		}
	}
}

// A fresh machine's state goes to ~/.potluck-driver-<name>; one that has
// ~/.zm-run-<name> keeps it (never moved).
func TestDriverStateDir(t *testing.T) {
	home := t.TempDir()
	if got := DriverStateDir(home, "tart"); got != filepath.Join(home, ".potluck-driver-tart") {
		t.Fatalf("fresh: %s", got)
	}
	old := filepath.Join(home, ".zm-run-tart")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DriverStateDir(home, "tart"); got != old {
		t.Fatalf("existing old: %s", got)
	}
	if err := os.Mkdir(filepath.Join(home, ".potluck-driver-tart"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DriverStateDir(home, "tart"); got != filepath.Join(home, ".potluck-driver-tart") {
		t.Fatalf("both: %s", got)
	}
}

func TestRunnerStatusValid(t *testing.T) {
	at := time.Now()
	ok := RunnerStatus{Image: "a@sha256:1", Version: "2.338.2", Refresh: RunnerNotNeeded, At: at}
	for name, c := range map[string]struct {
		s    RunnerStatus
		want bool
	}{
		"not needed":                       {ok, true},
		"failed":                           {RunnerStatus{Image: "a", ImageVersion: "2.1.0", Refresh: RunnerFailed, Why: WhyChecksumMismatch, At: at}, true},
		"failed, no why":                   {RunnerStatus{Image: "a", Refresh: RunnerFailed, At: at}, true},
		"unknown state":                    {RunnerStatus{Image: "a", Refresh: "great", At: at}, false},
		"empty state":                      {RunnerStatus{Image: "a", At: at}, false},
		"zero time":                        {RunnerStatus{Image: "a", Refresh: RunnerRefreshed}, false},
		"no image":                         {RunnerStatus{Refresh: RunnerRefreshed, At: at}, false},
		"free text why":                    {RunnerStatus{Image: "a", Refresh: RunnerFailed, Why: "open /x: no", At: at}, false},
		"why when ok":                      {RunnerStatus{Image: "a", Refresh: RunnerRefreshed, Why: WhyTimedOut, At: at}, false},
		"long version":                     {RunnerStatus{Image: "a", Version: strings.Repeat("1", 100), Refresh: RunnerRefreshed, At: at}, false},
		"long image name":                  {RunnerStatus{Image: strings.Repeat("a", 400), Refresh: RunnerRefreshed, At: at}, false},
		"longest image":                    {RunnerStatus{Image: strings.Repeat("a", MaxRunnerImage), Refresh: RunnerRefreshed, At: at}, true},
		"image with a control character":   {RunnerStatus{Image: "a\x1b[31m@sha256:1", Refresh: RunnerRefreshed, At: at}, false},
		"image with a newline":             {RunnerStatus{Image: "a\n@sha256:1", Refresh: RunnerRefreshed, At: at}, false},
		"version with a control character": {RunnerStatus{Image: "a", Version: "2.338.2\x00", Refresh: RunnerRefreshed, At: at}, false},
		"image version with an escape":     {RunnerStatus{Image: "a", ImageVersion: "\x1b[2J", Refresh: RunnerFailed, At: at}, false},
		"image not utf-8":                  {RunnerStatus{Image: "a\xff", Refresh: RunnerRefreshed, At: at}, false},
	} {
		if got := c.s.Valid(); got != c.want {
			t.Errorf("%s: Valid() = %v, want %v", name, got, c.want)
		}
	}
	got := CleanRunners([]RunnerStatus{ok, {Image: "b", Refresh: "x", At: at}, ok, {Image: "c", Refresh: RunnerRefreshed, At: at}}, 2)
	if len(got) != 2 || got[0].Image != "a@sha256:1" || got[1].Image != "c" {
		t.Errorf("CleanRunners = %+v", got)
	}
	if CleanRunners(nil, 5) != nil {
		t.Error("CleanRunners of nothing is not nil")
	}
}

// More valid results than the cap: the failures stay, the cut falls on the
// rest, and the order is the host's.
func TestCleanRunnersCutKeepsFailures(t *testing.T) {
	at := time.Now()
	var in []RunnerStatus
	for i := 0; i < 5; i++ {
		in = append(in, RunnerStatus{Image: fmt.Sprintf("ok%d", i), Version: "2.338.2", Refresh: RunnerNotNeeded, At: at})
	}
	in = append(in,
		RunnerStatus{Image: "bad1", Refresh: RunnerFailed, Why: WhyTimedOut, At: at},
		RunnerStatus{Image: "bad2", Refresh: RunnerFailed, At: at})
	got := CleanRunners(in, 4)
	var names []string
	for _, r := range got {
		names = append(names, r.Image)
	}
	if want := "ok0 ok1 bad1 bad2"; strings.Join(names, " ") != want {
		t.Errorf("CleanRunners cut to %v, want %s", names, want)
	}
	if got := CleanRunners(in, 1); len(got) != 1 || got[0].Image != "bad1" {
		t.Errorf("CleanRunners to 1 = %+v, want the first failure", got)
	}
}
