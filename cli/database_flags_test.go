package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
	"github.com/spf13/cobra"
)

type databaseCLIAsk struct {
	target  string
	request computeapi.AcquireRunTargetRequest
}

func databaseCLI(t *testing.T, asks *[]databaseCLIAsk, calls *int, args ...string) error {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		view := func(name string, secret bool) computeapi.RunTargetView {
			caps := []string{protocol.CapDatabase, protocol.CapVolumes, protocol.CapVolumesSeal, protocol.CapVolumesKeep, computeapi.EncodingBase64}
			cpu := 1
			if secret {
				caps = append(caps, protocol.CapBoxSecrets)
				cpu = 2
			}
			return computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: "box", Images: []string{refPin, agentPin}, TTLMaxS: 7200}, HostClass: api.MachineTrusted, Describe: &computeapi.RunTargetDescribe{Kind: "box", Available: true, Verbs: []string{"spawn", "exec"}, Capabilities: caps, TTLMaxS: 7200, Size: &computeapi.RunTargetSize{Name: name, OS: "linux", CPU: cpu, MemoryMB: 2048}}}
		}
		switch {
		case r.URL.Path == "/v0/run_targets":
			api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: []computeapi.RunTargetView{view("database-only", false), view("database-secrets", true)}})
		case strings.HasSuffix(r.URL.Path, "/acquire"):
			var request computeapi.AcquireRunTargetRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v0/run_targets/"), "/acquire")
			*asks = append(*asks, databaseCLIAsk{target, request})
			api.WriteError(w, api.Errorf(api.CodeForbidden, "test acquire boundary"))
		case strings.HasPrefix(r.URL.Path, "/v0/run_targets/"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v0/run_targets/"), "/describe")
			api.WriteJSON(w, 200, view(name, name == "database-secrets"))
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
		}
	})
	c := client.WithHandler(handler)
	c.Project, c.ProjectFrom = "pool", "flag"
	cfg, err := config.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	root := &cobra.Command{Use: "example", SilenceErrors: true, SilenceUsage: true}
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.AddCommand(Commands(Host{Prefix: "example", Client: func() (*client.Client, *config.Config, error) { return c, cfg, nil }, Stdin: strings.NewReader("test task"), Stdout: &stdout, Stderr: &stderr})...)
	root.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return root.ExecuteContext(ctx)
}

func TestDatabaseCLIForwardingPreservesLayersAndCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-test")
	t.Setenv("ANTHROPIC_API_KEY", "")
	repo := t.TempDir()
	for name, text := range map[string]string{"package.json": `{"name":"example","version":"1.0.0"}`, "package-lock.json": `{"name":"example","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"example","version":"1.0.0"}}}`} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	t.Chdir(repo)
	for _, test := range []struct {
		name     string
		args     []string
		target   string
		database bool
	}{
		{"create", []string{"sandbox", "create", "--ref", "HEAD", "--database", "example-db", "--deps", "npm"}, "database-only", true},
		{"run", []string{"run", "--database", "example-db", "--deps", "npm", "--", "true"}, "database-only", true},
		{"agent-task", []string{"agent-task", "--database", "example-db", "--deps", "npm", "--prompt-file", "-"}, "database-secrets", true},
		{"agent default template", []string{"agent-task", "--deps", "npm", "--prompt-file", "-"}, "database-secrets", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var asks []databaseCLIAsk
			calls := 0
			err := databaseCLI(t, &asks, &calls, test.args...)
			if err == nil || !strings.Contains(err.Error(), "test acquire boundary") {
				t.Fatalf("command never reached acquire: %v", err)
			}
			if len(asks) != 1 {
				t.Fatalf("acquires=%+v", asks)
			}
			ask := asks[0]
			if ask.target != test.target || len(ask.request.Deps) != 1 || ask.request.Base == nil {
				t.Fatalf("placement/layers lost: %+v", ask)
			}
			if test.database {
				if ask.request.Database != "example-db" || ask.request.Template != "" || ask.request.Image != "" {
					t.Fatalf("database request overwritten: %+v", ask.request)
				}
			} else if ask.request.Database != "" || ask.request.Template != "agent" {
				t.Fatalf("default agent template changed: %+v", ask.request)
			}
		})
	}
}

func TestDatabaseCLIConflictsBeforeAnyRequest(t *testing.T) {
	const noName = "--database needs a project recipe name"
	for _, test := range []struct {
		args []string
		want string // in the refusal
	}{
		{[]string{"sandbox", "create", "--database", "example-db", "--image", "custom"}, "[database image]"},
		{[]string{"sandbox", "create", "--database", "example-db", "--template", "agent"}, "[database template]"},
		{[]string{"sandbox", "create", "--database", "example-db", "--persist"}, "[database persist]"},
		{[]string{"run", "--database", "example-db", "--image", "custom"}, "[database image]"},
		{[]string{"run", "--database", "example-db", "--template", "agent"}, "[database template]"},
		{[]string{"agent-task", "--database", "example-db", "--image", "custom"}, "[database image]"},
		{[]string{"sandbox", "create", "--database", ""}, noName},
		{[]string{"run", "--database", ""}, noName},
		{[]string{"agent-task", "--database", ""}, noName},
		{[]string{"run", "--database", " example-db"}, noName},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			var asks []databaseCLIAsk
			calls := 0
			err := databaseCLI(t, &asks, &calls, test.args...)
			if err == nil || !strings.Contains(err.Error(), test.want) || calls != 0 {
				t.Fatalf("err=%v (want %q) HTTP calls=%d asks=%+v", err, test.want, calls, asks)
			}
		})
	}
}
