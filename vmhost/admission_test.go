package vmhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeController is an admission controller keeping the contract in
// admission.go: room in vCPU, leases by holder, a revoke switch.
type fakeController struct {
	mu       sync.Mutex
	room     int // vCPU left
	leases   map[string]ctlLease
	byHolder map[string]string
	revoked  map[string]bool
	token    string
	calls    []string
	n        int
}

type ctlLease struct {
	holder string
	vcpu   int
}

func newController(room int) (*fakeController, *httptest.Server) {
	c := &fakeController{room: room, leases: map[string]ctlLease{}, byHolder: map[string]string{}, revoked: map[string]bool{}}
	srv := httptest.NewServer(c)
	return c, srv
}

func (c *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	if c.token != "" && r.Header.Get("Authorization") != "Bearer "+c.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	exp := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/leases":
		var in struct {
			VCPU      int    `json:"vcpu"`
			MemoryMiB int    `json:"memory_mib"`
			Holder    string `json:"holder"`
			TTL       int    `json:"ttl_s"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if id, ok := c.byHolder[in.Holder]; ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": id, "expires_at": exp})
			return
		}
		if in.VCPU <= 0 || in.MemoryMiB <= 0 || in.TTL <= 0 || !strings.HasPrefix(in.Holder, "zm:") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if in.VCPU > c.room {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"reason": "CI jobs are queued", "retry_s": 30})
			return
		}
		c.n++
		id := "L" + string(rune('0'+c.n))
		c.room -= in.VCPU
		c.leases[id], c.byHolder[in.Holder] = ctlLease{in.Holder, in.VCPU}, id
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": id, "expires_at": exp})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/renew"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/leases/"), "/renew")
		if _, ok := c.leases[id]; !ok || c.revoked[id] {
			w.WriteHeader(http.StatusGone)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"expires_at": exp})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/leases/"):
		id := strings.TrimPrefix(r.URL.Path, "/leases/")
		l, ok := c.leases[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		c.room += l.vcpu
		delete(c.leases, id)
		delete(c.byHolder, l.holder)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (c *fakeController) live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.leases)
}

func TestAdmissionClient(t *testing.T) {
	ctl, srv := newController(4)
	defer srv.Close()
	ctl.token = "s3cret"
	tok := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(tok, []byte("s3cret\n"), 0o644)
	_ = os.Chmod(tok, 0o644) // WriteFile's mode is masked by the umask (077 on some runners)
	if _, err := newAdmission(config{AdmissionURL: srv.URL, AdmissionTokenFile: tok, AdmissionTTL: 120}); err == nil {
		t.Fatal("a world-readable token file was accepted")
	}
	_ = os.Chmod(tok, 0o600)
	a, err := newAdmission(config{AdmissionURL: srv.URL + "/", AdmissionTokenFile: tok, AdmissionTTL: 120})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l, err := a.take(ctx, 2, 4096, "zm:t/r1")
	if err != nil || l.Lease == "" || l.ExpiresAt.IsZero() {
		t.Fatalf("take = %+v %v", l, err)
	}
	if again, err := a.take(ctx, 2, 4096, "zm:t/r1"); err != nil || again.Lease != l.Lease {
		t.Fatalf("a repeat of the holder: %+v %v", again, err)
	}
	var nr noRoom
	if _, err := a.take(ctx, 4, 4096, "zm:t/r2"); !errors.As(err, &nr) || nr.retry != 30 || !strings.Contains(err.Error(), "CI jobs are queued") {
		t.Fatalf("no room: %v", err)
	}
	if exp, err := a.renew(ctx, l.Lease); err != nil || exp.IsZero() {
		t.Fatalf("renew: %v %v", exp, err)
	}
	if err := a.release(ctx, l.Lease); err != nil {
		t.Fatal(err)
	}
	if err := a.release(ctx, l.Lease); err != nil {
		t.Fatalf("a second release: %v", err)
	}
	if _, err := a.renew(ctx, l.Lease); !errors.Is(err, errLeaseGone) {
		t.Fatalf("renew of a released lease: %v", err)
	}
	ctl.token = "other"
	if _, err := a.take(ctx, 1, 1024, "zm:t/r3"); err == nil {
		t.Fatal("a refused token passed")
	}
}

func TestAdmissionKeep(t *testing.T) {
	ctl, srv := newController(4)
	defer srv.Close()
	old := renewRetry
	renewRetry = 0
	defer func() { renewRetry = old }()
	a := &admission{url: srv.URL, ttl: 3, hc: http.DefaultClient}
	ctx := context.Background()
	l, err := a.take(ctx, 1, 1024, "zm:t/k")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	h := &held{a: a, lease: l, renewed: t0, tried: t0}
	if h.due(t0.Add(500 * time.Millisecond)) {
		t.Fatal("due before a third of the TTL")
	}
	if err := h.keep(ctx, t0.Add(1100*time.Millisecond)); err != nil || h.renewed.Before(t0.Add(time.Second)) {
		t.Fatalf("a due renewal: %v", err)
	}
	// The controller unreachable with time left: keep trying, no end.
	srv.Close()
	if err := h.keep(ctx, h.renewed.Add(1100*time.Millisecond)); err != nil {
		t.Fatalf("a failed renewal with time left ended the VM: %v", err)
	}
	// Past expires_at with no renewal: the VM must end.
	if err := h.keep(ctx, h.lease.ExpiresAt.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "lapsed") {
		t.Fatalf("a lapsed lease: %v", err)
	}
	// Taken back: the VM must end at once.
	_, srv2 := newController(4)
	defer srv2.Close()
	h2 := &held{a: &admission{url: srv2.URL, ttl: 3, hc: http.DefaultClient}, lease: admissionLease{Lease: "gone", ExpiresAt: time.Now().Add(time.Hour)}}
	if err := h2.keep(ctx, time.Now()); !errors.Is(err, errLeaseGone) {
		t.Fatalf("a revoked lease: %v", err)
	}
	_ = ctl
}

func TestAdmissionConfig(t *testing.T) {
	m := cfg(t)
	m["admission_url"] = "http://127.0.0.1:9/cap"
	if _, err := parseConfig(m); err == nil || !strings.Contains(err.Error(), "cpu and memory") {
		t.Fatalf("no sizes: %v", err)
	}
	m["cpu"], m["memory"] = "2", "4096"
	c, err := parseConfig(m)
	if err != nil || c.AdmissionTTL != 120 {
		t.Fatalf("defaults: %+v %v", c.AdmissionTTL, err)
	}
	again, err := parseConfig((&drv{cfg: c}).rawConfig())
	if err != nil || again.AdmissionURL != c.AdmissionURL || again.AdmissionTTL != c.AdmissionTTL {
		t.Fatalf("rawConfig round trip: %+v %v", again, err)
	}
	for k, v := range map[string]string{"admission_url": "ftp://x/", "admission_ttl_s": "60"} {
		bad := map[string]string{}
		for a, b := range m {
			bad[a] = b
		}
		bad[k] = v
		if _, err := parseConfig(bad); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
	for _, u := range []string{"http://user:pw@127.0.0.1/", "http://127.0.0.1/?t=1"} {
		m["admission_url"] = u
		if _, err := parseConfig(m); err == nil {
			t.Errorf("admission_url %s accepted", u)
		}
	}
	delete(m, "admission_url")
	m["admission_token_file"] = "/x"
	if _, err := parseConfig(m); err == nil {
		t.Fatal("a token file without a url was accepted")
	}
}

// The whole path over the fake layer: no room refuses the acquire with the
// controller's reason; room gives a run that holds a lease; release gives it
// back; a controller taking the room back ends the VM as reclaimed.
func TestAdmissionThroughTheDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a renewal")
	}
	ctl, srv := newController(1)
	defer srv.Close()
	_, repo, m := fakeBinding(t, "linux-img")
	m["target"], m["admission_url"], m["cpu"], m["memory"], m["boot_timeout_s"], m["admission_ttl_s"] = "fake", srv.URL, "2", "2048", "1", "31"
	run := func(key string) (map[string]any, error) {
		return call(t, m, "acquire", "", map[string]any{"request_id": key, "ttl_s": 60, "ref": "main",
			"commit": repo.Commit, "script": "probe.sh", "args": []string{"sleep", "60"}})
	}
	if _, err := run("full"); code(err) != "unavailable" || !strings.Contains(err.Error(), "CI jobs are queued") {
		t.Fatalf("no room: %v", err)
	}
	if ctl.live() != 0 {
		t.Fatal("a refused acquire holds a lease")
	}
	ctl.mu.Lock()
	ctl.room = 4
	ctl.mu.Unlock()
	a, err := run("one")
	if err != nil {
		t.Fatal(err)
	}
	if ctl.live() != 1 {
		t.Fatalf("leases: %d", ctl.live())
	}
	if _, err := call(t, m, "release", a["handle"].(string), nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ctl.live() != 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the lease outlived the release")
		}
	}
	b, err := run("two")
	if err != nil {
		t.Fatal(err)
	}
	ctl.mu.Lock()
	for id := range ctl.leases {
		ctl.revoked[id] = true
	}
	ctl.mu.Unlock()
	// The first renewal is due a third of the TTL (31 s with boot_timeout_s 1) in.
	for deadline := time.Now().Add(25 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		st, err := call(t, m, "status", b["handle"].(string), nil)
		if err == nil && st["state"] == "reclaimed" {
			if st["exit"] != nil || !strings.Contains(st["why"].(string), "took the room back") {
				t.Fatalf("reclaimed: %v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a revoked lease did not end the VM: %v %v", st, err)
		}
	}
}

// A redirect is an error, never followed: a 3xx would change the method or
// send the call elsewhere.
func TestAdmissionRefusesRedirects(t *testing.T) {
	_, good := newController(4)
	defer good.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, good.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	a, err := newAdmission(config{AdmissionURL: redir.URL, AdmissionTTL: 120})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.take(context.Background(), 1, 1024, "zm:t/r"); err == nil {
		t.Fatal("a redirected take was followed")
	}
}
