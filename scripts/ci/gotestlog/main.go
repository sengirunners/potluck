// Command gotestlog reads `go test -json` on stdin and writes the log a person reads: what a plain `go test` prints,
// without the per-test noise. Package-level lines (the "ok  pkg" and "FAIL pkg" lines, build errors) come out when a
// package ends, preceded, for a package that did not pass, by the output of its tests that never reached pass or
// skip (failed, or killed by a panic or timeout), so a hang's stacks show when that package ends, not when the whole
// run does. A passing test's output is dropped as soon as it passes. A line that is not a JSON object is written as
// it is. scripts/ci/go-test-json.sh runs it; the raw JSON is kept beside it.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Bounds on what is held (the raw JSON keeps everything): one test's output, a package's package-level lines, and the
// output of a package's passing tests, which prints only if the package fails (go test credits a failing parallel
// test's own log lines, a race report or a server's log, to whichever test was last named, so they can land on a
// test that passed).
const (
	maxHeld    = 1 << 20
	maxLines   = 2000
	maxPassed  = 64 << 10
	passedNote = "--- output of tests that passed in this failed package (lines of a failing parallel test can be credited to one) ---\n"
)

type event struct {
	Action     string
	Package    string
	Test       string
	Output     string
	OutputType string
	ImportPath string
}

type testState struct {
	name      string
	out       []byte // the current run's output
	kept      []byte // output of earlier runs that failed (-count > 1), capped like out
	keptCut   bool
	running   bool // started and not yet passed, skipped or failed: killed, or still running at end of input
	failed    bool // some run failed
	truncated bool
}

type pkgState struct {
	tests  []*testState
	byName map[string]*testState
	lines  []string // package-level output, held until the package ends
	passed []byte   // the last of the passing tests' output
}

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gotestlog:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	w := bufio.NewWriter(out)
	defer w.Flush()
	r := bufio.NewReaderSize(in, 1<<20)
	pkgs := map[string]*pkgState{}
	var order []string // packages in first-seen order, for the flush at end of input
	flush := func(name string, failedPkg bool) {
		p := pkgs[name]
		if p == nil {
			return
		}
		delete(pkgs, name)
		anyUnpassed := false
		for _, t := range p.tests {
			if !t.failed && !t.running {
				continue
			}
			anyUnpassed = true
			if t.truncated || t.keptCut {
				fmt.Fprintf(w, "... earlier output of %s dropped (the raw JSON has it)\n", t.name)
			}
			w.Write(t.kept)
			w.Write(t.out)
		}
		if (failedPkg || anyUnpassed) && len(p.passed) > 0 {
			w.WriteString(passedNote)
			w.Write(p.passed)
		}
		lines := p.lines
		if !failedPkg && !anyUnpassed && len(lines) > 1 {
			lines = lines[len(lines)-1:] // a passing package: its summary line, as plain go test prints
		}
		for _, l := range lines {
			w.WriteString(l)
		}
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var e event
			raw := bytes.TrimRight(line, "\r\n")
			if json.Unmarshal(raw, &e) != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || e == (event{}) {
				w.Write(raw)
				w.WriteByte('\n')
			} else {
				handle(&e, w, pkgs, &order, flush)
			}
			w.Flush() // a person watching the log sees each package as it ends
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
	}
	for _, name := range order {
		flush(name, false)
	}
	return nil
}

func handle(e *event, w *bufio.Writer, pkgs map[string]*pkgState, order *[]string, flush func(string, bool)) {
	if e.Package == "" { // build output and the like: not part of any package's run
		w.WriteString(e.Output)
		return
	}
	p := pkgs[e.Package]
	if p == nil {
		p = &pkgState{byName: map[string]*testState{}}
		pkgs[e.Package] = p
		*order = append(*order, e.Package)
	}
	if e.Test == "" {
		if e.Output != "" && e.Output != "PASS\n" {
			p.lines = append(p.lines, e.Output)
			if len(p.lines) > 2*maxLines {
				p.lines = append([]string(nil), p.lines[len(p.lines)-maxLines:]...)
			}
		}
		switch e.Action {
		case "pass", "skip":
			flush(e.Package, false)
		case "fail":
			flush(e.Package, true)
		}
		return
	}
	t := p.byName[e.Test]
	if t == nil {
		t = &testState{name: e.Test}
		p.byName[e.Test] = t
		p.tests = append(p.tests, t)
	}
	switch e.Action {
	case "run": // a rerun (-count) starts clean, keeping what an earlier failed run printed
		t.out, t.running, t.truncated = nil, true, false
	case "output":
		// test2json's framing lines ("=== RUN", "--- PASS") are left out, but "--- FAIL: TestX" is how a log says which
		// test failed (plain `go test` prints it), so it stays.
		if (e.OutputType == "frame" || e.OutputType == "") && isControl(e.Output) {
			return
		}
		t.running = true
		t.out = append(t.out, e.Output...)
		if len(t.out) > maxHeld {
			t.out = append([]byte(nil), t.out[len(t.out)-maxHeld/2:]...)
			t.truncated = true
		}
	case "pass", "skip":
		p.passed = append(p.passed, t.out...)
		if len(p.passed) > maxPassed {
			p.passed = append([]byte(nil), p.passed[len(p.passed)-maxPassed/2:]...)
		}
		t.out, t.running, t.truncated = nil, false, false
	case "fail":
		t.kept = append(t.kept, t.out...)
		t.keptCut = t.keptCut || t.truncated
		if len(t.kept) > maxHeld {
			t.kept = append([]byte(nil), t.kept[len(t.kept)-maxHeld/2:]...)
			t.keptCut = true
		}
		t.out, t.running, t.failed, t.truncated = nil, false, true, false
	}
}

// isControl reports test2json's own framing lines, which plain `go test` does not print: "=== RUN" and the like, and
// the "--- PASS" and "--- SKIP" lines. "--- FAIL" is not one of them.
func isControl(l string) bool {
	for _, p := range []string{"=== RUN ", "=== PAUSE ", "=== CONT ", "=== NAME ", "--- PASS: ", "--- SKIP: ", "    --- PASS: ", "    --- SKIP: "} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}
