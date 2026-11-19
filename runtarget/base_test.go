package runtarget_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/protocol"
)

var testBaseRepo = strings.Repeat("ab", 32)

func newBaseFix(t *testing.T) *depsFix {
	t.Helper()
	f := newDepsFixConfig(t, map[string]string{"net_off": "1"})
	t.Cleanup(func() {
		for _, c := range []*client.Client{f.ana, f.bo, f.owner} {
			handles, _ := computeclient.RunTargetHandles(context.Background(), c, "deps", true)
			for _, h := range handles {
				_, _ = computeclient.ReleaseRunTarget(context.Background(), c, "deps", idOf(h.Handle))
			}
		}
	})
	return f
}

func baseReq(id string, fill bool) computeapi.AcquireRunTargetRequest {
	return computeapi.AcquireRunTargetRequest{RequestID: id, Base: &computeapi.BoxBase{Repo: testBaseRepo, Fill: fill}}
}

func baseReason(err error) string {
	var e *api.Error
	var v struct{ Reason string }
	if errors.As(err, &e) {
		_ = json.Unmarshal(e.Data, &v)
	}
	return v.Reason
}

func releaseBase(ctx context.Context, c *client.Client, target string, h computeapi.RunTargetHandle, seal bool) error {
	var out computeapi.RunTargetRelease
	return c.Do(ctx, http.MethodPost, "/v0/run_targets/"+client.Seg(target)+"/handles/"+client.Seg(idOf(h.Handle))+"/release", computeapi.ReleaseRunTargetRequest{Seal: seal}, &out)
}

func fillBase(t *testing.T, c *client.Client, target, id string) computeapi.RunTargetHandle {
	t.Helper()
	h, err := computeclient.AcquireRunTarget(context.Background(), c, target, baseReq(id, true))
	if err != nil {
		t.Fatal(err)
	}
	x, err := computeclient.ExecRunTarget(context.Background(), c, target, idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", `mkdir -p "$HOME/.potluck-base/objects" "$HOME/.potluck-base/refs/heads" && printf object > "$HOME/.potluck-base/objects/marker" && printf commit > "$HOME/.potluck-base/refs/heads/base" && printf private > "$HOME/.potluck-base/discard"`}})
	if err != nil || x.Exit != 0 {
		t.Fatalf("fill: %+v %v", x, err)
	}
	if err := releaseBase(context.Background(), c, target, h, true); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPushBaseLifecycle(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	_, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", baseReq("reader", false))
	if !api.IsCode(err, api.CodeNotFound) || baseReason(err) != computeapi.BaseMissing {
		t.Fatalf("missing base: %v", err)
	}
	if got := f.requests(t, "acquire", true); len(got) != 0 {
		t.Fatalf("missing made %d boxes", len(got))
	}
	req := baseReq("builder", true)
	req.Ref, req.Commit, req.JITConfig, req.Script, req.Name = "unwanted", "unwanted", "unwanted", "unwanted", "unwanted"
	req.Net, req.Persist, req.SelfRenew, req.TTLS = computeapi.NetOn, true, false, 10000
	req.Deps, req.Volumes = []computeapi.BoxDeps{npmDeps("unwanted")}, []computeapi.BoxVolume{{Name: "cache", Path: "cache"}}
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if err != nil {
		t.Fatal(err)
	}
	if h.Base == nil || h.Base.Hit || h.Base.Path != ".potluck-base" || h.Net != computeapi.NetOff || !h.SelfRenew || h.TTLS > 900 {
		t.Fatalf("builder: %+v", h)
	}
	if len(h.Volumes) != 1 || h.Volumes[0].Mode != protocol.VolumeBuild {
		t.Fatalf("builder volumes %+v", h.Volumes)
	}
	for _, a := range f.builders(t) {
		for _, k := range []string{"ref", "commit", "jit_config", "persist", "deps", "script", "name"} {
			if _, ok := a[k]; ok {
				t.Errorf("builder sent %s", k)
			}
		}
	}
	// A repeat is the same builder, but another request meets its lock.
	again, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if err != nil || again.Handle != h.Handle {
		t.Fatalf("repeat %+v %v", again, err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, f.ana, "deps", baseReq("second-builder", true))
	if !api.IsCode(err, api.CodeUnavailable) || baseReason(err) != protocol.ReasonBuilding {
		t.Fatalf("second builder %v", err)
	}
	if _, err := computeclient.RenewRunTarget(ctx, f.ana, "deps", idOf(h.Handle), 60); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("renew builder: %v", err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, f.ana, "deps", idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"true"}, Secrets: map[string]string{"TOKEN": "private"}}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("builder secrets: %v", err)
	}
	if _, err := computeclient.SpawnRunTarget(ctx, f.ana, "deps", idOf(h.Handle), computeapi.SpawnRunTargetRequest{RequestID: "secret", Argv: []string{"true"}, Secrets: map[string]string{"TOKEN": "private"}}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("builder spawn secrets: %v", err)
	}
	var secretsOut any
	if err := f.ana.Do(ctx, http.MethodPost, "/v0/run_targets/deps/handles/"+client.Seg(idOf(h.Handle))+"/secrets", map[string]any{"secrets": map[string]string{"TOKEN": "private"}}, &secretsOut); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("builder box secrets: %v", err)
	}
	var releaseOut any
	if err := f.ana.Do(ctx, http.MethodPost, "/v0/run_targets/deps/handles/"+client.Seg(idOf(h.Handle))+"/release", map[string]any{"seal": true, "keep": []string{"discard"}}, &releaseOut); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("caller keep: %v", err)
	}
	if err := releaseBase(ctx, f.owner, "deps", h, true); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("owner sealing another holder: %v", err)
	}
	x := boxExec(t, f.ana, h, `mkdir -p "$HOME/.potluck-base/objects" "$HOME/.potluck-base/refs/heads"; echo object > "$HOME/.potluck-base/objects/marker"; echo commit > "$HOME/.potluck-base/refs/heads/base"; echo private > "$HOME/.potluck-base/discard"`)
	if x.Exit != 0 {
		t.Fatalf("fill %+v", x)
	}
	if err := releaseBase(ctx, f.ana, "deps", h, true); err != nil {
		t.Fatal(err)
	}
	got, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", baseReq("reader", false))
	if err != nil || got.Base == nil || !got.Base.Hit {
		t.Fatalf("hit %+v %v", got, err)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Mode != computeapi.VolumeRO || !got.Volumes[0].Sealed {
		t.Fatalf("hit volumes %+v", got.Volumes)
	}
	x = boxExec(t, f.ana, got, `cat "$HOME/.potluck-base/objects/marker"; cat "$HOME/.potluck-base/refs/heads/base"; test ! -e "$HOME/.potluck-base/discard"`)
	if x.Exit != 0 || !strings.Contains(x.Stdout, "object") || !strings.Contains(x.Stdout, "commit") {
		t.Fatalf("sealed contents %+v", x)
	}
	if err := releaseBase(ctx, f.ana, "deps", got, true); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("seal reader: %v", err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, f.ana, "deps", baseReq("sealed-builder", true))
	if !api.IsCode(err, api.CodeConflict) {
		t.Errorf("fill sealed base: %v", err)
	}
	seals := 0
	for _, a := range f.requests(t, "release") {
		if string(a["seal"]) == "true" {
			var keep []string
			_ = json.Unmarshal(a["keep"], &keep)
			if !slices.Equal(keep, []string{"objects", "refs"}) {
				t.Errorf("keep %v", keep)
			}
			seals++
		}
	}
	if seals != 1 {
		t.Errorf("seal calls %d", seals)
	}
}

func TestPushBaseIsolationAndDiscard(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	first := fillBase(t, f.ana, "deps", "fill")
	_, err := computeclient.AcquireRunTarget(ctx, f.bo, "deps", baseReq("other-holder", false))
	if baseReason(err) != computeapi.BaseMissing {
		t.Fatalf("other holder: %v", err)
	}
	other, err := computeclient.AcquireRunTarget(ctx, f.bo, "deps", baseReq("other-fill", true))
	if err != nil {
		t.Fatal(err)
	}
	if other.Volumes[0].Name != first.Volumes[0].Name {
		t.Fatal("holder belongs in volume owner, not repository name")
	}
	if err := releaseBase(ctx, f.bo, "deps", other, false); err != nil {
		t.Fatal(err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, f.bo, "deps", baseReq("discarded", false))
	if baseReason(err) != computeapi.BaseMissing {
		t.Fatalf("discarded: %v", err)
	}
	// Same actor and binding name in a different project gets a different volume.
	_, err = f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "other"})
	if err != nil {
		t.Fatal(err)
	}
	otherProject := *f.owner
	otherProject.Project = "other"
	_, err = computeclient.BindRunTarget(ctx, &otherProject, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "deps", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"net_off": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	req := baseReq("project-fill", true)
	req.Actor = first.Actor
	p, err := computeclient.AcquireRunTarget(ctx, &otherProject, "deps", req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Volumes[0].Name == first.Volumes[0].Name {
		t.Fatal("projects share base name")
	}
	f.svc.SetNow(func() time.Time { return time.Now().Add(7 * 24 * time.Hour) })
	t.Cleanup(func() { f.svc.SetNow(time.Now) })
	week, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", baseReq("next-week", true))
	if err != nil {
		t.Fatal(err)
	}
	if week.Volumes[0].Name == first.Volumes[0].Name {
		t.Fatal("weeks share base name")
	}
}

func TestPushBaseWithDependencies(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	req := baseReq("both", false)
	req.Deps = []computeapi.BoxDeps{npmDeps("app")}
	_, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if baseReason(err) != computeapi.BaseMissing {
		t.Fatalf("both missing: %v", err)
	}
	if len(f.builders(t)) != 0 {
		t.Fatal("base miss started a dependency builder")
	}
	fillBase(t, f.ana, "deps", "fill")
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if err != nil || h.Base == nil || !h.Base.Hit || len(h.Deps) != 1 || !h.Deps[0].Built || len(h.Volumes) != 2 {
		t.Fatalf("base plus built deps: %+v %v", h, err)
	}
	req.RequestID = "both-hit"
	h, err = computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if err != nil || h.Base == nil || !h.Base.Hit || len(h.Deps) != 1 || !h.Deps[0].Hit {
		t.Fatalf("both hit: %+v %v", h, err)
	}
}

func TestPushBaseValidationAndFallback(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	for i, edit := range []func(*computeapi.AcquireRunTargetRequest){
		func(r *computeapi.AcquireRunTargetRequest) { r.Base.Repo = "not-a-key" },
		func(r *computeapi.AcquireRunTargetRequest) {
			r.Volumes = []computeapi.BoxVolume{{Name: "mine", Path: ".potluck-base/objects"}}
		},
		func(r *computeapi.AcquireRunTargetRequest) {
			r.Deps = []computeapi.BoxDeps{{Kind: "npm", Path: ".potluck-base"}}
		},
		func(r *computeapi.AcquireRunTargetRequest) {
			r.Base = nil
			r.Volumes = []computeapi.BoxVolume{{Name: "base-stolen", Path: "other"}}
		},
		func(r *computeapi.AcquireRunTargetRequest) {
			for n := 0; n < computeapi.VolumesMax; n++ {
				r.Volumes = append(r.Volumes, computeapi.BoxVolume{Name: fmt.Sprintf("v%d", n), Path: fmt.Sprintf("v%d", n)})
			}
		},
	} {
		req := baseReq(fmt.Sprintf("invalid%d", i), false)
		edit(&req)
		if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("invalid %d: %v", i, err)
		}
	}
	if len(f.requests(t, "acquire")) != 0 {
		t.Fatal("invalid request reached driver")
	}
	for i, conf := range []map[string]string{{"volumes": "0"}, {"seal": "0", "net_off": "1"}, {"keep": "0", "net_off": "1"}, {}} {
		target := fmt.Sprintf("plain%d", i)
		_, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: target, Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, Config: conf}})
		if err != nil {
			t.Fatal(err)
		}
		h, err := computeclient.AcquireRunTarget(ctx, f.ana, target, baseReq("plain-"+target, false))
		if err != nil || h.Base == nil || h.Base.Hit || h.Base.Why == "" || len(h.Volumes) != 0 {
			t.Fatalf("unsupported: %+v %v", h, err)
		}
		_, err = computeclient.AcquireRunTarget(ctx, f.ana, target, baseReq("fill-"+target, true))
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("unsupported fill: %v", err)
		}
	}
}

// Expiry cannot publish a partially filled base, even when the holder later
// asks to seal it. A base miss also leaves its request ID free for fallback.
func TestPushBaseExpiredBuilderAndFallback(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	req := baseReq("short", true)
	req.TTLS = 1
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if err != nil {
		t.Fatal(err)
	}
	if h.TTLS > 1 {
		t.Fatalf("short builder TTL %d", h.TTLS)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := releaseBase(ctx, f.ana, "deps", h, true); err != nil {
		t.Fatal(err)
	}
	req = baseReq("fallback", false)
	_, err = computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if baseReason(err) != computeapi.BaseMissing {
		t.Fatalf("expired base: %v", err)
	}
	req.Base = nil
	plain, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if err != nil || plain.Base != nil || len(plain.Volumes) != 0 {
		t.Fatalf("same-ID fallback: %+v %v", plain, err)
	}
	if err := releaseBase(ctx, f.ana, "deps", plain, true); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("seal ordinary box: %v", err)
	}
}

func TestPushBaseRetryKeepsItsWeek(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	before := time.Date(2026, time.November, 22, 23, 59, 59, 0, time.UTC)
	f.svc.SetNow(func() time.Time { return before })
	fillBase(t, f.ana, "deps", "seed-week")
	readerReq := baseReq("week-reader", false)
	reader, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", readerReq)
	if err != nil {
		t.Fatal(err)
	}
	builderReq := baseReq("week-builder", true)
	builderReq.Base.Repo = strings.Repeat("cd", 32)
	builder, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", builderReq)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.SetNow(func() time.Time { return before.Add(2 * time.Second) })
	for _, tc := range []struct {
		req computeapi.AcquireRunTargetRequest
		h   computeapi.RunTargetHandle
	}{{readerReq, reader}, {builderReq, builder}} {
		got, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", tc.req)
		if err != nil || got.Handle != tc.h.Handle || got.Volumes[0].Name != tc.h.Volumes[0].Name {
			t.Fatalf("week replay: %+v %v, original %+v", got, err, tc.h)
		}
		changed := tc.req
		changed.Base = &computeapi.BoxBase{Repo: strings.Repeat("ef", 32), Fill: tc.req.Base.Fill}
		if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", changed); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("changed repo reused request: %v", err)
		}
	}
}

func TestPushBaseRetryAfterPlainFallback(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	req := baseReq("fallback-reader", false)
	plain := req
	plain.Base = nil
	first, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", plain)
	if err != nil {
		t.Fatal(err)
	}
	// A client that lost the fallback response starts over with its base
	// preference. It must recover the same plain box and disclose no hit.
	again, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
	if err != nil || again.Handle != first.Handle || again.Base == nil || again.Base.Hit || len(again.Volumes) != 0 {
		t.Fatalf("fallback replay: %+v %v", again, err)
	}
	req.TTLS = 123
	if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("changed settings accepted: %v", err)
	}
}
