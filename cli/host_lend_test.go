package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/runtarget/rt"
	"github.com/sengirunners/potluck/vmhost"
)

// memLend is the lender's dials in memory, for lendNow and lendSet.
type memLend struct {
	l   vmhost.HostLend
	err error // the file cannot be read
}

func (m *memLend) now(int) (vmhost.HostLend, error) { return m.l, m.err }

func (m *memLend) set(l vmhost.HostLend) (vmhost.HostLend, error) {
	if l.Cores < 0 || l.MemoryMB < 0 || l.KeepFreeGB < 0 {
		return vmhost.HostLend{}, fmt.Errorf("negative dial %+v", l)
	}
	if l.Since.IsZero() {
		l.Since = time.Now().UTC()
	}
	m.l, m.err = l, nil
	return l, nil
}

// useLend gives the test dials of its own, all as shared.
func useLend(t *testing.T) *memLend {
	t.Helper()
	m, oldNow, oldSet := &memLend{}, lendNow, lendSet
	lendNow, lendSet = m.now, m.set
	t.Cleanup(func() { lendNow, lendSet = oldNow, oldSet })
	return m
}

// potluck host lend changes only the dials it is given, says what they now
// lend, and says when running boxes hold more (they finish).
func TestHostLend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pl := filepath.Join(home, ".potluck")
	t.Setenv(EnvHome, pl)
	t.Setenv("ZM_MACHINE", "lend-test")
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(filepath.Join(state, "runs"), 0o700); err != nil || os.MkdirAll(pl, 0o700) != nil {
		t.Fatal(err)
	}
	box := computeapi.HostRunTarget{RunTarget: computeapi.RunTarget{Name: "small", Driver: "potluck-driver-tart", Kind: computeapi.RunTargetKindBox, Isolation: "ephemeral",
		Images: []string{"ghcr.io/o/a@sha256:" + strings.Repeat("ab", 32)}, Config: map[string]string{"state_dir": state}}, Share: []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}
	if err := os.WriteFile(filepath.Join(pl, "run_targets.toml"), []byte(rt.EncodeHostRunTargets([]computeapi.HostRunTarget{box})), 0o600); err != nil {
		t.Fatal(err)
	}
	// One box running: its supervisor is this test, so it is live.
	rec := map[string]any{"Handle": "box:small/r1", "Target": "small", "ID": "r1", "Kind": "box", "State": "ready",
		"Start": time.Now().Add(-time.Minute).UTC(), "Supervisor": os.Getpid(), "Config": map[string]string{"cpu": "4", "memory": "8192", "size": "small"}}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(state, "runs", "r1.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	ml := useLend(t)
	run := func(args ...string) (string, string, int) {
		var o, e bytes.Buffer
		code := Run(context.Background(), append([]string{"host", "lend"}, args...), strings.NewReader(""), &o, &e)
		return o.String(), e.String(), code
	}

	out, _, code := run("--cores", "2")
	if code != 0 || ml.l.Cores != 2 || ml.l.MemoryMB != 0 {
		t.Fatalf("--cores 2: exit %d, %+v", code, ml.l)
	}
	if !strings.Contains(out, "lending at most 2 cores and all the memory shared") || !strings.Contains(out, "1 running job holds 4 cores and 8.0 GB of memory: they finish") {
		t.Errorf("--cores 2 says:\n%s", out)
	}
	if _, _, code := run("--memory-gb", "12.5", "--keep-free-gb", "50"); code != 0 || ml.l.Cores != 2 || ml.l.MemoryMB != 12800 || ml.l.KeepFreeGB != 50 {
		t.Fatalf("memory and disk: exit %d, %+v", code, ml.l)
	}
	var ans lendAnswer
	o, _, code := run("--json", "--cores", "all")
	if code != 0 {
		t.Fatalf("--cores all: exit %d", code)
	}
	if err := json.Unmarshal([]byte(o), &ans); err != nil || ans.Cores != 0 || ans.MemoryMB != 12800 || ans.Over != "" ||
		ans.Summary != "lending all the cores shared and at most 12.5 GB of memory; 50 GB of disk kept free for you" {
		t.Errorf("--json: %+v %v\n%s", ans, err, o)
	}
	for _, bad := range [][]string{{"--cores", "0"}, {"--cores", "x"}, {"--memory-gb", "-1"}, {"--keep-free-gb", "1.5"}} {
		before := ml.l
		if _, _, code := run(bad...); code == 0 || ml.l != before {
			t.Errorf("%v: exit %d, %+v", bad, code, ml.l)
		}
	}
	if out, _, code := run(); code != 0 || !strings.Contains(out, "50 GB of disk kept free for you") {
		t.Errorf("no flags: exit %d\n%s", code, out)
	}
	// Dials that cannot be read are not changed one at a time; all three
	// write them anew.
	ml.err = errors.New("host-lend.json: bad")
	if _, e, code := run("--cores", "4"); code == 0 || !strings.Contains(e, "none is changed") {
		t.Errorf("one dial over an unreadable file: exit %d %s", code, e)
	}
	if _, _, code := run("--cores", "4", "--memory-gb", "all", "--keep-free-gb", "0"); code != 0 || ml.l.Cores != 4 || ml.l.MemoryMB != 0 || ml.l.KeepFreeGB != 0 {
		t.Errorf("all three over an unreadable file: exit %d %+v", code, ml.l)
	}
}
