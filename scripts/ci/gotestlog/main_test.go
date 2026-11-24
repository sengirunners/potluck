package main

import (
	"fmt"
	"strings"
	"testing"
)

func ev(action, pkg, test, output string) string {
	s := fmt.Sprintf("{%q:%q,%q:%q", "Action", action, "Package", pkg)
	if test != "" {
		s += fmt.Sprintf(",%q:%q", "Test", test)
	}
	if output != "" {
		s += fmt.Sprintf(",%q:%q", "Output", output)
	}
	return s + "}\n"
}

func render(t *testing.T, in ...string) string {
	t.Helper()
	var out strings.Builder
	if err := run(strings.NewReader(strings.Join(in, "")), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestPassingPackagePrintsOnlyItsLine(t *testing.T) {
	got := render(t,
		ev("run", "p/a", "TestOK", ""), ev("output", "p/a", "TestOK", "=== RUN   TestOK\n"),
		ev("output", "p/a", "TestOK", "    a_test.go:1: quiet\n"), ev("pass", "p/a", "TestOK", ""),
		ev("output", "p/a", "", "PASS\n"), ev("output", "p/a", "", "ok  \tp/a\t0.1s\n"), ev("pass", "p/a", "", ""))
	if got != "ok  \tp/a\t0.1s\n" {
		t.Fatalf("log = %q", got)
	}
}

func TestFailedTestOutputComesBeforeThePackageLine(t *testing.T) {
	got := render(t,
		ev("run", "p/b", "TestBad", ""), ev("output", "p/b", "TestBad", "=== RUN   TestBad\n"),
		ev("output", "p/b", "TestBad", "    b_test.go:3: failure-detail\n"), ev("output", "p/b", "TestBad", "--- FAIL: TestBad (0.00s)\n"),
		ev("fail", "p/b", "TestBad", ""),
		ev("run", "p/b", "TestGood", ""), ev("output", "p/b", "TestGood", "    quiet\n"), ev("pass", "p/b", "TestGood", ""),
		ev("output", "p/b", "", "FAIL\n"), ev("output", "p/b", "", "FAIL\tp/b\t0.1s\n"), ev("fail", "p/b", "", ""))
	want := "    b_test.go:3: failure-detail\n--- FAIL: TestBad (0.00s)\n" + passedNote + "    quiet\nFAIL\nFAIL\tp/b\t0.1s\n"
	if got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestRerunKeepsTheFailure(t *testing.T) {
	// -count=2: pass, then fail; and fail, then pass (a failed run's output stays whichever run came last).
	got := render(t,
		ev("run", "p/c", "TestFlaky", ""), ev("output", "p/c", "TestFlaky", "run1\n"), ev("pass", "p/c", "TestFlaky", ""),
		ev("run", "p/c", "TestFlaky", ""), ev("output", "p/c", "TestFlaky", "run2-failure\n"), ev("fail", "p/c", "TestFlaky", ""),
		ev("fail", "p/c", "", ""))
	if got != "run2-failure\n"+passedNote+"run1\n" {
		t.Fatalf("pass then fail: log = %q", got)
	}
	got = render(t,
		ev("run", "p/c", "TestFlaky", ""), ev("output", "p/c", "TestFlaky", "run1-failure\n"), ev("fail", "p/c", "TestFlaky", ""),
		ev("run", "p/c", "TestFlaky", ""), ev("output", "p/c", "TestFlaky", "run2\n"), ev("pass", "p/c", "TestFlaky", ""),
		ev("fail", "p/c", "", ""))
	if got != "run1-failure\n"+passedNote+"run2\n" {
		t.Fatalf("fail then pass: log = %q", got)
	}
}

func TestHungTestIsFlushedWhenItsPackageEnds(t *testing.T) {
	got := render(t,
		ev("run", "p/d", "TestHang", ""), ev("output", "p/d", "TestHang", "before-hang\n"),
		ev("output", "p/d", "TestHang", "panic: test timed out after 2s\n"),
		ev("output", "p/d", "", "FAIL\tp/d\t2.0s\n"), ev("fail", "p/d", "", ""))
	if want := "before-hang\npanic: test timed out after 2s\nFAIL\tp/d\t2.0s\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestKilledRunFlushesAtEndOfInput(t *testing.T) {
	got := render(t, ev("run", "p/e", "TestX", ""), ev("output", "p/e", "TestX", "last words\n"))
	if got != "last words\n" {
		t.Fatalf("log = %q", got)
	}
}

func TestStrayLinesAndBuildOutputPassThrough(t *testing.T) {
	got := render(t, "42\n", "[\"x\"]\n", "not json at all\n", "null\n",
		`{"ImportPath":"p/f [p/f.test]","Action":"build-output","Output":"f.go:3: syntax error\n"}`+"\n",
		ev("output", "p/f", "", "FAIL\tp/f [build failed]\n"), ev("fail", "p/f", "", ""))
	want := "42\n[\"x\"]\nnot json at all\nnull\nf.go:3: syntax error\nFAIL\tp/f [build failed]\n"
	if got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestHugeTestOutputIsBounded(t *testing.T) {
	var in []string
	in = append(in, ev("run", "p/g", "TestChatty", ""))
	chunk := strings.Repeat("x", 1023) + "\n"
	for i := 0; i < 4096; i++ { // 4 MiB
		in = append(in, ev("output", "p/g", "TestChatty", chunk))
	}
	in = append(in, ev("fail", "p/g", "TestChatty", ""), ev("fail", "p/g", "", ""))
	got := render(t, in...)
	if len(got) > 2*maxHeld+200 || !strings.Contains(got, "earlier output of TestChatty dropped") {
		t.Fatalf("log is %d bytes, truncation note present: %v", len(got), strings.Contains(got, "dropped"))
	}
}

func TestPassingPackageHidesItsOwnNoise(t *testing.T) {
	got := render(t,
		ev("output", "p/h", "", "MAIN-SETUP-LINE\n"), ev("output", "p/h", "", "PASS\n"),
		ev("output", "p/h", "", "ok  \tp/h\t0.1s\n"), ev("pass", "p/h", "", ""))
	if got != "ok  \tp/h\t0.1s\n" {
		t.Fatalf("log = %q", got)
	}
}

func TestFailingParallelTestsLinesCreditedToAPassingTestStillShow(t *testing.T) {
	got := render(t,
		ev("run", "p/i", "TestA", ""), ev("run", "p/i", "TestB", ""),
		ev("output", "p/i", "TestB", "LOG-FROM-A-GOROUTINE\n"),
		ev("pass", "p/i", "TestB", ""), ev("output", "p/i", "TestA", "--- FAIL: TestA\n"), ev("fail", "p/i", "TestA", ""),
		ev("fail", "p/i", "", ""))
	if !strings.Contains(got, "LOG-FROM-A-GOROUTINE") {
		t.Fatalf("log = %q", got)
	}
}

func TestFrameLinesAreLeftOutByType(t *testing.T) {
	got := render(t,
		ev("run", "p/j", "TestBad", ""),
		`{"Action":"output","Package":"p/j","Test":"TestBad","Output":"=== RUN   TestBad\n","OutputType":"frame"}`+"\n",
		`{"Action":"output","Package":"p/j","Test":"TestBad","Output":"--- FAIL: TestBad (0.00s)\n","OutputType":"frame"}`+"\n",
		`{"Action":"output","Package":"p/j","Test":"TestBad","Output":"=== RUN   my own line\n","OutputType":"text"}`+"\n",
		ev("fail", "p/j", "TestBad", ""), ev("fail", "p/j", "", ""))
	if got != "--- FAIL: TestBad (0.00s)\n=== RUN   my own line\n" {
		t.Fatalf("log = %q", got)
	}
}
