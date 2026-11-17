package cli

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// Inside a box, with POTLUCK_BOX_CREDENTIAL naming the credential its
// potluck-token delivered, a command talks to the authority over its
// tunnel: no daemon in the box, no home there. A file that is not a
// credential fails the command, naming the variable.
func TestCommandFromABox(t *testing.T) {
	ctx := context.Background()
	a := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	a.ok(t, "create", "--via", "tunnel", "orchid")
	svc := tunnel.From(a.dm.Deps)
	srv := httptest.NewServer(svc.Handler(a.dm.Handler))
	t.Cleanup(srv.Close)
	fp, err := svc.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	reg := a.dm.Deps.Projects
	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	key, _ := tunnel.NewBoxKey()
	out, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "ci", Project: "orchid", Role: "member", TTLSeconds: 600, NoGrant: true, BoxKey: tunnel.BoxKeyID(key)})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := tunnel.BoxCredential{Token: out.Secret, Key: key, URL: srv.URL, Home: fp}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "POTLUCK_TOKEN")
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		t.Fatal(err)
	}
	box := &machine{name: "box", home: filepath.Join(dir, "no-home")} // no daemon, no home
	t.Setenv(EnvBoxCredential, path)
	var st struct {
		Pools []struct {
			Pool string `json:"pool"`
			Role string `json:"role"`
		} `json:"pools"`
	}
	if err := json.Unmarshal([]byte(box.ok(t, "status", "--json")), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Pools) != 1 || st.Pools[0].Pool != "orchid" || st.Pools[0].Role != "member" {
		t.Fatalf("status from the box: %+v, want orchid as member", st.Pools)
	}

	if err := os.WriteFile(path, []byte(out.Secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, e, code := box.run(t, "status"); code == 0 || !strings.Contains(e, EnvBoxCredential) || strings.Contains(e, out.Secret) {
		t.Fatalf("a bare token as the credential: exit %d, %q", code, e)
	}
}
