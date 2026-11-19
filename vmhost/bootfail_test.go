package vmhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failingBox is a box binding over the fake layer whose every boot prints
// out and exits 1 (fakeBootFail).
func failingBox(t *testing.T, out string) map[string]string {
	t.Helper()
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(m["state_dir"], fakeBootFail), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	return m
}

// Secrets a failed boot's output may hold; none may reach its words or its
// report.
var (
	leakToken  = "potluck_" + strings.Repeat("q7", 26)
	leakGitHub = "ghs_" + strings.Repeat("Ab9", 12)
	leakInvite = "v1." + strings.Repeat("aW52aXRl", 6)
	leakJIT    = strings.Repeat("eyJydW5uZXIiOiJBYkM5", 8)
)

// The Softnet seen on macOS 15 (Bailey's Mac Studio, 2026-11-18: Softnet
// 0.23.0) refuses a rule and tart run exits 1. The acquire's refusal names
// Softnet's words, the layer's components and a setup fault, never a retry;
// the VM and the record go, and a bounded, redacted report stays.
func TestABootSoftnetRefusesSaysWhyAndKeepsAReport(t *testing.T) {
	var out strings.Builder
	for i := 0; out.Len() < 3*reportTailMax; i++ { // the start of the output does not fit the report
		fmt.Fprintf(&out, "boot noise %d\n", i)
	}
	fmt.Fprintf(&out, "joining with https://example.com/join#%s and token %s\nGITHUB_TOKEN=%s\nconfig %s\n", leakInvite, leakToken, leakGitHub, leakJIT)
	out.WriteString("Error: invalid value 'out proto udp @host port 53' for '--allow <ALLOW>': unknown rule syntax\n")
	out.WriteString("Softnet process terminated prematurely\n")
	m := failingBox(t, out.String())

	_, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60})
	if code(err) != "unavailable" {
		t.Fatalf("acquire: %v, want unavailable", err)
	}
	words := err.Error()
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	if !strings.HasPrefix(words, "unavailable: the box did not start: ") || !strings.HasSuffix(words, ".txt (on "+host+")") {
		t.Errorf("a box's refusal says what did not start and names the machine its report is on (zm-9xzw.14): %s", words)
	}
	for _, want := range []string{"the VM exited while booting (exit 1)", "Softnet refused a network rule", "out proto udp @host port 53",
		"[setup fault; fake layer 1.0, isolated true]", "report: "} {
		if !strings.Contains(words, want) {
			t.Errorf("the refusal does not say %q: %s", want, words)
		}
	}
	setup, report := StartAdvice(words)
	if !strings.Contains(setup, "fails the same way until Softnet changes") || strings.Contains(strings.ToLower(setup), "try again") {
		t.Errorf("setup advice %q", setup)
	}
	if report == "" {
		t.Fatalf("no report in %s", words)
	}
	if h, _ := os.UserHomeDir(); strings.HasPrefix(report, "~/") {
		report = filepath.Join(h, report[2:])
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the report after cleanup: %v", err)
	}
	rep := string(b)
	if !strings.Contains(rep, "invalid value 'out proto udp @host port 53'") || !strings.Contains(rep, "Softnet process terminated prematurely") {
		t.Errorf("the report lacks Softnet's words:\n%s", rep[max(0, len(rep)-800):])
	}
	if !strings.Contains(rep, tailCut) || strings.Contains(rep, "boot noise 0\n") || len(rep) > 2*reportTailMax+4<<10 {
		t.Errorf("the report is not the bounded end of the output: %d bytes, cut %v", len(rep), strings.Contains(rep, tailCut))
	}
	for _, leak := range []string{leakToken, leakGitHub, leakInvite, leakJIT} {
		if strings.Contains(rep, leak) || strings.Contains(words, leak) {
			t.Errorf("a secret reached the report or the refusal: %s", leak)
		}
	}
	if !strings.Contains(rep, "https://example.com/join#[redacted]") {
		t.Errorf("the invite link is not redacted in place")
	}

	// cleanup (the supervisor's, just after acquire answers): no VM, no
	// record, no logs; the report alone stays
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		vs, _ := (dirLayer{filepath.Join(m["state_dir"], "fakevms")}).List()
		left, _ := filepath.Glob(filepath.Join(m["state_dir"], "logs", "r*.log"))
		runs, _ := filepath.Glob(filepath.Join(m["state_dir"], "runs", "*.json"))
		if len(vs)+len(left)+len(runs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("left after the failure: %v %v %v", vs, left, runs)
		}
	}
	if !isFile(report) {
		t.Errorf("the report went with the VM")
	}
}

// A boot failure that is no known setup fault quotes the layer's last line
// and gives no setup advice, so a caller may say to try again.
func TestABootFailureOfUnknownCauseQuotesItsLastLine(t *testing.T) {
	m := failingBox(t, "starting the VM\nthe hypervisor is busy right now\n\n")
	_, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60})
	if code(err) != "unavailable" || !strings.Contains(err.Error(), `(exit 1): "the hypervisor is busy right now" [fake layer 1.0, isolated true]`) {
		t.Fatalf("acquire: %v", err)
	}
	if setup, report := StartAdvice(err.Error()); setup != "" || report == "" {
		t.Errorf("advice %q, report %q", setup, report)
	}
}

// Reports are kept failKeep at most, the newest, and none past keepFor.
func TestFailureReportsAreBounded(t *testing.T) {
	st := store{t.TempDir()}
	old := filepath.Join(st.dir, failedDir, "r000000000000.txt")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-keepFor - time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	var last string
	for i := 1; i <= failKeep+5; i++ {
		id := fmt.Sprintf("r%012x", i)
		last = st.keepFailure(record{ID: id, Handle: "box:t/" + id}, "a boot failed")
		at := time.Now().Add(time.Duration(i-failKeep-10) * time.Minute) // in order, all newer than old
		_ = os.Chtimes(filepath.Join(st.dir, failedDir, id+".txt"), at, at)
	}
	st.pruneFailures()
	ps, _ := filepath.Glob(filepath.Join(st.dir, failedDir, "*.txt"))
	if len(ps) != failKeep {
		t.Errorf("%d reports kept, want %d", len(ps), failKeep)
	}
	if isFile(old) || isFile(filepath.Join(st.dir, failedDir, "r000000000001.txt")) || !isFile(last) {
		t.Errorf("kept the wrong reports: %v", ps)
	}
}

// Tart's components are read from the programs a boot runs, each bounded;
// one whose version cannot be read says so.
func TestTartNamesItsComponents(t *testing.T) {
	bin := t.TempDir()
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script("tart", `[ "$1" = --version ] && echo 2.42.0`)
	script("softnet", `[ "$1" = --version ] && echo softnet 0.23.0`)
	script("sw_vers", `echo 15.6`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tart := tartCLI{bin: filepath.Join(bin, "tart")}
	if got, want := tart.Components(true), "tart 2.42.0, softnet 0.23.0 at "+tilde(filepath.Join(bin, "softnet"))+", macOS 15.6"; got != want {
		t.Errorf("components %q, want %q", got, want)
	}
	if got := tart.Components(false); got != "tart 2.42.0, macOS 15.6" {
		t.Errorf("without Softnet: %q", got)
	}
	script("softnet", `echo "error: unexpected argument '--version' found" >&2; exit 2`)
	if got := tart.Components(true); !strings.Contains(got, "softnet (version unreadable) at ") {
		t.Errorf("a Softnet with no version: %q", got)
	}
}

// A pinned Softnet is the one Boot runs, so the failure's words name it, not
// whichever softnet the daemon's PATH holds first.
func TestTartNamesItsPinnedSoftnet(t *testing.T) {
	dir, pinDir := t.TempDir(), t.TempDir()
	script := func(d, name, body string) string {
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script(dir, "tart", `[ "$1" = --version ] && echo 2.42.0`)
	script(dir, "softnet", `[ "$1" = --version ] && echo softnet 0.1.0`)
	script(dir, "sw_vers", `echo 15.6`)
	pinned := script(pinDir, "softnet", `[ "$1" = --version ] && echo softnet 0.23.0`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tart := tartCLI{bin: filepath.Join(dir, "tart"), softnetPath: pinned}
	if got, want := tart.Components(true), "tart 2.42.0, softnet 0.23.0 at "+tilde(pinned)+", macOS 15.6"; got != want {
		t.Errorf("components %q, want %q", got, want)
	}
}
