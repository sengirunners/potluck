package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/runtarget"
)

// A host run whose join waits for a tailnet login carries the login's URL
// on its join stage in host status --json while it waits, and not once the
// stage ends; the stderr line stays. The record's again_args need the invite
// appended (nothing joined), and the invite is in neither the record nor
// host status.
func TestHostJoinLoginURLInHostStatus(t *testing.T) {
	oldDetect, oldRunner, oldDriver := detectHost, shareRunner, shareDriver
	t.Cleanup(func() { detectHost, shareRunner, shareDriver = oldDetect, oldRunner, oldDriver })
	shareDriver = "ref"
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true, Swap: hostswap.Encrypted,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	tn := tailnettest.New()
	owner := startMachine(t, tn, "owner", "owner@example.com")
	lender := startMachine(t, tn, "lender", "lender@example.com")
	// host finds its driver in the drivers directory; the conformance run
	// that would start it is faked (shareRunner).
	drv := filepath.Join(lender.home, runtarget.DriversDir, "zm-run-ref")
	if err := os.MkdirAll(filepath.Dir(drv), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drv, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	owner.ok(t, "create", "friends")
	invite := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "lender", "--host"))
	if invite == nil {
		t.Fatal("no host invite")
	}

	// The lender's daemon, behind one that answers its first join with a
	// tailnet login and holds the next until the test has looked.
	const url = "https://login.example/a/1"
	asked, release := make(chan struct{}), make(chan struct{})
	joins := 0
	loginGate(t, lender.home, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v0/team/join" {
			return false
		}
		joins++
		if joins == 1 {
			json.NewEncoder(w).Encode(api.JoinResponse{AuthURL: url})
			close(asked)
			return true
		}
		<-release
		api.WriteError(w, api.Errorf(api.CodeForbidden, "this invite was spent"))
		return true
	})

	t.Setenv(EnvHome, lender.home)
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), []string{"host", invite[1], "--no-sudo", "--sizes", "c1m2", "--reclaim-idle-s", "0"}, strings.NewReader(""), new(bytes.Buffer), &stderr)
	}()
	select {
	case <-asked:
	case code := <-done:
		t.Fatalf("host ended (exit %d) before its join asked:\n%s", code, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("no join")
	}
	status := func() (menubar.Status, string) {
		t.Helper()
		var o, e bytes.Buffer
		if code := Run(context.Background(), []string{"--json", "host", "status"}, strings.NewReader(""), &o, &e); code != 0 {
			t.Fatalf("host status: exit %d\n%s", code, e.String())
		}
		var st menubar.Status
		if err := json.Unmarshal(o.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st, o.String()
	}
	joinStage := func(st menubar.Status) hostsetup.Stage {
		for _, s := range st.Setup.Stages {
			if s.Name == hostsetup.Join {
				return s
			}
		}
		return hostsetup.Stage{}
	}
	var st menubar.Status
	var raw string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) { // the hook writes just after the answer
		if st, raw = status(); joinStage(st).OpenURL == url || time.Now().After(deadline) {
			break
		}
	}
	if s := joinStage(st); st.Setup.State != menubar.SetupRunning || s.Result != hostsetup.Running || s.OpenURL != url {
		t.Fatalf("waiting for the login: state %s, join %+v\n%s", st.Setup.State, s, raw)
	}
	if want := []string{"host", "--no-sudo=true", "--reclaim-idle-s=0", "--sizes=c1m2"}; !slices.Equal(st.Setup.AgainArgs, want) || !st.Setup.NeedsInvite {
		t.Fatalf("again_args %q, needs_invite %v", st.Setup.AgainArgs, st.Setup.NeedsInvite)
	}
	if strings.Contains(raw, invite[1]) {
		t.Fatalf("host status holds the invite:\n%s", raw)
	}
	// The app's button is the login page, from the setup check.
	if c := setupByID(st.Setup, "setup"); c.Do == nil || c.Do.URL != url || len(c.Do.Args) != 0 || st.Setup.NextID != "setup" || !strings.Contains(st.Setup.Next, url) {
		t.Fatalf("while waiting: setup check %+v do %+v, next %q (%s)", c, c.Do, st.Setup.Next, st.Setup.NextID)
	}
	close(release)
	select {
	case code := <-done:
		if code == 0 {
			t.Fatalf("a refused join exited 0:\n%s", stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("host did not end")
	}
	if !strings.Contains(stderr.String(), "log in to your tailnet to continue (waiting): "+url) {
		t.Errorf("no login line on stderr:\n%s", stderr.String())
	}
	st, raw = status()
	if s := joinStage(st); st.Setup.State != menubar.SetupBlocked || s.Result != hostsetup.Blocked || s.OpenURL != "" || strings.Contains(raw, url) {
		t.Fatalf("after the join: state %s, join %+v\n%s", st.Setup.State, s, raw)
	}
	// Blocked: the button runs the same setup in the background, the invite
	// pasted again and given on stdin.
	want := []string{"--json", "host", "--no-sudo=true", "--reclaim-idle-s=0", "--sizes=c1m2", "--yes", "--detach"}
	if c := setupByID(st.Setup, "setup"); c.Do == nil || !slices.Equal(c.Do.Args, append(slices.Clone(want), "--instead-of-invite=", "--")) || !slices.Equal(c.Do.InviteArgs, append(want, "--invite-stdin")) || c.Do.URL != "" || st.Setup.NextID != "setup" {
		t.Fatalf("blocked: setup check %+v do %+v, next_id %q", c, c.Do, st.Setup.NextID)
	}
	if b, _ := os.ReadFile(filepath.Join(lender.home, hostsetup.FileName)); strings.Contains(string(b), invite[1]) || strings.Contains(string(b), url) {
		t.Fatalf("setup record:\n%s", b)
	}
}

func setupByID(s menubar.Setup, id string) menubar.Check {
	for _, c := range s.Checks {
		if c.ID == id {
			return c
		}
	}
	return menubar.Check{}
}

// loginGate puts a daemon in front of the one listening in home: answer
// takes the requests it answers itself (true), and the rest go through.
func loginGate(t *testing.T, home string, answer func(http.ResponseWriter, *http.Request) bool) {
	t.Helper()
	sock := filepath.Join(home, "potluck.sock")
	real := filepath.Join(home, "real.sock")
	if err := os.Rename(sock, real); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "daemon" },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", real)
		}},
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol) // as a daemon answers
		if !answer(w, r) {
			w.Header().Del(api.HeaderProtocol) // the daemon behind says its own
			w.Header().Del(api.HeaderNew(api.HeaderProtocol))
			proxy.ServeHTTP(w, r)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.Rename(real, sock) })
}
