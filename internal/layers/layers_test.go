// Package layers holds the module's layer rule. Every Go package in this
// module is KIT (everything under kit/: identity, projects, transports, the
// daemon shell) or COMPUTE (everything else: boxes, sandboxes, runners,
// their drivers and wiring). KIT's non-test code imports no COMPUTE package;
// KIT's tests may (a KIT test may run a daemon wired by wiring.Compute).
// The module also depends on no module that requires it (zm and other
// programs import this module, never the other way round).
package layers

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/sengirunners/potluck"

type pkg struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// root is the module's directory.
func root(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

func goList(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = root(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v\n%s", args, err, stderr.String())
	}
	return out
}

// isKit is whether import path p is a KIT package of this module.
func isKit(p string) bool {
	return p == module+"/kit" || strings.HasPrefix(p, module+"/kit/")
}

func inModule(p string) bool {
	return p == module || strings.HasPrefix(p, module+"/")
}

func TestKitImportsNoCompute(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(goList(t, "-json", "./...")))
	kits := 0
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if !isKit(p.ImportPath) {
			continue
		}
		kits++
		for _, imp := range p.Imports {
			if inModule(imp) && !isKit(imp) {
				t.Errorf("%s (KIT) imports %s (COMPUTE): KIT's non-test code imports only KIT", p.ImportPath, imp)
			}
		}
	}
	if kits == 0 {
		t.Fatal("no KIT package found: is the test running in the module?")
	}
}

// modulePath is the path of a go mod graph node (path or path@version).
func modulePath(node string) string {
	path, _, _ := strings.Cut(node, "@")
	return path
}

func TestNoDependencyOnAnImporter(t *testing.T) {
	cmd := exec.Command("go", "mod", "graph")
	cmd.Dir = root(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod graph: %v\n%s", err, stderr.String())
	}
	edges := 0
	for _, line := range strings.Split(string(out), "\n") {
		from, to, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		edges++
		if modulePath(to) == module && modulePath(from) != module {
			t.Errorf("go mod graph: %s requires this module: this module must not depend on a module that imports it", from)
		}
	}
	if edges == 0 {
		t.Fatal("go mod graph listed no requirements: is the test running in the module?")
	}
}
