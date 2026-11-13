package runtarget_test

import (
	"context"
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

// TestBoxTerminal: a process on a terminal through the daemon (SPEC 20
// "Boxes", terminals; zm-eakh.12.5.21): spawn's pty only to a driver
// listing it and never with secrets, input the holder's only, the size,
// the input, a resize and a Ctrl-C reach the box, and input to an ended
// process answers running false.
func TestBoxTerminal(t *testing.T) {
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
	for name, bug := range map[string]string{"ref": "", "nopty": "box-no-pty"} {
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
	for _, d := range []string{"ref", "nopty"} {
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
	tty := &computeapi.RunTargetPTY{Rows: 24, Cols: 80}
	for name, req := range map[string]computeapi.SpawnRunTargetRequest{
		"pty rows 0":       {Argv: []string{"sh"}, RequestID: "x1", PTY: &computeapi.RunTargetPTY{Cols: 80}},
		"pty cols 1001":    {Argv: []string{"sh"}, RequestID: "x2", PTY: &computeapi.RunTargetPTY{Rows: 24, Cols: 1001}},
		"a bad term":       {Argv: []string{"sh"}, RequestID: "x3", PTY: &computeapi.RunTargetPTY{Rows: 24, Cols: 80, Term: "a b"}},
		"pty with secrets": {Argv: []string{"sh"}, RequestID: "x4", PTY: tty, Secrets: map[string]string{"A": "b"}},
	} {
		if _, err := computeclient.SpawnRunTarget(ctx, ana, "box-ref", id, req); code(err) != api.CodeBadRequest {
			t.Errorf("spawn with %s: %v", name, err)
		}
	}
	p, err := computeclient.AcquireRunTarget(ctx, ana, "box-nopty", computeapi.AcquireRunTargetRequest{RequestID: "n1", TTLS: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.SpawnRunTarget(ctx, ana, "box-nopty", idOf(p.Handle), computeapi.SpawnRunTargetRequest{Argv: []string{"sh"}, RequestID: "n2", PTY: tty}); code(err) != api.CodeBadRequest ||
		!strings.Contains(err.Error(), "pty") {
		t.Errorf("pty to a driver without it: %v", err)
	}

	script := `stty size; echo "term=$TERM"; read x; echo "got:$x"; stty size; read y; echo never`
	sp, err := computeclient.SpawnRunTarget(ctx, ana, "box-ref", id, computeapi.SpawnRunTargetRequest{Argv: []string{"sh", "-c", script}, RequestID: "t1", PTY: tty})
	if err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	var next int64
	until := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); !strings.Contains(seen.String(), want); {
			o, err := computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: next, WaitS: 1})
			if err != nil {
				t.Fatal(err)
			}
			seen.WriteString(o.Stdout)
			next = o.StdoutNext
			if !time.Now().Before(deadline) {
				t.Fatalf("no %q in %q", want, seen.String())
			}
		}
	}
	until("24 80")
	until("term=xterm-256color")
	if _, err := computeclient.InputRunTarget(ctx, bo, "box-ref", id, computeapi.InputRunTargetRequest{PID: sp.PID, Data: "x"}); code(err) != api.CodeForbidden {
		t.Errorf("input by another member: %v", err)
	}
	for name, req := range map[string]computeapi.InputRunTargetRequest{
		"no pid":           {Data: "x"},
		"rows alone":       {PID: sp.PID, Rows: 3},
		"cols past 1000":   {PID: sp.PID, Rows: 3, Cols: 1001},
		"over 64 KiB":      {PID: sp.PID, Data: strings.Repeat("a", 64<<10+1)},
		"a bad encoding":   {PID: sp.PID, Data: "x", Encoding: "hex"},
		"base64 not valid": {PID: sp.PID, Data: "!", Encoding: computeapi.EncodingBase64},
	} {
		if _, err := computeclient.InputRunTarget(ctx, ana, "box-ref", id, req); code(err) != api.CodeBadRequest {
			t.Errorf("input with %s: %v", name, err)
		}
	}
	if in, err := computeclient.InputRunTarget(ctx, ana, "box-ref", id, computeapi.InputRunTargetRequest{PID: sp.PID, Data: "hello\n", Rows: 30, Cols: 100}); err != nil || in.Written != 6 || !in.Running {
		t.Fatalf("input: %+v %v", in, err)
	}
	until("got:hello")
	until("30 100")
	if _, err := computeclient.InputRunTarget(ctx, ana, "box-ref", id, computeapi.InputRunTargetRequest{PID: sp.PID, Data: "\x03"}); err != nil {
		t.Fatal(err)
	}
	o, err := computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: next, WaitS: 10})
	for err == nil && o.Running {
		o, err = computeclient.RunTargetOutput(ctx, ana, "box-ref", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: o.StdoutNext, WaitS: 10})
	}
	if err != nil || o.Exit == nil || *o.Exit != 130 {
		t.Fatalf("after Ctrl-C: %+v %v, want exit 130", o, err)
	}
	if in, err := computeclient.InputRunTarget(ctx, ana, "box-ref", id, computeapi.InputRunTargetRequest{PID: sp.PID, Data: "late"}); err != nil || in.Running || in.Written != 0 {
		t.Errorf("input after the end: %+v %v", in, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box-ref", id); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.InputRunTarget(ctx, ana, "box-ref", id, computeapi.InputRunTargetRequest{PID: sp.PID, Data: "x"}); code(err) != api.CodeConflict {
		t.Errorf("input after release: %v", err)
	}
}
