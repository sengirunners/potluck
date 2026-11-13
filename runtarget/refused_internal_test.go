package runtarget

import (
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Only a box's unavailable refusal, with no data of its own, is marked as a
// driver's made-nothing: other codes may be about a request id a box was
// already made for, a run's unavailable may answer while its run starts, and
// a "building" answer's data is the daemon's own signal.
func TestDriverRefusedMarksOnlyABoxsUnavailable(t *testing.T) {
	building := api.Errorf(api.CodeUnavailable, "building")
	building.Data = []byte(`{"reason":"building","retry_s":3}`)
	for _, c := range []struct {
		name, kind string
		err        error
		marked     bool
	}{
		{"box unavailable", computeapi.RunTargetKindBox, api.Errorf(api.CodeUnavailable, "no capacity"), true},
		{"box bad_request", computeapi.RunTargetKindBox, api.Errorf(api.CodeBadRequest, "request_id reused"), false},
		{"box conflict", computeapi.RunTargetKindBox, api.Errorf(api.CodeConflict, "no"), false},
		{"box building", computeapi.RunTargetKindBox, building, false},
		{"run unavailable", computeapi.RunTargetKindRun, api.Errorf(api.CodeUnavailable, "still being prepared"), false},
	} {
		_, ok := computeapi.RefusedBeforeRecord(driverRefused(c.kind, c.err))
		if ok != c.marked {
			t.Errorf("%s: marked %v, want %v", c.name, ok, c.marked)
		}
	}
}
