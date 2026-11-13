package runners_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// Every decision must agree with the real parser and placement used for
// workflow_job deliveries, including refusals, defaults and explicit overrides.
func TestRunsOn(t *testing.T) {

	tests := []struct {
		name string
		edit func(*computeapi.RunTargetView)
		ttl  int
		os   string
	}{
		{name: "linux", os: "linux"},
		{name: "darwin", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.OS = "Darwin" }, os: "macos"},
		{name: "macos", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.OS = "macOS" }, os: "macos"},
		{name: "windows", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.OS = "WINDOWS" }, os: "windows"},
		{name: "mixed case linux", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.OS = "Linux" }, os: "linux"},
		{name: "unlimited", edit: func(v *computeapi.RunTargetView) { v.TTLMaxS = 0; v.Describe.TTLMaxS = 0 }, os: "linux"},
		{name: "binding unlimited", edit: func(v *computeapi.RunTargetView) { v.TTLMaxS = 0 }, os: "linux"},
		{name: "driver unlimited", edit: func(v *computeapi.RunTargetView) { v.Describe.TTLMaxS = 0 }, os: "linux"},
		{name: "custom job ttl", edit: func(v *computeapi.RunTargetView) { v.TTLMaxS = 2100 }, ttl: 1800, os: "linux"},
		{name: "binding too short", edit: func(v *computeapi.RunTargetView) { v.TTLMaxS = 3599 }},
		{name: "driver too short", edit: func(v *computeapi.RunTargetView) { v.Describe.TTLMaxS = 3599 }},
		{name: "longer job", ttl: 5400},
		{name: "public host", edit: func(v *computeapi.RunTargetView) { v.HostClass = api.MachinePublic }},
		{name: "unknown host", edit: func(v *computeapi.RunTargetView) { v.HostClass = "" }},
		{name: "not a box", edit: func(v *computeapi.RunTargetView) { v.Kind = "run" }},
		{name: "describe error", edit: func(v *computeapi.RunTargetView) { v.Error = "offline" }},
		{name: "no describe", edit: func(v *computeapi.RunTargetView) { v.Describe = nil }},
		{name: "no size", edit: func(v *computeapi.RunTargetView) { v.Describe.Size = nil }},
		{name: "busy", edit: func(v *computeapi.RunTargetView) { v.Describe.Available = false }},
		{name: "unguarded", edit: func(v *computeapi.RunTargetView) { v.Describe.Capabilities = []string{"github-jit"} }},
		{name: "no jit", edit: func(v *computeapi.RunTargetView) { v.Describe.Capabilities = []string{"github-jit-guard"} }},
		{name: "uppercase size", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.Name = "C1m2" }},
		{name: "empty size", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.Name = "" }},
		{name: "invalid size", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.Name = "bad:size" }},
		{name: "long size", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.Name = strings.Repeat("x", 33) }},
		{name: "unknown os", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.OS = "freebsd" }},
		{name: "empty os", edit: func(v *computeapi.RunTargetView) { v.Describe.Size.OS = "" }},
	}

	for _, mode := range []string{"unset default", "other default", "mapped here", "mapped away", "default mapped away"} {
		t.Run(mode, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					ttl := tt.ttl
					if ttl == 0 {
						ttl = 3600
					}
					f := newPoolFixture(t, func(c *config.Runners) {
						c.Label = "MyPool"
						c.JobTTLS = ttl
						if mode == "other default" {
							c.Pool.DefaultSize = "other-size"
						}
						if mode == "mapped here" {
							c.Pool.Sizes = map[string]string{"!unnameable": "box@host", "alias": "box@host", "z-alias": "box@host"}
						}
						if mode == "mapped away" || mode == "default mapped away" {
							c.Pool.Sizes = map[string]string{"c1m2": "other@host"}
							if mode == "default mapped away" {
								c.Pool.DefaultSize = "c1m2"
							}
						}
					})
					var setup runners.Setup
					if err := f.owner.Do(context.Background(), "GET", "/v0/runners/pool", nil, &setup); err != nil {
						t.Fatal(err)
					}
					v := computeapi.RunTargetView{
						RunTarget: computeapi.RunTarget{Name: "box@host", Kind: "box", TTLMaxS: 3900},
						HostClass: api.MachineTrusted,
						Describe: &computeapi.RunTargetDescribe{Available: true, TTLMaxS: 7200,
							Capabilities: []string{"github-jit", "github-jit-guard"},
							Size:         &computeapi.RunTargetSize{Name: "c1m2", OS: "linux"}},
					}
					if tt.edit != nil {
						tt.edit(&v)
					}
					labels, ok, why := runners.RunsOn(v, setup)
					expected := tt.os != "" && mode != "default mapped away"
					if ok != expected || (ok && why != "") || (!ok && (labels != nil || why == "")) {
						t.Fatalf("RunsOn = %v, %v, %q; want serves=%v", labels, ok, why, expected)
					}
					probe := labels
					if !ok {
						size := ""
						if v.Describe != nil && v.Describe.Size != nil {
							size = v.Describe.Size.Name
						}
						probe = []string{"mypool:" + size, "linux"}
						if mode == "mapped here" {
							probe = []string{"mypool:alias", "linux"}
						}
					}
					names, placeWhy := f.svc.FitLabels([]computeapi.RunTargetView{v}, probe)
					if slices.Contains(names, v.Name) != ok {
						t.Fatalf("parser+place(%v) = %v, %q; RunsOn = %v, %q", probe, names, placeWhy, ok, why)
					}
					if ok {
						size, os := "c1m2", tt.os
						if mode == "mapped here" {
							size = "alias"
							if os == "" {
								os = "linux"
							}
						}
						want := []string{"mypool:" + size, os}
						if mode == "mapped away" {
							want[0] = "mypool"
						}
						if !slices.Equal(labels, want) {
							t.Fatalf("labels %v, want %v", labels, want)
						}
					} else if placeWhy != "" && !strings.HasPrefix(placeWhy, "labels not served:") && why != placeWhy {
						t.Fatalf("reason %q differs from placement %q", why, placeWhy)
					}
					// A pool-only job must also reject unnameable driver sizes when no
					// default_size or override hides automatic placement's size rule.
					if mode == "unset default" && strings.Contains(tt.name, "size") && tt.os == "" {
						if names, why := f.svc.FitLabels([]computeapi.RunTargetView{v}, []string{"mypool"}); len(names) != 0 {
							t.Fatalf("pool-only job placed on invalid size: %v %s", names, why)
						}
					}
				})
			}
		})
	}
}

// A caller off the broker's machine can read both inputs without access to
// the local setup route or guessing the host's trust class.
func TestRunsOnRemoteReader(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.JobTTLS = 1800 })
	ctx := context.Background()
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "pool", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	remote := client.WithHandler(f.dm.Handler)
	remote.Token, remote.Project, remote.ProjectFrom = tok.Secret, "pool", "flag"
	var setup runners.Setup
	if err := remote.Do(ctx, "GET", "/v0/runners/pool", nil, &setup); err != nil {
		t.Fatal(err)
	}
	if setup.Label != "pool" || setup.JobTTLS != 1800 || setup.App != nil || setup.Listen != "" || setup.PublicURL != "" {
		t.Fatalf("remote pool inputs: %+v", setup)
	}
	if err := remote.Do(ctx, "GET", "/v0/runners/setup", nil, nil); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("remote local setup: %v", err)
	}
	guest := client.WithHandler(f.dm.Handler)
	if err := guest.Do(ctx, "GET", "/v0/runners/pool", nil, nil); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("anonymous pool read: %v", err)
	}
	if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	other, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "elsewhere", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	guest.Token = other.Secret
	if err := guest.Do(ctx, "GET", "/v0/runners/pool", nil, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("unrelated project's pool read: %v", err)
	}
}

// Project-scoped views carry the trust class needed by remote callers.
func TestRunsOnRemoteTarget(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.JobTTLS = 1800 }, box("small", "c1m2", "darwin", "1"))
	ctx := context.Background()
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "pool", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	remote := client.WithHandler(f.dm.Handler)
	remote.Token, remote.Project, remote.ProjectFrom = tok.Secret, "pool", "flag"
	var setup runners.Setup
	if err := remote.Do(ctx, "GET", "/v0/runners/pool", nil, &setup); err != nil {
		t.Fatal(err)
	}
	var views computeapi.RunTargetList
	if err := remote.Do(ctx, "GET", "/v0/run_targets?describe=1", nil, &views); err != nil {
		t.Fatal(err)
	}
	if len(views.Targets) != 1 {
		t.Fatalf("views: %+v", views)
	}
	labels, ok, why := runners.RunsOn(views.Targets[0], setup)
	names, placeWhy := f.svc.FitLabels(views.Targets, labels)
	if !ok || !slices.Equal(labels, []string{"pool:c1m2", "macos"}) || !slices.Equal(names, []string{"small"}) {
		t.Fatalf("remote RunsOn: %v %v %s; place: %v %s", labels, ok, why, names, placeWhy)
	}
}

// Unrelated token holders see the same response for healthy, absent and
// broken pool configuration, including the existing links and jobs readers.
func TestRunsOnPoolLookupPrivacy(t *testing.T) {
	for _, pool := range []string{"pool", "", "private-missing-pool"} {
		t.Run(pool, func(t *testing.T) {
			f := newPoolFixture(t, func(c *config.Runners) { c.Pool.Project = pool })
			ctx := context.Background()
			if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "elsewhere"}); err != nil {
				t.Fatal(err)
			}
			tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "elsewhere", Role: "member"})
			if err != nil {
				t.Fatal(err)
			}
			other := client.WithHandler(f.dm.Handler)
			other.Token = tok.Secret
			for _, path := range []string{"/v0/runners/pool", "/v0/runners/links", "/v0/runners/jobs"} {
				err := other.Do(ctx, "GET", path, nil, nil)
				if !api.IsCode(err, api.CodeNotFound) || strings.Contains(err.Error(), "private-missing-pool") || strings.Contains(err.Error(), "not set up") {
					t.Fatalf("%s: leaked pool state: %v", path, err)
				}
			}
			if pool != "pool" {
				if err := f.owner.Do(ctx, "GET", "/v0/runners/pool", nil, nil); !api.IsCode(err, api.CodeUnavailable) {
					t.Fatalf("owner diagnostic: %v", err)
				}
			}
		})
	}
}

// Default eligibility and a real queued delivery share alias selection and
// the same fallback-plus-startup budget; lowering a later job's fallback
// changes both without letting the first selected job silently shorten.
func TestRunsOnAliasUsesFallbackPlusStartup(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.JobTTLS = 3301; c.Pool.Sizes = map[string]string{"friendly": "a"} }, box("a", "c1m2", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != 200 {
		t.Fatalf("link: %s", w.Body)
	}
	ctx := context.Background()
	var views computeapi.RunTargetList
	if err := f.owner.Do(ctx, "GET", "/v0/run_targets?describe=1", nil, &views); err != nil {
		t.Fatal(err)
	}
	if len(views.Targets) != 1 {
		t.Fatalf("views: %+v", views)
	}
	var setup runners.Setup
	if err := f.owner.Do(ctx, "GET", "/v0/runners/pool", nil, &setup); err != nil {
		t.Fatal(err)
	}
	if labels, ok, why := runners.RunsOn(views.Targets[0], setup); ok || labels != nil || !strings.Contains(why, "3601") {
		t.Fatalf("eligibility bypassed startup lifetime via alias: %v %v %s", labels, ok, why)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 99131, 1001, "acme/app", true, "pool:friendly", "linux"))
	eventually(t, "actual broker refuses same alias budget", func() bool { j := f.jobs(t)[99131]; return j.State == "waiting" && strings.Contains(j.Why, "3601") })
	if _, err := f.set("job_ttl_s", "3000"); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Do(ctx, "GET", "/v0/runners/pool", nil, &setup); err != nil {
		t.Fatal(err)
	}
	labels, ok, why := runners.RunsOn(views.Targets[0], setup)
	if !ok || !slices.Equal(labels, []string{"pool:friendly", "linux"}) {
		t.Fatalf("fitting alias refused: %v %v %s", labels, ok, why)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 99132, 1002, "acme/app", true, labels...))
	eventually(t, "published labels actually place next job", func() bool { j := f.jobs(t)[99132]; return j.State == "serving" && j.Target == "a" && j.LimitS == 3000 })
	if j := f.jobs(t)[99131]; j.LimitS != 3301 || j.State != "waiting" {
		t.Fatalf("earlier selected policy changed: %+v", j)
	}
}
