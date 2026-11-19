package runtarget_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/runtarget"
)

// The host's error data crosses both driver and relay allowlists. An old
// host's untyped not_found, or a missing binding, never becomes base_missing.
func TestPushBaseOnLentBinding(t *testing.T) {
	rp := newRelayPairConfigured(t, nil, map[string]string{"net_off": "1"})
	rp.svc.SetHostClass(api.MachineTrusted)
	ctx := context.Background()
	var noData, missingBinding, badMount bool
	releases := 0
	rp.svc.FakeHost(rp.project, "lender", rp.svc.HostAdverts(ctx, rp.project), func(ctx context.Context, op computeapi.RunTargetOp) (json.RawMessage, error) {
		if missingBinding && op.Verb == "acquire" {
			return runtarget.AsRelayed(op.Verb, nil, api.NotFound("binding", "sandbox"))
		}
		res, err := rp.svc.HostCall(ctx, rp.project, op.Binding, op.Verb, op.ID, op.Args)
		if op.Verb == "release" {
			releases++
		}
		if op.Verb == "acquire" {
			if noData {
				var e *api.Error
				if errors.As(err, &e) {
					e.Data = nil
				}
			}
			if badMount && err == nil {
				var v map[string]any
				_ = json.Unmarshal(res, &v)
				delete(v, "volumes")
				res, _ = json.Marshal(v)
			}
		}
		return runtarget.AsRelayed(op.Verb, res, err)
	})
	for _, mode := range []string{"typed", "old", "missing-binding"} {
		noData, missingBinding = mode == "old", mode == "missing-binding"
		_, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, baseReq(mode, false))
		if !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("%s: %v", mode, err)
		}
		if got := baseReason(err); (got == computeapi.BaseMissing) != (mode == "typed") {
			t.Fatalf("%s reason %q", mode, got)
		}
	}
	noData, missingBinding = false, false
	fillBase(t, rp.owner, lent, "fill")
	h, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, baseReq("hit", false))
	if err == nil {
		held := h.Handle
		t.Cleanup(func() { _, _ = computeclient.ReleaseRunTarget(context.Background(), rp.owner, lent, idOf(held)) })
	}
	if err != nil || h.Base == nil || !h.Base.Hit {
		t.Fatalf("remote hit %+v %v", h, err)
	}
	badMount = true
	before := releases
	_, err = computeclient.AcquireRunTarget(ctx, rp.owner, lent, baseReq("stale-host", false))
	if !api.IsCode(err, api.CodeInternal) || !strings.Contains(err.Error(), "did not confirm") || releases != before+1 {
		t.Fatalf("unconfirmed mount: %v releases %d -> %d", err, before, releases)
	}
	badMount = false
	rp.svc.SetHostClass(api.MachinePublic)
	h, err = computeclient.AcquireRunTarget(ctx, rp.owner, lent, baseReq("public", false))
	if err != nil || h.Base == nil || h.Base.Hit || h.Base.Why == "" || len(h.Volumes) != 0 {
		t.Fatalf("public host fallback: %+v %v", h, err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, rp.owner, lent, baseReq("public-fill", true))
	if !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("public fill: %v", err)
	}
}
