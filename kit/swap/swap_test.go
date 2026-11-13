package swap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

func build(routes []string, proxied ...string) api.BuildManifest {
	fwd := set(proxied)
	return NewBuild(routes, func(m, p string) bool { return fwd[m+" "+p] }, map[string][]string{KindRegistry: {"a/0001"}})
}

var routes = []string{"GET /v0/health", "GET /v0/issues", "GET /v0/issues/{id}", "GET /v0/work", "POST /v0/mail"}

// TestPreflightRefusesBuildMissingForward is 2026-11-01: the running build
// forwards GET /v0/issues for the joined project orchid, the new one does
// not.
func TestPreflightRefusesBuildMissingForward(t *testing.T) {
	old := build(routes, "GET /v0/issues", "GET /v0/issues/x", "GET /v0/work")
	nu := build(routes, "GET /v0/work")
	run := &Running{Health: api.Health{Protocol: api.Protocol, Version: "v1", Commit: "old"},
		Manifest: &api.DaemonManifest{Build: old, Joined: []string{"orchid"}}}
	pf := Check(nu, run, nil, nil)
	if pf.OK || len(pf.Problems) != 2 || !strings.Contains(pf.Problems[0], "joined projects (orchid)") ||
		!strings.Contains(strings.Join(pf.Problems, "\n"), "forwards GET /v0/issues to their authority") {
		t.Fatalf("want a refusal naming the forwarded routes: %+v", pf)
	}
	// Nothing joined: the forward is not needed.
	run.Manifest.Joined = nil
	if pf := Check(nu, run, nil, nil); !pf.OK {
		t.Fatalf("no joined project, want ok: %+v", pf)
	}
}

func TestPreflightRoutes(t *testing.T) {
	old := build(routes)
	nu := build([]string{"GET /v0/health", "GET /v0/issues", "GET /v0/work"})
	run := &Running{Health: api.Health{Protocol: api.Protocol}, Manifest: &api.DaemonManifest{Build: old, Used: []string{"POST /v0/mail"}}}
	pf := Check(nu, run, nil, nil)
	if pf.OK || len(pf.Problems) != 1 || !strings.Contains(pf.Problems[0], "POST /v0/mail was answered") {
		t.Fatalf("a used route missing must refuse: %+v", pf)
	}
	if len(pf.Warnings) != 1 || !strings.Contains(pf.Warnings[0], "GET /v0/issues/{id}") {
		t.Fatalf("an unused route missing is a warning: %+v", pf.Warnings)
	}
	run.Health.Protocol = "zm/v1"
	if pf := Check(nu, run, nil, nil); pf.OK {
		t.Fatal("another protocol major must refuse")
	}
}

func TestPreflightSchemaAndLegacy(t *testing.T) {
	nu := build(routes, "GET /v0/work")
	applied := map[string][]string{"registry.db": {"a/0001", "b/0002"}, "projects/X/issues.db": {"9999"}}
	kinds := map[string]string{"registry.db": KindRegistry, "projects/X/issues.db": KindIssues}
	pf := Check(nu, nil, applied, kinds)
	if pf.OK || len(pf.Problems) != 1 || !strings.Contains(pf.Problems[0], "registry.db has migration b/0002") {
		t.Fatalf("a newer database must refuse (a kind the build does not list is skipped): %+v", pf)
	}
	// A daemon older than manifests, with a project joined: the legacy set.
	run := &Running{Health: api.Health{Protocol: api.Protocol}, Joined: []string{"orchid"}}
	pf = Check(nu, run, nil, nil)
	if pf.OK || !strings.Contains(strings.Join(pf.Problems, "\n"), "GET /v0/issues to their authority") || len(pf.Notes) == 0 {
		t.Fatalf("legacy: %+v", pf)
	}
}

func TestLockOneAtATime(t *testing.T) {
	home := t.TempDir()
	if _, held := Holder(home); held {
		t.Fatal("held before anyone took it")
	}
	l, err := Acquire(home, api.SwapHolder{By: "mini/aaaa1111", What: "upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	h, held := Holder(home)
	if !held || h.By != "mini/aaaa1111" || h.Nonce == "" || h.PID != os.Getpid() {
		t.Fatalf("holder: %+v %v", h, held)
	}
	_, err = Acquire(home, api.SwapHolder{By: "mini/bbbb2222", What: "restart"})
	var busy *BusyError
	if !errors.As(err, &busy) || !strings.Contains(err.Error(), "upgrade of this daemon is in progress: mini/aaaa1111") {
		t.Fatalf("second acquire: %v", err)
	}
	l.Release()
	if _, held := Holder(home); held {
		t.Fatal("still held after release")
	}
	l2, err := Acquire(home, api.SwapHolder{By: "x", What: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

func TestPlaceKeepsPrevAndRestores(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "not", "yet") // Place makes the directory
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(src, "zm"), "new zm")
	write(filepath.Join(src, "bd-zm"), "new bd")
	write(filepath.Join(src, "notes.txt"), "not a binary")
	names, err := Installable(src)
	if err != nil || strings.Join(names, ",") != "bd-zm,zm" {
		t.Fatalf("installable: %v %v", names, err)
	}
	if _, err := Place(src, dst, names); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dst, "zm"), "old zm")
	write(filepath.Join(dst, "bd-zm.gone"), "")
	_ = os.Remove(filepath.Join(dst, "bd-zm"))
	p, err := Place(src, dst, names)
	if err != nil {
		t.Fatal(err)
	}
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(dst, n)); return string(b) }
	if read("zm") != "new zm" || read("zm.prev") != "old zm" || read("bd-zm") != "new bd" {
		t.Fatal("not placed")
	}
	if err := p.Restore(); err != nil {
		t.Fatal(err)
	}
	if read("zm") != "old zm" {
		t.Fatal("zm not restored")
	}
	if _, err := os.Stat(filepath.Join(dst, "bd-zm")); !os.IsNotExist(err) {
		t.Fatal("bd-zm had no previous file: restore removes it")
	}
}

func TestRegressions(t *testing.T) {
	before := api.ServeCheck{Projects: []string{"orchid", "gone"}, Items: []api.ServeItem{
		{Kind: ItemIssues, Name: "orchid", OK: true}, {Kind: ItemWork, Name: "orchid", OK: true},
		{Kind: ItemWork, Name: "gone", OK: true}, {Kind: ItemAdapter, Name: "orchid/bd", OK: true},
		{Kind: ItemAdapter, Name: "orchid/gh"}, // failing before: not expected
	}}
	after := api.ServeCheck{Projects: []string{"orchid"}, Items: []api.ServeItem{
		{Kind: ItemWork, Name: "orchid", OK: true}, {Kind: ItemAdapter, Name: "orchid/bd", Error: "no zm-work-acme on PATH"},
	}}
	got := strings.Join(Regressions(before, after), "\n")
	if !strings.Contains(got, "issues orchid: served before, missing now") || !strings.Contains(got, "adapter orchid/bd: served before, now no zm-work-acme") ||
		strings.Contains(got, "gone") || strings.Contains(got, "gh") {
		t.Fatalf("regressions:\n%s", got)
	}
	// A joined project is expected even when after's lists drop it: a build
	// that cannot serve it does not list it either.
	before.Items = append(before.Items, api.ServeItem{Kind: ItemJoined, Name: "team", OK: true}, api.ServeItem{Kind: ItemIssues, Name: "team", OK: true})
	got = strings.Join(Regressions(before, after), "\n")
	if !strings.Contains(got, "joined team: served before, missing now") || !strings.Contains(got, "issues team: served before, missing now") {
		t.Fatalf("a joined project's absence is a regression:\n%s", got)
	}
}

// TestSplitRegressionsRemote is 2026-11-03: seconds after a start the new
// daemon's tsnet node is not up, so a joined project's reads fail
// unavailable "authority ... unreachable". Those can still come good;
// 2026-11-01's bad_request for the same project, and a local project's
// failure, cannot.
func TestSplitRegressionsRemote(t *testing.T) {
	before := api.ServeCheck{Projects: []string{"solo"}, Items: []api.ServeItem{
		{Kind: ItemJoined, Name: "orchid", OK: true}, {Kind: ItemIssues, Name: "orchid", OK: true},
		{Kind: ItemWork, Name: "orchid", OK: true}, {Kind: ItemAdapter, Name: "orchid/bd", OK: true},
		{Kind: ItemWork, Name: "solo", OK: true},
	}}
	unreachable := "authority zm-host-c.example.ts.net:7433 unreachable: dial tcp 100.64.0.5:7433: i/o timeout"
	after := api.ServeCheck{Projects: []string{"solo"}, Items: []api.ServeItem{
		{Kind: ItemIssues, Name: "orchid", Code: api.CodeUnavailable, Error: unreachable},
		{Kind: ItemWork, Name: "orchid", Code: api.CodeUnavailable, Error: unreachable},
		{Kind: ItemAdapter, Name: "orchid/bd", Code: api.CodeUnavailable, Error: unreachable},
		{Kind: ItemWork, Name: "solo", OK: true},
	}}
	local, remote := SplitRegressions(before, after)
	if len(local) != 0 || len(remote) != 4 { // joined orchid is missing too: its remote is not ok yet
		t.Fatalf("warming tailnet: local %v, remote %v", local, remote)
	}
	after.Items[1] = api.ServeItem{Kind: ItemWork, Name: "orchid", Code: api.CodeBadRequest, Error: `no project "orchid"`}
	after.Items[3] = api.ServeItem{Kind: ItemWork, Name: "solo", Code: api.CodeUnavailable, Error: "no zm-work-acme"}
	local, _ = SplitRegressions(before, after)
	if got := strings.Join(local, "\n"); len(local) != 2 || !strings.Contains(got, "work orchid") || !strings.Contains(got, "work solo") {
		t.Fatalf("a bad_request of a joined project and a local failure are local: %v", local)
	}
	if n := len(Regressions(before, after)); n != 5 {
		t.Fatalf("Regressions is both halves: %d", n)
	}
}

// TestPreflightNotesOneWaySwap: a build that registers migrations the
// home's data lacks says the swap is one-way.
func TestPreflightNotesOneWaySwap(t *testing.T) {
	nu := NewBuild(routes, func(string, string) bool { return false }, map[string][]string{KindRegistry: {"a/0001", "session/0008_presence_outcome"}})
	applied := map[string][]string{"registry.db": {"a/0001"}}
	kinds := map[string]string{"registry.db": KindRegistry}
	pf := Check(nu, nil, applied, kinds)
	if !pf.OK || !strings.Contains(strings.Join(pf.Notes, "\n"), "applies 1 migrations at its first start (registry session/0008_presence_outcome): the swap is one-way") {
		t.Fatalf("want a one-way note: %+v", pf)
	}
	applied["registry.db"] = append(applied["registry.db"], "session/0008_presence_outcome")
	if pf := Check(nu, nil, applied, kinds); strings.Contains(strings.Join(pf.Notes, "\n"), "one-way") {
		t.Fatalf("nothing to apply, no note: %+v", pf.Notes)
	}
}

// A running build that listed GET /v0/sessions/{id}/activity as forwarded
// (its sample path read as a session address; no real call was) does not
// hold back a build that does not list it.
func TestPreflightIgnoresNeverForwarded(t *testing.T) {
	rs := append([]string{"GET /v0/sessions/{id}/activity"}, routes...)
	old := build(rs, "GET /v0/sessions/x/activity", "GET /v0/work")
	nu := build(rs, "GET /v0/work")
	run := &Running{Health: api.Health{Protocol: api.Protocol, Version: "v1", Commit: "old"},
		Manifest: &api.DaemonManifest{Build: old, Joined: []string{"orchid"}}}
	if len(old.Proxied) != 2 {
		t.Fatalf("old build forwards %v", old.Proxied)
	}
	if pf := Check(nu, run, nil, nil); !pf.OK {
		t.Fatalf("preflight refused: %v", pf.Problems)
	}
	// Any other forward the new build lacks is still refused.
	old = build(rs, "GET /v0/sessions/x/activity", "GET /v0/work", "GET /v0/issues")
	run.Manifest.Build = old
	if pf := Check(nu, run, nil, nil); pf.OK {
		t.Fatal("a dropped forward passed")
	}
}
