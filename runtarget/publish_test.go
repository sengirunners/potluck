package runtarget_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// publishDaemon serves a run target "vm" with the reference driver (with bug,
// behind front, when it is not nil: front wraps the driver's path) that runs
// script, the commit's probe.sh, with ZM_PUBLISH_FILE=out/pub.json in the
// daemon's environment; it answers the owner's client.
func publishDaemon(t *testing.T, bug, script string, front func(ref string) string) *client.Client {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZM_PUBLISH_FILE", "out/pub.json")
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	repo, err := refdriver.NewRepo(fix, script)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, bug)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	if front != nil {
		drv = front(drv)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{
		Name: "vm", Driver: "ref", Kind: "run", Isolation: "ephemeral", Repo: repo.Remote, Branch: "main",
		Config: map[string]string{"remote": repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	return owner
}

// publishRun runs script on publishDaemon and answers the handle's status
// when it ended, or after wait if it never does, with the handle list and a
// later status.
func publishRun(t *testing.T, bug, script string, wait time.Duration) (st computeapi.RunTargetHandle, listed []computeapi.RunTargetHandle, later computeapi.RunTargetHandle) {
	t.Helper()
	return publishRunOn(t, publishDaemon(t, bug, script, nil), wait)
}

// publishRunOn is publishRun on a daemon publishDaemon made.
func publishRunOn(t *testing.T, owner *client.Client, wait time.Duration) (st computeapi.RunTargetHandle, listed []computeapi.RunTargetHandle, later computeapi.RunTargetHandle) {
	t.Helper()
	ctx := context.Background()
	h, err := computeclient.AcquireRunTarget(ctx, owner, "vm", computeapi.AcquireRunTargetRequest{RequestID: "pub1", Ref: "main", Script: "probe.sh", TTLS: 60})
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(wait); ; time.Sleep(100 * time.Millisecond) {
		if st, err = computeclient.RunTargetStatus(ctx, owner, "vm", idOf(h.Handle)); err != nil {
			t.Fatal(err)
		}
		if st.State == "ended" || !time.Now().Before(end) {
			break
		}
	}
	if listed, err = computeclient.RunTargetHandles(ctx, owner, "vm", false); err != nil {
		t.Fatal(err)
	}
	if later, err = computeclient.RunTargetStatus(ctx, owner, "vm", idOf(h.Handle)); err != nil {
		t.Fatal(err)
	}
	return st, listed, later
}

const publishScript = "#!/bin/sh\nmkdir -p \"$(dirname \"$ZM_PUBLISH_FILE\")\"\nprintf '{ \"url\": \"https://example.test/a\",\\n \"n\": 2 }' > \"$ZM_PUBLISH_FILE\"\nexit 0\n"

// An ended run's handle carries what its script left in the file named by
// ZM_PUBLISH_FILE, in the status answer, the handle list and every later status.
func TestEndedRunCarriesItsPublish(t *testing.T) {
	st, listed, later := publishRun(t, "", publishScript, 20*time.Second)
	const want = `{"url":"https://example.test/a","n":2}`
	if st.State != "ended" || string(st.Publish) != want {
		t.Fatalf("status %s publish %q, want ended and %s", st.State, st.Publish, want)
	}
	if len(listed) != 1 || string(listed[0].Publish) != want || string(later.Publish) != want {
		t.Errorf("listed %+v, later %q", listed, later.Publish)
	}
	if b, _ := json.Marshal(st); !json.Valid(b) {
		t.Errorf("the handle does not marshal: %s", b)
	}
}

// A daemon whose own environment names no publish file (unset or empty)
// names protocol.DefaultPublishFile, so a run's script can publish on a
// machine nobody set up for it.
func TestRunPublishesWithoutTheDaemonNamingAFile(t *testing.T) {
	for name, unset := range map[string]bool{"unset": true, "empty": false} {
		t.Run(name, func(t *testing.T) {
			script := "#!/bin/sh\n[ \"$ZM_PUBLISH_FILE\" = \"$PWD/.potluck-publish.json\" ] || exit 7\necho '{\"n\":1}' > \"$ZM_PUBLISH_FILE\"\n"
			owner := publishDaemon(t, "", script, nil)
			t.Setenv("ZM_PUBLISH_FILE", "")
			if unset {
				os.Unsetenv("ZM_PUBLISH_FILE")
			}
			st, _, _ := publishRunOn(t, owner, 20*time.Second)
			if st.State != "ended" || st.Exit == nil || *st.Exit != 0 || string(st.Publish) != `{"n":1}` {
				exit := -1
				if st.Exit != nil {
					exit = *st.Exit
				}
				t.Errorf("state %s exit %d publish %q, want ended, 0 and {\"n\":1}", st.State, exit, st.Publish)
			}
		})
	}
}

// A handle that has not ended carries no publish, whatever its driver says
// (the reference driver with publish-early answers the file while it runs); and
// a publish over 64 KiB is dropped, whatever its driver says.
func TestDaemonBoundsAPublish(t *testing.T) {
	st, _, _ := publishRun(t, "publish-early", "#!/bin/sh\nmkdir -p out\necho '{\"early\":true}' > out/pub.json\nsleep 30\n", 3*time.Second)
	if st.State != "running" || len(st.Publish) != 0 {
		t.Errorf("a running run: state %s, publish %q", st.State, st.Publish)
	}
	big := "#!/bin/sh\nmkdir -p out\n{ printf '{\"x\":\"'; head -c 70000 /dev/zero | tr '\\0' a; printf '\"}'; } > out/pub.json\nexit 0\n"
	st, listed, _ := publishRun(t, "publish-uncapped", big, 20*time.Second)
	if st.State != "ended" || len(st.Publish) != 0 || len(listed) != 1 || len(listed[0].Publish) != 0 {
		t.Errorf("an oversize publish: state %s, publish %d bytes", st.State, len(st.Publish))
	}
}

// A run that ended before its acquire was found again (the driver made it and
// failed without an answer; list finds it ended) still carries its publish and
// exit: they are asked of the driver once, when it is found.
func TestRunFoundEndedByListCarriesItsPublish(t *testing.T) {
	ctx := context.Background()
	fail := filepath.Join(t.TempDir(), "fail-next-acquire")
	owner := publishDaemon(t, "", publishScript, func(ref string) string {
		return inFront(t, ref, fmt.Sprintf(`case "$in" in *'"verb":"acquire"'*) if [ -e %q ]; then rm -f %q; sleep 2; echo 'not an answer'; exit 1; fi ;; esac`, fail, fail))
	})
	touch(t, fail)
	req := computeapi.AcquireRunTargetRequest{RequestID: "lost1", Ref: "main", Script: "probe.sh", TTLS: 60}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "vm", req); err == nil {
		t.Fatal("the acquire that failed after making the run answered a handle")
	}
	var h computeapi.RunTargetHandle
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		hs, err := computeclient.RunTargetHandles(ctx, owner, "vm", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(hs) == 1 && hs[0].State == "ended" {
			h = hs[0]
			break
		}
	}
	const want = `{"url":"https://example.test/a","n":2}`
	if h.State != "ended" || string(h.Publish) != want || h.Exit == nil || *h.Exit != 0 {
		t.Errorf("the run found ended: %+v, want publish %s and exit 0", h, want)
	}
}
