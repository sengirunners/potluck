package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type macFacts struct {
	fakeFacts
	env      map[string]string
	diskDirs []string // every dir FreeDiskGB was asked about
	disk     map[string]float64
}

func (m *macFacts) Getenv(k string) string { return m.env[k] }
func (m *macFacts) FreeDiskGB(d string) (float64, error) {
	m.diskDirs = append(m.diskDirs, d)
	return m.disk[d], nil
}

func mac(softnet bool) *macFacts {
	f := fakeFacts{os: "darwin", arch: "arm64", cores: 10, ramMB: 32 << 10, bins: map[string]bool{"tart": true, "softnet": softnet}}
	return &macFacts{fakeFacts: f, env: map[string]string{"HOME": "/Users/o"}, disk: map[string]float64{"/Users/o/.tart": 200, "/state": 5}}
}

func macRunner(softnet, pmset string) *fakeRunner {
	out := map[string]Result{"sysctl -n kern.hv_support": ok("1\n"), "tart --version": ok("2.20.0\n"),
		"stat -L -f %Sp %Su /fake/softnet": ok(softnet + "\n")}
	if pmset != "" {
		out["pmset -g"] = ok(pmset)
	}
	return &fakeRunner{out: out}
}

func TestTartSoftnet(t *testing.T) {
	cases := []struct {
		name    string
		have    bool
		stat    string
		avail   bool
		problem string
		steps   []string
	}{
		{"setuid root", true, "-rwsr-xr-x root", true, "", nil},
		{"installed, not setuid", true, "-rwxr-xr-x root", false, "not setuid root", []string{"chown root '/fake/softnet'", "chmod u+s,go-w '/fake/softnet'"}},
		{"setuid but owned by the user", true, "-rwsr-xr-x jo", false, "not setuid root", []string{"chown root '/fake/softnet'", "chmod u+s,go-w '/fake/softnet'"}},
		{"setuid root but group-writable", true, "-rwsrwxr-x root", false, "another user can write", []string{"chown root '/fake/softnet'", "chmod u+s,go-w '/fake/softnet'"}},
		{"not installed", false, "", false, "Softnet is not installed", nil},
		{"unreadable", true, "garbage", false, "permissions could not be read", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Detect(context.Background(), macRunner(c.stat, ""), mac(c.have), "/state")
			l, _ := r.Layer("tart")
			if l.Available != c.avail {
				t.Errorf("available %t: %+v", l.Available, l)
			}
			if c.problem != "" && !strings.Contains(strings.Join(l.Problems, "|"), c.problem) {
				t.Errorf("problems %q lack %q", l.Problems, c.problem)
			}
			if strings.Join(l.RootSteps, "|") != strings.Join(c.steps, "|") {
				t.Errorf("root steps %q, want %q", l.RootSteps, c.steps)
			}
		})
	}
	// The one admin step reaches doctor's root line.
	r := Detect(context.Background(), macRunner("-rwxr-xr-x root", ""), mac(true), "/state")
	if got := RootCommand(r.RootSteps("")); !strings.Contains(got, "chmod u+s") {
		t.Errorf("root command %q", got)
	}
}

func TestTartDiskIsWhereTartStoresImages(t *testing.T) {
	f := mac(true)
	r := Detect(context.Background(), macRunner("-rwsr-xr-x root", ""), f, "/state")
	if r.TartDir != "/Users/o/.tart" || r.TartFreeGB != 200 {
		t.Errorf("tart dir %q, %v GB", r.TartDir, r.TartFreeGB)
	}
	if l, _ := r.Layer("tart"); len(l.Warnings) != 0 {
		t.Errorf("200 GB free warns: %q", l.Warnings)
	}
	// TART_HOME moves it; low disk there warns (not the state dir's 5 GB).
	f = mac(true)
	f.env["TART_HOME"] = "/Volumes/big/tart"
	f.disk["/Volumes/big/tart"] = 20
	r = Detect(context.Background(), macRunner("-rwsr-xr-x root", ""), f, "/state")
	l, _ := r.Layer("tart")
	if r.TartDir != "/Volumes/big/tart" || len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "20 GB free in /Volumes/big/tart") || !strings.Contains(l.Warnings[0], "needs about 60 GB") {
		t.Errorf("dir %q warnings %q", r.TartDir, l.Warnings)
	}
	if !l.Available {
		t.Error("low disk is a warning, not a reason the layer is unavailable")
	}
	// Free disk is read at the Tart dir, never under ~/.zm.
	for _, d := range f.diskDirs {
		if strings.Contains(d, ".zm") {
			t.Errorf("disk read under %s", d)
		}
	}
}

func TestTartSleepWarning(t *testing.T) {
	cases := []struct {
		name, pmset string
		warn        bool
	}{
		{"sleeps after 10 minutes", "System-wide power settings:\nCurrently in use:\n standby 1\n sleep 10\n displaysleep 5\n", true},
		{"sleep prevented right now", " sleep 1 (sleep prevented by caffeinate, powerd)\n", true},
		{"sleep disabled", " SleepDisabled 1\n sleep 10\n", false},
		{"never sleeps", "Currently in use:\n sleep                0\n displaysleep 5\n", false},
		{"displaysleep alone", " displaysleep 5\n", false},
		{"pmset unavailable", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Detect(context.Background(), macRunner("-rwsr-xr-x root", c.pmset), mac(true), "/state")
			l, _ := r.Layer("tart")
			got := strings.Contains(strings.Join(l.Warnings, "|"), "system sleep")
			if got != c.warn || !l.Available {
				t.Errorf("warnings %q available %t", l.Warnings, l.Available)
			}
		})
	}
}

// stuckRunner is a fakeRunner whose stat and pmset never answer until their
// context ends, as with a wedged powerd or a stale mount on PATH.
type stuckRunner struct{ *fakeRunner }

func (r stuckRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if name == "stat" || name == "pmset" {
		<-ctx.Done()
		return Result{Exit: -1}, ctx.Err()
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// A stuck stat or pmset does not hang doctor: each gives up after
// macProbeTimeout, Softnet reads as unreadable and no sleep warning is made up.
func TestTartPreflightCommandsAreBounded(t *testing.T) {
	defer func(d time.Duration) { macProbeTimeout = d }(macProbeTimeout)
	macProbeTimeout = 50 * time.Millisecond
	done := make(chan Report, 1)
	go func() {
		done <- Detect(context.Background(), stuckRunner{macRunner("-rwsr-xr-x root", " sleep 10\n")}, mac(true), "/state")
	}()
	select {
	case r := <-done:
		l, _ := r.Layer("tart")
		if !strings.Contains(strings.Join(l.Problems, "|"), "permissions could not be read") {
			t.Errorf("problems %q", l.Problems)
		}
		if strings.Contains(strings.Join(l.Warnings, "|"), "system sleep") {
			t.Errorf("warnings %q", l.Warnings)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Detect did not return with stat and pmset stuck")
	}
}

// streamRunner is a fakeRunner whose pulls print progress.
type streamRunner struct {
	*fakeRunner
	chunks []string
	block  bool
}

func (s streamRunner) RunStream(ctx context.Context, w io.Writer, name string, args ...string) (Result, error) {
	for _, c := range s.chunks {
		io.WriteString(w, c)
	}
	if s.block {
		<-ctx.Done()
		return Result{Exit: -1}, ctx.Err()
	}
	return Result{}, nil
}

func tartShareOptions(progress func(string)) ShareOptions {
	return ShareOptions{Report: Report{OS: "darwin", Arch: "arm64", Cores: 10, RAMMB: 32 << 10,
		Layers: []Layer{{Name: "tart", Available: true, Supported: true}}}, Images: []string{pinA}, Sizes: []string{"c2m4"},
		NamePrefix: "h", Progress: progress, PullTimeout: 50 * time.Millisecond}
}

func TestPullStreamsProgressAndTimesOut(t *testing.T) {
	var lines []string
	o := tartShareOptions(func(s string) { lines = append(lines, s) })
	sr := streamRunner{fakeRunner: &fakeRunner{}, block: true, chunks: []string{"pulling manifest\r[=>      ] 12.5%  1 GB / 8 GB\r"}}
	_, err := shareOn(context.Background(), sr, nil, o)
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("err %v", err)
	}
	// The throttle shows the first line at once and the last at the end.
	got := strings.Join(lines, "|")
	if !strings.Contains(got, "pull "+pinA) || !strings.Contains(got, "pulling manifest") || !strings.Contains(got, "pulling reg.example/base: 12.5%") {
		t.Errorf("progress: %q", got)
	}
}

func TestPullWithoutStreamingStillTimesOut(t *testing.T) {
	// A Runner that cannot stream: killed at the limit it reports exit -1.
	run := &fakeRunner{on: func(line string) (Result, bool) {
		time.Sleep(80 * time.Millisecond)
		return Result{Exit: -1}, true
	}}
	_, err := shareOn(context.Background(), run, nil, tartShareOptions(nil))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v", err)
	}
}

func TestLineWriterThrottles(t *testing.T) {
	var got []string
	now := time.Unix(0, 0)
	lw := &lineWriter{emit: func(s string) { got = append(got, s) }, every: 5 * time.Second, now: func() time.Time { return now }}
	fmt.Fprint(lw, "a 1%\r")
	now = now.Add(time.Second)
	fmt.Fprint(lw, "a 2%\ra 3%\r")
	now = now.Add(6 * time.Second)
	fmt.Fprint(lw, "a 9%\n\n")
	fmt.Fprint(lw, "done")
	lw.flush()
	if want := "a 1%|a 9%|done"; strings.Join(got, "|") != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// The real streaming path: output reaches the writer as it is printed, is kept
// in the Result, and a killed command returns at the limit even when a child
// keeps the pipes open.
func TestExecRunnerRunStream(t *testing.T) {
	var sb strings.Builder
	res, err := ExecRunner{}.RunStream(context.Background(), &sb, "sh", "-c", "echo out; echo err >&2; exit 3")
	if err != nil || res.Exit != 3 || !strings.Contains(sb.String(), "out") || !strings.Contains(sb.String(), "err") || res.Stdout != "out\n" {
		t.Fatalf("%v %+v %q", err, res, sb.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = ExecRunner{}.RunStream(ctx, io.Discard, "sh", "-c", "sleep 30 & wait")
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}

func TestParentDeadlineIsNotAPullTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	o := tartShareOptions(nil)
	o.PullTimeout = time.Hour
	run := &fakeRunner{on: func(string) (Result, bool) { <-ctx.Done(); return Result{Exit: -1}, true }}
	_, err := shareOn(ctx, run, nil, o)
	if err == nil || strings.Contains(err.Error(), "--pull-timeout") {
		t.Fatalf("err %v", err)
	}
}

func mustLayer(t *testing.T, r Report) Layer {
	t.Helper()
	l, ok := r.Layer("tart")
	if !ok {
		t.Fatal("no tart layer")
	}
	return l
}

// neverRuns fails the test when a share runs any command at all.
func neverRuns(t *testing.T) *fakeRunner {
	return &fakeRunner{on: func(line string) (Result, bool) {
		t.Errorf("ran %q; nothing may run before the host is ready", line)
		return Result{}, true
	}}
}

// A Mac without Softnet is refused, with what to do, before a download.
func TestShareRefusesWithoutSoftnetBeforeAnyPull(t *testing.T) {
	for _, c := range []struct {
		name, stat string
		have       bool
		root       bool
	}{{"not installed", "", false, false}, {"not setuid", "-rwxr-xr-x root", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			rep := Detect(context.Background(), macRunner(c.stat, ""), mac(c.have), "/state")
			o := tartShareOptions(nil)
			o.Report = rep
			run := neverRuns(t)
			_, err := shareOn(context.Background(), run, nil, o)
			var nr *NotReadyError
			if !errors.As(err, &nr) || !strings.Contains(err.Error(), "Softnet") {
				t.Fatalf("err %v", err)
			}
			if got := nr.RootCommand != ""; got != c.root {
				t.Errorf("root command %q, want one: %t", nr.RootCommand, c.root)
			}
			if len(run.calls) != 0 {
				t.Errorf("ran %q", run.calls)
			}
		})
	}
}

// tartListing is a runner that answers `tart list` with the given OCI images
// cached and fails the test on any other command (no pull, no conformance run).
func tartListing(t *testing.T, cached ...string) *fakeRunner {
	var entries []string
	for _, im := range cached {
		entries = append(entries, fmt.Sprintf(`{"Source":"oci","Name":%q}`, im))
	}
	return &fakeRunner{on: func(line string) (Result, bool) {
		if line != "tart list --format json" {
			t.Errorf("ran %q; only the cache listing may run before the disk is checked", line)
		}
		return ok("[" + strings.Join(entries, ",") + "]"), true
	}}
}

func macReportWithDisk(t *testing.T, freeGB float64) Report {
	f := mac(true)
	f.disk["/Users/o/.tart"] = freeGB
	return Detect(context.Background(), macRunner("-rwsr-xr-x root", ""), f, "/state")
}

// A Tart disk under what the selected images need is refused up front, in a
// dry run too, before any pull; the need is the largest min_free_gb of the
// images' pins.
func TestShareRefusesATooSmallTartDiskBeforeAnyPull(t *testing.T) {
	for _, c := range []struct {
		name, with string
		free       float64
		need       string
	}{{"default image", "", 40, "at least 60 GB"}, {"Xcode image", "xcode", 100, "at least 200 GB"}} {
		for _, dry := range []bool{false, true} {
			o := tartShareOptions(nil)
			o.Report, o.Images, o.With, o.DryRun = macReportWithDisk(t, c.free), nil, c.with, dry
			run := tartListing(t)
			_, err := shareOn(context.Background(), run, nil, o)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("only %.0f GB free in /Users/o/.tart", c.free)) || !strings.Contains(err.Error(), c.need) || !strings.Contains(err.Error(), "nothing was downloaded") {
				t.Fatalf("%s, dry %t: err %v", c.name, dry, err)
			}
		}
	}
	o := tartShareOptions(nil)
	o.Report, o.Images, o.With, o.DryRun = macReportWithDisk(t, 250), nil, "xcode", true
	if _, err := shareOn(context.Background(), tartListing(t), nil, o); err != nil {
		t.Errorf("250 GB free for the 200 GB image: %v", err)
	}
}

// An image Tart already has needs no pull, so a Mac with little free disk
// still lends it; the same disk is refused for an image it must pull.
func TestShareDoesNotRefuseACachedImage(t *testing.T) {
	pin, err := DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	o := tartShareOptions(nil)
	o.Report, o.Images, o.DryRun = macReportWithDisk(t, 5), nil, true
	if _, err := shareOn(context.Background(), tartListing(t, pin.Image), nil, o); err != nil {
		t.Errorf("cached image, 5 GB free: %v", err)
	}
	if _, err := shareOn(context.Background(), tartListing(t), nil, o); err == nil {
		t.Error("image not cached, 5 GB free: not refused")
	}
}

type unreadableDisk struct{ *macFacts }

func (unreadableDisk) FreeDiskGB(string) (float64, error) { return 0, errors.New("statfs: denied") }

// A disk that cannot be read is said so, in doctor and in share's notes, and
// is not taken for an empty one: share goes on.
func TestTartDiskUnreadIsLoudNotRefused(t *testing.T) {
	r := Detect(context.Background(), macRunner("-rwsr-xr-x root", ""), unreadableDisk{mac(true)}, "/state")
	l := mustLayer(t, r)
	if r.TartDiskUnread == "" || len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "could not read the free disk") {
		t.Errorf("unread %q warnings %q", r.TartDiskUnread, l.Warnings)
	}
	o := tartShareOptions(nil)
	o.Report, o.Images, o.DryRun = r, nil, true
	res, err := shareOn(context.Background(), neverRuns(t), nil, o)
	if err != nil {
		t.Fatalf("refused on a disk it could not read: %v", err)
	}
	if n := strings.Join(res.Notes, "|"); !strings.Contains(n, "disk check was skipped") || !strings.Contains(n, "statfs: denied") {
		t.Errorf("notes %q", res.Notes)
	}
}
