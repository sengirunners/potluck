package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// Connect the real SDK client to the command's standard streams. Any extra
// stdout is a protocol error, and disconnect must finish the command.
func mcpCommand(t *testing.T, args ...string) *sdk.ClientSession {
	t.Helper()
	return mcpProgram(t, Program{}, args...)
}

// mcpProgram is mcpCommand for a program built on the potluck command.
func mcpProgram(t *testing.T, p Program, args ...string) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	in, cw := io.Pipe()
	cr, out := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- RunProgram(ctx, p, args, in, out, io.Discard); out.Close() }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &sdk.IOTransport{Reader: cr, Writer: cw}, nil)
	if err != nil {
		cancel()
		in.Close()
		cr.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		cancel()
		in.Close()
		cr.Close()
		select {
		case code := <-done:
			if code != 0 && code != 1 {
				t.Errorf("mcp exit%d", code)
			}
		case <-time.After(3 * time.Second):
			t.Error("mcp command did not stop")
		}
	})
	return cs
}

func mcpCall(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) json.RawMessage {
	t.Helper()
	r, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil || r.IsError {
		t.Fatalf("%s: %+v %v", name, r, err)
	}
	for _, content := range r.Content {
		if txt, ok := content.(*sdk.TextContent); ok && json.Valid([]byte(txt.Text)) {
			return json.RawMessage(txt.Text)
		}
	}
	t.Fatalf("%s returned no JSON: %+v", name, r)
	return nil
}

func TestMCPNoDaemon(t *testing.T) {
	t.Setenv("POTLUCK_AS", "")
	t.Setenv("POTLUCK_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_HOST", "http://invalid.example")
	t.Setenv("ZM_TOKEN", "must-not-inherit")
	t.Setenv("ZM_AS", "must-not-inherit")
	cs := mcpCommand(t, "mcp")
	ls, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("discovery %+v %v", ls, err)
	}
	want := []string{"sandbox_create", "sandbox_delete", "sandbox_exec", "sandbox_list", "sandbox_output", "sandbox_preview_list", "sandbox_preview_publish", "sandbox_preview_revoke", "sandbox_read_file", "sandbox_renew", "sandbox_signal", "sandbox_spawn", "sandbox_start", "sandbox_stop", "sandbox_write_file"}
	names := make([]string, 0, len(ls.Tools))
	for _, tool := range ls.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if !slices.Equal(names, want) {
		t.Fatalf("discovery names %v want %v", names, want)
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"sandbox_list", map[string]any{}},
		{"sandbox_preview_list", map[string]any{"handle": "box:remote/a"}},
		{"sandbox_preview_publish", map[string]any{"handle": "box:remote/a", "port": 3000, "public": true}},
		{"sandbox_preview_revoke", map[string]any{"handle": "box:remote/a", "preview": "owned-preview"}},
	} {
		r, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || !r.IsError {
			t.Fatalf("%s missing daemon: %+v %v", call.name, r, err)
		}
	}
}

// Same two-daemon pool and reference driver as the CLI quick start; all
// operations below travel through MCP stdio, A's socket and the team relay.
func TestMCPSandboxesAcrossTwoMachines(t *testing.T) {
	t.Setenv("POTLUCK_AS", "")
	t.Setenv("POTLUCK_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_AS", "unrelated/session")
	t.Setenv("ZM_TOKEN", "unrelated-token")
	oldRunner, oldDetect, oldDriver := shareRunner, detectHost, shareDriver
	t.Cleanup(func() { shareRunner, detectHost, shareDriver = oldRunner, oldDetect, oldDriver })
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true, Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"
	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "lender", "carl@example.com")
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	module[*runtarget.Service](t, b).SetLookPath(func(string) (string, error) { return drv, nil })
	installed := filepath.Join(b.home, runtarget.DriversDir, "zm-run-ref")
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, installed); err != nil {
		t.Fatal(err)
	}
	stop := beat(module[*teamlink.Service](t, b))
	defer stop()
	a.ok(t, "create", "friends")
	invite := hostCodeRe.FindStringSubmatch(a.ok(t, "invite", "carl", "--host"))
	if invite == nil {
		t.Fatal("no host invite")
	}
	b.ok(t, "join", invite[1])
	b.ok(t, "host", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "lender", "--reclaim-idle-s", "0", "--no-sudo")
	t.Setenv(EnvHome, a.home)
	cs := mcpCommand(t, "mcp") // the one pool is resolved lazily on the first call
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("committed"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"init", "-q"}, {"add", "tracked.txt"}, {"commit", "-q", "-m", "fixture"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, argv...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git:%s %v", out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty tracked file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("not-for-upload"), 0600); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"ttl_s": 120, "persist": true, "client_request_id": "mcp-test-create", "ref": "HEAD", "dir": repo, "dirty": true}
	created := mcpCall(t, cs, "sandbox_create", args)
	var h struct {
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(created, &h); err != nil || !strings.Contains(h.Handle, "@lender/") {
		t.Fatalf("remote sandbox %s %v", created, err)
	}
	duplicate := mcpCall(t, cs, "sandbox_create", args)
	var again struct{ Handle string }
	json.Unmarshal(duplicate, &again)
	if h.Handle != again.Handle {
		t.Fatal("create retry allocated twice")
	}
	call := func(name string, fields map[string]any) json.RawMessage {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["handle"] = h.Handle
		return mcpCall(t, cs, name, fields)
	}
	pushed := call("sandbox_exec", map[string]any{"argv": []string{"sh", "-c", "cat checkout/tracked.txt; test ! -e checkout/.env"}})
	if !strings.Contains(string(pushed), "dirty tracked file") || !strings.Contains(string(pushed), `"exit": 0`) {
		t.Fatalf("checkout push%s", pushed)
	}
	call("sandbox_write_file", map[string]any{"path": "note", "content": "hello sandbox"})
	pg := call("sandbox_read_file", map[string]any{"path": "note", "offset": 6, "max_bytes": 7})
	if !strings.Contains(string(pg), `"content": "sandbox"`) {
		t.Fatalf("file page%s", pg)
	}
	exec := call("sandbox_exec", map[string]any{"argv": []string{"cat", "note"}})
	if !strings.Contains(string(exec), "hello sandbox") {
		t.Fatalf("exec%s", exec)
	}
	call("sandbox_renew", map[string]any{"ttl_s": 180})
	procs := call("sandbox_spawn", map[string]any{"argv": []string{"sh", "-c", "echo started; sleep 30"}, "client_request_id": "background"})
	var p struct{ PID string }
	json.Unmarshal(procs, &p)
	if p.PID == "" {
		t.Fatalf("no pid%s", procs)
	}
	call("sandbox_output", map[string]any{"pid": p.PID, "wait_s": 1})
	call("sandbox_signal", map[string]any{"pid": p.PID, "signal": "KILL"})
	listed := mcpCall(t, cs, "sandbox_list", map[string]any{"live": true})
	if !strings.Contains(string(listed), h.Handle) {
		t.Fatalf("list%s", listed)
	}
	call("sandbox_stop", nil)
	call("sandbox_start", map[string]any{"ttl_s": 120})
	pg = call("sandbox_read_file", map[string]any{"path": "note"})
	if !strings.Contains(string(pg), "hello sandbox") {
		t.Fatal("persistent disk lost")
	}
	// A different member cannot use the owner's sandbox, even through the
	// same local socket and relay. A token's identity must survive pool resolution.
	cfg, err := config.LoadFrom(a.home)
	if err != nil {
		t.Fatal(err)
	}
	owner := client.NewUnix(cfg.SocketPath())
	token, err := owner.MintToken(context.Background(), api.MintTokenRequest{As: "other", Project: "friends", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POTLUCK_TOKEN", token.Secret)
	other := mcpCommand(t, "--pool", "friends", "mcp")
	refused, err := other.CallTool(context.Background(), &sdk.CallToolParams{Name: "sandbox_exec", Arguments: map[string]any{"handle": h.Handle, "argv": []string{"cat", "note"}}})
	if err != nil || !refused.IsError {
		t.Fatalf("another member read sandbox: %+v %v", refused, err)
	}
	t.Setenv("POTLUCK_TOKEN", "")
	call("sandbox_delete", nil)

	// A sandbox without persist is not stopped: stop would delete it and its
	// files, so it is refused, and the sandbox keeps working until deleted.
	// A new session: this test has already used most of the first one's 30 s.
	cs = mcpCommand(t, "mcp")
	plain := mcpCall(t, cs, "sandbox_create", map[string]any{"ttl_s": 120, "client_request_id": "mcp-test-plain"})
	var ph struct{ Handle string }
	if err := json.Unmarshal(plain, &ph); err != nil || ph.Handle == "" {
		t.Fatalf("plain sandbox %s %v", plain, err)
	}
	mcpCall(t, cs, "sandbox_write_file", map[string]any{"handle": ph.Handle, "path": "note", "content": "kept"})
	refusedStop, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "sandbox_stop", Arguments: map[string]any{"handle": ph.Handle}})
	if err != nil || !refusedStop.IsError {
		t.Fatalf("stop of a sandbox without persist: %+v %v", refusedStop, err)
	}
	if text := refusedStop.Content[0].(*sdk.TextContent).Text; !strings.Contains(text, "without persist") || !strings.Contains(text, "sandbox_delete") {
		t.Fatalf("stop refusal does not point at sandbox_delete: %s", text)
	}
	if pg := mcpCall(t, cs, "sandbox_read_file", map[string]any{"handle": ph.Handle, "path": "note"}); !strings.Contains(string(pg), "kept") {
		t.Fatalf("refused stop cost the sandbox its files%s", pg)
	}
	mcpCall(t, cs, "sandbox_delete", map[string]any{"handle": ph.Handle})
}
