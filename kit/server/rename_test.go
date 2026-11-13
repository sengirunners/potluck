package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

// stepOneReads is how a step 1 build (potluck 358a1e8c) reads an answer's
// protocol: Potluck-Protocol first, then Zm-Protocol, the same major as
// zm/v0 (potluck/1 counts as one). This build reads the same way.
func stepOneReads(h http.Header) bool {
	return api.SameProtocol(api.GetHeader(h, api.HeaderProtocol), "zm/v0")
}

// A daemon of this build (rename step 2) against a step 1 caller, which
// sends only the old names (Zm-*, zm/v0, a zm_ token), and against a caller
// of this build (Potluck-*, potluck/1, a potluck_ token). Both are read; the
// answer is Potluck-Protocol: potluck/1, which the step 1 reading rules take.
func TestStepTwoDaemonInterop(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dm, err := New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	}()
	owner := client.NewUnix(cfg.SocketPath())
	if err := owner.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	// What this build mints is potluck_.
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "victor", Project: "side", Role: "guest"})
	if err != nil || !strings.HasPrefix(tok.Secret, "potluck_") || !authz.LooksLikeToken(tok.Secret) {
		t.Fatalf("mint: %q %v", tok.Secret, err)
	}
	// A token a step 1 daemon minted (zm_): the registry holds the hash of
	// its zm_ form, as it would after an upgrade.
	tok2, err := owner.MintToken(ctx, api.MintTokenRequest{As: "nina", Project: "side", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	old := "zm_" + strings.TrimPrefix(tok2.Secret, authz.TokenPrefix)
	if _, err := dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE tokens SET hash = ? WHERE id = ?`, authz.HashToken(old), tok2.Token.ID); err != nil {
		t.Fatal(err)
	}

	hc := clientHTTP(cfg.SocketPath())
	call := func(path string, h map[string]string, out any) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://zm"+path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if out != nil && resp.StatusCode == 200 {
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatalf("%s: %v %s", path, err, b)
			}
		}
		return resp
	}
	var w api.Whoami
	// A step 1 caller: old names only, its zm_ token.
	step1 := map[string]string{"Zm-Protocol": "zm/v0", "Authorization": "Bearer " + old}
	resp := call("/v0/whoami", step1, &w)
	if resp.StatusCode != 200 || w.Identity.Name != "nina" || w.Via != authz.ViaToken || w.Protocol != "potluck/1" {
		t.Fatalf("step 1 caller: %d %+v", resp.StatusCode, w)
	}
	if resp.Header.Get("Potluck-Protocol") != "potluck/1" || resp.Header.Get("Zm-Protocol") != "" || !stepOneReads(resp.Header) {
		t.Fatalf("the answer's protocol header: %v", resp.Header)
	}
	// A caller of this build: new names, a potluck_ token.
	step2 := map[string]string{"Potluck-Protocol": "potluck/1", "Authorization": "Bearer " + tok.Secret}
	resp = call("/v0/whoami", step2, &w)
	if resp.StatusCode != 200 || w.Identity.Name != "victor" || w.Via != authz.ViaToken {
		t.Fatalf("step 2 caller: %d %+v", resp.StatusCode, w)
	}
	var hl api.Health
	if resp := call("/v0/health", step1, &hl); resp.StatusCode != 200 || hl.Protocol != "potluck/1" || !api.SameProtocol(hl.Protocol, "zm/v0") {
		t.Fatalf("health: %d %+v", resp.StatusCode, hl)
	}

	// Projects and addresses under either name; the new name first.
	var res api.Resolution
	for _, h := range []map[string]string{
		{"Zm-Protocol": "zm/v0", "Zm-Project": "side", "Zm-Project-From": "env"},
		{"Potluck-Protocol": "potluck/1", "Potluck-Project": "side", "Potluck-Project-From": "env"},
	} {
		resp = call("/v0/projects/resolve", h, &res)
		if resp.StatusCode != 200 || res.Project == nil || res.Project.Slug != "side" || res.Rule != "env" {
			t.Fatalf("resolve %v: %d %+v", h, resp.StatusCode, res)
		}
	}
	resp = call("/v0/projects/resolve", map[string]string{"Potluck-Project": "side", "Zm-Project": "nope"}, &res)
	if resp.StatusCode != 200 || res.Project == nil || res.Project.Slug != "side" {
		t.Fatalf("Potluck-Project beside Zm-Project: %d %+v", resp.StatusCode, res)
	}
	for _, k := range []string{"Zm-As", "Potluck-As"} {
		resp = call("/v0/whoami", map[string]string{k: "mini/abcd1234"}, &w)
		if resp.StatusCode != 200 || w.Session != "mini/abcd1234" {
			t.Fatalf("%s: %d %+v", k, resp.StatusCode, w)
		}
		if resp := call("/v0/whoami", map[string]string{k: "other/abcd1234"}, nil); resp.StatusCode != 403 {
			t.Fatalf("a foreign %s: %d", k, resp.StatusCode)
		}
	}

	// Protocols: another major is refused under either name.
	for _, c := range []struct {
		h    map[string]string
		want int
	}{
		{map[string]string{"Potluck-Protocol": "potluck/2"}, 400},
		{map[string]string{"Zm-Protocol": "zm/v9"}, 400},
		{map[string]string{"Zm-Protocol": "zm/v0"}, 200},
		{map[string]string{"Potluck-Protocol": "potluck/1"}, 200},
		{map[string]string{"Potluck-Protocol": "potluck/1", "Zm-Protocol": "zm/v9"}, 200},
		{map[string]string{"Potluck-Protocol": "zm/v9", "Zm-Protocol": "zm/v0"}, 400},
	} {
		if resp := call("/v0/health", c.h, nil); resp.StatusCode != c.want {
			t.Errorf("%v: %d, want %d", c.h, resp.StatusCode, c.want)
		}
	}

	// The client of this build (Potluck-* names) works end to end.
	c := client.NewUnix(cfg.SocketPath())
	c.Token = tok.Secret
	if who, err := c.Whoami(ctx); err != nil || who.Identity.Name != "victor" {
		t.Fatalf("client: %+v %v", who, err)
	}
}
