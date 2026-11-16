package grants_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"

	_ "modernc.org/sqlite"
)

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	// A host's adverts carry its swap state: not this machine's, whatever runs the tests.
	hostswap.Local = func(context.Context) hostswap.Reading { return hostswap.Reading{State: hostswap.Encrypted} }
	os.Exit(m.Run())
}

const refPin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// canary is the secret value: it must reach the box and nothing else.
const canary = "grants-canary-value-4b1e9d"

func code(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return "other"
	}
	return ""
}

type fixture struct {
	home       string
	fix        string
	owner      *client.Client
	ana, bob   *client.Client
	gus        *client.Client // a guest of the pool
	box, boxID string
	errs       []string // every error string a caller saw
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
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
	dir := filepath.Join(fix, "ref")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	drv, err := refdriver.Install(dir, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	f := &fixture{home: home, fix: fix, box: "box"}
	f.owner = client.WithHandler(dm.SocketHandler())
	f.owner.Project, f.owner.ProjectFrom = "acme", "flag"
	if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	member := func(name, role string) *client.Client {
		tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: name, Project: "acme", Role: role})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	f.ana, f.bob, f.gus = member("ana", "member"), member("bob", "member"), member("gus", "guest")
	b := computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
		Config: map[string]string{"remote": repo.Remote, "jit": "1"}}
	if _, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "g-1", TTLS: 300})
	if err != nil {
		t.Fatal(err)
	}
	f.boxID = h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	return f
}

// exec runs argv in ana's box with secrets, keeping any error's text.
func (f *fixture) exec(t *testing.T, c *client.Client, argv []string, secrets map[string]string) (computeapi.RunTargetExec, error) {
	t.Helper()
	out, err := computeclient.ExecRunTarget(context.Background(), c, f.box, f.boxID, computeapi.ExecRunTargetRequest{Argv: argv, Secrets: secrets})
	if err != nil {
		f.errs = append(f.errs, err.Error())
	}
	return out, err
}

// ledger is every secret.* event in the pool, oldest first.
func (f *fixture) ledger(t *testing.T) []api.Event { return f.events(t, f.owner, "dev.zm.secret.") }

// events is every event of types (a prefix) c may read in the pool.
func (f *fixture) events(t *testing.T, c *client.Client, types string) []api.Event {
	t.Helper()
	var out []api.Event
	err := c.Events(context.Background(), api.EventQuery{Types: types, Project: "acme", Since: "0", NoLive: true},
		func(e api.Event) error { out = append(out, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// noCanary: the value is in no file under the daemon's home (the project
// database with its event log, the registry, logs), no event and no error
// a caller saw.
func (f *fixture) noCanary(t *testing.T) {
	t.Helper()
	_ = filepath.WalkDir(f.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(canary)) {
			t.Errorf("%s holds the secret's value", p)
		}
		return nil
	})
	for _, e := range f.ledger(t) {
		if bytes.Contains(e.Data, []byte(canary)) {
			t.Errorf("event %s holds the value: %s", e.Type, e.Data)
		}
	}
	for _, s := range f.errs {
		if strings.Contains(s, canary) {
			t.Errorf("an error holds the value: %s", s)
		}
	}
}

func secretEvent(t *testing.T, e api.Event) computeapi.SecretEvent {
	t.Helper()
	var s computeapi.SecretEvent
	if err := json.Unmarshal(e.Data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A pool with no grant rows behaves as before grants: ana's exec and spawn
// with a secret run as they did, and the ledger names the secret (never its
// value) as granted by the default and delivered.
func TestNoRowsBehavesAsBefore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	l, err := computeclient.SecretGrants(ctx, f.ana)
	if err != nil || len(l.Grants) != 2 || l.Grants[0].ID != "default" || l.Grants[1].ID != "default-jit" || l.Grants[0].Off {
		t.Fatalf("ls with no rows: %+v %v", l, err)
	}
	out, err := f.exec(t, f.ana, []string{"sh", "-c", `test "$(cat "$POTLUCK_SECRETS/API_KEY")" = "` + canary + `" && echo ok`},
		map[string]string{"API_KEY": canary})
	if err != nil || out.Exit != 0 || strings.TrimSpace(out.Stdout) != "ok" {
		t.Fatalf("exec with a secret and no rows: %+v %v", out, err)
	}
	if _, err := computeclient.SpawnRunTarget(ctx, f.ana, f.box, f.boxID, computeapi.SpawnRunTargetRequest{RequestID: "sp-1",
		Argv: []string{"sh", "-c", `cat "$POTLUCK_SECRETS/API_KEY" >/dev/null`}, Secrets: map[string]string{"API_KEY": canary}}); err != nil {
		t.Fatalf("spawn with a secret and no rows: %v", err)
	}
	evs := f.ledger(t)
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	want := []string{computeapi.EventSecretGranted, computeapi.EventSecretDelivered, computeapi.EventSecretGranted, computeapi.EventSecretDelivered}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Fatalf("ledger %v, want %v", types, want)
	}
	s := secretEvent(t, evs[0])
	if s.Names[0] != "API_KEY" || s.Sizes["API_KEY"] != len(canary) || s.Grants["API_KEY"] != "default" || s.Kind != "value" ||
		s.HostClass != api.MachineTrusted || s.Verb != "exec" || !strings.Contains(s.Box, f.boxID) || s.By == "" {
		t.Errorf("granted event: %+v", s)
	}
	if s := secretEvent(t, evs[3]); s.Verb != "spawn" || s.Failed {
		t.Errorf("spawn's delivered event: %+v", s)
	}
	f.noCanary(t)
}

// Only admins edit the table; with the default off, a secret no grant
// covers is refused before anything reaches the driver, naming nothing;
// an identity grant lets exactly that identity send exactly that name.
func TestGrantsDecide(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	add := func(c *client.Client, req computeapi.AddSecretGrantRequest) (computeapi.SecretGrant, error) {
		return computeclient.AddSecretGrant(ctx, c, req)
	}
	anaKey := computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Identity: "ana"}, Kind: "value", Name: "API_KEY",
		Ceiling: json.RawMessage(`{"max_bytes":64}`)}
	// Members edit nothing.
	if _, err := add(f.ana, anaKey); code(err) != api.CodeForbidden {
		t.Fatalf("a member adds a grant: %v", err)
	}
	if _, err := computeclient.SetSecretGrantDefault(ctx, f.ana, false); code(err) != api.CodeForbidden {
		t.Fatalf("a member turns the default off: %v", err)
	}
	// Refused at add, the stricter way.
	for name, req := range map[string]computeapi.AddSecretGrantRequest{
		"a public host":    {To: computeapi.GrantTo{Role: "member"}, Kind: "value", Name: "A", HostClass: api.MachinePublic},
		"no issuer":        {To: computeapi.GrantTo{Role: "member"}, Kind: "github-token", Name: "GH_TOKEN"},
		"an unknown kind":  {To: computeapi.GrantTo{Role: "member"}, Kind: "password", Name: "A"},
		"a guest role":     {To: computeapi.GrantTo{Role: "guest"}, Kind: "value", Name: "A"},
		"two subjects":     {To: computeapi.GrantTo{Role: "member", Identity: "ana"}, Kind: "value", Name: "A"},
		"a bad name":       {To: computeapi.GrantTo{Role: "member"}, Kind: "value", Name: "a b"},
		"a wide ceiling":   {To: computeapi.GrantTo{Role: "member"}, Kind: "value", Name: "A", Ceiling: json.RawMessage(`{"max_bytes":70000}`)},
		"an unknown field": {To: computeapi.GrantTo{Role: "member"}, Kind: "value", Name: "A", Ceiling: json.RawMessage(`{"names":["A"]}`)},
		"a box scope typo": {To: computeapi.GrantTo{Role: "member"}, Kind: "value", Name: "A", Scopes: []string{"forever"}},
		"a value job": {To: computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/x.yml",
			Ref: "refs/heads/main", Events: []string{"push"}}}, Kind: "value", Name: "A"},
	} {
		if _, err := add(f.owner, req); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v, want bad_request", name, err)
		}
	}
	if _, err := add(f.owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Identity: "nobody"}, Kind: "value", Name: "A"}); code(err) != api.CodeNotFound {
		t.Errorf("an identity not in the pool: %v", err)
	}
	// The default off: refused before the driver runs anything.
	if _, err := computeclient.SetSecretGrantDefault(ctx, f.owner, false); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(f.fix, "ran")
	touch := []string{"sh", "-c", "touch " + mark}
	_, err := f.exec(t, f.ana, touch, map[string]string{"API_KEY": canary})
	if code(err) != api.CodeForbidden || strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("a secret with the default off: %v, want forbidden naming nothing", err)
	}
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Fatalf("the refused command ran: %v", err)
	}
	if _, err := computeclient.SpawnRunTarget(ctx, f.ana, f.box, f.boxID, computeapi.SpawnRunTargetRequest{RequestID: "sp-2",
		Argv: touch, Secrets: map[string]string{"API_KEY": canary}}); code(err) != api.CodeForbidden {
		t.Fatalf("spawn with the default off: %v", err)
	}
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Fatalf("the refused spawn ran: %v", err)
	}
	if out, err := f.exec(t, f.ana, touch, nil); err != nil || out.Exit != 0 {
		t.Fatalf("exec without secrets: %+v %v", out, err)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("the control command did not run: %v", err)
	}
	// ana's own grant: her name passes, up to its ceiling; another name, or
	// bob, does not.
	g, err := add(f.owner, anaKey)
	if err != nil || g.To.IdentityID == "" || g.HostClass != api.MachineTrusted || strings.Join(g.Scopes, ",") != "exec" {
		t.Fatalf("add: %+v %v", g, err)
	}
	if out, err := f.exec(t, f.ana, []string{"sh", "-c", `cat "$POTLUCK_SECRETS/API_KEY"`}, map[string]string{"API_KEY": canary}); err != nil || out.Stdout != canary {
		t.Fatalf("ana's granted secret: %+v %v", out, err)
	}
	for name, sec := range map[string]map[string]string{
		"another name":       {"OTHER_KEY": canary},
		"one name uncovered": {"API_KEY": canary, "OTHER_KEY": canary},
		"over the ceiling":   {"API_KEY": strings.Repeat(canary, 3)},
	} {
		if _, err := f.exec(t, f.ana, []string{"true"}, sec); code(err) != api.CodeForbidden || strings.Contains(err.Error(), "KEY") {
			t.Errorf("%s: %v", name, err)
		}
	}
	bobBox, err := computeclient.AcquireRunTarget(ctx, f.bob, "box", computeapi.AcquireRunTargetRequest{RequestID: "g-bob", TTLS: 300})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, f.bob, "box", bobBox.Handle[strings.LastIndex(bobBox.Handle, "/")+1:],
		computeapi.ExecRunTargetRequest{Argv: []string{"true"}, Secrets: map[string]string{"API_KEY": canary}}); code(err) != api.CodeForbidden {
		t.Errorf("bob under ana's grant: %v", err)
	}
	// ls: the default (off) and ana's grant; rm is an admin's.
	l, err := computeclient.SecretGrants(ctx, f.ana)
	if err != nil || len(l.Grants) != 3 || !l.Grants[0].Off || !l.Grants[1].Off || l.Grants[2].ID != g.ID || l.Grants[2].Stale {
		t.Fatalf("ls: %+v %v", l, err)
	}
	if _, err := computeclient.RemoveSecretGrant(ctx, f.ana, g.ID); code(err) != api.CodeForbidden {
		t.Errorf("a member removes a grant: %v", err)
	}
	if _, err := computeclient.RemoveSecretGrant(ctx, f.owner, "default"); code(err) != api.CodeBadRequest {
		t.Errorf("rm default: %v", err)
	}
	if _, err := computeclient.RemoveSecretGrant(ctx, f.owner, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.RemoveSecretGrant(ctx, f.owner, g.ID); code(err) != api.CodeNotFound {
		t.Errorf("rm twice: %v", err)
	}
	if _, err := f.exec(t, f.ana, []string{"true"}, map[string]string{"API_KEY": canary}); code(err) != api.CodeForbidden {
		t.Errorf("after rm: %v", err)
	}
	// The default back on: as before.
	if _, err := computeclient.SetSecretGrantDefault(ctx, f.owner, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(t, f.ana, []string{"true"}, map[string]string{"ANY_NAME": canary}); err != nil {
		t.Errorf("the default back on: %v", err)
	}
	// The ledger: refusals with names and a why, never a value.
	refused := 0
	for _, e := range f.ledger(t) {
		if e.Type == computeapi.EventSecretRefused {
			refused++
			if s := secretEvent(t, e); len(s.Names) == 0 || s.Why == "" {
				t.Errorf("refusal event: %+v", s)
			}
		}
	}
	// The default off: exec and spawn; another name, one uncovered, over the
	// ceiling; bob; after rm.
	if refused != 7 {
		t.Errorf("%d refusals in the ledger, want 7", refused)
	}
	f.noCanary(t)
}

// A guest reads neither the table nor the ledger; a member reads the
// ledger but not the table's edits. A grant whose identity left the pool
// stays stale when it comes back. A corrupt row lists (flagged), matches
// nothing, and is removed with its event.
func TestGrantsGuestsStaleCorrupt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := computeclient.SecretGrants(ctx, f.gus); code(err) != api.CodeForbidden {
		t.Fatalf("a guest lists grants: %v", err)
	}
	g, err := computeclient.AddSecretGrant(ctx, f.owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Identity: "ana"}, Kind: "value", Name: "API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.SetSecretGrantDefault(ctx, f.owner, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(t, f.ana, []string{"true"}, map[string]string{"API_KEY": canary}); err != nil {
		t.Fatalf("ana's grant: %v", err)
	}
	if evs := f.events(t, f.gus, "dev.zm.secret"); len(evs) != 0 {
		t.Errorf("a guest read %d secret events", len(evs))
	}
	if evs := f.events(t, f.ana, "dev.zm.secret_grant."); len(evs) != 0 {
		t.Errorf("a member read %d table edits", len(evs))
	}
	if evs := f.events(t, f.ana, "dev.zm.secret."); len(evs) != 2 {
		t.Errorf("a member read %d secret events, want granted and delivered", len(evs))
	}
	if evs := f.events(t, f.owner, "dev.zm.secret_grant."); len(evs) != 2 {
		t.Errorf("the owner read %d table edits, want added and default", len(evs))
	}
	// ana leaves the pool and comes back: her grant stays stale.
	time.Sleep(5 * time.Millisecond) // the registry keeps milliseconds
	if _, err := f.owner.Revoke(ctx, api.GrantRequest{Identity: "ana", Project: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Grant(ctx, api.GrantRequest{Identity: "ana", Role: "member", Project: "acme"}); err != nil {
		t.Fatal(err)
	}
	l, err := computeclient.SecretGrants(ctx, f.owner)
	if err != nil || len(l.Grants) != 3 || !l.Grants[2].Stale {
		t.Fatalf("ls after ana came back: %+v %v", l, err)
	}
	if _, err := f.exec(t, f.ana, []string{"true"}, map[string]string{"API_KEY": canary}); code(err) != api.CodeForbidden {
		t.Errorf("a stale grant: %v", err)
	}
	// A corrupt row.
	dbs, _ := filepath.Glob(filepath.Join(f.home, "projects", "*", "project.db"))
	if len(dbs) != 1 {
		t.Fatalf("project databases: %v", dbs)
	}
	raw, err := sql.Open("sqlite", dbs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE secret_grants SET ceiling='{oops', to_json='[' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	l, err = computeclient.SecretGrants(ctx, f.owner)
	if err != nil || len(l.Grants) != 3 || !l.Grants[2].Corrupt {
		t.Fatalf("ls with a corrupt row: %+v %v", l, err)
	}
	if _, err := computeclient.RemoveSecretGrant(ctx, f.owner, g.ID); err != nil {
		t.Fatalf("rm a corrupt row: %v", err)
	}
	removed := false
	for _, e := range f.events(t, f.owner, computeapi.EventSecretGrantRemoved) {
		removed = removed || e.Subject == g.ID
	}
	if !removed {
		t.Error("no removed event for the corrupt row")
	}
	f.noCanary(t)
}

// A jit_config on acquire is kind github-jit: the default covers it as
// before grants (a member acquiring on a trusted host), the ledger records
// it without its value, and with the defaults off it is refused before the
// driver makes a box.
func TestJITUnderGrants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const jit = "eyJzZWtyaXQiOiJncmFudHMtaml0LXZhbHVlIn0="
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, f.box, computeapi.AcquireRunTargetRequest{RequestID: "j-1", TTLS: 120, JITConfig: jit})
	if err != nil {
		t.Fatalf("a jit_config under the default: %v", err)
	}
	evs := f.ledger(t)
	if len(evs) != 2 || evs[0].Type != computeapi.EventSecretGranted || evs[1].Type != computeapi.EventSecretDelivered {
		t.Fatalf("ledger: %+v", evs)
	}
	if s := secretEvent(t, evs[0]); s.Kind != computeapi.SecretKindGitHubJIT || s.Grants[computeapi.SecretNameJIT] != computeapi.GrantDefaultJITID || s.Verb != "acquire" {
		t.Errorf("granted: %+v", s)
	}
	if _, err := computeclient.SetSecretGrantDefault(ctx, f.owner, false); err != nil {
		t.Fatal(err)
	}
	before, err := computeclient.RunTargetHandles(ctx, f.owner, f.box, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, f.ana, f.box, computeapi.AcquireRunTargetRequest{RequestID: "j-2", TTLS: 120, JITConfig: jit})
	if code(err) != api.CodeForbidden || strings.Contains(err.Error(), jit) {
		t.Fatalf("a jit_config with the defaults off: %v", err)
	}
	after, err := computeclient.RunTargetHandles(ctx, f.owner, f.box, false)
	if err != nil || len(after) != len(before) {
		t.Fatalf("a refused acquire made a handle: %d then %d (%v)", len(before), len(after), err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, f.ana, f.box, computeapi.AcquireRunTargetRequest{RequestID: "j-3", TTLS: 120}); err != nil {
		t.Fatalf("an acquire with no jit_config: %v", err)
	}
	for _, e := range f.events(t, f.owner, "dev.zm.") {
		if bytes.Contains(e.Data, []byte(jit)) {
			t.Errorf("event %s holds the jit_config", e.Type)
		}
	}
	_ = h
}

// A project's export carries no secret name: grant rows stay with their
// names blanked, and the ledger's events are not in it.
func TestExportCarriesNoSecretName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := computeclient.AddSecretGrant(ctx, f.owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Identity: "ana"},
		Kind: "value", Name: "EXPORT_NAME_ZZQ"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(t, f.ana, []string{"true"}, map[string]string{"LEDGER_NAME_ZZQ": canary}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.fix, "export")
	out, err := f.owner.ExportProject(ctx, "acme", api.ExportRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Files {
		b, _ := os.ReadFile(p)
		for _, x := range []string{"EXPORT_NAME_ZZQ", "LEDGER_NAME_ZZQ", canary} {
			if bytes.Contains(b, []byte(x)) {
				t.Errorf("%s holds %s", p, x)
			}
		}
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "project.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var rows, events int
	if err := raw.QueryRow(`SELECT count(*) FROM secret_grants`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("exported grant rows: %d %v", rows, err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM events WHERE type LIKE 'dev.zm.secret%'`).Scan(&events); err != nil || events != 0 {
		t.Errorf("exported ledger events: %d %v", events, err)
	}
}
