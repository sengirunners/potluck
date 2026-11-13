package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	sandboxmcp "github.com/sengirunners/potluck/mcp"
	"github.com/sengirunners/potluck/wiring"
)

// A program built on the potluck command runs its own daemon wiring under
// `daemon`, and adds its commands to the tree and its tools to `mcp`;
// Potluck's own command has none of them.
func TestProgramAddsItsDaemonCommandsAndTools(t *testing.T) {
	home, err := os.MkdirTemp("", "pl") // short: the socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv(EnvMachine, "solo")
	p := Program{
		Wiring: func(d *core.Deps) ([]core.Module, error) {
			d.AddHealthPart("program", func(context.Context) any { return "wired" })
			mods, err := wiring.Compute(d)
			for _, m := range mods {
				if s, ok := m.(*tailnet.Service); ok { // a test binary's node is a fake one
					s.NewNode = tailnettest.New().NodeFunc(tailnettest.Identity{NodeID: "nsolo", NodeKey: "nodekey:solo", Login: "solo@example.com", Host: "solo"})
				}
			}
			return mods, err
		},
		Commands: []Command{func(env Env) *cobra.Command {
			return &cobra.Command{Use: "part", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := env.Client()
				if err != nil {
					return err
				}
				var h map[string]json.RawMessage
				if err := c.Do(cmd.Context(), http.MethodGet, "/v0/health", nil, &h); err != nil {
					return err
				}
				if env.JSON() {
					return env.PrintJSON(h["program"])
				}
				env.Say("part %s", h["program"])
				return nil
			}}
		}},
		Tools: []sandboxmcp.Tools{func(tb *sandboxmcp.Toolbox) {
			sandboxmcp.Add(tb, "program_whoami", "Who the caller is.", nil, false, func(ctx context.Context, c *client.Client, _ struct{}) (any, error) {
				return c.Whoami(ctx)
			})
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var daemonOut, daemonErr bytes.Buffer
	go func() { done <- RunProgram(ctx, p, []string{"daemon"}, strings.NewReader(""), &daemonOut, &daemonErr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("daemon exit %d: %s", code, daemonErr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("the program's daemon did not stop")
		}
	})
	if err := client.NewUnix(filepath.Join(home, SocketName)).WaitReady(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	run := func(p Program, args ...string) (string, int) {
		var o, e bytes.Buffer
		code := RunProgram(context.Background(), p, args, strings.NewReader(""), &o, &e)
		return o.String() + e.String(), code
	}
	if out, code := run(p, "--json", "part"); code != 0 || strings.TrimSpace(out) != `"wired"` {
		t.Fatalf("the program's command on its daemon (--json): %d %q", code, out)
	}
	if out, code := run(p, "part"); code != 0 || strings.TrimSpace(out) != `part "wired"` {
		t.Fatalf("the program's command on its daemon: %d %q", code, out)
	}
	if out, code := run(Program{}, "part"); code == 0 {
		t.Fatalf("potluck itself ran the program's command: %q", out)
	}

	if out, code := run(p, "create", "friends"); code != 0 { // mcp's tools act in the only pool
		t.Fatalf("create: %d %q", code, out)
	}
	cs := mcpProgram(t, p, "mcp")
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "program_whoami") || !slices.Contains(names, "sandbox_create") {
		t.Fatalf("mcp tools %v", names)
	}
	if who := mcpCall(t, cs, "program_whoami", nil); !strings.Contains(string(who), "solo") {
		t.Fatalf("program_whoami: %s", who)
	}
	other := mcpCommand(t, "mcp")
	if tools, err := other.ListTools(context.Background(), nil); err != nil || slices.ContainsFunc(tools.Tools, func(x *sdk.Tool) bool { return x.Name == "program_whoami" }) {
		t.Fatalf("potluck's own mcp has the program's tool: %v", err)
	}
}

// A program's command or tool never replaces one of Potluck's.
func TestProgramCannotReplacePotlucks(t *testing.T) {
	refused := func(what string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s was accepted", what)
			}
		}()
		f()
	}
	refused("a command named run", func() {
		RunProgram(context.Background(), Program{Commands: []Command{func(Env) *cobra.Command { return &cobra.Command{Use: "run"} }}},
			[]string{"--help"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	})
	refused("a tool named sandbox_exec", func() {
		sandboxmcp.NewServer(nil, func(tb *sandboxmcp.Toolbox) {
			sandboxmcp.Add(tb, "sandbox_exec", "", nil, false, func(context.Context, *client.Client, struct{}) (any, error) { return nil, nil })
		})
	})
}
