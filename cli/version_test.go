package cli

import (
	"runtime/debug"
	"testing"
)

// A plain go build in a git checkout names its build: the module version go
// stamped and the commit; with no stamp the defaults stay.
func TestVersionOf(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261110174616-b28737bec6c6+dirty"},
		Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: "b28737bec6c6190353dbc1490f52f100dc2538eb"}}}
	if v, c := versionOf(bi, "v0.0.0-dev", "unknown"); v != "v0.0.0-20261110174616-b28737bec6c6+dirty" || c != "b28737bec6c6" {
		t.Fatalf("stamped: %q %q", v, c)
	}
	if v, c := versionOf(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "v0.0.0-dev", "unknown"); v != "v0.0.0-dev" || c != "unknown" {
		t.Fatalf("no stamp: %q %q", v, c)
	}
}
