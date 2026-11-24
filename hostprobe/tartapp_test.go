package hostprobe

import (
	"context"
	"strings"
	"testing"
)

const (
	appTart     = "/Applications/tart.app/Contents/MacOS/tart"
	homeAppTart = "/Users/o/Applications/tart.app/Contents/MacOS/tart"
)

// A Mac with Tart dragged into Applications has no tart on PATH. The facts
// say which files exist; nothing reads this Mac's /Applications.
func tartAppMac(files ...string) *macFacts {
	m := mac(true)
	m.bins = map[string]bool{"softnet": true}
	m.paths = map[string]bool{}
	for _, f := range files {
		m.paths[f] = true
	}
	return m
}

func tartAppRunner(tart string) *fakeRunner {
	run := macRunner("-rwsr-xr-x root", "")
	for _, sub := range []string{" --version", " run --help"} {
		run.out[tart+sub] = run.out["/fake/tart"+sub]
	}
	return run
}

func macTartLayer(f Facts, run Runner) Layer {
	l, _ := Detect(context.Background(), run, f, "/state").Layer("tart")
	return l
}

func TestTartFoundInApplications(t *testing.T) {
	for _, app := range []string{appTart, homeAppTart} {
		l := macTartLayer(tartAppMac(app), tartAppRunner(app))
		if !l.Available || l.CLI != app || len(l.Problems) != 0 {
			t.Errorf("tart.app at %s: %+v", app, l)
		}
	}
}

func TestTartOnPathBeatsApplications(t *testing.T) {
	m := tartAppMac(appTart)
	m.bins["tart"] = true
	l := macTartLayer(m, tartAppRunner(appTart))
	if l.CLI != "/fake/tart" {
		t.Fatalf("PATH did not win: %q", l.CLI)
	}
}

func TestTartAppBeforeHomeApplications(t *testing.T) {
	l := macTartLayer(tartAppMac(appTart, homeAppTart), tartAppRunner(appTart))
	if l.CLI != appTart {
		t.Fatalf("/Applications did not win over ~/Applications: %q", l.CLI)
	}
}

// A tart.app that is found but cannot run is a problem with Tart's own
// words, not a silent skip to "not installed".
func TestBrokenTartAppIsReported(t *testing.T) {
	run := tartAppRunner(appTart)
	delete(run.out, appTart+" --version")
	l := macTartLayer(tartAppMac(appTart), run)
	p := strings.Join(l.Problems, ";")
	if l.Available || l.CLI != appTart || !strings.Contains(p, "Tart at "+appTart+" cannot run on this host") {
		t.Fatalf("a Tart that cannot run: %+v", l)
	}
}

func TestHollowTartAppIsReported(t *testing.T) {
	l := macTartLayer(tartAppMac("/Applications/tart.app"), macRunner("-rwsr-xr-x root", ""))
	p := strings.Join(l.Problems, ";")
	if l.Available || l.CLI != "" || !strings.Contains(p, "/Applications/tart.app has no Contents/MacOS/tart") || strings.Contains(p, "not installed") {
		t.Fatalf("an empty tart.app: %+v", l)
	}
}

func TestNoTartSaysHowToInstallWithoutATerminal(t *testing.T) {
	l := macTartLayer(tartAppMac(), macRunner("-rwsr-xr-x root", ""))
	p := strings.Join(l.Problems, ";")
	for _, want := range []string{"tart is not installed", "github.com/openai/tart/releases/latest", "drag tart.app into Applications"} {
		if !strings.Contains(p, want) {
			t.Errorf("problems %q lack %q", p, want)
		}
	}
}
