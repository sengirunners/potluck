package conformance

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/wiring"
)

// boxClientArg runs this test binary as the client in a box: it calls the
// authority $POTLUCK_BOX_CREDENTIAL names and prints who it is there.
const boxClientArg = "conformance-box-client"

func boxClientMain() int {
	c, err := tunnel.ReadBoxCredential(os.Getenv("POTLUCK_BOX_CREDENTIAL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cl, err := tunnel.BoxClient(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w, err := cl.Whoami(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("identity " + w.Identity.Name)
	return 0
}

// testAuthority is a daemon (wiring.Kit) whose tunnel door serves on a
// loopback HTTP server, and a box credential for identity ci, member of
// orchid, bound to a key of its own: one per test process.
var testAuthority = sync.OnceValue(func() *BoxAuthority {
	home, err := os.MkdirTemp("", "zmba")
	if err != nil {
		panic(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		panic(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	cfg.File.Tunnel.URL = "http://" + srv.Listener.Addr().String()
	ctx := context.Background()
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wiring.Kit)
	if err != nil {
		panic(err)
	}
	svc := tunnel.From(dm.Deps)
	srv.Config.Handler = svc.Handler(dm.Handler)
	srv.Start()
	reg := dm.Deps.Projects
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "orchid"}); err != nil {
		panic(err)
	}
	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
		panic(err)
	}
	key, err := tunnel.NewBoxKey()
	if err != nil {
		panic(err)
	}
	out, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "ci", Project: "orchid", Role: "member", TTLSeconds: 3600, NoGrant: true, BoxKey: tunnel.BoxKeyID(key)})
	if err != nil {
		panic(err)
	}
	url, fp, err := svc.Home()
	if err != nil {
		panic(err)
	}
	cred, err := tunnel.BoxCredential{Token: out.Secret, Key: key, URL: url, Home: fp}.Encode()
	if err != nil {
		panic(err)
	}
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return &BoxAuthority{Credential: cred, Client: []string{self, boxClientArg}, Want: "identity ci"}
})

// withAuthority runs group box-authority alone, against testAuthority.
func withAuthority(o *Options) {
	o.BoxAuthority = testAuthority()
	o.Only = []string{"box-authority"}
}
