package hostprobe

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/softnet"
)

// managedMac is a Mac with a managed Softnet installed at /managed/current/softnet.
type managedMac struct {
	*macFacts
	st softnet.Status
}

func (m managedMac) ManagedSoftnet() (string, softnet.Status, bool) {
	return "/managed/current/softnet", m.st, true
}

func managedRunner(macOS string) *fakeRunner {
	run := macRunner("-rwsr-xr-x root", "")
	run.out["/usr/bin/sw_vers -productVersion"] = ok(macOS + "\n")
	run.out["/fake/softnet --version"] = ok("0.23.0\n")
	run.out[softnetProbeLine("/fake/softnet")] = Result{Exit: 2, Stderr: "error: invalid value 'out proto udp @host port 53'"}
	run.out["/managed/current/softnet --version"] = ok(softnet.Builds[0].Version + "\n")
	run.out[softnetProbeLine("/managed/current/softnet")] = ok("")
	run.out["stat -L -f %Sp %Su /managed/current/softnet"] = ok("-rwsr-xr-x root\n")
	return run
}

// The managed install is what Potluck pins for Tart, whatever softnet PATH
// finds first, and preflight says the one on PATH is not used.
func TestManagedSoftnetWinsOverPATH(t *testing.T) {
	f := managedMac{mac(true), softnet.Status{Current: &softnet.Installed{ID: softnet.Builds[0].ID, Pinned: true}}}
	r := Detect(context.Background(), managedRunner("15.6"), f, "/state")
	l, _ := r.Layer("tart")
	if !l.Available || l.SoftnetPath != "/managed/current/softnet" || l.SoftnetBuild != softnet.Builds[0].ID {
		t.Fatalf("layer %+v", l)
	}
	if w := strings.Join(l.Warnings, ";"); !strings.Contains(w, "the softnet first on PATH (/fake/softnet) is not the one they run") {
		t.Fatalf("warnings %q", w)
	}
}

func TestAManagedSoftnetNotThisPotlucksBuildIsNamed(t *testing.T) {
	old := softnet.Builds
	t.Cleanup(func() { softnet.Builds = old })
	newer := old[0]
	newer.ID = "0.23.0-potluck-pr227-candidate.2"
	softnet.Builds = append([]softnet.Build{newer}, old...)
	f := managedMac{mac(false), softnet.Status{Current: &softnet.Installed{ID: old[0].ID, Pinned: true}}}
	l, _ := Detect(context.Background(), managedRunner("15.6"), f, "/state").Layer("tart")
	if w := strings.Join(l.Warnings, ";"); !l.Available || !strings.Contains(w, "update it with `potluck softnet install`") {
		t.Fatalf("layer %+v", l)
	}
	f.st.Current.Pinned, f.st.Current.Problem = false, "it is not setuid"
	l, _ = Detect(context.Background(), managedRunner("15.6"), f, "/state").Layer("tart")
	if l.Available || !strings.Contains(strings.Join(l.Problems, ";"), "install it again with `potluck softnet install`") {
		t.Fatalf("unsound managed install: %+v", l)
	}
}

// A macOS 15 Mac is never told to install upstream's latest release, which
// needs macOS 26.
func TestAMacOS15MacIsNotSentToUpstreamLatest(t *testing.T) {
	for _, have := range []bool{false, true} {
		run := managedRunner("15.6")
		l, _ := Detect(context.Background(), run, mac(have), "/state").Layer("tart")
		p := strings.Join(l.Problems, ";")
		if l.Available || strings.Contains(p, "install the latest upstream") || !strings.Contains(p, "need macOS 26") {
			t.Fatalf("softnet on PATH %v: %q", have, p)
		}
	}
	l, _ := Detect(context.Background(), managedRunner("26.1"), mac(false), "/state").Layer("tart")
	if p := strings.Join(l.Problems, ";"); !strings.Contains(p, "install the latest upstream Softnet") {
		t.Fatalf("macOS 26: %q", p)
	}
}

// A Mac whose macOS has no managed build in the matrix (upgraded to one
// upstream covers) does not use a managed copy left behind: preflight
// pins the softnet on PATH and says how to remove the managed one.
func TestAManagedSoftnetIsNotUsedOnAMacOSItIsNotFor(t *testing.T) {
	f := managedMac{mac(true), softnet.Status{Current: &softnet.Installed{ID: softnet.Builds[0].ID, Pinned: true}}}
	run := managedRunner("26.1")
	run.out[softnetProbeLine("/fake/softnet")] = ok("")
	l, _ := Detect(context.Background(), run, f, "/state").Layer("tart")
	if l.SoftnetPath != "/fake/softnet" || l.SoftnetBuild != "" {
		t.Fatalf("layer %+v", l)
	}
	if w := strings.Join(l.Warnings, ";"); !strings.Contains(w, "is not for this Mac's macOS (26.1)") || !strings.Contains(w, "softnet uninstall") {
		t.Fatalf("warnings %q", w)
	}
}

// A managed build the matrix still pins but not for this macOS is refused.
func TestAManagedBuildForAnotherMacOSIsAProblem(t *testing.T) {
	old := softnet.Builds
	t.Cleanup(func() { softnet.Builds = old })
	other := old[0]
	other.ID, other.MinMacOS, other.MaxMacOS = "0.23.0-test-for-16", 16, 16
	softnet.Builds = append(append([]softnet.Build{}, old...), other)
	f := managedMac{mac(false), softnet.Status{Current: &softnet.Installed{ID: other.ID, Pinned: true}}}
	l, _ := Detect(context.Background(), managedRunner("15.6"), f, "/state").Layer("tart")
	if l.Available || !strings.Contains(strings.Join(l.Problems, ";"), "is not built for macOS 15") {
		t.Fatalf("layer %+v", l)
	}
}

// A Softnet that does not run says which Softnet this Mac should use.
func TestASoftnetThatDoesNotRunGetsTheMatrixAdvice(t *testing.T) {
	run := managedRunner("15.6")
	run.out["/fake/softnet --version"] = Result{Exit: 134, Stderr: "dyld: Symbol not found"}
	l, _ := Detect(context.Background(), run, mac(true), "/state").Layer("tart")
	if p := strings.Join(l.Problems, ";"); !strings.Contains(p, "cannot run on this host") || !strings.Contains(p, "need macOS 26") {
		t.Fatalf("problems %q", p)
	}
}

// A withdrawn build that was this macOS's only one: preflight names the
// reason and the uninstall, and does not blame the macOS version.
func TestAWithdrawnSoleManagedBuildIsAProblemWithItsReason(t *testing.T) {
	old := softnet.Builds
	t.Cleanup(func() { softnet.Builds = old })
	w := old[0]
	w.Withdrawn = "it lets a box reach the LAN"
	softnet.Builds = []softnet.Build{w}
	f := managedMac{mac(false), softnet.Status{Current: &softnet.Installed{ID: w.ID, Problem: "Softnet " + w.ID + " is withdrawn: " + w.Withdrawn}}}
	l, _ := Detect(context.Background(), managedRunner("15.6"), f, "/state").Layer("tart")
	p := strings.Join(l.Problems, ";")
	if l.Available || !strings.Contains(p, "withdrawn: it lets a box reach the LAN") || !strings.Contains(p, "softnet uninstall") || strings.Contains(p, "is not for this Mac's macOS") {
		t.Fatalf("problems %q", p)
	}
}
