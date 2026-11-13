package runners

import "testing"

// Rename step 2: the broker names new runners potluck-<job>[-n] and still
// takes zm-<job>[-n] (a step 1 build's) as its own, so old runners are
// cleaned up (H8); anything else is someone else's.
func TestOurRunnerNames(t *testing.T) {
	for name, ours := range map[string]bool{
		"zm-123": true, "zm-123-2": true, "potluck-123": true, "potluck-123-2": true,
		"zm-abc": false, "potluck-": false, "other-123": false, "zm-runners": false, "potluck-123-x": false,
		"potluck-build": false, "potluck-runners": false,
	} {
		if ourRunner.MatchString(name) != ours {
			t.Errorf("%q: ours = %v", name, !ours)
		}
	}
	if runnerPrefix != "potluck-" {
		t.Fatalf("new runners are named %s<job>", runnerPrefix)
	}
}
