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

	"github.com/sengirunners/potluck/protocol"
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
	_, err := run("full")
	if code(err) != "unavailable" || !strings.Contains(err.Error(), "CI jobs are queued") {
		t.Fatalf("no room: %v", err)
	}
	// The answer carries the controller's reason and retry time as data, so
	// a caller waits and asks again (roomwait) instead of failing.
	if bz, ok := noRoomData(err); !ok || bz.Why != "CI jobs are queued" || bz.RetryS != 30 {
		t.Errorf("no room's data: %+v %v", bz, ok)
	}
	// Its words are the controller's answer alone: no "the run did not
	// start", and no report path, which would be a file on this machine
	// shown to a caller on another (zm-9xzw.14).
	if e := (refusal{}); !errors.As(err, &e) || e.msg != "no room on this host now: CI jobs are queued (retry in 30 s)" {
		t.Errorf("no room's words: %v", err)
	}
	if ctl.live() != 0 {
		t.Fatal("a refused acquire holds a lease")
	}
	// describe reports the refusal (host status and the advert read it),
	// standing until the controller's retry time.
	refused := func() *protocol.HostRefusal {
		t.Helper()
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(d["host"])
		var h protocol.HostStatus
		if err := json.Unmarshal(b, &h); err != nil || !h.Valid() {
			t.Fatalf("describe's host %s: %v", b, err)
		}
		return h.Refused
	}
	r := refused()
	if r == nil || r.Reason != "no room on this host now: CI jobs are queued" {
		t.Fatalf("describe after a refusal: %+v", r)
	}
	if at, until := mustTime(t, r.At), mustTime(t, r.Until); until.Sub(at) != 30*time.Second {
		t.Fatalf("refused at %s until %s, want the controller's 30 s", r.At, r.Until)
	}
	if d, _ := call(t, m, "describe", "", nil); d["available"] != true {
		t.Fatalf("a refusal made the binding unavailable (acquire must still ask the controller): %v", d["why"])
	}
	ctl.mu.Lock()
	ctl.room = 4
	ctl.mu.Unlock()
	a, err := run("one")
	if err != nil {
		t.Fatal(err)
	}
	if r := refused(); r != nil {
		t.Fatalf("a granted lease left the refusal standing: %+v", r)
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

// A refusal stands until the controller's retry time (two minutes when it
// gives none), then describe no longer shows it; another controller's
// refusal is not this binding's.
func TestAdmissionRefusalExpires(t *testing.T) {
	st := store{dir: t.TempDir()}
	now := time.Now()
	st.recordRefusal("http://ctl/a", noRoom{reason: "busy", retry: 0}, now)
	if r := st.refusedNow("http://ctl/a", now.Add(time.Minute)); r == nil || r.Reason != "no room on this host now: busy" {
		t.Fatalf("within two minutes: %+v", r)
	}
	if r := st.refusedNow("http://ctl/a", now.Add(refusalHold)); r != nil {
		t.Fatalf("past two minutes: %+v", r)
	}
	// A controller's reason with a trailing newline still shows, as one line.
	st.recordRefusal("http://ctl/c", noRoom{reason: "CPU busy\n", retry: 0}, now)
	if r := st.refusedNow("http://ctl/c", now); r == nil || r.Reason != "no room on this host now: CPU busy" {
		t.Fatalf("a reason ending in a newline: %+v", r)
	}
	if r := st.refusedNow("http://ctl/b", now); r != nil {
		t.Fatalf("another controller's refusal: %+v", r)
	}
	st.recordRefusal("http://ctl/a", noRoom{reason: strings.Repeat("x", 1000), retry: 1 << 40}, now)
	r := st.refusedNow("http://ctl/a", now.Add(23*time.Hour))
	if r == nil || len(r.Reason) > protocol.MaxRefusalReason || mustTime(t, r.Until).Sub(mustTime(t, r.At)) != maxRefusalHold {
		t.Fatalf("a long reason and retry are bounded: %+v", r)
	}
	// A lease asked for before the refusal does not clear it; one asked for
	// after does.
	st.clearRefusal("http://ctl/a", now.Add(-time.Second))
	if r := st.refusedNow("http://ctl/a", now); r == nil {
		t.Fatal("a grant asked for before the refusal cleared it")
	}
	st.clearRefusal("http://ctl/a", now.Add(time.Second))
	if r := st.refusedNow("http://ctl/a", now); r != nil {
		t.Fatalf("cleared: %+v", r)
	}
}

// A refusal while the last one stands, or soon after its retry time,
// continues its run: since stays at the first. A push is counted only when
// the promised retry time came and a refusal then named a later one, never
// for asks before it, however often (zm-eakh.12.24.18.4). A grant ends the
// run; a refusal long after the last starts a new one.
func TestAdmissionRefusalPushedBack(t *testing.T) {
	st := store{dir: t.TempDir()}
	url := "http://ctl/a"
	t0 := time.Date(2026, 11, 19, 18, 50, 0, 0, time.UTC)
	settle := func(at time.Time, retry int) {
		st.recordRefusal(url, noRoom{reason: "letting other work settle", retry: retry}, at)
	}
	settle(t0, 600)
	if r := st.refusedNow(url, t0); r == nil || r.Since != "" || r.Pushed != 0 || !r.Valid() {
		t.Fatalf("the first refusal: %+v", r)
	}
	for s := 10; s < 600; s += 10 { // a retry loop every 10 s, each told 600 s again
		settle(t0.Add(time.Duration(s)*time.Second), 600)
	}
	if r := st.refusedNow(url, t0.Add(590*time.Second)); r == nil || r.Pushed != 0 || r.Since != "2026-11-19T18:50:00Z" || r.Until != "2026-11-19T19:09:50Z" {
		t.Fatalf("asks before the promised time: %+v", r)
	}
	settle(t0.Add(600*time.Second), 600)  // the promised time came: refused, ten minutes on
	settle(t0.Add(610*time.Second), 600)  // an ask before the new promise: not counted
	settle(t0.Add(1200*time.Second), 300) // the new promise came: refused again
	r := st.refusedNow(url, t0.Add(1201*time.Second))
	if r == nil || r.Pushed != 2 || r.Since != "2026-11-19T18:50:00Z" || r.Until != "2026-11-19T19:15:00Z" || !r.Valid() {
		t.Fatalf("pushed back twice: %+v", r)
	}
	st.clearRefusal(url, t0.Add(1300*time.Second))
	settle(t0.Add(1400*time.Second), 600)
	if r := st.refusedNow(url, t0.Add(1400*time.Second)); r == nil || r.Pushed != 0 || r.Since != "" {
		t.Fatalf("after a grant: %+v", r)
	}
	settle(t0.Add(3*time.Hour), 600)
	if r := st.refusedNow(url, t0.Add(3*time.Hour)); r == nil || r.Pushed != 0 || r.Since != "" {
		t.Fatalf("long after the last: %+v", r)
	}
}

// A refusal kept by a build without since and due continues as a run from
// its own time and retry time.
func TestAdmissionRefusalFromAnOlderBuild(t *testing.T) {
	st := store{dir: t.TempDir()}
	t0 := time.Date(2026, 11, 19, 18, 50, 0, 0, time.UTC)
	old := `{"http://ctl/a":{"reason":"busy","retry_s":60,"at":"2026-11-19T18:50:00Z"}}`
	if err := os.WriteFile(st.refusalFile(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st.recordRefusal("http://ctl/a", noRoom{reason: "busy", retry: 60}, t0.Add(time.Minute))
	if r := st.refusedNow("http://ctl/a", t0.Add(time.Minute)); r == nil || r.Pushed != 1 || r.Since != "2026-11-19T18:50:00Z" || !r.Valid() {
		t.Fatalf("after an older build's refusal: %+v", r)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// noRoomData is err's no-room answer data (protocol.ReasonNoRoom).
func noRoomData(err error) (protocol.Busy, bool) {
	var e refusal
	if !errors.As(err, &e) {
		return protocol.Busy{}, false
	}
	bz, ok := e.data.(protocol.Busy)
	return bz, ok && bz.Reason == protocol.ReasonNoRoom
}
