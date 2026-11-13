package runtarget_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestBoxSecrets (SPEC 20 "Boxes", Secrets): a renter's secrets reach the
// command as files under $ZM_SECRETS; a bad name or more than 64 KiB is
// refused without naming it; a driver without the capability is refused
// (never run without them); and the value is in no file under zm's home.
func TestBoxSecrets(t *testing.T) {
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
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
	drivers := map[string]string{}
	for name, bug := range map[string]string{"ref": "", "refnosec": "secrets-ignored", "refecho": "secrets-refuse-echo"} {
		dir := filepath.Join(fix, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(dir, "state")
		if drivers[name], err = refdriver.Install(dir, state, bug); err != nil {
			t.Fatal(err)
		}
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
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	ana := client.WithHandler(dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "acme", "flag"
	for name, drv := range map[string]string{"box": "ref", "nosec": "refnosec", "echo": "refecho"} {
		b := computeapi.RunTarget{Name: name, Driver: drv, Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
			Config: map[string]string{"remote": repo.Remote}}
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(target string) string {
		h, err := computeclient.AcquireRunTarget(ctx, ana, target, computeapi.AcquireRunTargetRequest{RequestID: "s-" + target, TTLS: 120})
		if err != nil {
			t.Fatal(err)
		}
		return idOf(h.Handle)
	}
	const val = "zm-secret-canary-7f3a9c"
	id := acquire("box")
	out, err := computeclient.ExecRunTarget(ctx, ana, "box", id, computeapi.ExecRunTargetRequest{
		Argv: []string{"sh", "-c", `test "$(cat "$ZM_SECRETS/API_KEY")" = "$(printf '%s' zm-secret-canary-7f3a9c)" && env | grep -c canary`}, Secrets: map[string]string{"API_KEY": val}})
	if err != nil || out.Exit != 1 || strings.TrimSpace(out.Stdout) != "0" {
		t.Fatalf("exec with a secret: %+v %v", out, err)
	}
	for name, s := range map[string]map[string]string{
		"a bad name":    {"bad name zzq": "x"},
		"lower case":    {"api_key_zzq": "x"},
		"over 64 KiB":   {"BIG": strings.Repeat("x", 64<<10)},
		"no capability": {"API_KEY": "x"},
	} {
		target, tid := "box", id
		if name == "no capability" {
			target, tid = "nosec", acquire("nosec")
		}
		_, err := computeclient.ExecRunTarget(ctx, ana, target, tid, computeapi.ExecRunTargetRequest{Argv: []string{"true"}, Secrets: s})
		if code(err) != api.CodeBadRequest || strings.Contains(err.Error(), "zzq") {
			t.Errorf("%s: %v, want a bad_request that names nothing given", name, err)
		}
	}
	// A driver quoting the secret in its refusal: the caller never sees it.
	_, err = computeclient.ExecRunTarget(ctx, ana, "echo", acquire("echo"), computeapi.ExecRunTargetRequest{Argv: []string{"true"},
		Secrets: map[string]string{"API_KEY_ZZQ": val}})
	if err == nil || strings.Contains(err.Error(), val) || strings.Contains(err.Error(), "API_KEY_ZZQ") {
		t.Errorf("a driver's echoing refusal: %v", err)
	}
	// A malformed secrets field: the decode error names neither key nor value.
	for _, body := range []string{`{"argv":["true"],"secrets":{"ZZQNAME":12345}}`, `{"argv":["true"],"secrets":"zzqvalue"}`} {
		err := ana.Do(ctx, "POST", "/v0/run_targets/box/handles/"+id+"/exec", json.RawMessage(body), nil)
		if code(err) != api.CodeBadRequest || strings.Contains(err.Error(), "ZZQNAME") || strings.Contains(err.Error(), "zzqvalue") || strings.Contains(err.Error(), "12345") {
			t.Errorf("malformed secrets %s: %v", body, err)
		}
	}
	// Nothing under zm's home holds the value: not run_handles, not logs.
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(val)) {
			t.Errorf("%s holds the secret", p)
		}
		return nil
	})
}
