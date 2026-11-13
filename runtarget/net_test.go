package runtarget_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestBoxNet (zm-eakh.12.5.19): a box on this machine (a
// trusted host) is sent net on, so a driver that honours net (capability
// net-off) makes it on the binding's filtered network; net off is sent as
// off and is part of the request. A driver without the capability is sent
// nothing, as before, and an off it cannot honour is refused.
func TestBoxNet(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
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
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	seen := filepath.Join(fix, "requests")
	drv := filepath.Join(fix, "zm-run-tap")
	if err := os.WriteFile(drv, []byte(fmt.Sprintf("#!/bin/sh\ntee -a %q | exec %q\n", seen, ref)), 0o755); err != nil {
		t.Fatal(err)
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
	for name, c := range map[string]map[string]string{"netbox": {"net_off": "1"}, "abox": nil,
		"zliar": {"net_off": "1", "net_answer": "on"}, "zmute": {"net_off": "1", "net_answer": "omit"}} {
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: name, Driver: "ref", Kind: "box",
			Isolation: "ephemeral", Images: []string{refPin}, Config: c}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "runner", Driver: "ref", Kind: "run",
		Isolation: "ephemeral", Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	sentNet := func(key string) string {
		b, _ := os.ReadFile(seen)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		got := "(not sent)"
		for {
			var env struct {
				Verb string `json:"verb"`
				Args struct {
					RequestID string  `json:"request_id"`
					Net       *string `json:"net"`
				} `json:"args"`
			}
			if dec.Decode(&env) != nil {
				return got
			}
			if env.Verb == "acquire" && strings.HasSuffix(env.Args.RequestID, "-"+key) {
				got = "(unset)"
				if env.Args.Net != nil {
					got = *env.Args.Net
				}
			}
		}
	}
	acquire := func(target, key, net string) (computeapi.RunTargetHandle, error) {
		h, err := computeclient.AcquireRunTarget(ctx, owner, target, computeapi.AcquireRunTargetRequest{RequestID: key, TTLS: 120, Net: net})
		if err == nil {
			t.Cleanup(func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, target, idOf(h.Handle)) })
		}
		return h, err
	}
	for _, c := range []struct{ key, net, want string }{{"d1", "", "on"}, {"d2", "on", "on"}, {"d3", "off", "off"}} {
		h, err := acquire("netbox", c.key, c.net)
		if err != nil {
			t.Fatalf("net %q: %v", c.net, err)
		}
		if h.Net != c.want { // the answer says what the driver made
			t.Errorf("net %q: the answer says net %q, want %s", c.net, h.Net, c.want)
		}
		if got := sentNet(c.key); got != c.want {
			t.Errorf("net %q on this machine: the driver was sent %s, want %s", c.net, got, c.want)
		}
	}
	// on is the default: the same request as unset; off is another.
	if _, err := acquire("netbox", "d1", "on"); err != nil {
		t.Errorf("request_id d1 again with net on: %v", err)
	}
	if _, err := acquire("netbox", "d1", "off"); code(err) != api.CodeConflict {
		t.Errorf("request_id d1 again with net off: %v, want conflict", err)
	}
	if _, err := acquire("netbox", "bad", "yes"); code(err) != api.CodeBadRequest {
		t.Errorf("net yes: %v, want bad_request", err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "runner", computeapi.AcquireRunTargetRequest{RequestID: "r1", Ref: "main", Script: "probe.sh", Net: "off"}); code(err) != api.CodeBadRequest {
		t.Errorf("net on a run: %v, want bad_request", err)
	}
	// Fail closed: a driver sent net off that does not answer exactly off (a
	// host that downgraded zm-run-tart while its advert still lists net-off)
	// may have made the box with a network: zm releases it and says so.
	for _, target := range []string{"zliar", "zmute"} {
		_, err := computeclient.AcquireRunTarget(ctx, owner, target, computeapi.AcquireRunTargetRequest{RequestID: "x-" + target, TTLS: 120, Net: "off"})
		if code(err) != api.CodeInternal || !strings.Contains(err.Error(), "released") {
			t.Errorf("%s: net off answered otherwise: %v, want internal, released", target, err)
		}
		hs, err := computeclient.RunTargetHandles(ctx, owner, target, true)
		if err != nil || len(hs) != 0 {
			t.Errorf("%s: live handles after the refusal: %+v %v", target, hs, err)
		}
	}
	if h, err := acquire("zmute", "m-on", ""); err != nil || h.Net != "" { // net on: a driver that says nothing is believed as before
		t.Errorf("mute, net on: %+v %v", h, err)
	}
	// Placement for net off skips a binding whose driver cannot honour it.
	if got, err := computeclient.PlaceBox(ctx, owner, computeclient.BoxWant{}); err != nil || len(got) == 0 || got[0] != "abox" {
		t.Errorf("placement: %q %v, want abox (first by name)", got, err)
	}
	if got, err := computeclient.PlaceBox(ctx, owner, computeclient.BoxWant{Net: computeapi.NetOff}); err != nil || len(got) == 0 || got[0] != "netbox" {
		t.Errorf("placement for net off: %q %v, want netbox", got, err)
	}
	// A driver that does not list net-off: nothing sent, and off refused before it.
	if _, err := acquire("abox", "o1", ""); err != nil {
		t.Fatal(err)
	}
	if h, _ := acquire("abox", "o1", ""); h.Net != "" {
		t.Errorf("a driver that says nothing of net: the answer says %q", h.Net)
	}
	if got := sentNet("o1"); got != "(unset)" {
		t.Errorf("a driver without net-off was sent net %s", got)
	}
	if _, err := acquire("abox", "o2", "off"); code(err) != api.CodeBadRequest || !strings.Contains(err.Error(), "net-off") {
		t.Errorf("net off on a driver without net-off: %v, want bad_request", err)
	}
	if got := sentNet("o2"); got != "(not sent)" {
		t.Errorf("an off it cannot honour reached the driver: %s", got)
	}
}
