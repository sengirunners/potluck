package runtarget_test

import (
	"context"
	"database/sql"
	"errors"
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
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/runtarget/rt"
	"github.com/sengirunners/potluck/wiring"

	_ "modernc.org/sqlite"
)

// The door (rt.Door) writes, as the authority, a name the holder's route
// refuses (an issuer grant's GH_TOKEN, the ready marker) into a ready box
// whose driver keeps box secrets. It keeps every other gate: a box not
// ready, past its expiry or ended, a driver without box-secrets, a public
// host, an unknown handle are each refused and nothing is written. BoxLive
// and BoxClass answer from the record.
func TestDoorWritesWhatTheHolderMayNot(t *testing.T) {
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
	door := rt.DoorFrom(dm.Deps)
	if door == nil {
		t.Fatal("wiring.Compute provides no door")
	}
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
	drivers := map[string]string{}
	for name, bug := range map[string]string{"ref": "", "refnobox": "no-box-secrets"} {
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
	p, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	ana := client.WithHandler(dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "acme", "flag"
	for name, drv := range map[string]string{"box": "ref", "nobox": "refnobox"} {
		b := computeapi.RunTarget{Name: name, Driver: drv, Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
			Config: map[string]string{"remote": repo.Remote}}
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(target, req string) string {
		h, err := computeclient.AcquireRunTarget(ctx, ana, target, computeapi.AcquireRunTargetRequest{RequestID: req, TTLS: 120})
		if err != nil {
			t.Fatal(err)
		}
		return h.Handle
	}
	// The holder may set any name under a value grant, but not an issuer
	// grant's name nor the ready marker.
	if _, err := computeclient.AddSecretGrant(ctx, owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Role: "member"},
		Kind: computeapi.SecretKindValue, Name: "*", Scopes: []string{computeapi.SecretScopeBox}}); err != nil {
		t.Fatal(err)
	}
	dbs, _ := filepath.Glob(filepath.Join(home, "projects", "*", "project.db"))
	if len(dbs) != 1 {
		t.Fatalf("project databases: %v", dbs)
	}
	raw, err := sql.Open("sqlite", dbs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO secret_grants (id, area, to_json, kind, name, ceiling, host_class, scopes, created)
  VALUES ('iss-gh', '', '{"role":"member"}', 'github-token', 'GH_TOKEN', '{}', 'trusted', 'box', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	h := acquire("box", "door-1")
	holderSet := func(name, val string) error {
		return ana.Do(ctx, "POST", "/v0/run_targets/box/handles/"+idOf(h)+"/secrets", map[string]any{"secrets": map[string]string{name: val}}, nil)
	}
	if err := holderSet("FREE_NAME", "x"); err != nil {
		t.Fatalf("the holder under a value grant: %v", err)
	}
	for _, name := range []string{"GH_TOKEN", protocol.SecretNameReady} {
		if err := holderSet(name, "holder-forged"); code(err) != api.CodeForbidden {
			t.Errorf("the holder sets reserved %s: %v, want forbidden", name, err)
		}
	}
	read := func(name string) string {
		t.Helper()
		out, err := computeclient.ExecRunTarget(ctx, ana, "box", idOf(h), computeapi.ExecRunTargetRequest{
			Argv: []string{"sh", "-c", `cat "$POTLUCK_SECRETS/` + name + `" 2>/dev/null || printf absent`}})
		if err != nil || out.Exit != 0 {
			t.Fatalf("reading %s: %+v %v", name, out, err)
		}
		return out.Stdout
	}
	if got := read(protocol.SecretNameReady); got != "absent" {
		t.Fatalf("a refused holder call wrote the marker: %q", got)
	}

	// The door writes both.
	if err := door.BoxSecret(ctx, p.ID, h, "GH_TOKEN", secrets.NewValue("ghs_door_canary")); err != nil {
		t.Fatalf("door GH_TOKEN: %v", err)
	}
	if err := door.BoxSecret(ctx, p.ID, h, protocol.SecretNameReady, secrets.NewValue("4242")); err != nil {
		t.Fatalf("door marker: %v", err)
	}
	if got := read("GH_TOKEN"); got != "ghs_door_canary" {
		t.Errorf("GH_TOKEN in the box: %q", got)
	}
	if got := read(protocol.SecretNameReady); got != "4242" {
		t.Errorf("the marker in the box: %q", got)
	}
	if err := door.BoxSecret(ctx, p.ID, h, "bad name", secrets.NewValue("x")); code(err) != api.CodeBadRequest {
		t.Errorf("a bad name: %v", err)
	}
	if live, err := door.BoxLive(ctx, p.ID, h); !live || err != nil {
		t.Errorf("BoxLive of a ready box: %v %v", live, err)
	}
	if c, err := door.BoxClass(ctx, p.ID, h); c != api.MachineTrusted || err != nil {
		t.Errorf("BoxClass of a box of this machine: %q %v", c, err)
	}

	// Not ready, past its expiry: refused, nothing written.
	set := func(col, val string) {
		t.Helper()
		if _, err := raw.Exec(`UPDATE run_handles SET `+col+` = ? WHERE handle = ?`, val, h); err != nil {
			t.Fatal(err)
		}
	}
	set("state", "acquiring")
	if err := door.BoxSecret(ctx, p.ID, h, "GH_TOKEN", secrets.NewValue("not-ready")); code(err) != api.CodeConflict {
		t.Errorf("a box not ready: %v, want conflict", err)
	}
	set("state", "ready")
	var exp string
	if err := raw.QueryRow(`SELECT expires_at FROM run_handles WHERE handle = ?`, h).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	set("expires_at", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	if err := door.BoxSecret(ctx, p.ID, h, "GH_TOKEN", secrets.NewValue("expired")); code(err) != api.CodeConflict {
		t.Errorf("a box past its expiry: %v, want conflict", err)
	}
	set("expires_at", exp)
	if got := read("GH_TOKEN"); got != "ghs_door_canary" {
		t.Errorf("a refused door call changed GH_TOKEN: %q", got)
	}

	// A driver without box-secrets: refused, and not a maybe-delivered.
	nb := acquire("nobox", "door-2")
	if err := door.BoxSecret(ctx, p.ID, nb, "GH_TOKEN", secrets.NewValue("x")); code(err) != api.CodeBadRequest || errors.Is(err, rt.ErrMaybeDelivered) {
		t.Errorf("a driver without box-secrets: %v, want bad_request", err)
	}

	// Unknown handle, another project: not found and not live.
	for _, c := range [][2]string{{p.ID, "box:box/nope"}, {"01NOPROJECT0000000000000000", h}, {p.ID, ""}} {
		if err := door.BoxSecret(ctx, c[0], c[1], "GH_TOKEN", secrets.NewValue("x")); err == nil {
			t.Errorf("BoxSecret %v: written", c)
		}
		if live, _ := door.BoxLive(ctx, c[0], c[1]); live {
			t.Errorf("BoxLive %v: live", c)
		}
	}

	// Ended: not live, refused.
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box", idOf(h)); err != nil {
		t.Fatal(err)
	}
	if live, err := door.BoxLive(ctx, p.ID, h); live || err != nil {
		t.Errorf("BoxLive after release: %v %v", live, err)
	}
	if err := door.BoxSecret(ctx, p.ID, h, "GH_TOKEN", secrets.NewValue("x")); code(err) != api.CodeConflict {
		t.Errorf("a released box: %v, want conflict", err)
	}
}

// A box on another machine whose class the authority cannot read as
// trusted (no team link: public) is given nothing by the door either.
func TestDoorRefusesAPublicHost(t *testing.T) {
	rp := newRelayPair(t)
	h := rp.acquire(t, "door-pub", 120)
	ctx := context.Background()
	if c, err := rp.svc.BoxClass(ctx, rp.project, h.Handle); c != api.MachinePublic || err != nil {
		t.Errorf("BoxClass of a box on a public host: %q %v", c, err)
	}
	if live, err := rp.svc.BoxLive(ctx, rp.project, h.Handle); !live || err != nil {
		t.Errorf("BoxLive: %v %v", live, err)
	}
	err := rp.svc.BoxSecret(ctx, rp.project, h.Handle, "GH_TOKEN", secrets.NewValue("ghs_public_canary"))
	if code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "public host") || strings.Contains(err.Error(), "ghs_public_canary") {
		t.Errorf("a public host: %v, want forbidden", err)
	}
}
