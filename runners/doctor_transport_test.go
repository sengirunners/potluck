package runners_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/release"
)

// doctorOnly is f's doctor of repo with no gh sign-in: the checks that
// read the broker's own records still answer.
func (f *fixture) doctorOnly(repo string) []string {
	f.svc.DoctorRead = func(context.Context, string, any) error { return fmt.Errorf("not signed in") }
	r := f.svc.Doctor(context.Background(), repo)
	var out []string
	for _, c := range r.Checks {
		out = append(out, c.Status+": "+c.Message)
	}
	return out
}

// Check 15 reads the checker's own record of its rounds (Checked): none
// yet warns, a complete round passes, one failed round warns, a round that
// answered long ago with nothing since warns, and rounds that keep failing
// fail, told once to the installation's owners. In both modes: with no
// public_url (check) the checker asks every 15 s, and a failing list backs
// off to the 5 minute catch-up, which must not split the failing streak.
func TestDoctorTransport(t *testing.T) {
	for _, mode := range []string{"webhook", "check"} {
		t.Run(mode, func(t *testing.T) { testDoctorTransport(t, mode) })
	}
}

func testDoctorTransport(t *testing.T, mode string) {
	gh := &doctorTransport{covered: true}
	f := doctorFixture(t, gh)
	if mode == "check" {
		f.unreachable(t)
		f.svc.HTTP = &http.Client{Transport: gh}
	}
	if got := f.setup(t).Check.Mode; got != mode {
		t.Fatalf("mode %s, want %s", got, mode)
	}
	check := func(want, text string) {
		t.Helper()
		if got := f.doctorOnly("acme/app")[14]; !strings.HasPrefix(got, want+": ") || !strings.Contains(got, text) {
			t.Fatalf("check 15: %s, want %s containing %q", got, want, text)
		}
	}
	check("warn", "not completed a check of GitHub")

	a := f.actions()
	f.svc.Check(context.Background())
	check("pass", "last completed a check of GitHub at")

	f.clock.advance(3 * time.Hour) // asleep: no round since
	check("warn", "none has run since")
	f.svc.Check(context.Background())
	check("pass", "last completed a check of GitHub at")

	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repos/acme/app/actions/runs" {
			return false
		}
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"Server Error"}`)
		return true
	}
	f.gh.mu.Unlock()
	f.clock.advance(5 * time.Minute)
	f.svc.Check(context.Background())
	check("warn", "a check since could not read: listing acme/app's queued runs")

	// Asleep for two hours; the first round after the wake fails too: a
	// new streak, not two hours of failing.
	f.clock.advance(2 * time.Hour)
	f.svc.Check(context.Background())
	check("warn", "a check since could not read: listing acme/app's queued runs")

	for range 5 { // failing round after round for 25 minutes: not yet
		f.clock.advance(5 * time.Minute)
		f.svc.Check(context.Background())
	}
	check("warn", "a check since could not read")
	for range 2 { // 35 minutes
		f.clock.advance(5 * time.Minute)
		f.svc.Check(context.Background())
	}
	check("fail", "checks have failed since")
	notices := f.noticesOf(15)
	var about []string
	f.dm.Deps.AddNotifier(func(_ context.Context, no core.Notice) error {
		if strings.Contains(no.Body, "check 15:") {
			about = append(about, no.Body)
		}
		return nil
	})
	f.svc.DoctorSelfCheck(context.Background())
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 1 || len(about) != 1 || !strings.HasPrefix(about[0], "GitHub installation ") {
		t.Fatalf("check 15 notices %d, want 1 about the installation: %q", *notices, about)
	}
}

// Check 17 compares the running build with the release this home installed
// last (release's State.Runs). It never claims a build is the newest, and
// tells a restart only when a restart runs the installed release: not for a
// build made from source, which a restart runs again.
func TestDoctorBuild(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.DoctorRead = func(context.Context, string, any) error { return fmt.Errorf("not signed in") }
	check := func(want, text, fix string) {
		t.Helper()
		c := f.svc.Doctor(context.Background(), "acme/app").Checks[16]
		if c.Status != want || !strings.Contains(c.Message, text) || !strings.Contains(c.Fix, fix) {
			t.Fatalf("check 17: %s: %s (fix %q), want %s containing %q, fix containing %q", c.Status, c.Message, c.Fix, want, text, fix)
		}
	}
	oldVersion, oldCommit := api.Version, api.Commit
	t.Cleanup(func() { api.Version, api.Commit = oldVersion, oldCommit })
	const full = "1eacf23e722f2df10a113480c8496e29607ba005"
	build := func(version, commit string) { api.Version, api.Commit = version, commit }

	build("v0.0.0-dev", "unknown")
	check("warn", "a development build that names no commit", "")
	build("v0.9.0", full) // a release build, and no release state
	check("warn", "not installed through the release channel", "")
	build("v0.0.0-20261112000000-1eacf23e722f+dirty", full[:12])
	check("warn", "made from source, and this machine installed no release", "")

	st := release.State{Version: "v0.9.0", Commit: "1eacf23", Created: time.Now()}
	if err := release.WriteState(f.cfg.Home, st); err != nil {
		t.Fatal(err)
	}
	build("v0.9.0", full)
	check("pass", "runs release v0.9.0", "")
	// Built from source at the release's commit: not the release, and a
	// restart would run the same build.
	build("v0.0.0-20261112000000-1eacf23e722f+dirty", full[:12])
	check("warn", "made from source, not release v0.9.0", "a restart runs the same build")

	st.Version, st.Commit = "v0.9.1", "2bbbbbbbbbbbbbbb"
	if err := release.WriteState(f.cfg.Home, st); err != nil {
		t.Fatal(err)
	}
	build("v0.9.0", full) // the old release still running
	check("warn", "not release v0.9.1", "to run the installed release")

	st.Commit = ""
	if err := release.WriteState(f.cfg.Home, st); err != nil {
		t.Fatal(err)
	}
	check("warn", "unavailable: the record of the release this machine installed (v0.9.1) names no commit", "")

	if err := os.WriteFile(release.StatePath(f.cfg.Home), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check("warn", "unavailable: the release this machine installed could not be read", "")
}

// check15 is check 15 of f's doctor of repo.
func (f *fixture) check15(t *testing.T, repo, want, text string) {
	t.Helper()
	if got := f.doctorOnly(repo)[14]; !strings.HasPrefix(got, want+": ") || !strings.Contains(got, text) {
		t.Fatalf("check 15 of %s: %s, want %s containing %q", repo, got, want, text)
	}
}

// refuseRuns makes acme/app's run lists answer 502, each after wait on the
// broker's clock (a request that runs into its timeout).
func (f *fixture) refuseRuns(a *fakeActions, wait time.Duration) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repos/acme/app/actions/runs" {
			return false
		}
		f.clock.advance(wait)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"Server Error"}`)
		return true
	}
}

// GitHub out of reach (no route to it): the doctor's own GitHub checks
// (1 and 2) cannot answer, but check 15 reads the broker's record, so a
// lasting outage fails it, told once to the installation's owners.
func TestDoctorTransportGitHubDown(t *testing.T) {
	gh := &doctorTransport{covered: true}
	f := doctorFixture(t, gh)
	f.actions()
	f.svc.Check(context.Background())
	f.check15(t, "acme/app", "pass", "last completed a check of GitHub at")

	gh.down.Store(true)
	for range 7 {
		f.clock.advance(5 * time.Minute)
		f.svc.Check(context.Background())
	}
	got := f.doctorOnly("acme/app")
	if !strings.HasPrefix(got[0], "warn: unavailable") {
		t.Fatalf("check 1 with GitHub out of reach: %s", got[0])
	}
	f.check15(t, "acme/app", "fail", "checks have failed since")
	notices := f.noticesOf(15)
	f.svc.DoctorSelfCheck(context.Background())
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 1 {
		t.Fatalf("check 15 notices %d, want 1", *notices)
	}
}

// Rounds that could not begin at all (the App's key unreadable) fail check
// 15 once they last, though no GitHub check of the doctor's answers; one
// cause behind two installations is told once.
func TestDoctorTransportCouldNotBegin(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	if err := f.svc.LinkAlso(context.Background(), inst, 12, "bob", "bob/app"); err != nil {
		t.Fatal(err)
	}
	f.restart(t, func(c *config.Config) { c.File.Runners.GitHub.PrivateKey = "file:/nonexistent/key.pem" })
	f.svc.Check(context.Background())
	f.check15(t, "acme/app", "warn", "last check of GitHub could not begin")
	for range 7 { // 35 minutes
		f.clock.advance(5 * time.Minute)
		f.svc.Check(context.Background())
	}
	for _, repo := range []string{"acme/app", "bob/app"} {
		f.check15(t, repo, "fail", "could not begin since")
	}
	notices := f.noticesOf(15)
	f.svc.DoctorSelfCheck(context.Background())
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 1 {
		t.Fatalf("check 15 notices %d for one cause behind two installations, want 1", *notices)
	}
}

// On a dead network every request waits for its timeout, and a round of
// many repositories outlasts the window between rounds: its failing rounds
// are still one streak, and check 15 fails once it lasts.
func TestDoctorTransportRoundsLongerThanTheWindow(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	a := f.actions()
	f.svc.Check(context.Background())
	f.refuseRuns(a, 15*time.Minute) // each round: 15 minutes
	for range 3 {
		f.clock.advance(5 * time.Minute)
		f.svc.Check(context.Background())
	}
	f.check15(t, "acme/app", "fail", "checks have failed since")
}

// A clock moved back past the start of a failing streak starts it again:
// the streak's start is not left in the future, where it could not last.
func TestDoctorTransportClockMovedBack(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	a := f.actions()
	f.svc.Check(context.Background())
	f.refuseRuns(a, 0)
	f.clock.advance(5 * time.Minute)
	f.svc.Check(context.Background())
	f.clock.advance(-time.Hour)
	for range 7 { // 35 minutes after the clock moved back
		f.clock.advance(5 * time.Minute)
		f.svc.Check(context.Background())
	}
	f.check15(t, "acme/app", "fail", "checks have failed since")
}

// A last round long ago with nothing since is a sleep while the daemon has
// not been awake as long; awake all that while, a round is stuck or the
// checker stopped, and check 15 fails.
func TestDoctorTransportStuckChecker(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.actions()
	f.svc.Check(context.Background())
	f.clock.advance(40 * time.Minute)
	f.check15(t, "acme/app", "warn", "none has run since")
	f.svc.Awake(f.clock.now().Add(-10 * time.Minute)) // woke ten minutes ago
	f.check15(t, "acme/app", "warn", "none has run since")
	f.svc.Awake(f.clock.now().Add(-2 * time.Hour))
	f.check15(t, "acme/app", "fail", "a round is stuck, or the checker stopped")
}

// A round cut by GitHub's rate limit after a complete one warns (it read
// less than everything), whether or not the complete one is still recent;
// a cut noted with the complete round itself (a list at its page cap)
// passes; a last round later than this machine's clock is no evidence.
func TestDoctorTransportCuts(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.PollBudget = 1 << 20
	a := f.actions()
	f.gh.mu.Lock()
	for i := range 301 {
		a.runs["acme/app"] = append(a.runs["acme/app"], &fakeRun{id: int64(20000 + i), status: "queued", private: true})
	}
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	f.check15(t, "acme/app", "pass", "That check read up to its limit: only the newest 300")

	f.gh.mu.Lock()
	a.refuse = lowRate("acme/app")
	f.gh.mu.Unlock()
	f.clock.advance(2 * time.Minute)
	f.svc.Check(context.Background())
	f.check15(t, "acme/app", "warn", "A check since read less than everything: GitHub's API budget")

	f.clock.advance(30 * time.Minute) // the complete round is old now
	f.svc.Check(context.Background())
	f.check15(t, "acme/app", "warn", "; a check since read less than everything: GitHub's API budget")

	g := doctorFixture(t, &doctorTransport{covered: true})
	g.actions()
	g.svc.Check(context.Background())
	g.clock.advance(-10 * time.Minute)
	g.check15(t, "acme/app", "warn", "later than this machine's clock")
}

// GitHub names the owner's installation, and it is not the one linked here
// (the App reinstalled): check 4 fails for it, and check 15 does not read
// the old link's record in its place.
func TestDoctorTransportNotTheOldLink(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true, ownerInst: 9})
	f.actions()
	f.svc.Check(context.Background())
	f.svc.DoctorRead = func(context.Context, string, any) error { return fmt.Errorf("not signed in") }
	r := f.svc.Doctor(context.Background(), "acme/app")
	if r.Installation != 9 || r.Checks[3].Status != "fail" || !strings.Contains(r.Checks[14].Message, "prerequisite check did not complete") {
		t.Fatalf("installation %d, check 4 %+v, check 15 %+v", r.Installation, r.Checks[3], r.Checks[14])
	}
}

// Rounds that could not begin, and a clock moved back past their start:
// the streak starts again rather than staying in the future.
func TestDoctorTransportCouldNotBeginClockMovedBack(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.restart(t, func(c *config.Config) { c.File.Runners.GitHub.PrivateKey = "file:/nonexistent/key.pem" })
	f.svc.Check(context.Background())
	f.clock.advance(-time.Hour)
	for range 7 { // 35 minutes after the clock moved back
		f.clock.advance(5 * time.Minute)
		f.svc.Check(context.Background())
	}
	f.check15(t, "acme/app", "fail", "could not begin since")
}
