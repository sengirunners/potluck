package runtarget_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/runtarget"
)

// newDepsRelay is a relay pair whose host is trusted and builds layers
// with the test recipe; relay, when set, sees every op's answer first.
func newDepsRelay(t *testing.T, relay func(op computeapi.RunTargetOp, res json.RawMessage) json.RawMessage) (*relayPair, api.Project) {
	t.Helper()
	rp := newRelayPair(t)
	rp.svc.SetHostClass(api.MachineTrusted)
	rp.svc.SetDepsPoll(100 * time.Millisecond)
	runtarget.SetDepsRecipe(t, "npm", testRecipe)
	ctx := context.Background()
	p, err := rp.owner.GetProject(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	rp.svc.FakeHost(p.ID, "lender", rp.svc.HostAdverts(ctx, p.ID), func(ctx context.Context, op computeapi.RunTargetOp) (json.RawMessage, error) {
		res, err := rp.svc.HostCall(ctx, p.ID, op.Binding, op.Verb, op.ID, op.Args)
		if err == nil && relay != nil {
			res = relay(op, res)
		}
		return runtarget.AsRelayed(op.Verb, res, err)
	})
	return rp, p
}

// A binding another machine serves takes deps: its host advertises the
// binding's pins, the layer is keyed by the advertised digest (the same key
// as a binding of this machine's with that image), built on the host and a
// hit the next time.
func TestDepsOnALentBinding(t *testing.T) {
	rp, _ := newDepsRelay(t, nil)
	ctx := context.Background()
	ads := rp.svc.HostAdverts(ctx, rp.project)
	if len(ads) != 1 || len(ads[0].Describe.Images) != 1 || ads[0].Describe.Images[0] != refPin {
		t.Fatalf("the host's advert names images %+v, want its binding's pin", ads)
	}
	deps := []computeapi.BoxDeps{npmDeps("app")}
	h, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "r1", Deps: deps})
	if err != nil || len(h.Deps) != 1 || !h.Deps[0].Built {
		t.Fatalf("an acquire with deps on %s: %+v %v", lent, h.Deps, err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "ri", Image: "ref-image", Deps: deps}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "name a template") {
		t.Errorf("deps with an image name on %s: %v, want bad_request naming a template", lent, err)
	}
	h2, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "r2", Deps: deps})
	if err != nil || len(h2.Deps) != 1 || !h2.Deps[0].Hit {
		t.Fatalf("the next acquire: %+v %v, want a hit", h2.Deps, err)
	}
	if _, err := computeclient.BindRunTarget(ctx, rp.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "here", Driver: "ref", Kind: "box",
		Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
		t.Fatal(err)
	}
	hh, err := computeclient.AcquireRunTarget(ctx, rp.owner, "here", computeapi.AcquireRunTargetRequest{RequestID: "r3", Deps: deps})
	if err != nil || len(hh.Deps) != 1 || hh.Deps[0].Key != h.Deps[0].Key {
		t.Fatalf("the same files and image on this machine's binding: %+v %v, want key %s", hh.Deps, err, h.Deps[0].Key)
	}
}

// A host whose advert names no pin (one from before adverts carried them,
// or one naming only things that are not pins, which the authority drops)
// is refused deps before any box is asked for.
func TestDepsRefusedOnAHostAdvertisingNoPins(t *testing.T) {
	rp, p := newDepsRelay(t, nil)
	ctx := context.Background()
	ads := rp.svc.HostAdverts(ctx, rp.project)
	for name, images := range map[string][]string{"none": nil, "not pins": {"ref-image:latest", "ref-image@sha256:short", "@sha256:" + strings.Repeat("ab", 32)}} {
		ads[0].Describe.Images = images
		if err := rp.svc.Beat(ctx, p, "old", ads); err != nil {
			t.Fatal(err)
		}
		for _, ra := range rp.svc.RunTargetAdverts(ctx, rp.project) {
			if ra.Machine == "old" && len(ra.Describe.Images) != 0 {
				t.Errorf("%s: the authority kept advertised images %v", name, ra.Describe.Images)
			}
		}
		_, err := computeclient.AcquireRunTarget(ctx, rp.owner, "sandbox@old", computeapi.AcquireRunTargetRequest{RequestID: "r-" + strings.ReplaceAll(name, " ", "-"), Deps: []computeapi.BoxDeps{npmDeps("app")}})
		if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "advertises no pinned images") {
			t.Errorf("%s: %v, want bad_request naming the missing pins", name, err)
		}
	}
}

// A host whose box boots another image than the advert names (an advert gone
// stale) never gets a layer sealed under the advertised key: the builder is
// released without seal and the acquire refused; once the host boots the
// advertised image, the layer is built afresh, not found.
func TestDepsBuilderOffTheAdvertisedDigestIsNotSealed(t *testing.T) {
	stale := true
	other := "sha256:" + strings.Repeat("cd", 32)
	var seals int
	rp, _ := newDepsRelay(t, func(op computeapi.RunTargetOp, res json.RawMessage) json.RawMessage {
		if op.Verb == "release" && strings.Contains(string(op.Args), `"seal"`) {
			seals++
		}
		if op.Verb != "acquire" || !stale {
			return res
		}
		var m map[string]any
		if json.Unmarshal(res, &m) != nil {
			return res
		}
		m["image_digest"] = other
		out, _ := json.Marshal(m)
		return out
	})
	ctx := context.Background()
	deps := []computeapi.BoxDeps{npmDeps("app")}
	_, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "s1", Deps: deps})
	if err == nil || !strings.Contains(err.Error(), other) {
		t.Fatalf("an acquire whose builder booted %s: %v, want refused naming it", other, err)
	}
	if seals != 0 {
		t.Fatalf("a builder off the advertised digest was released with seal %d times", seals)
	}
	stale = false
	h, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "s2", Deps: deps})
	if err != nil || len(h.Deps) != 1 || !h.Deps[0].Built {
		t.Fatalf("once the host boots the advertised image: %+v %v, want a fresh build", h.Deps, err)
	}
}

// Acquires with deps that come in together on a binding another machine
// serves, for a layer nobody has built: one builds it on the host, and the
// others wait for it (the host's "building" answer crosses the relay) and
// then mount it; with deps_wait_s 0 one does not wait but takes a box
// without the layer at once.
func TestDepsConcurrentAsksOnALentBindingWait(t *testing.T) {
	var mu sync.Mutex
	builds := 0
	rp, _ := newDepsRelay(t, func(op computeapi.RunTargetOp, res json.RawMessage) json.RawMessage {
		if op.Verb == "acquire" && strings.Contains(string(op.Args), `"mode":"build"`) {
			mu.Lock()
			builds++
			mu.Unlock()
		}
		return res
	})
	runtarget.SetDepsRecipe(t, "npm", func(bool) string { return "sleep 2; mkdir -p node_modules\n" })
	ctx := context.Background()
	deps := []computeapi.BoxDeps{npmDeps("app")}
	hs := make([]computeapi.RunTargetHandle, 4)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range hs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hs[i], errs[i] = computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: fmt.Sprintf("c%d", i), Deps: deps})
		}()
	}
	wg.Wait()
	built, hit := 0, 0
	for i, h := range hs {
		if errs[i] != nil {
			t.Fatalf("acquire %d: %v, want it to wait for the build", i, errs[i])
		}
		if len(h.Deps) != 1 || h.Deps[0].Hit == h.Deps[0].Built {
			t.Fatalf("acquire %d's deps: %+v", i, h.Deps)
		}
		if h.Deps[0].Built {
			built++
		} else {
			hit++
		}
	}
	if builds != 1 || built != 1 || hit != 3 {
		t.Fatalf("builders asked of the host: %d, answers built %d hit %d: want one build and three hits", builds, built, hit)
	}

	// deps_wait_s 0 while the host builds another layer: a box without it,
	// before the build ends.
	runtarget.SetDepsRecipe(t, "npm", func(bool) string { return "sleep 15; mkdir -p node_modules\n" })
	slow := []computeapi.BoxDeps{npmDeps("slow")}
	done := make(chan error, 1)
	go func() {
		_, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "builder", Deps: slow})
		done <- err
	}()
	for by := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		mu.Lock()
		n := builds
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(by) {
			t.Fatal("no second build started")
		}
	}
	zero := 0
	h, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "plain", DepsWaitS: &zero, Deps: slow})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Deps) != 1 || h.Deps[0].Hit || h.Deps[0].Built || len(h.Volumes) != 0 {
		t.Errorf("deps_wait_s 0 during a build on the host: deps %+v volumes %+v, want a box without the layer", h.Deps, h.Volumes)
	}
	select {
	case err := <-done:
		t.Errorf("the build ended (%v) before the deps_wait_s 0 acquire answered: it waited for the build", err)
	default:
	}
	if err := <-done; err != nil {
		t.Fatalf("the building acquire: %v", err)
	}
}
