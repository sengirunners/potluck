package cli

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var updateOldSpellings = flag.Bool("update-old-spellings", false, "rewrite testdata/old_spellings.golden")

// jsonShape is v's shape: each field's path and JSON type, one per line,
// sorted (an array's elements under "[]").
func jsonShape(t *testing.T, raw string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	var lines []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
			return
		case []any:
			for _, e := range x {
				walk(path+"[]", e)
			}
			if len(x) == 0 {
				lines = append(lines, path+" array")
			}
			return
		case string:
			lines = append(lines, path+" string")
		case float64:
			lines = append(lines, path+" number")
		case bool:
			lines = append(lines, path+" bool")
		case nil:
			lines = append(lines, path+" null")
		}
	}
	walk("", v)
	slices.Sort(lines)
	return strings.Join(slices.Compact(lines), "\n")
}

// The old spellings programs run (spawn, output, kill, renew, cp's
// two-argument form) are the new commands underneath: each answers --json
// with the new command's shape, pinned in testdata/old_spellings.golden
// (the fields those programs read among them), and none shows in help.
func TestSandboxOldSpellings(t *testing.T) {
	m := onePoolMachine(t, refPin)
	var h struct{ Handle string }
	if err := json.Unmarshal([]byte(m.ok(t, "--json", "sandbox", "create", "--ttl", "5m")), &h); err != nil || !strings.HasPrefix(h.Handle, "box:") {
		t.Fatalf("create: %+v %v", h, err)
	}
	id := h.Handle // the old programs pass the handle, box: and all
	shapes := map[string]string{}
	pair := func(old, now []string) string {
		t.Helper()
		o := m.ok(t, append([]string{"--json", "sandbox"}, old...)...)
		n := m.ok(t, append([]string{"--json", "sandbox"}, now...)...)
		if so, sn := jsonShape(t, o), jsonShape(t, n); so != sn {
			t.Errorf("sandbox %s answers another shape than sandbox %s:\n%s\n--- vs\n%s", old[0], now[0], so, sn)
		}
		shapes[old[0]] = jsonShape(t, o)
		return o
	}
	var sp struct{ PID string }
	if err := json.Unmarshal([]byte(pair([]string{"spawn", id, "--request-id", "old-1", "--", "sh", "-c", "echo out; echo err >&2; exit 3"},
		[]string{"exec", id, "--detach", "--", "true"})), &sp); err != nil || sp.PID == "" {
		t.Fatalf("spawn: %+v %v", sp, err)
	}
	// output once the command ended: the answer the old programs read last.
	// The wait is on logs alone, so output and logs are compared only after
	// the end, never across it (an answer while running has no exit).
	var out struct {
		Stdout, Stderr string
		Running        bool
		Exit           *int
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "sandbox", "logs", id, sp.PID, "--wait", "2", "--max-bytes", "4096")), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Running || time.Now().After(deadline) {
			break
		}
	}
	if out.Running {
		t.Fatalf("the spawned command still runs after 20 s: %+v", out)
	}
	out.Exit = nil
	if err := json.Unmarshal([]byte(pair([]string{"output", id, sp.PID, "--wait", "2", "--max-bytes", "4096", "--stdout-from", "0", "--stderr-from", "0"},
		[]string{"logs", id, sp.PID, "--wait", "2", "--max-bytes", "4096"})), &out); err != nil {
		t.Fatal(err)
	}
	if out.Running || out.Exit == nil || *out.Exit != 3 || out.Stdout != "out\n" || out.Stderr != "err\n" {
		t.Fatalf("output: %+v", out)
	}
	// A spawn sent again with its request id answers the same pid.
	if again := m.ok(t, "--json", "sandbox", "spawn", id, "--request-id", "old-1", "--", "sh", "-c", "echo out; echo err >&2; exit 3"); !strings.Contains(again, sp.PID) {
		t.Fatalf("spawn again: %s", again)
	}
	long := m.ok(t, "sandbox", "spawn", id, "--", "sleep", "60")
	pair([]string{"kill", id, strings.TrimSpace(long), "--signal", "KILL"}, []string{"signal", id, strings.TrimSpace(long), "--signal", "KILL"})
	pair([]string{"renew", id, "--ttl", "4m"}, []string{"extend", id, "--ttl", "4m"})

	// cp's old form, both ways.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "up.txt"), []byte("sent up"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := m.ok(t, "--json", "sandbox", "cp", filepath.Join(dir, "up.txt"), id+":got.txt")
	shapes["cp"] = jsonShape(t, up)
	if o := m.ok(t, "sandbox", "exec", id, "--", "cat", "got.txt"); o != "sent up" {
		t.Fatalf("cp up: %q", o)
	}
	m.ok(t, "sandbox", "cp", id+":got.txt", filepath.Join(dir, "down.txt"))
	if b, err := os.ReadFile(filepath.Join(dir, "down.txt")); err != nil || string(b) != "sent up" {
		t.Fatalf("cp down: %q %v", b, err)
	}
	if _, e, code := m.run(t, "sandbox", "cp", id+":a", id+":b"); code != 1 || !strings.Contains(e, "one side is local") {
		t.Fatalf("old cp with two sandbox sides: exit %d %s", code, e)
	}
	if _, e, code := m.run(t, "sandbox", "cp", strings.TrimPrefix(id, "box:"), "./data"); code != 2 || !strings.Contains(e, "sandbox cp takes the sandbox first") {
		t.Fatalf("cp with its path forgotten: exit %d %s", code, e)
	}
	if _, e, code := m.run(t, "sandbox", "spawn", id, "--detach=false", "--", "true"); code != 2 || !strings.Contains(e, "always detaches") {
		t.Fatalf("spawn --detach=false: exit %d %s", code, e)
	}

	var got []string
	for _, k := range []string{"spawn", "output", "kill", "renew", "cp"} {
		got = append(got, "== sandbox "+k+" --json\n"+shapes[k])
	}
	checkGolden(t, filepath.Join("testdata", "old_spellings.golden"), strings.Join(got, "\n")+"\n", *updateOldSpellings)

	help := m.ok(t, "sandbox", "--help")
	for _, old := range []string{"spawn", "output", "kill", "renew"} {
		if strings.Contains(help, "\n  "+old+" ") {
			t.Errorf("sandbox --help lists the old spelling %s:\n%s", old, help)
		}
	}
	for old, now := range map[string]string{"spawn": "potluck sandbox exec --detach", "output": "potluck sandbox logs", "kill": "potluck sandbox signal", "renew": "potluck sandbox extend"} {
		if h := m.ok(t, "sandbox", old, "--help"); !strings.Contains(h, "The old spelling of "+now+",") {
			t.Errorf("sandbox %s --help does not name %s:\n%s", old, now, h)
		}
	}
	m.ok(t, "sandbox", "rm", id)
}

// checkGolden compares got with the file at path (update: rewrites it).
func checkGolden(t *testing.T, path, got string, update bool) {
	t.Helper()
	if update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("%s differs (-update-old-spellings rewrites it):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// A sandbox created with --persist stops with its disk kept and starts
// again on the same disk; one without is refused stop and keeps working.
func TestSandboxPersistStopStart(t *testing.T) {
	m := onePoolMachine(t, refPin)
	out := m.ok(t, "sandbox", "create", "--ttl", "5m", "--persist")
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !strings.Contains(out, " ready ") || !strings.Contains(out, " persist") {
		t.Fatalf("create --persist:\n%s", out)
	}
	m.ok(t, "sandbox", "exec", id, "--", "sh", "-c", "echo kept > note")
	if out := m.ok(t, "sandbox", "stop", id); !strings.Contains(out, " stopped ") || !strings.Contains(out, "persist (disk kept until expires; potluck sandbox start "+id+")") {
		t.Fatalf("stop:\n%s", out)
	}
	if _, e, code := m.run(t, "sandbox", "exec", id, "--", "true"); code == 0 {
		t.Fatalf("exec in a stopped sandbox: %s", e)
	}
	if ls := m.ok(t, "sandbox", "ls", "--live"); !strings.Contains(ls, id+" stopped") {
		t.Fatalf("ls:\n%s", ls)
	}
	var h struct{ State string }
	if err := json.Unmarshal([]byte(m.ok(t, "--json", "sandbox", "start", id, "--ttl", "2m")), &h); err != nil || h.State != "ready" {
		t.Fatalf("start: %+v %v", h, err)
	}
	if o := m.ok(t, "sandbox", "exec", id, "--", "cat", "note"); o != "kept\n" {
		t.Fatalf("the disk was not kept: %q", o)
	}
	if _, e, code := m.run(t, "sandbox", "start", id, "--ttl", "-1s"); code == 0 || !strings.Contains(e, "--ttl must not be negative") {
		t.Fatalf("start --ttl -1s: exit %d %s", code, e)
	}
	m.ok(t, "sandbox", "rm", id)

	out = m.ok(t, "sandbox", "create", "--ttl", "5m")
	plain, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if _, e, code := m.run(t, "sandbox", "stop", plain); code != 1 || !strings.Contains(e, "without persist") || !strings.Contains(e, "potluck sandbox create --persist") {
		t.Fatalf("stop of a sandbox without persist: exit %d %s", code, e)
	}
	m.ok(t, "sandbox", "exec", plain, "--", "true")
	m.ok(t, "sandbox", "rm", plain)
}

// agentPin is an image named as the agent template's, by digest.
const agentPin = "ghcr.io/sengirunners/potluck-box-agent@sha256:0000000000000000000000000000000000000000000000000000000000000002"

// run --template asks for the machine's image of the template's name, as
// sandbox create --template does; with no command, run runs the commit's
// .potluck/job, else its old .zm/job.
func TestRunTemplateAndJobFile(t *testing.T) {
	m := onePoolMachine(t, refPin, agentPin)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(repo, ".zm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".zm", "job"), []byte("echo old job; exit 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "fixture"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, argv...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	t.Chdir(repo)

	digestOf := func(handle string) string {
		t.Helper()
		var l struct {
			Handles []struct {
				Handle      string
				ImageDigest string `json:"image_digest"`
			}
		}
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "sandbox", "ls")), &l); err != nil {
			t.Fatal(err)
		}
		for _, h := range l.Handles {
			if h.Handle == handle {
				return h.ImageDigest
			}
		}
		t.Fatalf("no %s in sandbox ls", handle)
		return ""
	}
	type result struct {
		Handle, State  string
		Exit           *int
		Kept, Released bool
	}
	o, e, code := m.run(t, "--json", "run", "--keep", "--timeout", "2m", "--", "true")
	var r result
	if err := json.Unmarshal([]byte(o), &r); err != nil || code != 0 || r.State != "exited" || !r.Kept || r.Released {
		t.Fatalf("run --keep: exit %d %+v %v\n%s", code, r, err, e)
	}
	if d := digestOf(r.Handle); d != "sha256:"+strings.Repeat("0", 63)+"1" {
		t.Errorf("run booted %s, not the machine's first image", d)
	}
	m.ok(t, "sandbox", "rm", r.Handle)
	// The machine lends an image of the agent template's name, so placement
	// takes it; the reference driver boots its first image alone and refuses
	// any other by name. Only an acquire that carries the template is
	// refused so (without it, the first image boots and run passes), as
	// sandbox create --template agent is.
	_, ce, _ := m.run(t, "sandbox", "create", "--template", "agent")
	const refused = `image "ghcr.io/sengirunners/potluck-box-agent" is not one of the binding's images`
	if !strings.Contains(ce, refused) {
		t.Fatalf("sandbox create --template agent:\n%s", ce)
	}
	o, e, code = m.run(t, "--json", "run", "--timeout", "2m", "--template", "agent", "--", "true")
	var failed struct{ State, Error string }
	if err := json.Unmarshal([]byte(o), &failed); err != nil || code == 0 || failed.State != "failed" || !strings.Contains(failed.Error, refused) {
		t.Fatalf("run --template agent: exit %d %+v %v\n%s", code, failed, err, e)
	}
	if _, e, code := m.run(t, "run", "--template", "nope", "--", "true"); code == 0 || !strings.Contains(e, `no box template "nope"`) {
		t.Fatalf("run --template nope: exit %d %s", code, e)
	}

	o, e, code = m.run(t, "--json", "run", "--timeout", "2m")
	r = result{}
	if err := json.Unmarshal([]byte(o), &r); err != nil || code != 5 || r.Exit == nil || *r.Exit != 5 || !strings.Contains(e, "old job") {
		t.Fatalf("run of the commit's .zm/job: exit %d %+v %v\n%s", code, r, err, e)
	}
}
