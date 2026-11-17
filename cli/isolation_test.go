package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/runtarget"
)

// Doctor's isolation lines: a binding is isolated only when its driver
// answered and the stricter class may run anyone's code; a box binding shows
// its latest block test, and a record that cannot be read is never a pass.
func TestIsolationLines(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	cfg := map[string]string{"vm": "tart", "size": "c2m4"}
	sum := strings.Repeat("a", 64)
	writePins(t, home, map[string]string{"tart": sum})
	fp := hostprobe.BindingFingerprint("tart", sum, cfg, nil)
	if err := hostprobe.RecordBlockTest(home, hostprobe.BlockTestResult{Binding: "mac-c2m4", At: at, Passed: true, Fingerprint: fp, Forbid: []string{"192.168.1.1:53"}}); err != nil {
		t.Fatal(err)
	}
	if err := hostprobe.RecordBlockTest(home, hostprobe.BlockTestResult{Binding: "mac-moved", At: at, Passed: true, Fingerprint: fp, Forbid: []string{"192.168.1.1:53"}}); err != nil {
		t.Fatal(err)
	}
	if err := hostprobe.RecordBlockTest(home, hostprobe.BlockTestResult{Binding: "mac-c4m8", At: at, Error: "failed: a box cannot reach 192.168.1.1:53: it opened"}); err != nil {
		t.Fatal(err)
	}
	eph := computeapi.RunTargetClass{Isolation: computeapi.RunTargetIsolationEphemeral}
	box := func(name string, eff computeapi.RunTargetClass, answered bool) computeapi.RunTargetView {
		v := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: "box", Driver: "tart", Config: cfg}, Effective: eff}
		if answered {
			v.Describe = &computeapi.RunTargetDescribe{}
		} else {
			v.Error = "potluck-driver-tart: not found"
		}
		return v
	}
	var out bytes.Buffer
	printIsolation(&out, []bindingIsolation{
		isolationOf(box("mac-c2m4", eph, true), home),
		isolationOf(box("mac-c4m8", eph, true), home),
		func() bindingIsolation { // re-bound on another network since its test
			v := box("mac-moved", eph, true)
			v.Config = map[string]string{"vm": "oci", "size": "c2m4"}
			return isolationOf(v, home)
		}(),
		isolationOf(box("mac-new", eph, true), home),
		isolationOf(box("mac-gone", eph, false), home),
		isolationOf(computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "ci-db", Kind: "db"},
			Effective: computeapi.RunTargetClass{Isolation: "none", Credentials: true}, Describe: &computeapi.RunTargetDescribe{}}, home),
		isolationOf(box("pool-c2m4", eph, true), home),
	}, "", programName)
	got := out.String()
	for _, w := range []string{
		"mac-c2m4 (sandbox size): isolated: each piece of work runs in a throwaway machine walled off from this machine, its LAN and its tailnet",
		"block test passed 2026-11-12 10:00 UTC: a sandbox reached none of this machine, 192.168.1.1:53, or another sandbox",
		"block test FAILED 2026-11-12 10:00 UTC: failed: a box cannot reach 192.168.1.1:53: it opened",
		"mac-new (sandbox size): isolated",
		"block test: never run on this machine; run potluck isolation test pool-c2m4",
		"block test out of date: the size or its driver changed since the test of 2026-11-12 10:00 UTC, so it proves nothing now; run potluck isolation test mac-moved",
		"block test: never run on this machine; run potluck isolation test mac-new",
		"mac-gone (sandbox size): not checked: its driver did not answer (potluck-driver-tart: not found)",
		"ci-db (db): not isolated: work on it is not walled off from this machine, its LAN and its tailnet, and work on it could read the credentials it holds; so it takes only this machine's owner's own work",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "mac-gone (sandbox size): isolated") || strings.Contains(got, "ci-db (db): isolated") {
		t.Errorf("an unknown or unisolated class shown as isolated:\n%s", got)
	}
	if bi := isolationOf(box("mac-gone", eph, false), home); bi.Isolated {
		t.Errorf("a driver that did not answer: isolated")
	}
	if bi := isolationOf(box("mac-c2m4", eph, true), home); !bi.BlockTestOK {
		t.Errorf("a passed, current record: not ok: %+v", bi)
	}
	// The driver upgraded and re-pinned since the test: the pass proves
	// nothing now, in the lines and in --json.
	writePins(t, home, map[string]string{"tart": strings.Repeat("b", 64)})
	bi := isolationOf(box("mac-c2m4", eph, true), home)
	if !bi.Stale || bi.BlockTestOK {
		t.Errorf("a re-pinned driver: %+v", bi)
	}
	// No pin at all: never a pass.
	writePins(t, home, map[string]string{})
	if bi := isolationOf(box("mac-c2m4", eph, true), home); !bi.Stale || bi.BlockTestOK {
		t.Errorf("no pin: %+v", bi)
	}
}

// writePins writes home's driver pins file: driver to sha256.
func writePins(t *testing.T, home string, sums map[string]string) {
	t.Helper()
	m := map[string]runtarget.DriverPin{}
	for d, s := range sums {
		m[d] = runtarget.DriverPin{Path: "/drivers/potluck-driver-" + d, SHA256: s, Pinned: "2026-11-12T10:00:00Z"}
	}
	b, err := json.Marshal(map[string]any{"drivers": m})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, runtarget.PinFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// potluck isolation test refuses before it boots anything: a size this
// machine does not serve, a driver whose pinned file is not there, a
// sandbox still running of any size served here (shared by potluck host or
// a pool's own), an answer that cannot say, or a name two pools give
// different sizes. A pool's own size served here is found like a shared one.
// Each refusal leaves no record.
func TestIsolationTestRefuses(t *testing.T) {
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv("HOME", home)
	drivers := filepath.Join(home, runtarget.DriversDir)
	if err := os.MkdirAll(drivers, 0o700); err != nil {
		t.Fatal(err)
	}
	drv := filepath.Join(drivers, "potluck-driver-x")
	if err := os.WriteFile(drv, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	box := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "mac-c2m4", Kind: "box", Driver: "x", Images: []string{"img"}}}
	var live computeapi.RunTargetView
	// Pool p's own sizes: lent-c2m4 served here, far-c2m4 by another
	// machine. Pool q (run here too) serves q-c2m4, and then other sizes.
	lent := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "lent-c2m4", Kind: "box", Driver: "x", Images: []string{"img"}}, Host: "mini"}
	far := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "far-c2m4", Kind: "box", Driver: "x", Images: []string{"img"}}, Host: "other"}
	qbox := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "q-c2m4", Kind: "box", Driver: "x", Images: []string{"img"}}, Host: "mini"}
	qTargets := []computeapi.RunTargetView{qbox}
	var lentLive, qLive []computeapi.RunTargetHandle
	lentErr := false
	ln, err := net.Listen("unix", filepath.Join(home, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.WireProtocol)
		w.Header().Set("Content-Type", "application/json")
		var v any
		switch pool := api.GetHeader(r.Header, api.HeaderProject); {
		case r.URL.Path == "/v0/health":
			v = api.Health{OK: true, Protocol: api.WireProtocol, Machine: "mini"}
		case r.URL.Path == "/v0/projects" && r.URL.Query().Get("local") == "1":
			v = api.ProjectList{Projects: []api.Project{{Slug: "p"}, {Slug: "q"}}}
		case r.URL.Query().Get("host") == "1" && r.URL.Query().Get("live") == "1":
			v = computeapi.RunTargetList{Targets: []computeapi.RunTargetView{live}}
		case r.URL.Query().Get("host") == "1":
			v = computeapi.RunTargetList{Targets: []computeapi.RunTargetView{box}}
		case r.URL.Path == "/v0/run_targets/lent-c2m4/handles" && pool == "p":
			if lentErr {
				w.WriteHeader(http.StatusServiceUnavailable)
				v = api.Error{Code: api.CodeUnavailable, Message: "pool db locked"}
				break
			}
			v = computeapi.RunTargetHandles{Handles: append([]computeapi.RunTargetHandle{}, lentLive...)}
		case r.URL.Path == "/v0/run_targets" && pool == "p":
			v = computeapi.RunTargetList{Project: "p", Targets: []computeapi.RunTargetView{lent, far}}
		case r.URL.Path == "/v0/run_targets/q-c2m4/handles" && pool == "q":
			v = computeapi.RunTargetHandles{Handles: append([]computeapi.RunTargetHandle{}, qLive...)}
		case r.URL.Path == "/v0/run_targets" && pool == "q":
			v = computeapi.RunTargetList{Project: "q", Targets: qTargets}
		default:
			t.Errorf("unexpected call %s %s (pool %q)", r.Method, r.URL, pool)
			w.WriteHeader(http.StatusNotFound)
			v = api.Error{Code: api.CodeNotFound, Message: "no"}
		}
		json.NewEncoder(w).Encode(v)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	try := func(size string, want ...string) {
		t.Helper()
		var out, errout bytes.Buffer
		args := []string{"isolation", "test", size}
		if pool, ok := strings.CutPrefix(size, "-P="); ok { // -P=<pool>:<size>
			pool, size, _ = strings.Cut(pool, ":")
			args = []string{"isolation", "test", "-P", pool, size}
		}
		code := Run(context.Background(), args, strings.NewReader(""), &out, &errout)
		got := out.String() + errout.String()
		if code == 0 {
			t.Fatalf("%s: exit 0: %s", size, got)
		}
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: lacks %q: %s", size, w, got)
			}
		}
		if _, ok, _ := hostprobe.LoadBlockTest(home, size); ok {
			t.Errorf("%s: a refusal left a record", size)
		}
	}
	try("other", "this machine serves no size other")
	try("far-c2m4", "this machine serves no size far-c2m4") // the pool's, but served by another machine
	try("mac-c2m4", "driver x has no pin")
	try("lent-c2m4", "driver x has no pin") // a pool's own size served here is found
	if _, err := runtarget.PinDriver(home, runtarget.DriverDirs(home), "x"); err != nil {
		t.Fatal(err)
	}
	live = box
	live.Live = []string{"mac-c2m4-1"}
	try("mac-c2m4", "still running on this machine")
	live = box
	live.Error = "potluck-driver-x: timed out"
	try("mac-c2m4", "cannot tell whether sandboxes run on mac-c2m4")
	// A pool's own size served here counts toward idle, for either size.
	live = box
	live.Live = []string{}
	lentLive = []computeapi.RunTargetHandle{{Handle: "box:lent-c2m4/1"}}
	try("mac-c2m4", "1 sandbox is still running on this machine")
	try("lent-c2m4", "1 sandbox is still running on this machine")
	lentLive, lentErr = nil, true
	try("lent-c2m4", "cannot tell whether sandboxes run on lent-c2m4 in pool p")
	lentErr = false
	// --pool does not narrow the idle check: q's sandbox still counts.
	qLive = []computeapi.RunTargetHandle{{Handle: "box:q-c2m4/1"}}
	try("-P=p:lent-c2m4", "1 sandbox is still running on this machine")
	qLive = nil
	// One name for two different sizes here: refused, since the record is
	// kept by name (pool against pool, and pool against a shared size).
	other := lent
	other.Images = []string{"img2"}
	qTargets = []computeapi.RunTargetView{qbox, other}
	try("lent-c2m4", "lent-c2m4 names two different sizes here (pool p's own, and pool q's own)")
	shadow := box
	shadow.Host, shadow.Images = "mini", []string{"img3"}
	qTargets = []computeapi.RunTargetView{qbox, shadow}
	try("mac-c2m4", "mac-c2m4 names two different sizes here (shared by potluck host, and pool q's own)")
	qTargets = []computeapi.RunTargetView{qbox}
	if err := os.WriteFile(drv, []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	try("mac-c2m4", "not the file that was pinned")
	try("lent-c2m4", "not the file that was pinned") // past the idle check, to the driver
}

// potluck isolation keeps a pool's bindings served by the daemon's machine,
// named as the daemon names itself, even when this shell's machine name
// differs (ZM_MACHINE set on one side only), and drops another machine's.
func TestIsolationUsesDaemonMachine(t *testing.T) {
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv("HOME", home)
	t.Setenv("ZM_MACHINE", "shell-name")
	ln, err := net.Listen("unix", filepath.Join(home, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	ci := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "ci", Kind: "run", Driver: "local"}, Host: "daemon-name", Describe: &computeapi.RunTargetDescribe{}}
	other := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "elsewhere", Kind: "run", Driver: "local"}, Host: "shell-name", Describe: &computeapi.RunTargetDescribe{}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.WireProtocol)
		w.Header().Set("Content-Type", "application/json")
		var v any
		switch {
		case r.URL.Path == "/v0/health":
			v = api.Health{OK: true, Protocol: api.WireProtocol, Machine: "daemon-name"}
		case r.URL.Path == "/v0/projects":
			v = api.ProjectList{Projects: []api.Project{{Slug: "p"}}}
		case r.URL.Query().Get("host") == "1":
			v = computeapi.RunTargetList{}
		default:
			v = computeapi.RunTargetList{Project: "p", Targets: []computeapi.RunTargetView{ci, other}}
		}
		json.NewEncoder(w).Encode(v)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	var out, errout bytes.Buffer
	if code := Run(context.Background(), []string{"isolation"}, strings.NewReader(""), &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errout.String())
	}
	got := out.String()
	if !strings.Contains(got, "ci (run)") || strings.Contains(got, "elsewhere") {
		t.Errorf("want the daemon's own ci and not elsewhere:\n%s", got)
	}
}
