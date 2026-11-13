package tailnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// Peers named potluck-<machine> (a later daemon's node) are daemons too.
func TestMachineFromHostNewPrefix(t *testing.T) {
	for host, want := range map[string]string{"potluck-studio": "studio", "Potluck-Studio": "studio", "potluck-": "", "potluckstudio": ""} {
		m, ok := machineFromHost(host)
		if (want == "") == ok || (ok && m != want) {
			t.Errorf("%q -> %q %v, want %q", host, m, ok, want)
		}
	}
	for name, want := range map[string]bool{"zm-mini": true, "potluck-mini": true, "POTLUCK-x": true, "iphone": false} {
		if IsDaemonNode(name) != want {
			t.Errorf("IsDaemonNode(%q) = %v", name, !want)
		}
	}
}

// Node names (rename step 2): a fresh home's node is potluck-<machine>; a
// home whose node already exists (its tsnet state, no recorded name: a node
// made before step 2) keeps zm-<machine>; either is recorded in config.json
// BEFORE the node starts, so a restart can never pick the other; a recorded
// name is kept, whatever its prefix; a new name longer than a DNS label is
// refused with a clear error.
func TestNodeNameFreshAndExisting(t *testing.T) {
	up := func(e *env) (string, error) {
		t.Helper()
		var got string
		e.s.NewNode = func(c NodeConfig) Node { got = c.Hostname; e.node.set(func(n *fakeNode) { n.cfg = c }); return e.node }
		_, err := e.s.Up(context.Background(), api.TailnetUpRequest{})
		return got, err
	}
	restart := func(e *env) { // the same home, config read again, as a new daemon would
		t.Helper()
		e.s.Down(context.Background())
		c, err := config.LoadFrom(e.home)
		if err != nil {
			t.Fatal(err)
		}
		e.d.Config.File = c.File
	}
	saved := func(e *env) string {
		t.Helper()
		c, err := config.LoadFrom(e.home)
		if err != nil {
			t.Fatal(err)
		}
		return c.File.Tailnet.Hostname
	}

	// A fresh home.
	e := newEnv(t, "")
	if got, err := up(e); err != nil || got != "potluck-mini" || saved(e) != "potluck-mini" {
		t.Fatalf("fresh home: %q %v, saved %q", got, err, saved(e))
	}
	for i := 0; i < 2; i++ { // its state exists now: the recorded name wins
		restart(e)
		if got, err := up(e); err != nil || got != "potluck-mini" {
			t.Fatalf("fresh home, restart %d: %q %v", i+1, got, err)
		}
	}

	// A home whose node a step 1 daemon made: tsnet state, nothing recorded.
	e = newEnv(t, "")
	if err := os.MkdirAll(filepath.Join(e.home, StateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, StateDir, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if got, err := up(e); err != nil || got != "zm-mini" || saved(e) != "zm-mini" {
			t.Fatalf("existing node, start %d: %q %v, saved %q", i+1, got, err, saved(e))
		}
		restart(e)
	}

	// A recorded name, old prefix, on a home with no state yet.
	e = newEnv(t, `{"tailnet":{"hostname":"zm-studio"}}`)
	if got, err := up(e); err != nil || got != "zm-studio" || saved(e) != "zm-studio" {
		t.Fatalf("recorded name: %q %v, saved %q", got, err, saved(e))
	}

	// 63 characters at most, the prefix counted.
	e = newEnv(t, "")
	long := strings.Repeat("m", 56) // potluck- + 56 = 64
	e.d.Config.Machine = long
	e.s.NewNode = func(NodeConfig) Node { t.Error("started a node with a name over 63 characters"); return newFakeNode() }
	_, err := e.s.Up(context.Background(), api.TailnetUpRequest{})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeBadRequest || !strings.Contains(ae.Message, "at most 63") || !strings.Contains(ae.Hint, "55 characters") {
		t.Fatalf("a 64-character name: %v", err)
	}
	e.d.Config.Machine = long[:55]
	if got, err := up(e); err != nil || len(got) != 63 {
		t.Fatalf("a 63-character name: %q %v", got, err)
	}
	// An existing node's name is never newly refused.
	e = newEnv(t, "")
	if err := os.MkdirAll(filepath.Join(e.home, StateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, StateDir, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.d.Config.Machine = strings.Repeat("m", 62)
	if got, err := up(e); err != nil || got != "zm-"+e.d.Config.Machine {
		t.Fatalf("an existing node's long name: %q %v", got, err)
	}
}

// The node's own names, for the own-host check: both prefixes.
func TestURLForOldNodes(t *testing.T) {
	if URLFor("mini") != "http://zm-mini:7433" || HostPrefix != "potluck-" || HostPrefixOld != "zm-" {
		t.Fatalf("%s %s %s", URLFor("mini"), HostPrefix, HostPrefixOld)
	}
}

// The node's name cannot be saved: up refuses before starting the node, and
// no tsnet state is made under a name that is not on disk.
func TestNodeNameUnsavedRefusesToStart(t *testing.T) {
	e := newEnv(t, "")
	if err := os.Mkdir(filepath.Join(e.home, config.FileConfig), 0o700); err != nil { // config.json cannot be read or written
		t.Fatal(err)
	}
	e.s.NewNode = func(NodeConfig) Node { t.Error("started a node whose name is not saved"); return newFakeNode() }
	_, err := e.s.Up(context.Background(), api.TailnetUpRequest{})
	var ae *api.Error
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "could not save this node's name potluck-mini") || !strings.Contains(ae.Hint, "not started") {
		t.Fatalf("up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, StateDir)); !os.IsNotExist(err) {
		t.Fatalf("tsnet state made: %v", err)
	}
}
