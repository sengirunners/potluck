package swap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// An upgrade places the programs' new names too (potluck, potluck-*).
func TestInstallableNewNames(t *testing.T) {
	src := t.TempDir()
	for _, n := range []string{"zm", "zm-run-tart", "potluck", "potluck-driver-tart", "potluck-e2b", "potluck.txt"} {
		mode := os.FileMode(0o755)
		if strings.HasSuffix(n, ".txt") {
			mode = 0o644
		}
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), mode); err != nil {
			t.Fatal(err)
		}
	}
	names, err := Installable(src)
	if err != nil || strings.Join(names, ",") != "potluck,potluck-driver-tart,potluck-e2b,zm,zm-run-tart" {
		t.Fatalf("installable: %v %v", names, err)
	}
}

// The preflight takes a running daemon that speaks potluck/1 (a later
// build) as the same protocol, and still refuses another major.
func TestPreflightProtocolNames(t *testing.T) {
	build := api.BuildManifest{Protocol: api.Protocol, Version: "v2", Commit: "b"}
	for p, ok := range map[string]bool{"zm/v0": true, "potluck/1": true, "potluck/2": false, "zm/v9": false} {
		pf := Check(build, &Running{Health: api.Health{OK: true, Protocol: p, Version: "v1", Commit: "a"}}, nil, nil)
		refused := false
		for _, pr := range pf.Problems {
			refused = refused || strings.Contains(pr, "speaks")
		}
		if refused == ok {
			t.Errorf("running %s: problems %v", p, pf.Problems)
		}
	}
}
