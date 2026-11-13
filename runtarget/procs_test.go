package runtarget_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// TestBoxBackgroundProcesses: spawn, output and signal through the daemon
// (SPEC 20 "Boxes", background processes): the holder only, idempotent by
// request_id, pages while it runs, the group signalled, conflict once the
// box is gone; exec's base64 only to a driver that lists it.
func TestBoxBackgroundProcesses(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	drivers := map[string]string{}
	for name, bug := range map[string]string{"ref": "", "plain": "box-no-base64"} {
		state := filepath.Join(fix, "state-"+name)
		dir := filepath.Join(fix, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		drv, err := refdriver.Install(dir, state, bug)
		if err != nil {
			t.Fatal(err)
		}
		drivers[name] = drv
		t.Cleanup(func() { refdriver.KillAll(state) })
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(d string) (string, error) { return drivers[d], nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	mint := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	ana, bo := mint("ana"), mint("bo")
	for _, d := range []string{"ref", "plain"} {
		b := computeapi.RunTarget{Name: "box-" + d, Driver: d, Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := computeclient.AcquireRunTarget(ctx, ana, "box-ref", computeapi.AcquireRunTargetRequest{RequestID: "b1", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	id := idOf(h.Handle)
	script := `trap : TERM; echo start; sleep 30 & wait; wait; echo after-term; exit 3`
	sp, err := computeclient.SpawnRunTarget(ctx, ana, "box-ref", id, computeapi.SpawnRunTargetRequest{Argv: []string{"sh", "-c", script}, RequestID: "s1"})
	if err != nil || sp.PID == "" {
		t.Fatalf("spawn: %+v %v", sp, err)
	}
	if again, err := computeclient.SpawnRunTarget(ctx, ana, "box-ref", id, computeapi.SpawnRunTargetRequest{Argv: []string{"true"}, RequestID: "s1"}); err != nil || again.PID != sp.PID {
		t.Errorf("the same request_id: %+v %v, want %s", again, err, sp.PID)
	}
	for name, req := range map[string]computeapi.SpawnRunTargetRequest{
		"no request_id":   {Argv: []string{"true"}},
		"an empty argv":   {RequestID: "s2"},
		"a bad encoding":  {Argv: []string{"true"}, RequestID: "s3", Encoding: "hex"},
		"a negative time": {Argv: []string{"true"}, RequestID: "s4", TimeoutS: -1},
	} {
		if _, err := computeclient.SpawnRunTarget(ctx, ana, "box-ref", id, req); code(err) != api.CodeBadRequest {
			t.Errorf("spawn with %s: %v", name, err)
		}
	}
	for name, call := range map[string]func(*client.Client) error{
		"spawn": func(c *client.Client) error {
			_, err := computeclient.SpawnRunTarget(ctx, c, "box-ref", id, computeapi.SpawnRunTargetRequest{Argv: []string{"id"}, RequestID: "x"})
			return err
		},
		"output": func(c *client.Client) error {
			_, err := computeclient.RunTargetOutput(ctx, c, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID})
			return err
		},
		"signal": func(c *client.Client) error {
			_, err := computeclient.SignalRunTarget(ctx, c, "box-ref", id, computeapi.SignalRunTargetRequest{PID: sp.PID, Signal: "KILL"})
			return err
		},
	} {
		if err := call(bo); code(err) != api.CodeForbidden {
			t.Errorf("%s by another member: %v", name, err)
		}
	}
	for name, req := range map[string]computeapi.OutputRunTargetRequest{
		"no pid":            {},
		"max_bytes past 1M": {PID: sp.PID, MaxBytes: 1<<20 + 1},
		"wait_s past 20":    {PID: sp.PID, WaitS: 21},
		"a negative offset": {PID: sp.PID, StdoutFrom: -1},
	} {
		if _, err := computeclient.RunTargetOutput(ctx, ana, "box-ref", id, req); code(err) != api.CodeBadRequest {
			t.Errorf("output with %s: %v", name, err)
		}
	}
	if _, err := computeclient.SignalRunTarget(ctx, ana, "box-ref", id, computeapi.SignalRunTargetRequest{PID: sp.PID, Signal: "HUP"}); code(err) != api.CodeBadRequest {
		t.Errorf("signal HUP: %v", err)
	}
	o, err := computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID, WaitS: 5})
	if err != nil || o.Stdout != "start\n" || !o.Running || o.Exit != nil || o.StdoutNext != 6 {
		t.Fatalf("output while running: %+v %v", o, err)
	}
	start := time.Now()
	o, err = computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: 6, WaitS: 1})
	if err != nil || o.Stdout != "" || !o.Running || time.Since(start) < 900*time.Millisecond {
		t.Errorf("a wait with nothing new: %+v %v after %s", o, err, time.Since(start))
	}
	if s, err := computeclient.SignalRunTarget(ctx, ana, "box-ref", id, computeapi.SignalRunTargetRequest{PID: sp.PID, Signal: "TERM"}); err != nil || !s.Sent {
		t.Fatalf("signal: %+v %v", s, err)
	}
	o, err = computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: 6, WaitS: 10})
	for err == nil && o.Running {
		o, err = computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: o.StdoutNext, WaitS: 10})
	}
	if err != nil || o.Exit == nil || *o.Exit != 3 {
		t.Fatalf("after TERM to the group: %+v %v, want exit 3 once its child ended", o, err)
	}

	// base64 on exec: the driver lists it; one that does not is refused, not sent.
	raw := []byte{0, 0xff, 0x80, '\n'}
	in := base64.StdEncoding.EncodeToString(raw)
	out, err := computeclient.ExecRunTarget(ctx, ana, "box-ref", id, computeapi.ExecRunTargetRequest{Argv: []string{"cat"}, Stdin: in, Encoding: computeapi.EncodingBase64})
	if err != nil || out.Stdout != in || out.Encoding != computeapi.EncodingBase64 {
		t.Errorf("exec base64: %+v %v", out, err)
	}
	p, err := computeclient.AcquireRunTarget(ctx, ana, "box-plain", computeapi.AcquireRunTargetRequest{RequestID: "p1", TTLS: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, ana, "box-plain", idOf(p.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"cat"}, Stdin: in, Encoding: computeapi.EncodingBase64}); code(err) != api.CodeBadRequest ||
		!strings.Contains(err.Error(), "base64") {
		t.Errorf("base64 to a driver without it: %v", err)
	}

	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box-ref", id); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID}); code(err) != api.CodeConflict {
		t.Errorf("output after release: %v", err)
	}
	if _, err := computeclient.SpawnRunTarget(ctx, ana, "box-ref", id, computeapi.SpawnRunTargetRequest{Argv: []string{"true"}, RequestID: "s9"}); code(err) != api.CodeConflict {
		t.Errorf("spawn after release: %v", err)
	}
}
