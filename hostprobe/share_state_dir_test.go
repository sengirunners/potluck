package hostprobe

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

func TestShareBindsStateDir(t *testing.T) {
	state := filepath.Join(t.TempDir(), "driver records")
	calls := 0
	run := &fakeRunner{on: func(string) (Result, bool) { return ok(""), true }}
	binder := binderFunc(func(_ context.Context, req computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		calls++
		if got := req.Target.Config["state_dir"]; got != state {
			t.Errorf("bound state_dir = %q, want %q", got, state)
		}
		return computeapi.RunTargetView{}, nil
	})
	report := bigReport()
	report.StateDir = t.TempDir() // the probe path must not replace the driver's path
	result, err := Share(context.Background(), run, binder, ShareOptions{
		Report: report, StateDir: state, Images: []string{pinA}, Sizes: []string{"c1m2", "c2m4"},
		NamePrefix: "host", Driver: "ref", Conformance: testSuite, RunnerDir: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("bound %d sizes, want 2: %+v", calls, result)
	}
	for _, size := range result.Sizes {
		if !size.Passed || !size.Bound {
			t.Errorf("size did not pass and bind: %+v", size)
		}
		if !slices.Contains(size.Conformance, "state_dir="+state) {
			t.Errorf("conformance lost state_dir: %v", size.Conformance)
		}
	}
}
