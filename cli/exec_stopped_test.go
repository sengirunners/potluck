package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// A command sandbox exec stopped at its timeout says so on stderr, naming
// the limit and what to do instead, and still exits 124; --json carries the
// fact. A command that exits 124 by itself says nothing of the kind.
func TestSandboxExecSaysItStoppedAtTheTimeout(t *testing.T) {
	m := onePoolMachine(t, refPin)
	out := m.ok(t, "sandbox", "create", "--ttl", "5m")
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	o, e, code := m.run(t, "sandbox", "exec", id, "--timeout", "1", "--", "sh", "-c", "echo started; sleep 10")
	const want = "potluck: stopped after 1 s (--timeout 1): rerun with a longer --timeout (up to 600), or --detach for longer work (potluck sandbox logs reads its output)\n"
	if code != 124 || o != "started\n" || !strings.HasSuffix(e, want) {
		t.Fatalf("exec past --timeout: exit %d stdout %q stderr %q", code, o, e)
	}
	if _, e, code := m.run(t, "sandbox", "exec", id, "--", "sh", "-c", "exit 124"); code != 124 || strings.Contains(e, "stopped after") {
		t.Fatalf("a command's own 124: exit %d stderr %q", code, e)
	}
	o = m.ok(t, "--json", "sandbox", "exec", id, "--timeout", "1", "--", "sleep", "10")
	var x struct {
		Exit     int  `json:"exit"`
		TimedOut bool `json:"timed_out"`
		TimeoutS int  `json:"timeout_s"`
	}
	if err := json.Unmarshal([]byte(o), &x); err != nil || x.Exit != 124 || !x.TimedOut || x.TimeoutS != 1 {
		t.Fatalf("--json: %q %v", o, err)
	}
}

// The stopped line names the limit the command met: the default, --timeout,
// --timeout's most, or the sandbox's lease.
func TestExecStoppedWords(t *testing.T) {
	a := &app{host: Host{Prefix: "potluck"}}
	for _, c := range []struct {
		asked, ran int
		want       string
	}{
		{0, 30, "stopped after 30 s (the default): rerun with --timeout <seconds, up to 600>, or --detach for longer work (potluck sandbox logs reads its output)"},
		{0, 0, "stopped after 30 s (the default): rerun with --timeout <seconds, up to 600>, or --detach"},
		{120, 120, "stopped after 120 s (--timeout 120): rerun with a longer --timeout (up to 600), or --detach"},
		{600, 600, "stopped after 600 s (--timeout's most): rerun with --detach"},
		{120, 12, "stopped after 12 s, when the sandbox's lease ends: potluck sandbox extend, then rerun, or use --detach"},
	} {
		if got := a.execStopped(c.asked, c.ran); !strings.HasPrefix(got, c.want) {
			t.Errorf("asked %d ran %d: %q, want %q...", c.asked, c.ran, got, c.want)
		}
	}
}
