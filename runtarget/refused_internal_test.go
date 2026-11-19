package runtarget

import (
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
)

// Only a box's unavailable refusal, with no data of its own or a no-room
// answer's, is marked as a driver's made-nothing: other codes may be about a
// request id a box was already made for, a run's unavailable may answer while
// its run starts, and a "building" answer's data is the daemon's own signal.
func TestDriverRefusedMarksOnlyABoxsUnavailable(t *testing.T) {
	building := api.Errorf(api.CodeUnavailable, "building")
	building.Data = []byte(`{"reason":"building","retry_s":3}`)
	full := api.Errorf(api.CodeUnavailable, "no room")
	full.Data = []byte(`{"reason":"no_room","retry_s":600,"why":"network busy"}`)
	runFull := *full
	for _, c := range []struct {
		name, kind string
		err        error
		marked     bool
	}{
		{"box unavailable", computeapi.RunTargetKindBox, api.Errorf(api.CodeUnavailable, "no capacity"), true},
		{"box bad_request", computeapi.RunTargetKindBox, api.Errorf(api.CodeBadRequest, "request_id reused"), false},
		{"box conflict", computeapi.RunTargetKindBox, api.Errorf(api.CodeConflict, "no"), false},
		{"box building", computeapi.RunTargetKindBox, building, false},
		{"box no room", computeapi.RunTargetKindBox, full, true},
		{"run no room", computeapi.RunTargetKindRun, &runFull, false},
		{"run unavailable", computeapi.RunTargetKindRun, api.Errorf(api.CodeUnavailable, "still being prepared"), false},
	} {
		_, ok := computeapi.RefusedBeforeRecord(driverRefused(c.kind, "b1", c.err))
		if ok != c.marked {
			t.Errorf("%s: marked %v, want %v", c.name, ok, c.marked)
		}
	}
	// The no-room answer keeps what the waiting caller prints: the binding,
	// the host's words and the controller's retry time.
	got, ok := computeapi.NoRoom(driverRefused(computeapi.RunTargetKindBox, "b1", full))
	if want := (computeapi.AcquireRefused{Reason: computeapi.RefusedNoRoom, On: "b1", Why: "network busy", RetryS: 600}); !ok || got != want {
		t.Errorf("no room: %+v %v, want %+v", got, ok, want)
	}
}

// A binding whose describe says it is full only for now is refused as no
// room before the driver is asked; one paused by its owner, or whose driver
// says another reason with every slot taken, stays unavailable: waiting
// would not lift it.
func TestNotAvailableNamesOnlyAFullBindingNoRoom(t *testing.T) {
	b := computeapi.RunTarget{Name: "b1", Kind: computeapi.RunTargetKindBox}
	full := &computeapi.RunTargetDescribe{Why: "no free linux slot (8 in use)", Slots: &computeapi.RunTargetSlots{OS: "linux", Total: 8}}
	if r, ok := computeapi.NoRoom(notAvailable(b, full)); !ok || r.On != "b1" || r.Why != full.Why {
		t.Errorf("full: %+v %v", r, ok)
	}
	reclaimed := &computeapi.RunTargetDescribe{Why: "reclaimed: owner active", Host: &protocol.HostStatus{Mode: protocol.HostAuto, State: protocol.HostHolding}}
	if _, ok := computeapi.NoRoom(notAvailable(b, reclaimed)); !ok {
		t.Error("reclaimed by the auto policy: not marked no room")
	}
	broken := &computeapi.RunTargetDescribe{Why: "image potluck-box@sha256:ab is not readable", Slots: &computeapi.RunTargetSlots{OS: "linux", Total: 8}}
	if _, ok := computeapi.NoRoom(notAvailable(b, broken)); ok {
		t.Error("an unreadable image with no free slot: marked no room")
	}
	paused := &computeapi.RunTargetDescribe{Why: "closed by its owner", Slots: &computeapi.RunTargetSlots{OS: "linux", Total: 8},
		Host: &protocol.HostStatus{Mode: protocol.HostClosed, State: protocol.HostShut}}
	err := notAvailable(b, paused)
	if _, ok := computeapi.NoRoom(err); ok {
		t.Error("closed by its owner: marked no room")
	}
	if r, ok := computeapi.RefusedBeforeRecord(err); !ok || r != computeapi.RefusedUnavailable {
		t.Errorf("closed by its owner: refused %q %v, want unavailable", r, ok)
	}
}
