package computeclient

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/protocol"
)

// An acquire asking a dependency layer is placed only on a binding whose
// driver builds layers: a smaller or nearer one that cannot is passed over
// with its reason, as a database request is, never asked and refused.
func TestAcquireDepsPlacesOnABindingThatBuildsLayers(t *testing.T) {
	layers := []string{protocol.CapVolumes, protocol.CapVolumesSeal, protocol.CapVolumesKeep, computeapi.EncodingBase64}
	volumesOnly := boxView("tart", true, "", "small", "linux", 1, 512)
	volumesOnly.Describe.Capabilities = []string{protocol.CapVolumes, computeapi.EncodingBase64}
	builds := boxView("oci@trusted", true, "", "large", "linux", 4, 8192)
	builds.Host, builds.HostClass = "trusted", api.MachineTrusted
	builds.Describe.Capabilities = layers
	req := computeapi.AcquireRunTargetRequest{RequestID: "deps", Deps: []computeapi.BoxDeps{{Kind: "npm"}}}

	var asked []string
	c := client.WithHandler(acquireHandler(&asked, nil, volumesOnly, builds))
	target, _, err := AcquireBox(context.Background(), c, BoxWant{Volumes: true}, req, nil)
	if err != nil || target != builds.Name || len(asked) != 1 || !strings.HasPrefix(asked[0], builds.Name+" ") {
		t.Fatalf("target=%s asked=%v err=%v; want only %s asked", target, asked, err, builds.Name)
	}

	// None that builds layers: placement refuses before any acquire, naming
	// the capability the binding lacks.
	asked = nil
	c = client.WithHandler(acquireHandler(&asked, nil, volumesOnly))
	if _, _, err := AcquireBox(context.Background(), c, BoxWant{Volumes: true}, req, nil); err == nil ||
		!strings.Contains(err.Error(), "builds no dependency layers (capability "+protocol.CapVolumesSeal+")") || len(asked) != 0 {
		t.Fatalf("asked=%v err=%v; want a placement refusal naming %s", asked, err, protocol.CapVolumesSeal)
	}

	// A pool's only binding, asked nothing else: the fast path that takes it
	// without describing it does not apply to an acquire with deps.
	asked = nil
	c = client.WithHandler(acquireHandler(&asked, nil, volumesOnly))
	if _, _, err := AcquireBox(context.Background(), c, BoxWant{}, req, nil); err == nil ||
		!strings.Contains(err.Error(), "builds no dependency layers") || len(asked) != 0 {
		t.Fatalf("only binding: asked=%v err=%v; want a placement refusal before any acquire", asked, err)
	}

	// Without deps the same binding is fine.
	asked = nil
	c = client.WithHandler(acquireHandler(&asked, nil, volumesOnly, builds))
	if target, _, err := AcquireBox(context.Background(), c, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "plain"}, nil); err != nil || target != volumesOnly.Name {
		t.Fatalf("no deps: target=%s err=%v; want %s", target, err, volumesOnly.Name)
	}
}
