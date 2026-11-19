package computeclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// acquireHandler is targetsHandler that also answers each binding's
// acquire with answer[name] (nil: a handle), logging each ask as
// "<binding> <request id> <ttl_s>".
func acquireHandler(asked *[]string, answer map[string]*api.Error, targets ...computeapi.RunTargetView) http.Handler {
	list := targetsHandler(targets...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v0/run_targets/"), "/acquire")
		if !ok || r.Method != http.MethodPost {
			list.ServeHTTP(w, r)
			return
		}
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		var req computeapi.AcquireRunTargetRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		*asked = append(*asked, name+" "+req.RequestID+" "+jsonInt(req.TTLS))
		if e := answer[name]; e != nil {
			api.WriteError(w, e)
			return
		}
		api.WriteJSON(w, 200, computeapi.RunTargetHandle{Handle: "box:" + name + "/h1", RequestID: req.RequestID, State: "ready"})
	})
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

// TestAcquireBox: a refusal the daemon marks as made before it recorded the
// request passes to the next ranked binding, with the same request id; a
// timeout, a driver's refusal or a prep error ends the walk at that binding,
// so no second box is ever asked for; every binding refusing names each.
func TestAcquireBox(t *testing.T) {
	ctx := context.Background()
	small := boxView("small", true, "", "small", "linux", 1, 1024)
	std := boxView("std", true, "", "standard", "linux", 2, 4096)
	refused := func(code, why string) *api.Error {
		return api.Errorf(code, "refused").WithData(computeapi.AcquireRefused{Reason: why})
	}
	timeout := api.Errorf(api.CodeUnavailable, "potluck-driver-x did not answer acquire; retry with the same request_id")
	for _, tc := range []struct {
		name   string
		answer map[string]*api.Error
		target string // the binding answered ("" with an error)
		asked  []string
		reason []string // in the error, with an error
	}{
		{"the first choice", nil, "small", []string{"small k 60"}, nil},
		{"busy first choice lands on the second", map[string]*api.Error{"small": refused(api.CodeUnavailable, computeapi.RefusedUnavailable)}, "std", []string{"small k 60", "std k 60"}, nil},
		{"handles_max", map[string]*api.Error{"small": refused(api.CodeForbidden, computeapi.RefusedHandlesMax)}, "std", []string{"small k 60", "std k 60"}, nil},
		{"budget", map[string]*api.Error{"small": refused(api.CodeForbidden, computeapi.RefusedBudget)}, "std", []string{"small k 60", "std k 60"}, nil},
		{"lifetime", map[string]*api.Error{"small": refused(api.CodeBadRequest, computeapi.RefusedLifetime)}, "std", []string{"small k 60", "std k 60"}, nil},
		{"a timed-out acquire is not retried elsewhere", map[string]*api.Error{"small": timeout}, "", []string{"small k 60"}, []string{"did not answer acquire"}},
		{"a driver's refusal is not retried elsewhere", map[string]*api.Error{"small": api.Errorf(api.CodeUnavailable, "potluck-driver-x acquire: no")}, "", []string{"small k 60"}, nil},
		{"an unknown mark is not retried elsewhere", map[string]*api.Error{"small": api.Errorf(api.CodeUnavailable, "x").WithData(computeapi.AcquireRefused{Reason: "maybe"})}, "", []string{"small k 60"}, nil},
		{"a retry made on another binding is sent there", map[string]*api.Error{"small": api.Errorf(api.CodeConflict, "used").WithData(computeapi.AcquireUsedOn{Target: "std"})}, "std", []string{"small k 60", "std k 60"}, nil},
		{"a used_on naming the binding asked is not followed", map[string]*api.Error{"small": api.Errorf(api.CodeConflict, "used").WithData(computeapi.AcquireUsedOn{Target: "small"})}, "", []string{"small k 60"}, nil},
		{"a used_on is followed once", map[string]*api.Error{"small": api.Errorf(api.CodeConflict, "used").WithData(computeapi.AcquireUsedOn{Target: "std"}),
			"std": api.Errorf(api.CodeConflict, "used").WithData(computeapi.AcquireUsedOn{Target: "small"})}, "", []string{"small k 60", "std k 60"}, nil},
		{"every binding refuses", map[string]*api.Error{"small": refused(api.CodeUnavailable, computeapi.RefusedUnavailable), "std": refused(api.CodeForbidden, computeapi.RefusedBudget)}, "",
			[]string{"small k 60", "std k 60"}, []string{"small (unavailable)", "std (budget)"}},
	} {
		var asked []string
		target, h, err := AcquireBox(ctx, client.WithHandler(acquireHandler(&asked, tc.answer, std, small)), BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k", TTLS: 60}, nil)
		switch {
		case tc.target != "" && (err != nil || target != tc.target || h.Handle != "box:"+tc.target+"/h1"):
			t.Errorf("%s: %q %+v %v, want %s", tc.name, target, h, err, tc.target)
		case tc.target == "" && err == nil:
			t.Errorf("%s: %q %+v, want an error", tc.name, target, h)
		}
		if !slices.Equal(asked, tc.asked) {
			t.Errorf("%s: asked %q, want %q", tc.name, asked, tc.asked)
		}
		if _, ok := computeapi.RefusedBeforeRecord(err); ok != (tc.name == "every binding refuses") {
			t.Errorf("%s: %v refused before record %v", tc.name, err, ok) // only when nothing was made anywhere
		}
		for _, r := range tc.reason {
			if err == nil || !strings.Contains(err.Error(), r) {
				t.Errorf("%s: %v lacks %q", tc.name, err, r)
			}
		}
	}

	// prep readies each binding's request on its own; its error ends the walk.
	var asked []string
	c := client.WithHandler(acquireHandler(&asked, map[string]*api.Error{"small": refused(api.CodeForbidden, computeapi.RefusedHandlesMax)}, std, small))
	ttl := map[string]int{"small": 30, "std": 90}
	_, _, err := AcquireBox(ctx, c, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k"}, func(_ context.Context, target string, r *computeapi.AcquireRunTargetRequest) error {
		r.TTLS = ttl[target]
		return nil
	})
	if want := []string{"small k 30", "std k 90"}; err != nil || !slices.Equal(asked, want) {
		t.Errorf("prep: asked %q %v, want %q", asked, err, want)
	}
	asked = nil
	stop := errors.New("no")
	target, _, err := AcquireBox(ctx, c, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k"}, func(_ context.Context, target string, _ *computeapi.AcquireRunTargetRequest) error {
		if target == "std" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || target != "std" || !slices.Equal(asked, []string{"small k 0"}) {
		t.Errorf("a prep error: %q %v, asked %q", target, err, asked)
	}
	// A Misfit passes to the next binding, nothing asked of the first; every
	// binding a misfit or refused names each, and made nothing.
	asked = nil
	misfit := func(on ...string) func(context.Context, string, *computeapi.AcquireRunTargetRequest) error {
		return func(_ context.Context, target string, _ *computeapi.AcquireRunTargetRequest) error {
			if slices.Contains(on, target) {
				return &Misfit{Err: errors.New(target + " cannot")}
			}
			return nil
		}
	}
	free := client.WithHandler(acquireHandler(&asked, nil, std, small))
	if target, h, err := AcquireBox(ctx, free, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k"}, misfit("small")); err != nil || target != "std" || h.Handle != "box:std/h1" || !slices.Equal(asked, []string{"std k 0"}) {
		t.Errorf("a misfit first: %q %+v %v, asked %q", target, h, err, asked)
	}
	asked = nil
	_, _, err = AcquireBox(ctx, free, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k"}, misfit("small", "std"))
	if !MadeNothing(err) || !api.IsCode(err, api.CodeBadRequest) || len(asked) > 0 || !strings.Contains(err.Error(), "small (does not fit): small cannot") || !strings.Contains(err.Error(), "std (does not fit): std cannot") {
		t.Errorf("every binding a misfit: %v, asked %q", err, asked)
	}
	asked = nil
	_, _, err = AcquireBox(ctx, c, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k"}, misfit("std"))
	if why, ok := computeapi.RefusedBeforeRecord(err); !ok || why != computeapi.RefusedHandlesMax || !strings.Contains(err.Error(), "std (does not fit)") || !slices.Equal(asked, []string{"small k 0"}) {
		t.Errorf("refused, then a misfit: %v, asked %q", err, asked)
	}
	// A retry sent to the binding that holds its request, which no longer
	// fits: that binding may hold the box, so it is no Misfit.
	asked = nil
	moved := client.WithHandler(acquireHandler(&asked, map[string]*api.Error{"small": api.Errorf(api.CodeConflict, "used").WithData(computeapi.AcquireUsedOn{Target: "std"})}, std, small))
	if _, _, err := AcquireBox(ctx, moved, BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k"}, misfit("std")); err == nil || MadeNothing(err) || !slices.Equal(asked, []string{"small k 0"}) {
		t.Errorf("a retry's binding a misfit: %v (made nothing %v), asked %q", err, MadeNothing(err), asked)
	}
	// Placement's refusal comes back as is, with no binding asked.
	asked = nil
	if target, _, err := AcquireBox(ctx, client.WithHandler(acquireHandler(&asked, nil)), BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k"}, nil); !api.IsCode(err, api.CodeNotFound) || target != "" || len(asked) > 0 {
		t.Errorf("no box: %q %v, asked %q", target, err, asked)
	}
}

// A pool with no room now (zm-eakh.12.24.18.5): placement passes over a
// binding that is only full and takes another that has room; with none that
// has room, the refusal is a no-room one naming the full binding, so the
// caller waits (roomwait), unless the only full one would not fit anyway.
// When every binding refused at acquire and one of them was only full, the
// combined refusal is that one's no room.
func TestNoRoomPlacesElsewhereThenWaits(t *testing.T) {
	ctx := context.Background()
	full := func(name, size string, cpu int) computeapi.RunTargetView {
		v := boxView(name, false, "no free linux slot (8 in use)", size, "linux", cpu, 1024*cpu)
		v.Describe.Slots = &computeapi.RunTargetSlots{OS: "linux", Total: 8}
		return v
	}
	std := boxView("std", true, "", "standard", "linux", 2, 4096)
	var asked []string
	c := client.WithHandler(acquireHandler(&asked, nil, full("small", "small", 1), std))
	if target, _, err := AcquireBox(ctx, c, BoxWant{OS: "linux"}, computeapi.AcquireRunTargetRequest{RequestID: "r1", TTLS: 60}, nil); err != nil || target != "std" {
		t.Errorf("one full, one with room: %q %v, want std", target, err)
	}

	asked = nil
	c = client.WithHandler(acquireHandler(&asked, nil, full("small", "small", 1), boxView("away", false, "closed by its owner", "standard", "linux", 2, 4096)))
	_, _, err := AcquireBox(ctx, c, BoxWant{OS: "linux"}, computeapi.AcquireRunTargetRequest{RequestID: "r1", TTLS: 60}, nil)
	if r, ok := computeapi.NoRoom(err); !ok || r.On != "small" || r.Why != "no free linux slot (8 in use)" || len(asked) != 0 {
		t.Errorf("only a full one fits: %+v %v (%v), asked %q", r, ok, err, asked)
	}
	if !MadeNothing(err) {
		t.Errorf("only a full one fits: not made-nothing: %v", err)
	}
	if _, _, err := AcquireBox(ctx, c, BoxWant{Size: "standard"}, computeapi.AcquireRunTargetRequest{RequestID: "r1", TTLS: 60}, nil); err == nil {
		t.Error("the full one is the wrong size: placed")
	} else if _, ok := computeapi.NoRoom(err); ok {
		t.Errorf("the full one is the wrong size: a no room, so a wait for a box no host offers: %v", err)
	}

	asked = nil
	noRoom := api.Errorf(api.CodeUnavailable, "no room on this host now: network busy").
		WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedNoRoom, On: "small", Why: "network busy", RetryS: 600})
	budget := api.Errorf(api.CodeUnavailable, "budget spent").WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedBudget})
	small := boxView("small", true, "", "small", "linux", 1, 1024)
	c = client.WithHandler(acquireHandler(&asked, map[string]*api.Error{"small": noRoom, "std": budget}, small, std))
	_, _, err = AcquireBox(ctx, c, BoxWant{OS: "linux"}, computeapi.AcquireRunTargetRequest{RequestID: "r1", TTLS: 60}, nil)
	if r, ok := computeapi.NoRoom(err); !ok || r.On != "small" || r.RetryS != 600 || len(asked) != 2 || !strings.Contains(err.Error(), "std (budget)") {
		t.Errorf("no room on one, budget on the other: %+v %v (%v), asked %q", r, ok, err, asked)
	}
}
