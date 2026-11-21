package server_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

// TestUnlimitedTokenGolden walks every route of the kit's daemon with a
// token that has no limit, over a WhoIs listener and over the tunnel's
// shape, and with none on the socket, and compares each answer (status and
// body, with ids, times and paths masked) with testdata/unlimited_walk.txt.
// The file was written by this same test file run on origin/main at
// df74e20, the commit before route limits, so a change in what an unlimited
// caller gets fails here. Not covered: POST /v0/daemon/stop (it stops the
// daemon) and the /tailnet/ and join routes (they reach for a tailnet or
// the network). POTLUCK_UPDATE_GOLDEN=1 rewrites the file; rewrite it only
// for a change meant to alter these answers.
func TestUnlimitedTokenGolden(t *testing.T) {
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "owner", "host"
	dm, err := New(context.Background(), cfg, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	ctx := context.Background()
	sock := client.WithHandler(dm.SocketHandler())
	if _, err := sock.CreateProject(ctx, api.NewProjectRequest{Slug: "proj"}); err != nil {
		t.Fatal(err)
	}
	owner, err := sock.MintToken(ctx, api.MintTokenRequest{As: "helper", Project: "proj", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := sock.MintToken(ctx, api.MintTokenRequest{As: "visitor", Project: "proj", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	// The tunnel key is made fresh for each home: its fingerprint, which the
	// term names to a tunnel caller, is masked by value.
	tunnelKey, err := dm.Deps.HomeFingerprint(ctx, core.ViaTunnel)
	if err != nil {
		t.Fatal(err)
	}
	mask := masker(cfg.Home, map[string]string{owner.Secret: "<secret>", guest.Secret: "<secret>", tunnelKey: "<tunnel-key>"})
	callers := []struct {
		name   string
		secret string
		rm     *core.Remote
		local  bool
	}{
		{"owner-token", owner.Secret, &core.Remote{Login: "x@example.com", NodeID: "nDEV", NodeKey: "nodekey:dev"}, false},
		{"guest-token", guest.Secret, &core.Remote{NodeID: "nDEV", NodeKey: "nodekey:dev"}, false},
		{"token-no-whois", owner.Secret, nil, false},
		{"token-tunnel", owner.Secret, &core.Remote{Via: core.ViaTunnel, NodeID: "zmkey:x"}, false},
		{"bad-token", "potluck_" + strings.Repeat("a", 52), &core.Remote{NodeID: "nDEV", NodeKey: "nodekey:dev"}, false},
		{"socket-token", owner.Secret, nil, true},
	}
	var b strings.Builder
	for _, rt := range dm.Router.Routes() {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		if path == "/v0/daemon/stop" || path == "/v0/authority/freeze" || strings.Contains(path, "/tailnet/") || strings.Contains(path, "/join") {
			continue // stops the daemon (a freeze parks its home too); or reaches for a tailnet or the network
		}
		path = goldenPath(path)
		for _, c := range callers {
			rctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			req := httptest.NewRequestWithContext(rctx, method, path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(api.HeaderAuthorization, "Bearer "+c.secret)
			if c.rm != nil {
				req = req.WithContext(core.WithRemote(req.Context(), *c.rm))
			}
			w := httptest.NewRecorder()
			if c.local {
				dm.SocketHandler().ServeHTTP(w, req)
			} else {
				dm.Handler.ServeHTTP(w, req)
			}
			cancel()
			fmt.Fprintf(&b, "%s %s [%s] %d %s\n", method, path, c.name, w.Code, mask(strings.TrimSpace(w.Body.String())))
		}
	}
	golden := filepath.Join("testdata", "unlimited_walk.txt")
	if os.Getenv("POTLUCK_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Errorf("line %d\n got %s\nwant %s", i+1, g, w)
			}
		}
	}
}

func goldenPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if strings.HasPrefix(s, "{") {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

// masker hides what differs between runs: ULIDs, times, numbers that count
// time, the home path, and each value in made (secrets, keys) by its mask.
func masker(home string, made map[string]string) func(string) string {
	ulid := regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)
	ts := regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)
	nums := regexp.MustCompile(`"(pid|uptime_s|uptime|age_s|ms|dur_ms|version|commit)":("[^"]*"|-?\d+(\.\d+)?)`)
	return func(s string) string {
		s = strings.ReplaceAll(s, home, "<home>")
		if r, err := filepath.EvalSymlinks(home); err == nil {
			s = strings.ReplaceAll(s, r, "<home>")
		}
		for x, m := range made {
			s = strings.ReplaceAll(s, x, m)
		}
		s = ulid.ReplaceAllString(s, "<id>")
		s = ts.ReplaceAllString(s, "<time>")
		return nums.ReplaceAllString(s, `"$1":<n>`)
	}
}
