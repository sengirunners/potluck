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

func TestAcquireDatabasePlacesOnCapableTrustedBox(t *testing.T) {
	preferred := boxView("local-tart", true, "", "small", "linux", 1, 512)
	capable := boxView("oci@trusted", true, "", "large", "linux", 4, 8192)
	capable.Host, capable.HostClass = "trusted", api.MachineTrusted
	capable.Describe.Capabilities = []string{protocol.CapDatabase}
	public := boxView("oci@public", true, "", "tiny", "linux", 1, 256)
	public.Host, public.HostClass = "public", api.MachinePublic
	public.Describe.Capabilities = []string{protocol.CapDatabase}
	var asked []string
	c := client.WithHandler(acquireHandler(&asked, nil, preferred, public, capable))
	target, _, err := AcquireBox(context.Background(), c, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "db", Database: "example-db"}, nil)
	if err != nil || target != capable.Name || len(asked) != 1 || asked[0] != capable.Name+" db 0" {
		t.Fatalf("target=%s asked=%v err=%v", target, asked, err)
	}
	// The single-binding fast path must also describe and reject unsupported
	// database requests before acquire, rather than treating them as ordinary boxes.
	asked = nil
	c = client.WithHandler(acquireHandler(&asked, nil, preferred))
	if _, _, err := AcquireBox(context.Background(), c, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "db", Database: "example-db"}, nil); err == nil || !strings.Contains(err.Error(), protocol.CapDatabase) || len(asked) != 0 {
		t.Fatalf("unsupported singleton: asked=%v err=%v", asked, err)
	}
	// An explicitly selected binding remains the caller's choice. Its refusal
	// is returned; placement must never silently move it to the capable host.
	asked = nil
	c = client.WithHandler(acquireHandler(&asked, map[string]*api.Error{preferred.Name: api.Errorf(api.CodeBadRequest, "no database capability")}, preferred, capable))
	target, _, err = AcquireBox(context.Background(), c, BoxWant{On: preferred.Name}, computeapi.AcquireRunTargetRequest{RequestID: "db", Database: "example-db"}, nil)
	if !api.IsCode(err, api.CodeBadRequest) || target != preferred.Name || len(asked) != 1 || asked[0] != preferred.Name+" db 0" {
		t.Fatalf("explicit binding: target=%s asked=%v err=%v", target, asked, err)
	}
}
