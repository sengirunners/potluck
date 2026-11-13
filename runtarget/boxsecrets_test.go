package runtarget_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

	_ "modernc.org/sqlite"
)

// TestBoxSecretsThroughTheDaemon (zm-eakh.12.48.3): POST .../secrets is the
// holder's, refused with no grant at scope box (no default covers it),
// allowed under one with a ledger entry of names and sizes only, refused for
// a name the authority mints, reaches a driver listing box-secrets and no
// other, and no value or name it sent comes back in a refusal, nor lands
// under the daemon's home.
func TestBoxSecretsThroughTheDaemon(t *testing.T) {
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
	for name, bug := range map[string]string{"ref": "", "refnobox": "no-box-secrets", "refecho": "box-secrets-refuse-echo"} {
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
	member := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	ana, bob := member("ana"), member("bob")
	for name, drv := range map[string]string{"box": "ref", "nobox": "refnobox", "echo": "refecho"} {
		b := computeapi.RunTarget{Name: name, Driver: drv, Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
			Config: map[string]string{"remote": repo.Remote}}
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(target string) string {
		h, err := computeclient.AcquireRunTarget(ctx, ana, target, computeapi.AcquireRunTargetRequest{RequestID: "bs-" + target, TTLS: 120})
		if err != nil {
			t.Fatal(err)
		}
		return idOf(h.Handle)
	}
	const val = "zm-boxsecret-canary-91c4e2"
	set := func(c *client.Client, target, id string, body any) (map[string]any, error) {
		var out map[string]any
		err := c.Do(ctx, "POST", "/v0/run_targets/"+target+"/handles/"+id+"/secrets", body, &out)
		return out, err
	}
	noLeak := func(what string, err error) {
		t.Helper()
		if err != nil && (strings.Contains(err.Error(), val) || strings.Contains(err.Error(), "API_KEY_ZZQ")) {
			t.Errorf("%s: the refusal quotes what was sent: %v", what, err)
		}
	}
	id := acquire("box")
	body := map[string]any{"secrets": map[string]string{"API_KEY_ZZQ": val}}

	// No grant at scope box: refused, and nothing reached the box.
	_, err = set(ana, "box", id, body)
	if code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "grant") {
		t.Errorf("with no grant: %v, want forbidden naming the grant", err)
	}
	noLeak("no grant", err)
	noGrant := err.Error()
	out, err := computeclient.ExecRunTarget(ctx, ana, "box", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", `test -z "${POTLUCK_SECRETS:-}"`}})
	if err != nil || out.Exit != 0 {
		t.Errorf("a refused call left a secrets directory: %+v %v", out, err)
	}

	// Under a grant: allowed, and the ledger has names, sizes and the
	// caller, never a value.
	if _, err := computeclient.AddSecretGrant(ctx, owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Role: "member"},
		Kind: computeapi.SecretKindValue, Name: "*", Scopes: []string{computeapi.SecretScopeBox}}); err != nil {
		t.Fatal(err)
	}
	res, err := set(ana, "box", id, body)
	if err != nil || res["count"] != 1.0 || !strings.HasPrefix(fmt.Sprint(res["dir"]), "/") || strings.Contains(fmt.Sprint(res), val) {
		t.Fatalf("an allowed secrets call: %v %v", res, err)
	}
	var ledger []api.Event
	if err := owner.Events(ctx, api.EventQuery{Types: computeapi.EventSecretGranted, Project: "acme", Since: "0", NoLive: true},
		func(e api.Event) error { ledger = append(ledger, e); return nil }); err != nil {
		t.Fatal(err)
	}
	var q computeapi.SecretEvent
	if len(ledger) != 1 || json.Unmarshal(ledger[0].Data, &q) != nil {
		t.Fatalf("the ledger: %+v", ledger)
	}
	if !strings.Contains(q.By, "ana") || q.Scope != "box" || q.Kind != "value" || q.HostClass != api.MachineTrusted || q.Verb != "secrets" ||
		len(q.Names) != 1 || q.Names[0] != "API_KEY_ZZQ" || q.Sizes["API_KEY_ZZQ"] != len(val) || q.Target != "box" || !strings.HasSuffix(q.Box, id) {
		t.Errorf("the ledger entry: %+v", q)
	}
	if bytes.Contains(ledger[0].Data, []byte(val)) {
		t.Errorf("the ledger holds the value: %s", ledger[0].Data)
	}
	// A name the authority mints into this box (a jit_config's; a name an
	// issuer's grant of this box's area, or of every area, names): neither
	// set nor unset by the holder, refused exactly as a name no grant
	// covers. An issuer's grant of another area reserves nothing here.
	// (Issuer rows written straight to the table: no issuer runs here.)
	dbs, _ := filepath.Glob(filepath.Join(home, "projects", "*", "project.db"))
	if len(dbs) != 1 {
		t.Fatalf("project databases: %v", dbs)
	}
	raw, err := sql.Open("sqlite", dbs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, r := range [][2]string{{"", "ROOT_TOKEN"}, {"elsewhere", "OTHER_TOKEN"}} {
		if _, err := raw.Exec(`INSERT INTO secret_grants (id, area, to_json, kind, name, ceiling, host_class, scopes, created)
  VALUES (?, ?, '{"role":"member"}', 'github-token', ?, '{}', 'trusted', 'box', '2026-01-01T00:00:00.000000000Z')`, "iss-"+r[1], r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []any{map[string]any{"secrets": map[string]string{computeapi.SecretNameJIT: val}},
		map[string]any{"unset": []string{computeapi.SecretNameJIT}},
		map[string]any{"secrets": map[string]string{"ROOT_TOKEN": val}},
		map[string]any{"unset": []string{"ROOT_TOKEN"}}} {
		if _, err := set(ana, "box", id, b); code(err) != api.CodeForbidden || err.Error() != noGrant {
			t.Errorf("a reserved name %v: %v, want the grant check's own refusal", b, err)
		}
	}
	if _, err := set(ana, "box", id, map[string]any{"secrets": map[string]string{"OTHER_TOKEN": val}}); err != nil {
		t.Errorf("a name an issuer's grant of another area names: %v", err)
	}
	if _, err := set(ana, "box", id, map[string]any{"unset": []string{"OTHER_TOKEN"}}); err != nil {
		t.Errorf("unset it: %v", err)
	}
	out, err = computeclient.ExecRunTarget(ctx, ana, "box", id, computeapi.ExecRunTargetRequest{
		Argv: []string{"sh", "-c", `test "$(cat "$POTLUCK_SECRETS/API_KEY_ZZQ")" = "$(printf '%s' zm-boxsecret-canary-91c4e2)" && env | grep -c canary`}})
	if err != nil || out.Exit != 1 || strings.TrimSpace(out.Stdout) != "0" {
		t.Errorf("a later exec reads the box secret: %+v %v", out, err)
	}
	if _, err := set(ana, "box", id, map[string]any{"unset": []string{"API_KEY_ZZQ"}}); err != nil {
		t.Errorf("unset: %v", err)
	}

	// Refusals before any driver call, naming nothing sent.
	for name, b := range map[string]any{
		"a bad name":   map[string]any{"secrets": map[string]string{"bad name zzq": "x"}},
		"lower case":   map[string]any{"secrets": map[string]string{"api_key_zzq": "x"}},
		"over 64 KiB":  map[string]any{"secrets": map[string]string{"BIG": strings.Repeat("x", 64<<10)}},
		"nothing":      map[string]any{},
		"a string":     json.RawMessage(`{"secrets":"zzqvalue"}`),
		"a number":     json.RawMessage(`{"secrets":{"ZZQNAME":12345}}`),
		"an odd field": json.RawMessage(`{"secret":{"ZZQNAME":"zzqvalue"}}`),
	} {
		_, err := set(ana, "box", id, b)
		if code(err) != api.CodeBadRequest || strings.Contains(err.Error(), "zzq") || strings.Contains(err.Error(), "ZZQ") || strings.Contains(err.Error(), "12345") {
			t.Errorf("%s: %v, want a bad_request that names nothing given", name, err)
		}
	}
	// The holder's alone; a driver without box-secrets is never sent them.
	if _, err := set(bob, "box", id, body); code(err) != api.CodeForbidden {
		t.Errorf("another member: %v, want forbidden", err)
	}
	if _, err := set(ana, "nobox", acquire("nobox"), body); code(err) != api.CodeBadRequest {
		t.Errorf("a driver without box-secrets: %v, want bad_request", err)
	}
	// A driver quoting the value in its refusal: the caller never sees it.
	_, err = set(ana, "echo", acquire("echo"), body)
	if err == nil {
		t.Error("the echoing driver took the call")
	}
	noLeak("a driver's echoing refusal", err)
	// After release: conflict.
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box", id); err != nil {
		t.Fatal(err)
	}
	if _, err := set(ana, "box", id, body); code(err) != api.CodeConflict {
		t.Errorf("after release: %v, want conflict", err)
	}
	// Nothing under the daemon's home holds the value: rows, logs, events.
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
