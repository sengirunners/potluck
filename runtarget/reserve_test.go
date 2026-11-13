package runtarget_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// MUST 17 of the secrets door: an issuer's grant may not take a name a live
// box's holder set a value under (that value would stay in the box, out of
// the holder's reach, taken for the authority's own): the add is refused
// until the holder unsets it, and then the name is reserved (the holder can
// no longer set it). A grant for another area is not refused; neither is
// one for a name only an ended box held.
func TestIssuerGrantRefusedOverAHolderValue(t *testing.T) {
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
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPath := filepath.Join(home, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.File.Secrets = config.Secrets{Issuers: map[string]config.SecretsIssuer{"gh": {Kind: issuers.KindGitHubApp, AppID: 42,
		PrivateKey: "file:" + keyPath, APIURL: "http://127.0.0.1:1",
		Ceiling: []config.SecretsCeiling{{Repo: "acme/app", Permissions: map[string]string{"contents": "write"}}}}}}
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
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
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
	b := computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
		Config: map[string]string{"remote": repo.Remote}}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.AddSecretGrant(ctx, owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Role: "member"},
		Kind: computeapi.SecretKindValue, Name: "*", Scopes: []string{computeapi.SecretScopeBox}}); err != nil {
		t.Fatal(err)
	}
	acquire := func(req string) string {
		h, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: req, TTLS: 120})
		if err != nil {
			t.Fatal(err)
		}
		return idOf(h.Handle)
	}
	set := func(id string, body any) error {
		return ana.Do(ctx, "POST", "/v0/run_targets/box/handles/"+id+"/secrets", body, nil)
	}
	grant := func(name, area string) error {
		_, err := computeclient.AddSecretGrant(ctx, owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Role: "member"}, Area: area,
			Kind: computeapi.SecretKindGitHubToken, Name: name, Scopes: []string{computeapi.SecretScopeBox},
			Ceiling: json.RawMessage(`{"repos":["acme/app"],"permissions":{"contents":"write"}}`)})
		return err
	}

	id := acquire("r1")
	if err := set(id, map[string]any{"secrets": map[string]string{"GH_TOKEN": "holder-value", "OTHER": "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := grant("GH_TOKEN", ""); code(err) != api.CodeConflict {
		t.Fatalf("an issuer's grant over a holder's live value: %v, want conflict", err)
	}
	if err := set(id, map[string]any{"unset": []string{"GH_TOKEN"}}); err != nil {
		t.Fatalf("the holder unsets it: %v", err)
	}
	if err := grant("GH_TOKEN", ""); err != nil {
		t.Fatalf("the grant once the value is gone: %v", err)
	}
	if err := set(id, map[string]any{"secrets": map[string]string{"GH_TOKEN": "again"}}); code(err) != api.CodeForbidden {
		t.Fatalf("the holder sets a reserved name: %v, want forbidden", err)
	}
	// A refused set records nothing: a later grant for another name is not
	// held up by it, and one for OTHER (still set) is.
	if err := grant("OTHER", ""); code(err) != api.CodeConflict {
		t.Fatalf("a grant over OTHER, still set: %v, want conflict", err)
	}
	// The box ends: its names no longer hold anything up.
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box", id); err != nil {
		t.Fatal(err)
	}
	if err := grant("OTHER", ""); err != nil {
		t.Fatalf("a grant over an ended box's name: %v", err)
	}
}
