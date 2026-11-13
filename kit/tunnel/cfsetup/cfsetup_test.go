package cfsetup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// fakeCloudflared writes a shell script that answers cloudflared's
// subcommands the way setup uses them, keeping its state in dir: the
// tunnels by name, the DNS record, every call. Failure switches are files.
func fakeCloudflared(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
D='` + dir + `'
echo "$*" >> "$D/calls"
sub=""; name=""; prev=""; cred=""
for a in "$@"; do
  case "$prev" in
    --credentials-file|--cred-file) cred="$a" ;;
    --name) name="$a" ;;
  esac
  case "$a" in
    login|list|create|token|route|delete|run) [ -z "$sub" ] && sub="$a" ;;
  esac
  prev="$a"
done
last=""; for a in "$@"; do last="$a"; done
case "$sub" in
login)
  [ -f "$D/login_fail" ] && { echo "login failed" >&2; exit 1; }
  echo "Please open the following URL and log in with your Cloudflare account:" >&2
  echo "" >&2
  echo "https://dash.cloudflare.com/argotunnel?aud=&callback=https%3A%2F%2Flogin.cloudflareaccess.org%2Fx" >&2
  mkdir -p "$HOME/.cloudflared" && echo cert > "$HOME/.cloudflared/cert.pem"
  echo "You have successfully logged in." >&2 ;;
list)
  if [ -f "$D/tunnels/$name" ]; then printf '[{"id":"%s","name":"%s","deleted_at":"0001-01-01T00:00:00Z"}]\n' "$(cat "$D/tunnels/$name")" "$name"; else echo '[]'; fi ;;
create)
  [ -f "$D/create_fail" ] && { echo "create failed" >&2; exit 1; }
  mkdir -p "$D/tunnels"; echo 6f1d2c3b-aaaa-bbbb-cccc-000000000001 > "$D/tunnels/$last"
  echo '{"AccountTag":"x","TunnelID":"6f1d2c3b-aaaa-bbbb-cccc-000000000001"}' > "$cred"
  # The shape real cloudflared 2026 prints: indented, nested brackets, a token.
  printf '{\n  "id": "6f1d2c3b-aaaa-bbbb-cccc-000000000001",\n  "name": "%s",\n  "created_at": "2026-11-02T21:21:34Z",\n  "deleted_at": "0001-01-01T00:00:00Z",\n  "connections": [],\n  "token": "secret-tunnel-token"\n}\n' "$last" ;;
token)
  echo '{"TunnelID":"from-token"}' > "$cred" ;;
route)
  [ -f "$D/dns_conflict" ] && { echo "Failed to add route: code: 1003, reason: An A, AAAA, or CNAME record with that host already exists." >&2; exit 1; }
  echo "$last" > "$D/dns" ;;
delete)
  for f in "$D"/tunnels/*; do [ -f "$f" ] && [ "$(cat "$f")" = "$last" ] && rm "$f"; done
  echo "$last" >> "$D/deleted" ;;
*) echo "fake cloudflared: $*" >&2; exit 2 ;;
esac
`
	p := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type fakeSup struct {
	mu        sync.Mutex
	dir       string
	installed map[string]Unit
	restarts  int
	fail      bool
}

func (f *fakeSup) Name() string       { return "fake" }
func (f *fakeSup) Path(u Unit) string { return filepath.Join(f.dir, u.Label+".unit") }
func (f *fakeSup) Install(_ context.Context, u Unit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("no service manager")
	}
	f.installed[u.Label] = u
	return os.WriteFile(f.Path(u), []byte(strings.Join(u.Args, " ")), 0o600)
}
func (f *fakeSup) Restart(_ context.Context, u Unit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return nil
}
func (f *fakeSup) Remove(_ context.Context, u Unit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.installed, u.Label)
	os.Remove(f.Path(u))
	return nil
}

type fakeZm struct {
	mu   sync.Mutex
	st   api.TunnelStatus
	ups  []api.TunnelUpRequest
	down int
}

func (f *fakeZm) Tunnel(context.Context) (api.TunnelStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, nil
}
func (f *fakeZm) TunnelUp(_ context.Context, r api.TunnelUpRequest) (api.TunnelStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ups = append(f.ups, r)
	now := time.Now()
	f.st.Running, f.st.Listen, f.st.URL, f.st.EdgeSecret, f.st.EdgeSeen = true, r.Listen, r.URL, r.EdgeSecret != "", &now
	return f.st, nil
}
func (f *fakeZm) TunnelDown(context.Context) (api.TunnelStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down++
	f.st = api.TunnelStatus{Fingerprint: f.st.Fingerprint}
	return f.st, nil
}

type rig struct {
	s     *Setup
	cfDir string
	sup   *fakeSup
	zm    *fakeZm
	out   *bytes.Buffer
	key   string // this home's Zm-Home-Key
}

func newRig(t *testing.T) *rig {
	t.Helper()
	home := t.TempDir()
	cfDir := t.TempDir()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	r := &rig{cfDir: cfDir, out: &bytes.Buffer{}, sup: &fakeSup{dir: t.TempDir(), installed: map[string]Unit{}},
		zm: &fakeZm{st: api.TunnelStatus{Fingerprint: tunnel.Fingerprint(pub)}}, key: base64.RawURLEncoding.EncodeToString(pub)}
	cf := fakeCloudflared(t, cfDir)
	r.s = &Setup{ZmHome: home, Hostname: "zm.example.com", CF: ExecRunner{Path: cf}, CFPath: cf, Sup: r.sup, Zm: r.zm, Out: r.out,
		VerifyFor: time.Second, Probe: func(context.Context, string, string) (Answer, error) {
			return Answer{Status: 401, HomeKey: r.key, Signed: true}, nil
		}}
	return r
}

func (r *rig) calls(t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(r.cfDir, "calls"))
	return string(b)
}

func (r *rig) flag(t *testing.T, name string, on bool) {
	p := filepath.Join(r.cfDir, name)
	if on {
		os.WriteFile(p, nil, 0o600)
	} else {
		os.Remove(p)
	}
}

func TestSetupFromNothing(t *testing.T) {
	r := newRig(t)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatalf("setup: %v\n%s", err, r.out)
	}
	calls := r.calls(t)
	for _, want := range []string{"tunnel login", "list --name potluck-zm-example-com", "list --name zm-zm-example-com", "create --credentials-file", "--output json potluck-zm-example-com", "route dns 6f1d2c3b-aaaa-bbbb-cccc-000000000001 zm.example.com"} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in calls:\n%s", want, calls)
		}
	}
	// The login URL stands on its own line, for a headless box.
	if !strings.Contains(r.out.String(), "\n    https://dash.cloudflare.com/argotunnel?") {
		t.Errorf("login URL not shown:\n%s", r.out)
	}
	// Nothing under ~/.cloudflared: the certificate is in the setup's dir.
	cert := filepath.Join(Dir(r.s.ZmHome), "home", ".cloudflared", "cert.pem")
	if fi, err := os.Stat(cert); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cert %v %v", fi, err)
	}
	st, _ := LoadState(r.s.ZmHome)
	if st.TunnelID == "" || !st.Created || !st.Routed || len(st.EdgeSecret) != 32 || st.Listen != DefaultListen {
		t.Fatalf("state %+v", st)
	}
	cfg, _ := os.ReadFile(filepath.Join(Dir(r.s.ZmHome), "config.yml"))
	for _, want := range []string{"tunnel: " + st.TunnelID, "hostname: zm.example.com", "service: http://127.0.0.1:7435",
		"httpHostHeader: potluck-edge-" + st.EdgeSecret, "service: http_status:404"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config.yml lacks %q:\n%s", want, cfg)
		}
	}
	if len(r.zm.ups) != 1 || r.zm.ups[0].URL != "https://zm.example.com" || r.zm.ups[0].EdgeSecret != st.EdgeSecret || r.zm.ups[0].Listen != DefaultListen {
		t.Fatalf("zm up %+v", r.zm.ups)
	}
	u, ok := r.sup.installed[Label(r.s.ZmHome)]
	if !ok || strings.Join(u.Args[1:], " ") != "--no-autoupdate tunnel --config "+filepath.Join(Dir(r.s.ZmHome), "config.yml")+" run" {
		t.Fatalf("service %+v", r.sup.installed)
	}
	if !strings.Contains(r.out.String(), "zm invite <project> --as <name> --role member --via tunnel") {
		t.Fatalf("no invite hint:\n%s", r.out)
	}
}

// A second run changes nothing in the account: no login, no new tunnel, the
// same edge secret.
func TestSetupRerunIsIdempotent(t *testing.T) {
	r := newRig(t)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st1, _ := LoadState(r.s.ZmHome)
	os.Remove(filepath.Join(r.cfDir, "calls"))
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatalf("again: %v\n%s", err, r.out)
	}
	calls := r.calls(t)
	if strings.Contains(calls, "login") || strings.Contains(calls, "create") || strings.Contains(calls, "token") {
		t.Fatalf("a re-run logged in or created:\n%s", calls)
	}
	st2, _ := LoadState(r.s.ZmHome)
	if st2.TunnelID != st1.TunnelID || st2.EdgeSecret != st1.EdgeSecret || !st2.Created {
		t.Fatalf("state moved: %+v -> %+v", st1, st2)
	}
}

// A re-run restarts the running cloudflared, so a rewritten ingress (a
// runners.listen route) is live, not only on disk; a first run does not.
func TestSetupRerunRestartsCloudflared(t *testing.T) {
	r := newRig(t)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.sup.restarts != 0 {
		t.Fatalf("a first run restarted cloudflared %d times", r.sup.restarts)
	}
	r.s.Routes = []Route{{Path: `^/v0/runners/hook$`, Listen: "127.0.0.1:7440"}}
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatalf("again: %v\n%s", err, r.out)
	}
	if r.sup.restarts != 1 {
		t.Fatalf("a re-run restarted cloudflared %d times, want 1", r.sup.restarts)
	}
	if b, _ := os.ReadFile(r.s.config()); !strings.Contains(string(b), "v0/runners/hook") {
		t.Fatalf("the re-run's config lacks the route:\n%s", b)
	}
}

// A tunnel of that name made elsewhere is reused: its credentials are
// fetched, nothing is created.
func TestSetupReusesAnExistingTunnel(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.cfDir, "tunnels"), 0o700)
	os.WriteFile(filepath.Join(r.cfDir, "tunnels", "zm-zm-example-com"), []byte("existing-id\n"), 0o600)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatalf("setup: %v\n%s", err, r.out)
	}
	calls := r.calls(t)
	if strings.Contains(calls, " create ") || !strings.Contains(calls, "token --cred-file") {
		t.Fatalf("calls:\n%s", calls)
	}
	if st, _ := LoadState(r.s.ZmHome); st.TunnelID != "existing-id" || st.Created {
		t.Fatalf("state %+v", st)
	}
}

// A hostname that already has another record: refused, and the tunnel this
// run created is deleted; nothing is left behind.
func TestSetupRollsBackOnDNSConflict(t *testing.T) {
	r := newRig(t)
	r.flag(t, "dns_conflict", true)
	err := r.s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already has a DNS record") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(r.calls(t), "delete -f 6f1d2c3b-aaaa-bbbb-cccc-000000000001") {
		t.Fatalf("the created tunnel was not deleted:\n%s", r.calls(t))
	}
	if _, err := os.Stat(filepath.Join(Dir(r.s.ZmHome), "state.json")); !os.IsNotExist(err) {
		t.Fatal("state left behind")
	}
	if len(r.sup.installed) != 0 || len(r.zm.ups) != 0 {
		t.Fatalf("local half made: %+v %+v", r.sup.installed, r.zm.ups)
	}
	// The login stays: a re-run after the fix does not log in again.
	r.flag(t, "dns_conflict", false)
	os.Remove(filepath.Join(r.cfDir, "calls"))
	if err := r.s.Run(context.Background()); err != nil || strings.Contains(r.calls(t), "login") {
		t.Fatalf("re-run: %v\n%s", err, r.calls(t))
	}
}

// The service cannot be installed after DNS points at the tunnel: the local
// half is undone (config, zm's listener), the tunnel stays (deleting it
// would break the name), and a re-run finishes.
func TestSetupRollsBackTheLocalHalf(t *testing.T) {
	r := newRig(t)
	r.sup.fail = true
	err := r.s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(r.calls(t), "delete") {
		t.Fatalf("a routed tunnel was deleted:\n%s", r.calls(t))
	}
	if r.zm.down != 1 {
		t.Fatalf("zm's listener left up (downs %d)", r.zm.down)
	}
	if _, err := os.Stat(filepath.Join(Dir(r.s.ZmHome), "config.yml")); !os.IsNotExist(err) {
		t.Fatal("cloudflared config left behind")
	}
	st, _ := LoadState(r.s.ZmHome)
	if !st.Routed || !st.Created || st.TunnelID == "" {
		t.Fatalf("state after a late failure %+v", st)
	}
	r.sup.fail = false
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}

// Bot Fight Mode's challenge is named, with the dashboard steps; the setup
// stays in place.
func TestVerifyNamesAChallenge(t *testing.T) {
	r := newRig(t)
	r.s.Probe = func(context.Context, string, string) (Answer, error) {
		return Answer{Status: 403, Challenge: true}, nil
	}
	err := r.s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Bot Fight Mode") || !strings.Contains(err.Error(), "example.com") {
		t.Fatalf("got %v", err)
	}
	if len(r.sup.installed) != 1 {
		t.Fatal("a failed check undid the setup")
	}
}

// Another zm home answering at the hostname (or a forged signature) is
// refused; an unsigned 401 is never taken for this home's.
func TestVerifyRefusesAnotherHome(t *testing.T) {
	r := newRig(t)
	r.s.Probe = func(context.Context, string, string) (Answer, error) {
		return Answer{Status: 401, OtherHome: true}, nil
	}
	if err := r.s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "another zm home") {
		t.Fatalf("got %v", err)
	}
	r2 := newRig(t)
	r2.s.Probe = func(context.Context, string, string) (Answer, error) {
		return Answer{Status: 401, HomeKey: r2.key}, nil
	}
	if err := r2.s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "did not answer as this zm home") {
		t.Fatalf("unsigned 401 taken: %v", err)
	}
}

func TestRemove(t *testing.T) {
	r := newRig(t)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.out.Reset()
	if err := r.s.Remove(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if r.zm.down != 1 || len(r.sup.installed) != 0 || !strings.Contains(r.calls(t), "delete -f 6f1d2c3b") {
		t.Fatalf("remove: down %d, services %v, calls\n%s", r.zm.down, r.sup.installed, r.calls(t))
	}
	if !strings.Contains(r.out.String(), "delete the CNAME zm.example.com") {
		t.Fatalf("no DNS step:\n%s", r.out)
	}
	if st, _ := LoadState(r.s.ZmHome); st.Hostname != "" {
		t.Fatalf("state after remove %+v", st)
	}
}

// A tunnel this machine only reused is not deleted by remove, nor is its
// DNS record named as a step: only the local half goes.
func TestRemoveKeepsAReusedTunnel(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.cfDir, "tunnels"), 0o700)
	os.WriteFile(filepath.Join(r.cfDir, "tunnels", "zm-zm-example-com"), []byte("existing-id\n"), 0o600)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.out.Reset()
	if err := r.s.Remove(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.calls(t), "delete") {
		t.Fatalf("a reused tunnel was deleted:\n%s", r.calls(t))
	}
	if r.zm.down != 1 || len(r.sup.installed) != 0 || !strings.Contains(r.out.String(), "kept tunnel zm-zm-example-com") || strings.Contains(r.out.String(), "delete the CNAME") {
		t.Fatalf("remove of a reused tunnel: down %d, services %v\n%s", r.zm.down, r.sup.installed, r.out)
	}
	if _, err := os.Stat(filepath.Join(Dir(r.s.ZmHome), "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("its credentials stayed on this machine")
	}
	if st, _ := LoadState(r.s.ZmHome); st.Hostname != "" {
		t.Fatalf("state after remove %+v", st)
	}
}

func TestOneHostnamePerHome(t *testing.T) {
	r := newRig(t)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.s.Hostname, r.s.Name = "other.example.com", ""
	if err := r.s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "zm tunnel remove") {
		t.Fatalf("got %v", err)
	}
}

func TestCheckHostname(t *testing.T) {
	for in, ok := range map[string]bool{"zm.example.net": true, "ZM.Example.com.": true, "localhost": false, "1.2.3.4": false,
		"a_b.example.com": false, "https://zm.example.com": false, "-x.example.com": false} {
		if _, err := CheckHostname(in); (err == nil) != ok {
			t.Errorf("%q: %v", in, err)
		}
	}
	if TunnelName("zm.example.net") != "potluck-zm-example-net" || TunnelNameOld("zm.example.net") != "zm-zm-example-net" {
		t.Fatal(TunnelName("zm.example.net"))
	}
}

// The rehearsal: a quick tunnel run by the service manager, its name read
// from the log, zm's listener on that name, nothing in any account.
func TestQuick(t *testing.T) {
	r := newRig(t)
	sup := &quickSup{fakeSup: r.sup}
	r.s.Sup = sup
	if err := r.s.RunQuick(context.Background()); err != nil {
		t.Fatalf("quick: %v\n%s", err, r.out)
	}
	if r.calls(t) != "" {
		t.Fatalf("cloudflared CLI called in a quick run:\n%s", r.calls(t))
	}
	st, _ := LoadState(r.s.ZmHome)
	u := r.sup.installed[Label(r.s.ZmHome)]
	cfgPath := filepath.Join(Dir(r.s.ZmHome), "config.yml")
	cfg, _ := os.ReadFile(cfgPath)
	fi, _ := os.Stat(cfgPath)
	if !st.Quick || st.Hostname != "abc-def.trycloudflare.com" || !strings.Contains(string(cfg), "http-host-header: potluck-edge-"+st.EdgeSecret) ||
		fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state %+v config %q %v", st, cfg, fi)
	}
	// The secret never reaches argv or the unit.
	for _, a := range append(u.Args, u.Log, u.Dir) {
		if strings.Contains(a, st.EdgeSecret) {
			t.Fatalf("edge secret in the unit: %+v", u)
		}
	}
	if len(r.zm.ups) != 1 || r.zm.ups[0].URL != "https://abc-def.trycloudflare.com" {
		t.Fatalf("zm up %+v", r.zm.ups)
	}
	if err := r.s.Remove(context.Background(), false); err != nil || len(r.sup.installed) != 0 {
		t.Fatalf("remove: %v %+v", err, r.sup.installed)
	}
}

// quickSup "runs" the quick tunnel: it writes the name cloudflared prints.
type quickSup struct{ *fakeSup }

func (q *quickSup) Install(ctx context.Context, u Unit) error {
	if err := q.fakeSup.Install(ctx, u); err != nil {
		return err
	}
	return os.WriteFile(u.Log, []byte("INF |  https://abc-def.trycloudflare.com  |\n"), 0o600)
}

// Probe checks the answer's signature over its own nonce, as a member's
// transport does: this home's door passes; another home's key, or a
// relayed answer to another nonce, does not.
func TestProbeChecksTheSignature(t *testing.T) {
	cfg := func(t *testing.T) *config.Config {
		c, err := config.LoadFrom(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	home := tunnel.New(&core.Deps{Config: cfg(t)})
	srv := httptest.NewServer(home.Handler(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	fp, _ := home.Fingerprint()
	a, err := Probe(context.Background(), srv.URL+"/v0/health", fp)
	if err != nil || a.Status != 401 || !a.Signed || a.OtherHome {
		t.Fatalf("this home: %+v %v", a, err)
	}
	other := tunnel.New(&core.Deps{Config: cfg(t)})
	ofp, _ := other.Fingerprint()
	if a, err := Probe(context.Background(), srv.URL+"/v0/health", ofp); err != nil || a.Signed || !a.OtherHome {
		t.Fatalf("pinned to another home: %+v %v", a, err)
	}
	// A replayed answer (signed for another nonce) is not this request's.
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("GET", srv.URL+"/v0/health", nil)
		req.Header.Set(tunnel.HeaderNonce, "another-nonce-of-22chr")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(replay.Close)
	if a, err := Probe(context.Background(), replay.URL+"/v0/health", fp); err != nil || a.Signed {
		t.Fatalf("a replayed answer: %+v %v", a, err)
	}
}

// TestLastJSON: real cloudflared's create output is indented and nests "[]"
// inside the object, and it may log lines with brackets first.
func TestLastJSON(t *testing.T) {
	obj := "{\n  \"id\": \"x\",\n  \"connections\": []\n}\n"
	for name, tc := range map[string]struct{ in, want string }{
		"object with a nested array": {obj, strings.TrimSpace(obj)},
		"after a log line":           {"2026 INF [cfd] {not json\n" + obj, strings.TrimSpace(obj)},
		"array":                      {"[{\"id\":\"x\"}]\n", `[{"id":"x"}]`},
		"nothing":                    {"no json here [\n", ""},
	} {
		if got := string(lastJSON([]byte(tc.in))); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// An extra route (the runner broker's public paths) goes before the door's
// rule on the same hostname; without routes the config is the door's alone.
func TestCloudflaredConfigRoutes(t *testing.T) {
	st := State{TunnelID: "tid", Hostname: "zm.example.com", EdgeSecret: "sek"}
	s := &Setup{ZmHome: t.TempDir(), Listen: "127.0.0.1:7435"}
	if got := s.cloudflaredConfig(st); strings.Contains(got, "path:") {
		t.Errorf("no routes, yet a path rule:\n%s", got)
	}
	s.Routes = []Route{{Path: `^/v0/runners/(hook|oauth|app/new|app/created)$`, Listen: "127.0.0.1:7440"}}
	got := s.cloudflaredConfig(st)
	want := "  - hostname: zm.example.com\n    path: '^/v0/runners/(hook|oauth|app/new|app/created)$'\n    service: http://127.0.0.1:7440\n  - hostname: zm.example.com\n    service: http://127.0.0.1:7435\n"
	if !strings.Contains(got, want) {
		t.Errorf("config:\n%s\nwant the route before the door:\n%s", got, want)
	}
}

// Ingress rewrites the named tunnel's config for new routes and restarts
// cloudflared once; nothing changed or no tunnel: nothing done.
func TestIngressRewritesAndRestartsOnlyOnChange(t *testing.T) {
	r := newRig(t)
	if changed, err := r.s.Ingress(context.Background()); err != nil || changed {
		t.Fatalf("no tunnel yet: %v %v", changed, err)
	}
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.s.Ingress(context.Background()); err != nil || changed || r.sup.restarts != 0 {
		t.Fatalf("same routes: %v %v, %d restarts", changed, err, r.sup.restarts)
	}
	r.s.Routes = []Route{{Path: `^/v0/runners/hook$`, Listen: "127.0.0.1:7440"}}
	if changed, err := r.s.Ingress(context.Background()); err != nil || !changed || r.sup.restarts != 1 {
		t.Fatalf("new route: %v %v, %d restarts", changed, err, r.sup.restarts)
	}
	b, _ := os.ReadFile(r.s.config())
	if !strings.Contains(string(b), "service: http://127.0.0.1:7440") || !strings.Contains(string(b), "service: http://"+DefaultListen) {
		t.Fatalf("config:\n%s", b)
	}
	if changed, _ := r.s.Ingress(context.Background()); changed || r.sup.restarts != 1 {
		t.Fatalf("again: %v, %d restarts", changed, r.sup.restarts)
	}
}

// A setup made before the rename's step 2 keeps what it made (H12): its
// tunnel name, the Host cloudflared sends (zm-edge-<secret>), its service
// label. A re-run, Ingress and Remove use what state.json names; the door
// takes either Host (tunnel.EdgeHost and EdgeHostNew).
func TestExistingSetupKeepsItsNames(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.cfDir, "tunnels"), 0o700)
	os.WriteFile(filepath.Join(r.cfDir, "tunnels", "zm-zm-example-com"), []byte("old-id\n"), 0o600)
	dir := Dir(r.s.ZmHome)
	os.MkdirAll(dir, 0o700)
	old := State{Hostname: "zm.example.com", Name: "zm-zm-example-com", TunnelID: "old-id", Created: true, Routed: true,
		Listen: DefaultListen, EdgeSecret: "0123456789abcdef0123456789abcdef", Label: LabelOld(r.s.ZmHome)}
	b, _ := json.Marshal(old)
	os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600)
	os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("{}"), 0o600)
	if err := r.s.Run(context.Background()); err != nil {
		t.Fatalf("re-run: %v\n%s", err, r.out)
	}
	if strings.Contains(r.calls(t), " create ") || strings.Contains(r.calls(t), "potluck-zm-example-com") {
		t.Fatalf("a re-run looked for or made a new tunnel:\n%s", r.calls(t))
	}
	st, _ := LoadState(r.s.ZmHome)
	if st.Name != "zm-zm-example-com" || st.TunnelID != "old-id" || st.Label != LabelOld(r.s.ZmHome) || st.EdgeHost != "" {
		t.Fatalf("state %+v", st)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if !strings.Contains(string(cfg), "httpHostHeader: zm-edge-"+old.EdgeSecret) {
		t.Fatalf("config.yml:\n%s", cfg)
	}
	if _, ok := r.sup.installed[LabelOld(r.s.ZmHome)]; !ok || len(r.sup.installed) != 1 {
		t.Fatalf("services %v", r.sup.installed)
	}
	if err := r.s.Remove(context.Background(), false); err != nil || len(r.sup.installed) != 0 {
		t.Fatalf("remove: %v %v", err, r.sup.installed)
	}
	if !strings.HasPrefix(Label("/h"), "dev.potluck.cloudflared.") || !strings.HasPrefix(LabelOld("/h"), "dev.zm.cloudflared.") ||
		Label("/h")[len("dev.potluck.cloudflared."):] != LabelOld("/h")[len("dev.zm.cloudflared."):] {
		t.Fatalf("labels %s %s", Label("/h"), LabelOld("/h"))
	}
}
