package runtarget_test

import (
	"context"
	"encoding/json"
	"strings"
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
