package tunnel_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/wiring"
)

// authority is a daemon whose tunnel door serves on a loopback HTTP server
// at the URL its config names, with identity ci holding member on orchid.
type authority struct {
	dm  *server.Daemon
	svc *tunnel.Service
	srv *httptest.Server
}

func newAuthority(t *testing.T) *authority {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	cfg.File.Tunnel.URL = "http://" + srv.Listener.Addr().String()
	ctx := context.Background()
	dm, err := server.New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	svc := tunnel.From(dm.Deps)
	srv.Config.Handler = svc.Handler(dm.Handler)
	srv.Start()
	t.Cleanup(srv.Close)
	reg := dm.Deps.Projects
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	return &authority{dm: dm, svc: svc, srv: srv}
}

// mint is a box's credential: a token for ci bound to a new key (boxKey
// false: a plain minted token), naming this authority.
func (a *authority) mint(t *testing.T, boxKey bool) tunnel.BoxCredential {
	t.Helper()
	key, err := tunnel.NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	req := api.MintTokenRequest{As: "ci", Project: "orchid", Role: "member", TTLSeconds: 600, NoGrant: true}
	if boxKey {
		req.BoxKey = tunnel.BoxKeyID(key)
	}
	reg := a.dm.Deps.Projects
	out, err := reg.Mint(context.Background(), reg.Local(""), req)
	if err != nil {
		t.Fatal(err)
	}
	url, fp, err := a.svc.Home()
	if err != nil {
		t.Fatal(err)
	}
	return tunnel.BoxCredential{Token: out.Secret, Key: key, URL: url, Home: fp}
}

// A box reaches its authority with its credential file alone: no daemon,
// no other file. The credential's token works only signed by its own key;
// a plain bearer token, another key, a revoked token, an authority with
// another key and the Unix socket are each refused.
func TestBoxReachesItsAuthority(t *testing.T) {
	ctx := context.Background()
	a := newAuthority(t)
	cred := a.mint(t, true)
	enc, err := cred.Encode()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "POTLUCK_TOKEN")
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tunnel.ReadBoxCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tunnel.BoxClient(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := c.Whoami(ctx)
	if err != nil {
		t.Fatalf("the box's whoami: %v", err)
	}
	if w.Identity.Name != "ci" || w.Via != "token" {
		t.Fatalf("the box is %+v, want ci by token", w)
	}

	unauth := func(name string, c *client.Client) {
		t.Helper()
		if _, err := c.Whoami(ctx); !api.IsCode(err, api.CodeUnauthenticated) {
			t.Errorf("%s: %v, want unauthenticated", name, err)
		}
	}
	other := cred
	other.Key, _ = tunnel.NewBoxKey()
	oc, _ := tunnel.BoxClient(other)
	unauth("its token signed by another key", oc)

	bearer := a.mint(t, false)
	bc, _ := tunnel.BoxClient(bearer)
	unauth("a plain minted token", bc)

	sock := client.WithHandler(a.dm.SocketHandler())
	sock.Token = cred.Token
	unauth("its token on the socket", sock)

	wrong := cred
	wrong.Home = strings.Repeat("a", 26)
	wc, _ := tunnel.BoxClient(wrong)
	if _, err := wc.Whoami(ctx); !errors.Is(err, tunnel.ErrHomeKey) {
		t.Errorf("an authority with another key: %v, want ErrHomeKey", err)
	}

	toks, err := a.dm.Deps.Projects.Tokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range toks {
		if _, err := a.dm.Deps.Projects.RevokeToken(ctx, tok.ID); err != nil {
			t.Fatal(err)
		}
	}
	unauth("a revoked token", c)
}

// A credential file round-trips; anything else is refused without quoting
// what it read.
func TestBoxCredentialFile(t *testing.T) {
	key, _ := tunnel.NewBoxKey()
	tok := "potluck_" + strings.Repeat("a", 52)
	c := tunnel.BoxCredential{Token: tok, Key: key, URL: "https://authority.example", Home: strings.Repeat("b", 26)}
	enc, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := tunnel.ParseBoxCredential(enc)
	if err != nil || back.Token != tok || !back.Key.Equal(key) || back.URL != c.URL || back.Home != c.Home {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	bad := map[string]string{
		"empty":       "",
		"not json":    tok,
		"other kind":  strings.Replace(enc, tunnel.BoxCredentialKind, "potluck-box/2", 1),
		"extra field": strings.Replace(enc, `{`, `{"extra":1,`, 1),
		"two objects": enc + enc,
		"http url":    strings.Replace(enc, "https://authority.example", "http://authority.example", 1),
		"url path":    strings.Replace(enc, "https://authority.example", "https://authority.example/x", 1),
		"short key":   strings.Replace(enc, `"key":"`, `"key":"00`, 1),
		"no token":    strings.Replace(enc, tok, "", 1),
	}
	for name, s := range bad {
		if _, err := tunnel.ParseBoxCredential(s); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), tok) || (len(s) > 0 && strings.Contains(err.Error(), s)) {
			t.Errorf("%s: the error quotes the file: %v", name, err)
		}
	}
	if _, err := (tunnel.BoxCredential{Token: tok, URL: c.URL, Home: c.Home}).Encode(); err == nil {
		t.Error("a credential with no key encoded")
	}
}
