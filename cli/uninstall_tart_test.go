package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/tartpin"
	"github.com/sengirunners/potluck/tartpin/tartpintest"
	"github.com/sengirunners/potluck/vmhost"
)

const pulledImage = "ghcr.io/potluck/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// lenderTart is a lender's fake Tart: Potluck's tart.app, and a list that
// holds entries (name to source).
type lenderTart struct {
	place   tartpin.Place
	entries map[string]string
	deleted []string
	broken  bool // the list cannot be read
	fails   bool // a delete fails
	ps      string
	psFails bool
}

// lenderTart puts Potluck's tart.app on the lender's Mac, with one image a
// share on home pulled.
func (l *lender) lenderTart(home string) *lenderTart {
	l.t.Helper()
	place, _ := tartPlace()
	f := &lenderTart{place: place, entries: map[string]string{pulledImage: "oci"}}
	tarball, _, b := tartpintest.Release(l.t, "1.0.0", tartpintest.Whole())
	if _, err := place.Install(tarball, b); err != nil {
		l.t.Fatal(err)
	}
	dir := os.Getenv("TART_HOME")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		l.t.Fatal(err)
	}
	start := time.Now()
	if err := hostprobe.StartTartShare(dir, home, []string{pulledImage}, start); err != nil {
		l.t.Fatal(err)
	}
	if err := hostprobe.RecordTartPull(dir, pulledImage, home, start, start); err != nil {
		l.t.Fatal(err)
	}
	repo, digest, _ := strings.Cut(pulledImage, "@")
	if err := os.MkdirAll(filepath.Join(dir, "cache", "OCIs", repo, digest), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache", "OCIs", repo, digest, "config.json"), nil, 0o600); err != nil {
		l.t.Fatal(err)
	}
	tartSearch = func() vmhost.TartSearch {
		return vmhost.TartSearch{LookPath: func(string) (string, bool) { return place.Program(), true }}
	}
	tartRunner = runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		if name == "/bin/ps" {
			if f.psFails {
				return hostprobe.Result{Exit: 1}, nil
			}
			return hostprobe.Result{Stdout: f.ps}, nil
		}
		if name != place.Program() {
			l.t.Errorf("ran %s", name)
		}
		switch {
		case args[0] == "list" && f.broken:
			return hostprobe.Result{Exit: 1}, nil
		case args[0] == "list":
			list := []map[string]any{}
			for n, src := range f.entries {
				list = append(list, map[string]any{"Source": src, "Name": n, "Size": 27})
			}
			out, _ := json.Marshal(list)
			return hostprobe.Result{Stdout: string(out)}, nil
		case args[0] == "delete" && f.fails:
			return hostprobe.Result{Exit: 1, Stderr: "no"}, nil
		case args[0] == "delete":
			f.deleted = append(f.deleted, args[1])
			delete(f.entries, args[1])
			return hostprobe.Result{}, nil
		}
		l.t.Errorf("tart %q", args)
		return hostprobe.Result{Exit: 2}, nil
	})
	return f
}

func (l *lender) uninstallJSON(args ...string) (uninstallAnswer, int) {
	l.t.Helper()
	out, words, code := softnetRunJSON(l.t, append([]string{"uninstall"}, args...)...)
	var a uninstallAnswer
	if err := json.Unmarshal(out, &a); err != nil || a.View == nil {
		l.t.Fatalf("%v: %s\n%s", err, out, words)
	}
	plainWords(l.t, a)
	return a, code
}

// jargon are words no one needs to know to remove an app, and what a path,
// a flag or a command looks like.
var jargon = regexp.MustCompile(`(?i)\b(daemon|token|softnet|sandbox(es)?|pools?|lease|json|sudo|root|argv|symlink|\.md)\b|--\w|/\w+/|~/|` + "`" + `|\bpotluck (host|daemon|uninstall|tart)\b`)

// plainWords checks every word of the answer that the window shows.
func plainWords(t *testing.T, a uninstallAnswer) {
	t.Helper()
	v := a.View
	shown := append([]string{v.Heading, v.Lead, v.Failed, v.Warning}, v.Stays...)
	seen := map[string]bool{}
	for _, s := range slices.Concat(v.Done, v.Left) {
		shown = append(shown, s.Will, s.Did)
		if s.ID == "" || seen[s.ID] || s.Will == "" || s.Did == "" {
			t.Errorf("step %+v", s)
		}
		seen[s.ID] = true
	}
	if v.Choice != nil {
		shown = append(shown, v.Choice.Label)
	}
	for _, o := range []*uninstallAction{a.Remove, a.Force} {
		if o != nil {
			shown = append(shown, o.Confirm, o.Detail)
		}
	}
	if a.AdminStep != nil {
		shown = append(shown, a.AdminStep.Why)
	}
	for _, s := range shown {
		if w := jargon.FindString(s); w != "" {
			t.Errorf("the window says %q: %s", w, s)
		}
	}
	if len(v.Done) != len(a.Done) || len(v.Left) != len(a.Left) {
		t.Errorf("the window's steps %+v %+v are not the terminal's %q %q", v.Done, v.Left, a.Done, a.Left)
	}
}

// Without the choice Tart and what was downloaded stay, and the window offers
// the choice with its own plan; with it they go, and the record with them.
func TestUninstallWithTart(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	plan, code := l.uninstallJSON("--dry-run")
	c := plan.View.Choice
	if code != 0 || c == nil || c.On || !slices.Equal(c.Args, []string{"--json", "uninstall", "--dry-run", "--with-tart"}) ||
		!strings.Contains(c.Label, "27 GB") || strings.Contains(strings.Join(plan.Remove.Args, " "), "--with-tart") ||
		!strings.Contains(strings.Join(plan.NotRemoved, "\n"), "tart.app Potluck installed") || !strings.Contains(strings.Join(plan.NotRemoved, "\n"), "tart-install.json") ||
		!slices.ContainsFunc(plan.View.Stays, func(s string) bool { return strings.HasPrefix(s, "Tart") && strings.Contains(s, "27 GB") }) {
		t.Fatalf("plan %+v %+v", plan, c)
	}
	ticked, _ := l.uninstallJSON(c.Args[2:]...)
	if b := ticked.View.Choice; b == nil || !b.On || !slices.Equal(b.Args, []string{"--json", "uninstall", "--dry-run"}) || !slices.Contains(ticked.Remove.Args, "--with-tart=all") ||
		!strings.Contains(strings.Join(ticked.Left, "\n"), "tart.app") || strings.Contains(strings.Join(ticked.NotRemoved, "\n"), "tart.app") || !strings.Contains(strings.Join(ticked.NotRemoved, "\n"), "potluck-pulls.json") || strings.Contains(strings.Join(ticked.NotRemoved, "\n"), "tart-install.json") {
		t.Fatalf("ticked %+v", ticked)
	}
	if len(f.deleted) != 0 || !l.exists(f.place.App()) {
		t.Fatal("a plan removed something")
	}
	if done, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart"); code != 0 || !slices.Equal(f.deleted, []string{pulledImage}) || l.exists(f.place.App()) || l.exists(filepath.Dir(f.place.RecordFile)) {
		t.Fatalf("deleted %v: %+v", f.deleted, done)
	}
}

func TestUninstallLeavesTartUnlessAsked(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	if a, code := l.uninstallJSON("--yes", "--no-sudo"); code != 0 || len(f.deleted) != 0 || !l.exists(f.place.Program()) || a.View.Choice != nil {
		t.Fatalf("%d %v %+v", code, f.deleted, a)
	}
}

// A VM or image of the person's own keeps Tart, and an unreadable list keeps
// everything; a run asked for the rest still removes Potluck.
func TestUninstallWithTartKeepsWhatIsTheirs(t *testing.T) {
	for name, c := range map[string]struct {
		set          func(f *lenderTart)
		deletes, app bool
	}{
		"their VM":       {func(f *lenderTart) { f.entries["my-vm"] = "local" }, true, true},
		"their image":    {func(f *lenderTart) { f.entries["ghcr.io/me/x:latest"] = "oci" }, true, true},
		"a job's VM":     {func(f *lenderTart) { f.entries["potluck-01234567-x"] = "local" }, true, true},
		"a running Tart": {func(f *lenderTart) { f.ps = "/opt/homebrew/bin/tart\n" }, true, true},
		"no list":        {func(f *lenderTart) { f.broken = true }, false, true},
		"their tart.app": {func(f *lenderTart) { _ = os.WriteFile(filepath.Join(f.place.App(), "mine"), nil, 0o600) }, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLender(t)
			m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
			t.Setenv(EnvHome, m.home)
			f := l.lenderTart(m.home)
			c.set(f)
			plan, _ := l.uninstallJSON("--dry-run", "--with-tart")
			if plan.View.Choice == nil || !strings.Contains(strings.Join(plan.Remove.Args, " "), "--with-tart") {
				t.Errorf("choice %+v, args %v", plan.View.Choice, plan.Remove.Args)
			}
			a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart")
			if name == "no list" { // it stops while it can be run again
				if code == 0 || len(f.deleted) != 0 || !l.exists(f.place.App()) || !l.exists(m.home) {
					t.Fatalf("exit %d: %+v", code, a)
				}
				return
			}
			if code != 0 || (len(f.deleted) == 1) != c.deletes || l.exists(f.place.App()) != c.app || l.exists(m.home) {
				t.Fatalf("exit %d deleted %v: %+v", code, f.deleted, a)
			}
		})
	}
}

// A removal whose plan took the images only never takes tart.app, though
// nothing keeps it any more.
func TestUninstallTakesNoMoreOfTartThanItsPlan(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	f.entries["my-vm"] = "local"
	plan, _ := l.uninstallJSON("--dry-run", "--with-tart")
	if !slices.Contains(plan.Remove.Args, "--with-tart=images") {
		t.Fatalf("%v", plan.Remove.Args)
	}
	delete(f.entries, "my-vm")
	if again, _ := l.uninstallJSON("--dry-run", "--with-tart=images"); strings.Contains(again.Remove.Detail, "Tart, which") || !strings.Contains(again.Remove.Detail, "disk images") {
		t.Fatalf("the question says %q", again.Remove.Detail)
	}
	if a, code := l.uninstallJSON(plan.Remove.Args[2:]...); code != 0 || len(f.deleted) != 1 || !l.exists(f.place.App()) {
		t.Fatalf("%d %v %+v", code, f.deleted, a)
	}
}

// What appears between the plan and the removal keeps Tart, and the window
// says Tart stayed rather than that it went.
func TestUninstallKeepsTartUsedMeanwhile(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	was := tartRunner
	tartRunner = runnerFunc(func(ctx context.Context, name string, args ...string) (hostprobe.Result, error) {
		r, err := was.Run(ctx, name, args...)
		if args[0] == "delete" {
			f.entries["my-vm"] = "local"
		}
		return r, err
	})
	a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart")
	if code != 0 || !l.exists(f.place.App()) || !slices.ContainsFunc(a.View.Stays, func(s string) bool { return strings.HasPrefix(s, "Tart: something") }) || slices.ContainsFunc(a.View.Done, func(s viewStep) bool { return s.ID == "tart" }) {
		t.Fatalf("%d %+v", code, a.View)
	}
}

// Of two images, the one deleted is done and the other is what stays.
func TestUninstallSaysWhichImagesItKept(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	second := "ghcr.io/potluck/second@sha256:" + strings.Repeat("b", 64)
	f.entries[second] = "oci"
	dir := filepath.Join(os.Getenv("TART_HOME"), "cache", "OCIs", strings.Replace(second, "@", "/", 1))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := hostprobe.RecordTartPull(os.Getenv("TART_HOME"), second, m.home, now, now); err != nil {
		t.Fatal(err)
	}
	was := tartRunner
	tartRunner = runnerFunc(func(ctx context.Context, name string, args ...string) (hostprobe.Result, error) {
		r, err := was.Run(ctx, name, args...)
		if args[0] == "delete" {
			f.entries["ghcr.io/potluck/second:mine"] = "oci"
		}
		return r, err
	})
	a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart")
	if code != 0 || len(f.deleted) != 1 || !slices.ContainsFunc(a.View.Done, func(s viewStep) bool { return s.Did == "Deleted some of the disk images Potluck downloaded" }) ||
		!strings.Contains(strings.Join(a.View.Stays, "\n"), "Some of the disk images") || strings.Contains(strings.Join(a.NotRemoved, "\n"), pulledImage) {
		t.Fatalf("%d %v %+v", code, f.deleted, a)
	}
}

// A removal of tart.app that was interrupted is finished by the next run.
func TestUninstallFinishesRemovingTart(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	aside := filepath.Join(f.place.AppsDir, ".tart.app.potluck-1")
	if err := os.Rename(f.place.App(), aside); err != nil {
		t.Fatal(err)
	}
	for _, unread := range []bool{true, false} { // another Tart is here
		f.broken = unread
		plan, _ := l.uninstallJSON("--dry-run", "--with-tart")
		if plan.View.Choice == nil || (plan.View.Left[0].ID == tartReadStep) != unread || !unread && !strings.Contains(strings.Join(plan.View.Stays, "\n"), "isn't where Potluck put one") {
			t.Fatalf("unread %v: %+v", unread, plan.View)
		}
	}
	none := func(string) bool { return false }
	tartSearch = func() vmhost.TartSearch {
		return vmhost.TartSearch{LookPath: func(string) (string, bool) { return "", false }, Executable: none, Exists: none, Home: l.home}
	}
	if plan, _ := l.uninstallJSON("--dry-run"); plan.View.Choice == nil {
		t.Fatalf("not offered: %+v", plan.View)
	}
	if a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart"); code != 0 || l.exists(aside) || l.exists(f.place.RecordFile) {
		t.Fatalf("%d %+v", code, a)
	}
}

// What cannot be read stops a run that may take something of Potluck's while
// the home and the programs are here to run it again with: in the plan, just
// before an image goes, and just before tart.app does. Without --with-tart, or
// with nothing of Potluck's there, it goes on.
func TestUninstallStopsWhenTartCannotBeRead(t *testing.T) {
	for name, c := range map[string]struct {
		at      int // the ps that fails, counted from 1 (0: none)
		args    []string
		deleted int
		goesOn  bool
		// noImages is that the retry finds no image of Potluck's.
		noImages bool
		set      func(f *lenderTart)
	}{
		"the plan's":           {at: 1, args: []string{"--with-tart"}},
		"the images step's":    {at: 2, args: []string{"--with-tart"}},
		"before an image's":    {at: 3, args: []string{"--with-tart=images"}},
		"before tart.app's":    {at: 4, args: []string{"--with-tart"}, deleted: 1},
		"not asked for":        {at: 1, goesOn: true},
		"nothing of Potluck's": {at: 1, args: []string{"--with-tart=images"}, goesOn: true, set: func(*lenderTart) { _ = os.Remove(filepath.Join(os.Getenv("TART_HOME"), hostprobe.TartPullsFile)) }},
		"an unreadable record's": {args: []string{"--with-tart=images"}, set: func(*lenderTart) {
			_ = os.WriteFile(filepath.Join(os.Getenv("TART_HOME"), hostprobe.TartPullsFile), []byte("{"), 0o600)
		}},
		"the tart.app's, with no record": {at: 1, noImages: true, args: []string{"--with-tart"}, set: func(*lenderTart) { _ = os.Remove(filepath.Join(os.Getenv("TART_HOME"), hostprobe.TartPullsFile)) }},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLender(t)
			m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
			t.Setenv(EnvHome, m.home)
			f := l.lenderTart(m.home)
			if c.set != nil {
				c.set(f)
			}
			was, n := tartRunner, 0
			tartRunner = runnerFunc(func(ctx context.Context, prog string, args ...string) (hostprobe.Result, error) {
				if prog == "/bin/ps" {
					if n++; n == c.at {
						return hostprobe.Result{Exit: 1}, nil
					}
				}
				return was.Run(ctx, prog, args...)
			})
			args := append([]string{"--yes", "--no-sudo"}, c.args...)
			a, code := l.uninstallJSON(args...)
			if (code == 0) != c.goesOn || l.exists(m.home) == c.goesOn || len(f.deleted) != c.deleted || !l.exists(f.place.App()) {
				t.Fatalf("exit %d deleted %v: %+v", code, f.deleted, a)
			}
			if !c.goesOn && n <= 1 && len(a.Done) != 0 {
				t.Fatalf("it knew from the start, and did %q", a.Done)
			}
			if c.goesOn || c.at == 0 {
				return
			}
			if a, code := l.uninstallJSON(args...); code != 0 || len(f.deleted) != 1 != c.noImages || l.exists(m.home) { // the retry
				t.Fatalf("again: exit %d deleted %v: %+v", code, f.deleted, a)
			}
		})
	}
}

// The window runs what its plan answered: while Tart cannot be read that
// still asks for all of it, and the box is there to untick.
func TestUninstallWindowKeepsAskingForTart(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	for _, asked := range []string{"--with-tart", "--with-tart=images"} {
		for _, how := range []func(bad bool){func(bad bool) { f.broken = bad }, func(bad bool) {
			f.psFails = bad
		}} {
			how(true)
			p, code := l.uninstallJSON("--dry-run", asked)
			want := asked
			if asked == "--with-tart" {
				want += "=all"
			}
			if code != 0 || !slices.Contains(p.Remove.Args, want) || !strings.Contains(p.Remove.Detail, "can't read") ||
				strings.Contains(p.Remove.Detail, "virtual machines stored elsewhere") != (asked == "--with-tart") ||
				p.View.Choice == nil || !p.View.Choice.On || p.View.Left[0].ID != tartReadStep || len(f.deleted) != 0 {
				t.Fatalf("%s: %d %+v %+v", asked, code, p.Remove, p.View)
			}
			a, code := l.uninstallJSON(p.Remove.Args[2:]...)
			if code == 0 || len(a.Done) != 0 || !l.exists(m.home) || !strings.Contains(a.View.Lead, "deselect the Tart option") || !strings.Contains(a.Error, "run potluck uninstall "+asked+" --no-sudo again") || len(f.deleted) != 0 {
				t.Fatalf("%s: %d %+v", asked, code, a)
			}
			off, code := l.uninstallJSON(p.View.Choice.Args[2:]...)
			if code != 0 || off.View.Choice == nil || off.View.Choice.On || strings.Contains(strings.Join(off.Remove.Args, " "), "tart") ||
				!strings.Contains(strings.Join(off.View.Stays, "\n"), "may hold disk images Potluck downloaded") {
				t.Fatalf("%s: %d %+v %+v", asked, code, off.Remove, off.View)
			}
			how(false)
		}
	}
	f.psFails = true
	p, _ := l.uninstallJSON("--dry-run", "--with-tart")
	f.psFails = false
	if a, code := l.uninstallJSON(p.Remove.Args[2:]...); code != 0 || len(f.deleted) != 1 || l.exists(f.place.App()) || l.exists(m.home) {
		t.Fatalf("%d %+v", code, a)
	}
}

// A removal that takes none of Tart waits no longer for it than a plan.
func TestUninstallWaitsForTartOnlyToRemoveIt(t *testing.T) {
	for args, long := range map[string]bool{"--yes --no-sudo": false, "--dry-run --with-tart": false, "--yes --no-sudo --with-tart": true} {
		l := newLender(t)
		m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
		t.Setenv(EnvHome, m.home)
		l.lenderTart(m.home)
		was, first := tartRunner, time.Duration(0)
		tartRunner = runnerFunc(func(ctx context.Context, prog string, a ...string) (hostprobe.Result, error) {
			if by, ok := ctx.Deadline(); ok && first == 0 {
				first = time.Until(by)
			}
			return was.Run(ctx, prog, a...)
		})
		l.uninstallJSON(strings.Fields(args)...)
		if first == 0 || first > uninstallTartWait != long {
			t.Errorf("%s waits %s", args, first)
		}
	}
}

// A folder that may hold the lender's own tart.app stays, and is said.
func TestUninstallSaysWhichTartFolderStays(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	theirs := filepath.Join(f.place.AppsDir, ".tart.app.swap-9")
	if err := os.MkdirAll(filepath.Join(theirs, "tart.app", "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart"); code != 0 || l.exists(f.place.App()) || !l.exists(theirs) || !l.exists(f.place.RecordFile) || l.exists(m.home) ||
		!slices.ContainsFunc(a.View.Done, func(s viewStep) bool { return s.Did == "Removed Tart, which Potluck installed" }) ||
		!strings.Contains(strings.Join(a.View.Stays, "\n"), "may hold something of yours") || !strings.Contains(strings.Join(a.NotRemoved, "\n")+a.View.Details, ".tart.app.swap-9") {
		t.Fatalf("%d %+v", code, a)
	}
}

// A failed step is said in plain words with the terminal's in Details.
func TestUninstallViewOfAFailure(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	f.fails = true
	a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart")
	if code == 0 || !strings.Contains(a.View.Lead, "Delete the disk images") || !strings.Contains(a.View.Lead, "keep the rest of Tart") || !strings.Contains(a.Error, "or without --with-tart") || !strings.Contains(a.View.Details, "tart delete failed") || !l.exists(f.place.App()) || !l.exists(m.home) {
		t.Fatalf("%d %+v", code, a.View)
	}
}

func TestUninstallViewSaysWhyItStopped(t *testing.T) {
	p := uninstallPlan{}
	lending := uninstallStep{id: "lending", will: "Stop lending this Mac", did: "Stopped lending this Mac"}
	for _, c := range []struct {
		err     error
		forced  bool
		heading string
		anyway  bool
	}{
		{&needsForce{context.Canceled, true}, false, "Jobs are still running on this Mac", true},
		{&needsForce{context.Canceled, false}, false, "Potluck can't tell whether jobs are running on this Mac", true},
		{context.Canceled, true, "Not everything is removed yet", false},
	} {
		v := p.view(uninstallFlags{}, []viewStep{}, []uninstallStep{lending}, outcomeOf(lending, c.err, c.forced), uninstallAnswer{Left: []string{"x"}, Error: "x"})
		if v.Heading != c.heading || strings.Contains(v.Lead, "Remove anyway…") != c.anyway {
			t.Errorf("%v: %+v", c.err, v)
		}
		plainWords(t, uninstallAnswer{View: v, Left: []string{"x"}})
	}
}
