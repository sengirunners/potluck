package hostprobe

import (
	"context"
	"github.com/sengirunners/potluck/computeapi"
	"strings"
	"testing"
	"time"
)

// Named images lend alone; placement checks the binding's advertised name.
func TestShareXcode(t *testing.T) {
	for _, c := range []struct {
		name                string
		root, set, wantRoot bool
		runner, wantRunner  string
	}{
		{"defaults", false, false, true, "", "actions-runner"},
		{"no root", false, true, false, "", "actions-runner"},
		{"no runner", false, false, true, "none", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := checkOn(context.Background(), nil, ShareOptions{Report: macReport(), With: "xcode", NamePrefix: "mac", DryRun: true, Root: c.root, RootSet: c.set, RunnerDir: c.runner})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Images) != 1 || !strings.Contains(res.Images[0], "macos-tahoe-xcode@sha256:39980f3a654be50fab35d8ead868b25b7b5c52adb59ec2fca23073c714cac9c6") || res.RunnerDir != c.wantRunner || res.Root != c.wantRoot {
				t.Fatalf("unexpected selection: %+v", res)
			}
			for _, s := range res.Sizes {
				if s.Config["image_name"] != "xcode" || s.Config["runner_dir"] != c.wantRunner || (s.Config["root"] == "on") != c.wantRoot || s.Size.CPU < 2 || s.Size.MemoryMB < 4096 {
					t.Fatalf("bad binding: %+v", s)
				}
			}
		})
	}
	for _, o := range []ShareOptions{
		{Report: macReport(), With: "missing"},
		{Report: bigReport(), With: "xcode"},
		{Report: macReport(), With: "xcode", Images: []string{pinA}},
	} {
		o.NamePrefix, o.DryRun = "mac", true
		if _, err := checkOn(context.Background(), nil, o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
}

func TestNamedPinValidation(t *testing.T) {
	for _, line := range []string{
		"tart darwin - name=Xcode", "tart darwin - name=xcode name=other",
		"tart darwin - root=off", "tart darwin - name=xcode template=agent",
		"tart darwin - name=xcode\ntart darwin - name=xcode",
		"tart darwin - min_free_gb=0", "tart darwin - pull_minutes=-1",
	} {
		if _, err := ParsePins(line); err == nil {
			t.Errorf("accepted %q", line)
		}
	}
	if _, err := ParsePins("tart darwin -\ntart darwin - name=xcode root=on"); err != nil {
		t.Fatal(err)
	}
}

func TestShareReusesOnlyExactTartCachePin(t *testing.T) {
	for _, c := range []struct {
		name, listing string
		exit          int
		reuse         bool
	}{
		{"exact OCI digest", `[{"Source":"OCI","Name":"` + pinA + `"}]`, 0, true},
		{"mutable local VM", `[{"Source":"local","Name":"` + pinA + `"}]`, 0, false},
		{"different digest", `[{"Source":"OCI","Name":"` + pinB + `"}]`, 0, false},
		{"tag", `[{"Source":"OCI","Name":"reg.example/base:latest"}]`, 0, false},
		{"unreadable", `not json`, 0, false},
		{"failed", `[]`, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := &fakeRunner{on: func(line string) (Result, bool) {
				if line == "tart list --format json" {
					return Result{Stdout: c.listing, Exit: c.exit}, true
				}
				return ok(""), true
			}}
			res, err := checkOn(context.Background(), run, ShareOptions{Report: macReport(), Images: []string{pinA}, NamePrefix: "mac", Sizes: []string{"c2m4"}})
			if err != nil {
				t.Fatal(err)
			}
			pulled := false
			for _, call := range run.calls {
				if call == "tart pull -- "+pinA {
					pulled = true
				}
			}
			if pulled == c.reuse || !res.Sizes[0].Passed {
				t.Fatalf("reuse=%v, calls=%v, result=%+v", c.reuse, run.calls, res)
			}
		})
	}
}

// The selected image's budgets govern its disk refusal and actual pull.
func TestXcodeBudgetsAndRerun(t *testing.T) {
	report := macReport()
	report.TartFreeGB = 250
	var budget time.Duration
	run := &fakeRunner{on: func(line string) (Result, bool) { return ok(""), true }}
	_, err := checkOn(context.Background(), deadlineRunner{Runner: run, budget: &budget}, ShareOptions{Report: report, With: "xcode", NamePrefix: "mac", Sizes: []string{"c2m4"}})
	if err != nil {
		t.Fatal(err)
	}
	if budget < 179*time.Minute || budget > 180*time.Minute {
		t.Fatalf("pull budget %s", budget)
	}
	earlier := []computeapi.RunTarget{{Name: "mac-c2m4", Images: []string{pinA}, Config: map[string]string{"vm": "tart", "image_name": "xcode"}}}
	o := ShareOptions{Report: report, Earlier: earlier, NamePrefix: "mac", DryRun: true}
	if _, err := checkOn(context.Background(), nil, o); err == nil || !strings.Contains(err.Error(), "repeat --with xcode") {
		t.Fatalf("silent replacement: %v", err)
	}
	o.With = "xcode"
	if _, err := checkOn(context.Background(), nil, o); err != nil {
		t.Fatal(err)
	}
	o.With = ""
	o.Images = []string{pinA}
	if _, err := checkOn(context.Background(), nil, o); err != nil {
		t.Fatal(err)
	}
	budget = 0
	o = ShareOptions{Report: report, With: "xcode", NamePrefix: "mac", Sizes: []string{"c2m4"}, PullTimeout: time.Minute}
	if _, err := checkOn(context.Background(), deadlineRunner{Runner: run, budget: &budget}, o); err != nil {
		t.Fatal(err)
	}
	if budget <= 0 || budget > time.Minute {
		t.Fatalf("explicit timeout ignored: %v", budget)
	}
}

type deadlineRunner struct {
	Runner
	budget *time.Duration
}

func (r deadlineRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if name == "tart" && len(args) > 0 && args[0] == "pull" {
		end, ok := ctx.Deadline()
		if ok {
			*r.budget = time.Until(end)
		}
	}
	return r.Runner.Run(ctx, name, args...)
}

// A machine lent on an earlier version of a published pin re-runs host with
// no flags and upgrades; an earlier Xcode digest still asks for --with xcode,
// and an image from no published repository is still refused.
func TestRerunAfterPinMoved(t *testing.T) {
	stale := func(layer, name string) string {
		for _, p := range DefaultPins() {
			if p.Layer == layer && p.Name == name && p.Template == "" && p.Image != "" {
				repo, _, _ := strings.Cut(p.Image, "@")
				return repo + "@sha256:" + strings.Repeat("0", 64)
			}
		}
		t.Fatalf("no %s pin named %q", layer, name)
		return ""
	}
	cases := []struct {
		image, want string
	}{
		{stale("tart", ""), ""},
		{stale("tart", "xcode"), "repeat --with xcode"},
		{"ghcr.io/example/other@sha256:" + strings.Repeat("1", 64), "unknown image"},
	}
	for _, c := range cases {
		earlier := []computeapi.RunTarget{{Name: "mac-c2m4", Images: []string{c.image}, Config: map[string]string{"vm": "tart"}}}
		_, err := checkOn(context.Background(), nil, ShareOptions{Report: macReport(), Earlier: earlier, NamePrefix: "mac", DryRun: true})
		if c.want == "" && err != nil {
			t.Fatalf("%s: upgrade refused: %v", c.image, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Fatalf("%s: got %v, want %q", c.image, err, c.want)
		}
	}
}

func TestPlainPinDuplicateDiagnostic(t *testing.T) {
	_, err := ParsePins("tart darwin -\ntart darwin -")
	if err == nil || !strings.Contains(err.Error(), "tart darwin twice") {
		t.Fatalf("duplicate plain pin: %v", err)
	}
}

func TestImplicitImageRequiresEarlierBindings(t *testing.T) {
	for _, dry := range []bool{false, true} {
		_, err := checkOn(context.Background(), &fakeRunner{}, ShareOptions{Report: macReport(), NamePrefix: "mac", DryRun: dry, EarlierUnread: "daemon unavailable"})
		if err == nil || !strings.Contains(err.Error(), "daemon unavailable") {
			t.Fatalf("dry=%v: %v", dry, err)
		}
	}
}
