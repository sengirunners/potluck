package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/agentrunners"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// fakePool is a daemon with one box binding "t" that spawns with secrets:
// it records every request body by path and answers each process as ended
// with exit code exit; with untilTerm, as running until a TERM, then ended
// with exit 0. running is closed at the first output call.
type fakePool struct {
	mu        sync.Mutex
	bodies    map[string][]string
	exit      int
	untilTerm bool
	running   chan struct{}
	once      sync.Once
}

func (f *fakePool) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies[path])
}

func (f *fakePool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies[r.URL.Path] = append(f.bodies[r.URL.Path], string(b))
	f.mu.Unlock()
	w.Header().Set(api.HeaderProtocol, api.Protocol)
	view := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "t", Kind: "box"}, HostClass: api.MachineTrusted, Describe: &computeapi.RunTargetDescribe{Kind: "box", Available: true,
		Verbs: []string{"spawn", "output", "signal"}, Capabilities: []string{"secrets"}, Size: &computeapi.RunTargetSize{Name: "c2m4", OS: "linux", CPU: 2, MemoryMB: 4096}}}
	code := f.exit
	switch r.URL.Path {
	case "/v0/run_targets":
		api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: []computeapi.RunTargetView{view}})
	case "/v0/run_targets/t":
		api.WriteJSON(w, 200, view)
	case "/v0/run_targets/t/acquire":
		api.WriteJSON(w, 200, computeapi.RunTargetHandle{Handle: "box:t/h1", Target: "t", Kind: "box", State: "ready"})
	case "/v0/run_targets/t/handles/h1/exec":
		api.WriteJSON(w, 200, computeapi.RunTargetExec{})
	case "/v0/run_targets/t/handles/h1/spawn":
		api.WriteJSON(w, 200, computeapi.RunTargetSpawn{PID: "p1"})
	case "/v0/run_targets/t/handles/h1/output":
		if f.running != nil {
			f.once.Do(func() { close(f.running) })
		}
		if f.untilTerm {
			if f.count("/v0/run_targets/t/handles/h1/signal") == 0 {
				api.WriteJSON(w, 200, computeapi.RunTargetOutput{Running: true})
				return
			}
			code = 0
		}
		api.WriteJSON(w, 200, computeapi.RunTargetOutput{Stdout: "session done\n", StdoutNext: 13, Exit: &code})
	case "/v0/run_targets/t/handles/h1/signal":
		api.WriteJSON(w, 200, map[string]bool{"sent": true})
	case "/v0/run_targets/t/handles/h1/release":
		api.WriteJSON(w, 200, computeapi.RunTargetRelease{Handle: "box:t/h1", Released: true})
	default:
		api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
	}
}

// TestAgentRunnerOnce: one job places a box that spawns with secrets,
// acquires it with net on, sends the key only as the runner's spawn secret
// (never in an argv or a file written), follows the runner to its exit and
// releases the box.
func TestAgentRunnerOnce(t *testing.T) {
	f := &fakePool{bodies: map[string][]string{}}
	ad, _ := agentrunners.Lookup("claude")
	var out, errw bytes.Buffer
	o := agentRunnerOptions{adapter: ad, secret: "ccenv-sekrit", count: 1, once: true}
	o.timeout, o.out, o.errw = time.Hour, &out, &errw
	o.want.Net, o.want.Secrets = computeapi.NetOn, true
	if err := agentRunners(context.Background(), client.WithHandler(f), o); err != nil {
		t.Fatalf("%v\n%s", err, errw.String())
	}
	var acq computeapi.AcquireRunTargetRequest
	if err := json.Unmarshal([]byte(f.bodies["/v0/run_targets/t/acquire"][0]), &acq); err != nil || acq.Net != computeapi.NetOn {
		t.Fatalf("acquire %+v %v", acq, err)
	}
	var sp computeapi.SpawnRunTargetRequest
	if err := json.Unmarshal([]byte(f.bodies["/v0/run_targets/t/handles/h1/spawn"][0]), &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Secrets[ad.Secret] != o.secret || len(sp.Secrets) != 1 {
		t.Fatalf("spawn secrets %v", sp.Secrets)
	}
	if strings.Contains(strings.Join(sp.Argv, " "), "sekrit") || sp.TimeoutS != 3600 || sp.RequestID != acq.RequestID {
		t.Fatalf("spawn %+v", sp)
	}
	for _, e := range f.bodies["/v0/run_targets/t/handles/h1/exec"] {
		if strings.Contains(e, "sekrit") {
			t.Fatalf("the key in an exec: %s", e)
		}
	}
	if len(f.bodies["/v0/run_targets/t/handles/h1/release"]) != 1 {
		t.Fatal("the box was not released")
	}
	if !strings.Contains(out.String(), "session done") || !strings.Contains(errw.String(), "exited, exit 0") {
		t.Fatalf("out %q err %q", out.String(), errw.String())
	}
}

// TestLinePrefix: lines from writers side by side come out whole, each
// prefixed, and a last line without a newline is flushed with one.
func TestLinePrefix(t *testing.T) {
	var b bytes.Buffer
	var mu sync.Mutex
	l1, l2 := &linePrefix{w: &b, mu: &mu, prefix: "[1] "}, &linePrefix{w: &b, mu: &mu, prefix: "[2] "}
	_, _ = l1.Write([]byte("a"))
	_, _ = l2.Write([]byte("x\ny"))
	_, _ = l1.Write([]byte("b\n"))
	l2.flush()
	if got, want := b.String(), "[2] x\n[1] ab\n[2] y\n"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

// TestAgentRunnerGivesUp: runners that exit non-zero at once (a revoked
// key) end the slot after agentRunnerGiveUp jobs, each box released, with
// the exit named.
func TestAgentRunnerGivesUp(t *testing.T) {
	defer func(b time.Duration, g int) { agentRunnerBackoff, agentRunnerGiveUp = b, g }(agentRunnerBackoff, agentRunnerGiveUp)
	agentRunnerBackoff, agentRunnerGiveUp = time.Millisecond, 3
	f := &fakePool{bodies: map[string][]string{}, exit: 1}
	ad, _ := agentrunners.Lookup("claude")
	var out, errw bytes.Buffer
	o := agentRunnerOptions{adapter: ad, secret: "k", count: 1}
	o.timeout, o.out, o.errw = time.Hour, &out, &errw
	o.want.Secrets = true
	err := agentRunners(context.Background(), client.WithHandler(f), o)
	if err == nil || !strings.Contains(err.Error(), "3 jobs in a row failed") || !strings.Contains(err.Error(), "exited 1") {
		t.Fatalf("err %v\n%s", err, errw.String())
	}
	if n := f.count("/v0/run_targets/t/handles/h1/release"); n != 3 {
		t.Fatalf("%d boxes released, want 3", n)
	}
}

// TestAgentRunnerDrainsOnCtrlC: Ctrl-C sends the runner TERM, keeps
// following it until it ends by itself (no KILL), then releases the box.
func TestAgentRunnerDrainsOnCtrlC(t *testing.T) {
	f := &fakePool{bodies: map[string][]string{}, untilTerm: true, running: make(chan struct{})}
	ad, _ := agentrunners.Lookup("claude")
	var out, errw bytes.Buffer
	o := agentRunnerOptions{adapter: ad, secret: "k", count: 1}
	o.timeout, o.out, o.errw, o.drain = time.Hour, &out, &errw, 30*time.Second
	o.want.Secrets = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-f.running; cancel() }()
	err := agentRunners(ctx, client.WithHandler(f), o)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err %v", err)
	}
	var sigs []string
	for _, b := range f.bodies["/v0/run_targets/t/handles/h1/signal"] {
		var s computeapi.SignalRunTargetRequest
		_ = json.Unmarshal([]byte(b), &s)
		sigs = append(sigs, s.Signal)
	}
	if strings.Join(sigs, ",") != "TERM" {
		t.Fatalf("signals %v, want TERM alone", sigs)
	}
	if !strings.Contains(out.String(), "session done") || f.count("/v0/run_targets/t/handles/h1/release") != 1 {
		t.Fatalf("out %q, releases %d", out.String(), f.count("/v0/run_targets/t/handles/h1/release"))
	}
}
