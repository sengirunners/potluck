package layers

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"testing"
)

// The rename's step 2 builds every program under its new name and keeps its
// old command directory as a one-line main over the same code, so zm's build
// lists (Makefile POTLUCK_CMDS, cmd/zm/daemon_update.go potluckCmds,
// scripts/release) and updaters that name the old paths keep building the
// same program (H2). go build ./cmd/... writes both names.
func TestOldCommandNamesBuildTheSameProgram(t *testing.T) {
	imports := map[string][]string{}
	dec := json.NewDecoder(bytes.NewReader(goList(t, "-json", "./cmd/...")))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		imports[p.ImportPath] = p.Imports
	}
	for old, nw := range map[string]string{
		"zm-run-tart":        "potluck-driver-tart",
		"zm-run-local":       "potluck-driver-local",
		"zm-run-conformance": "potluck-conformance",
		"zm-e2b":             "potluck-e2b",
	} {
		o, ok1 := imports[module+"/cmd/"+old]
		n, ok2 := imports[module+"/cmd/"+nw]
		if !ok1 || !ok2 {
			t.Errorf("cmd/%s (%v) and cmd/%s (%v) must both build", old, ok1, nw, ok2)
			continue
		}
		var entry string
		for _, imp := range o {
			if inModule(imp) {
				entry = imp
			}
		}
		if entry == "" || !slices.Contains(n, entry) {
			t.Errorf("cmd/%s calls %q; cmd/%s imports %v", old, entry, nw, n)
		}
	}
}
