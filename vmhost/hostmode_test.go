package vmhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
)

// overrideAt points this test's drivers and supervisors at a host override
// file of its own (none yet) and answers its path.
func overrideAt(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "potluck", "host-override.json")
	old := hostOverridePath
	hostOverridePath = p
	t.Setenv(envHostOverride, p)
	t.Cleanup(func() { hostOverridePath = old })
	return p
}

// hostSet runs `host set` as the owner would.
func hostSet(t *testing.T, args ...string) {
	t.Helper()
	var out, errs bytes.Buffer
	if c := hostCmd(append([]string{"set"}, args...), nil, &out, &errs); c != 0 {
		t.Fatalf("host set %v: exit %d: %s", args, c, errs.String())
	}
}

func TestOverrideFileIsReadFailingClosed(t *testing.T) {
	p := overrideAt(t)
	if o, err := readOverride(p, os.Getuid()); err != nil || o.Mode != modeAuto {
		t.Fatalf("no file: %+v %v, want auto", o, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	good := `{"mode":"drain","since":"` + since + `","deadline_s":60}`
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(good)
	if o, err := readOverride(p, os.Getuid()); err != nil || o.Mode != modeDrain || o.DeadlineS == nil || *o.DeadlineS != 60 {
		t.Fatalf("a drain: %+v %v", o, err)
	}
	for _, c := range []struct{ name, body string }{
		{"not JSON", "drain"},
		{"empty", ""},
		{"an unknown mode", `{"mode":"opne","since":"` + since + `"}`},
		{"an unknown field", `{"mode":"open","since":"` + since + `","owner_apps":["x"]}`},
		{"no since", `{"mode":"closed"}`},
		{"two values", good + good},
		{"until before since", `{"mode":"open","since":"` + since + `","until":"2001-01-01T00:00:00Z"}`},
		{"a deadline off a drain", `{"mode":"closed","since":"` + since + `","deadline_s":5}`},
		{"a negative deadline", `{"mode":"drain","since":"` + since + `","deadline_s":-1}`},
	} {
		write(c.body)
		if o, err := readOverride(p, os.Getuid()); err == nil {
			t.Errorf("%s: read as %+v, want an error", c.name, o)
		}
	}
	write(good)
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readOverride(p, os.Getuid()); err == nil || !strings.Contains(err.Error(), "written by others") {
		t.Errorf("a world-writable file: %v", err)
	}
	write(good)
	if os.Getuid() != 0 {
		if _, err := readOverride(p, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "not the host's owner") {
			t.Errorf("a file another user owns: %v", err)
		}
	}
	write(good)
	if err := os.Chmod(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := readOverride(p, os.Getuid()); err == nil || !strings.Contains(err.Error(), "written by others") {
		t.Errorf("a world-writable directory: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := readOverride(p, os.Getuid()); err == nil || !strings.Contains(err.Error(), "written by others") {
		t.Errorf("no file in a world-writable directory (anyone could have removed it): %v", err)
	}
	if err := os.Chmod(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	write(good)
	other := filepath.Join(filepath.Dir(p), "other.json")
	if err := os.Rename(p, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, p); err != nil {
		t.Fatal(err)
	}
	if _, err := readOverride(p, os.Getuid()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlink: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readOverride(p, os.Getuid()); err == nil {
		t.Error("a directory in the file's place reads")
	}
}

// The override decides before the policy; the policy only in auto.
func TestHostStateFromOverrideAndPolicy(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	at := func(t time.Time) *time.Time { return &t }
	n := func(i int) *int { return &i }
	asked := false
	says := func(ok bool, why string, rd reading) func() (bool, string, reading) {
		return func() (bool, string, reading) { asked = true; rd.at = now; return ok, why, rd }
	}
	outside := says(false, "outside the window 09:00-10:00", reading{outside: true})
	input := says(false, "the owner is active", reading{byInput: true})
	windowed := config{Reclaim: Policy{Window: "09:00-10:00"}, DrainDeadline: 3 * 3600}
	for _, c := range []struct {
		name   string
		cfg    config
		ov     override
		ovErr  error
		policy func() (bool, string, reading)
		want   hostVerdict
		asks   bool
		why    string
	}{
		{"no override, no policy", config{}, override{Mode: modeAuto}, nil, outside, hostAdmits, false, ""},
		{"unreadable", windowed, override{}, errors.New("bad"), says(true, "", reading{}), hostHolds, true, "cannot be read"},
		{"unreadable, owner input in the window", windowed, override{}, errors.New("bad"), input, hostCloses, true, "cannot be read"},
		{"unreadable, leaving the window", windowed, override{}, errors.New("bad"), outside, hostDrains, true, "outside"},
		{"open outside the window", windowed, override{Mode: modeOpen, Since: ago(time.Hour)}, nil, outside, hostAdmits, false, ""},
		{"open past its until", windowed, override{Mode: modeOpen, Since: ago(time.Hour), Until: at(ago(time.Minute))}, nil, outside, hostDrains, true, "outside"},
		{"closed", config{}, override{Mode: modeClosed, Since: ago(time.Hour)}, nil, says(true, "", reading{}), hostCloses, false, "closed"},
		{"drain inside its deadline", windowed, override{Mode: modeDrain, Since: ago(time.Minute)}, nil, says(true, "", reading{}), hostDrains, false, "draining"},
		{"drain past its deadline", windowed, override{Mode: modeDrain, Since: ago(time.Minute), DeadlineS: n(30)}, nil, nil, hostCloses, false, drainCut},
		{"drain with deadline 0", config{}, override{Mode: modeDrain, Since: now, DeadlineS: n(0)}, nil, nil, hostCloses, false, drainCut},
		{"auto, inside the window", windowed, override{Mode: modeAuto, Since: ago(time.Hour)}, nil, says(true, "", reading{}), hostAdmits, true, ""},
		{"auto, owner input in the window", windowed, override{Mode: modeAuto}, nil, input, hostCloses, true, "active"},
		{"auto, leaving the window drains", windowed, override{Mode: modeAuto}, nil, outside, hostDrains, true, "outside"},
		{"auto, leaving the window with deadline 0", config{Reclaim: Policy{Window: "09:00-10:00"}}, override{Mode: modeAuto}, nil, outside, hostCloses, true, drainCut},
		{"auto, two hours past a 30 min drain", config{Reclaim: Policy{Window: "09:00-10:00"}, DrainDeadline: 1800}, override{Mode: modeAuto}, nil, outside, hostCloses, true, drainCut},
	} {
		asked = false
		h := c.cfg.hostAt(now, c.ov, c.ovErr, c.policy)
		if h.verdict != c.want || asked != c.asks || !strings.Contains(h.why, c.why) {
			t.Errorf("%s: %s (asked the policy %v) why %q, want %s (%v) %q", c.name, h.state(), asked, h.why, hostState{verdict: c.want}.state(), c.asks, c.why)
		}
	}
	// An open that ended at 11:30, outside the window, began the drain then.
	short := config{Reclaim: Policy{Window: "09:00-10:00"}, DrainDeadline: 3600}
	if h := short.hostAt(now, override{Mode: modeOpen, Since: ago(5 * time.Hour), Until: at(ago(30 * time.Minute))}, nil, outside); h.verdict != hostDrains || !h.since.Equal(ago(30*time.Minute)) {
		t.Errorf("auto after an open ended: %s since %v, want draining since %v", h.state(), h.since, ago(30*time.Minute))
	}
	if h := short.hostAt(now, override{Mode: modeAuto, Since: ago(10 * time.Minute)}, nil, outside); h.verdict != hostDrains {
		t.Errorf("host set auto 10 min ago, outside the window: %s, want draining", h.state())
	}
	// Leaving the window at 10:00 began the drain then, not when it was seen.
	h := windowed.hostAt(now, override{Mode: modeAuto}, nil, outside)
	if want := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local); !h.since.Equal(want) || h.deadline != 3*time.Hour {
		t.Errorf("the window's drain began %v (deadline %v), want %v (3h)", h.since, h.deadline, want)
	}
}

func TestWindowsWithWeekdays(t *testing.T) {
	p := Policy{Windows: "mon-fri 18:00-08:00; sat,sun 00:00-00:00"}
	// 2026-11-13 is a Friday.
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{day(13, 12, 0), false}, // Friday noon
		{day(13, 18, 0), true},  // Friday evening
		{day(14, 7, 59), true},  // Saturday morning: Friday's window, and Saturday's
		{day(14, 12, 0), true},  // Saturday, all day
		{day(16, 7, 0), false},  // Monday 07:00: Sunday's all-day window ended at midnight; Monday's opens at 18:00
		{day(16, 9, 0), false},  // Monday morning
		{day(17, 7, 0), true},   // Tuesday 07:00: Monday's window
	} {
		in, err := p.inWindow(c.at)
		if err != nil {
			t.Fatal(err)
		}
		if in != c.want {
			t.Errorf("%s: in %v, want %v", c.at.Format("Mon 15:04"), in, c.want)
		}
	}
	if got, want := p.lastClose(day(16, 9, 0)), day(16, 0, 0); !got.Equal(want) {
		t.Errorf("last close before Monday 09:00: %v, want %v (Sunday's window)", got, want)
	}
	if got, want := p.lastClose(day(13, 12, 0)), day(13, 8, 0); !got.Equal(want) {
		t.Errorf("last close before Friday noon: %v, want %v (Thursday's window)", got, want)
	}
	// A DST day: the close is 17:00 on the wall clock, as inWindow reads it.
	if ny, err := time.LoadLocation("America/New_York"); err == nil {
		at := time.Date(2026, 3, 8, 17, 30, 0, 0, ny) // spring forward that morning
		if got := (Policy{Window: "09:00-17:00"}).lastClose(at); got.Hour() != 17 || got.Day() != 8 {
			t.Errorf("last close on a DST day: %v, want 17:00 that day", got)
		}
	}
	base := func(kv ...string) map[string]string {
		m := map[string]string{"target": "t", "kind": "run", "image": "img", "remote": "/srv/r.git", "state_dir": t.TempDir()}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	if c, err := parseConfig(base("windows", "mon-fri 18:00-08:00", "drain_deadline_s", "60")); err != nil || c.Reclaim.Windows == "" || c.DrainDeadline != 60 {
		t.Errorf("windows and drain_deadline_s: %+v %v", c.Reclaim, err)
	}
	if c, err := parseConfig(base()); err != nil || c.DrainDeadline != drainDeadlineDefault {
		t.Errorf("drain_deadline_s default: %d %v", c.DrainDeadline, err)
	}
	for _, kv := range [][]string{
		{"windows", "funday 09:00-10:00"},
		{"windows", "mon 9-10"},
		{"windows", "mon-fri 09:00-10:00;"},
		{"window", "09:00-10:00", "windows", "mon 09:00-10:00"},
		{"drain_deadline_s", "-1"},
		{"drain_deadline_s", "soon"},
		{"drain_deadline_s", "99999999"},
		{"host_owner_uid", "me"},
	} {
		if _, err := parseConfig(base(kv...)); err == nil {
			t.Errorf("%v is accepted", kv)
		}
	}
}

func TestHostSetArguments(t *testing.T) {
	now := time.Date(2026, 11, 12, 22, 0, 0, 0, time.Local)
	o, err := parseHostSet([]string{"open", "--for", "2h"}, now)
	if err != nil || o.Mode != modeOpen || o.Until == nil || !o.Until.Equal(now.Add(2*time.Hour)) || !o.Since.Equal(now) {
		t.Errorf("open --for 2h: %+v %v", o, err)
	}
	o, err = parseHostSet([]string{"--until", "07:30", "closed"}, now)
	if want := time.Date(2026, 11, 13, 7, 30, 0, 0, time.Local); err != nil || o.Until == nil || !o.Until.Equal(want) {
		t.Errorf("--until 07:30 closed: %+v %v, want until %v (tomorrow)", o, err, want)
	}
	// A DST day: --until is wall-clock time.
	if ny, err := time.LoadLocation("America/New_York"); err == nil {
		at := time.Date(2026, 3, 8, 1, 0, 0, 0, ny) // an hour before spring forward
		if o, err := parseHostSet([]string{"open", "--until", "09:00"}, at); err != nil || o.Until.In(ny).Hour() != 9 {
			t.Errorf("--until 09:00 on a DST day: %v %v, want 09:00", o.Until, err)
		}
	}
	o, err = parseHostSet([]string{"drain", "--deadline", "30m"}, now)
	if err != nil || o.DeadlineS == nil || *o.DeadlineS != 1800 {
		t.Errorf("drain --deadline 30m: %+v %v", o, err)
	}
	for _, bad := range [][]string{
		nil, {"wide-open"}, {"open", "--for", "2h", "--until", "23:00"}, {"auto", "--for", "1h"},
		{"open", "--deadline", "1m"}, {"drain", "--deadline", "soon"}, {"open", "--for", "-1h"},
		{"open", "--until", "2001-01-01T00:00:00Z"}, {"open", "--until", "25:00"}, {"open", "extra"},
		{"drain", "--deadline", "8000h"}, {"open", "--for", "500ms"}, {"open", "--for", "soon"},
		{"open", "--until", ""}, {"drain", "--deadline", ""},
	} {
		if o, err := parseHostSet(bad, now); err == nil {
			t.Errorf("%v: %+v, want an error", bad, o)
		}
	}
}

// host set writes a file every driver reads, 0644 in a 0755 directory, by
// rename; a drain set again keeps its start.
func TestHostSetWritesTheOverride(t *testing.T) {
	p := overrideAt(t)
	hostSet(t, "auto") // no file yet
	if o, err := readOverride(p, os.Getuid()); err != nil || o.Mode != modeAuto || o.Since.IsZero() {
		t.Fatalf("host set auto with no file: %+v %v", o, err)
	}
	future := fmt.Sprintf(`{"mode":"drain","since":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if err := os.WriteFile(p, []byte(future), 0o644); err != nil { // a drain written under a clock an hour ahead
		t.Fatal(err)
	}
	hostSet(t, "drain", "--for", "1m")
	if o, _ := readOverride(p, os.Getuid()); o.Since.After(time.Now()) {
		t.Errorf("a drain set again kept a start ahead of the clock: %v", o.Since)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	hostSet(t, "drain", "--deadline", "1h")
	o, err := readOverride(p, os.Getuid())
	if err != nil || o.Mode != modeDrain {
		t.Fatalf("after host set drain: %+v %v", o, err)
	}
	for path, want := range map[string]os.FileMode{p: 0o644, filepath.Dir(p): 0o755} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %o", path, fi.Mode(), err, want)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	hostSet(t, "drain", "--deadline", "2h")
	if again, err := readOverride(p, os.Getuid()); err != nil || !again.Since.Equal(o.Since) || *again.DeadlineS != 7200 {
		t.Errorf("a drain set again: %+v %v, want since %v kept, deadline 7200", again, err, o.Since)
	}
	hostSet(t, "auto")
	first, _ := readOverride(p, os.Getuid())
	time.Sleep(1100 * time.Millisecond)
	hostSet(t, "auto")
	if again, _ := readOverride(p, os.Getuid()); !again.Since.Equal(first.Since) {
		t.Errorf("auto set again moved its start from %v to %v (an auto drain's deadline would move)", first.Since, again.Since)
	}
	hostSet(t, "open", "--for", "1m")
	if o, err := readOverride(p, os.Getuid()); err != nil || o.Mode != modeOpen || o.Until == nil {
		t.Errorf("after open --for 1m: %+v %v", o, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Errorf("the directory holds %v, want the file alone (no temp left)", ents)
	}
	if err := os.Chmod(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if c := hostCmd([]string{"set", "closed"}, nil, &out, &errs); c == 0 {
		t.Error("host set into a world-writable directory succeeds")
	}
	if c := hostCmd([]string{"status"}, nil, &out, &errs); c != 2 {
		t.Errorf("host status: exit %d, want 2 (status reads through the daemon)", c)
	}
}

// acquire and describe follow the override; an unreadable one refuses new
// work and leaves running work alone; closed reclaims it.
func TestOverrideAdmitsAndReclaims(t *testing.T) {
	p := overrideAt(t)
	_, repo, m := fakeBinding(t, "linux-img")
	m["target"] = "t"
	n := 0
	acquire := func() (map[string]any, error) {
		n++
		return call(t, m, "acquire", "", map[string]any{"request_id": fmt.Sprint("k", n), "ttl_s": 60, "ref": "main",
			"commit": repo.Commit, "script": "probe.sh", "args": []string{"sleep", "60"}})
	}
	a, err := acquire()
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(); code(err) != "unavailable" || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("acquire with an unreadable override: %v, want unavailable", err)
	}
	d, _ := call(t, m, "describe", "", nil)
	if d["available"] != false || fmt.Sprint(d["host"]) != "map[mode:unreadable state:holding]" {
		t.Errorf("describe with an unreadable override: available %v host %v", d["available"], d["host"])
	}
	time.Sleep(1500 * time.Millisecond) // a supervisor tick or more
	if st, _ := call(t, m, "status", h, nil); st["state"] != "running" {
		t.Fatalf("a running run under an unreadable override: %v, want running", st)
	}
	hostSet(t, "open")
	m["window"], m["drain_deadline_s"] = time.Now().Add(-2*time.Hour).Format("15:04")+"-"+time.Now().Add(-time.Hour).Format("15:04"), "0"
	b, err := acquire()
	if err != nil {
		t.Fatalf("acquire outside the window while open: %v", err)
	}
	if _, err := call(t, m, "release", b["handle"].(string), nil); err != nil {
		t.Fatal(err)
	}
	delete(m, "window")
	hostSet(t, "closed")
	st := waitState(t, m, h, "reclaimed")
	if !strings.Contains(fmt.Sprint(st["why"]), "closed") {
		t.Errorf("closed: %v", st)
	}
	if _, err := acquire(); code(err) != "unavailable" || !strings.Contains(err.Error(), "closed") {
		t.Errorf("acquire while closed: %v", err)
	}
	hostSet(t, "auto")
	if _, err := acquire(); err != nil {
		t.Errorf("acquire back in auto: %v", err)
	}
}

// A drain lets a running run finish, refuses new work, and cuts the run at
// the deadline, counted in describe.
func TestDrainCutsAtTheDeadline(t *testing.T) {
	overrideAt(t)
	_, repo, m := fakeBinding(t, "linux-img")
	m["target"] = "t"
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "ref": "main",
		"commit": repo.Commit, "script": "probe.sh", "args": []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	hostSet(t, "drain", "--deadline", "3s")
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 60, "ref": "main",
		"commit": repo.Commit, "script": "probe.sh"}); code(err) != "unavailable" || !strings.Contains(err.Error(), "draining") {
		t.Errorf("acquire while draining: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if st, _ := call(t, m, "status", h, nil); st["state"] != "running" {
		t.Fatalf("a run 1.5 s into a 3 s drain: %v, want running", st)
	}
	st := waitState(t, m, h, "reclaimed")
	if !strings.HasSuffix(fmt.Sprint(st["why"]), drainCut) {
		t.Errorf("cut at the deadline: %v", st)
	}
	d, _ := call(t, m, "describe", "", nil)
	if host, _ := d["host"].(map[string]any); host["state"] != "closed" || host["drain_cut"] != float64(1) {
		t.Errorf("describe after the cut: %v", d["host"])
	}
	// The daemon's describe type carries the block (computeapi.RunTargetDescribe.Host).
	raw, _ := json.Marshal(d)
	var cd computeapi.RunTargetDescribe
	if err := json.Unmarshal(raw, &cd); err != nil || cd.Host == nil || cd.Host.Mode != modeDrain || cd.Host.State != "closed" ||
		cd.Host.DrainCut == nil || *cd.Host.DrainCut != 1 || cd.Host.DrainSince == "" || cd.Host.DrainDeadline == "" {
		t.Errorf("describe as the daemon reads it: %+v %v", cd.Host, err)
	}
}

// In a drain a runner box waiting for a job ends at once; one running a job
// finishes it, and its box ends when the runner exits.
func TestDrainEndsIdleRunnerBoxes(t *testing.T) {
	overrideAt(t)
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	dir := t.TempDir()
	runner := filepath.Join(dir, "actions-runner")
	if err := os.MkdirAll(runner, 0o700); err != nil {
		t.Fatal(err)
	}
	// A runner (its config is its name here) takes a job once the file
	// job-<config> appears, and exits once done-<config> does.
	run := fmt.Sprintf(`#!/bin/sh
while [ ! -e %[1]q/job-$2 ]; do sleep 0.1; done
echo "2026-11-12 12:00:00Z: Running job: build"
while [ ! -e %[1]q/done-$2 ]; do sleep 0.1; done
`, dir)
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(run), 0o755); err != nil {
		t.Fatal(err)
	}
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"], m["runner_dir"], m["linux_slots"] = "t", "box", "small", "1", "512", runner, "3"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	touch := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	boxes := map[string]string{}
	for _, cfg := range []string{"idleRunnerConfig01", "busyRunnerConfig01", "doneRunnerConfig01"} {
		a, err := call(t, m, "acquire", "", map[string]any{"request_id": cfg, "ttl_s": 60, "jit_config": cfg})
		if err != nil {
			t.Fatal(err)
		}
		boxes[cfg] = a["handle"].(string)
	}
	touch("job-busyRunnerConfig01")
	touch("job-doneRunnerConfig01")
	touch("done-doneRunnerConfig01") // its runner exits; the box stays ready, as outside a drain
	time.Sleep(time.Second)
	if st, _ := call(t, m, "status", boxes["doneRunnerConfig01"], nil); st["state"] != "ready" {
		t.Fatalf("a box whose runner exited, before the drain: %v, want ready", st)
	}
	hostSet(t, "drain", "--deadline", "1h")
	if st := waitState(t, m, boxes["idleRunnerConfig01"], "reclaimed"); !strings.Contains(fmt.Sprint(st["why"]), "had no job") {
		t.Errorf("the idle runner's box: %v", st)
	}
	if st := waitState(t, m, boxes["doneRunnerConfig01"], "reclaimed"); !strings.Contains(fmt.Sprint(st["why"]), "finished its job") {
		t.Errorf("the box whose runner finished: %v", st)
	}
	if st, _ := call(t, m, "status", boxes["busyRunnerConfig01"], nil); st["state"] != "ready" {
		t.Fatalf("a box running a job in a drain: %v, want ready", st)
	}
	touch("done-busyRunnerConfig01")
	if st := waitState(t, m, boxes["busyRunnerConfig01"], "reclaimed"); !strings.Contains(fmt.Sprint(st["why"]), "finished its job") {
		t.Errorf("the busy runner's box once its job ended: %v", st)
	}
}

// SetHost writes what host set writes and answers it as written: a drain set
// again answers the start it kept, and a bad request writes nothing.
func TestSetHostAnswersTheWrittenOverride(t *testing.T) {
	p := overrideAt(t)
	if _, err := SetHost("open", "1h", "23:00", ""); err == nil {
		t.Fatal("--for and --until together: no error")
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused SetHost wrote the file: %v", err)
	}
	first, err := SetHost("drain", "", "", "30m")
	if err != nil || first.Mode != modeDrain || first.DeadlineS == nil || *first.DeadlineS != 1800 || first.Since.IsZero() || first.Until != nil {
		t.Fatalf("drain --deadline 30m: %+v %v", first, err)
	}
	time.Sleep(1100 * time.Millisecond)
	again, err := SetHost("drain", "2h", "", "1h")
	if err != nil || !again.Since.Equal(first.Since) || again.Until == nil || again.DeadlineS == nil || *again.DeadlineS != 3600 {
		t.Fatalf("a drain set again: %+v %v, want since %v kept", again, err, first.Since)
	}
	if o, err := readOverride(p, os.Getuid()); err != nil || o.Since != again.Since || !o.Until.Equal(*again.Until) {
		t.Errorf("file %+v %v, SetHost answered %+v", o, err, again)
	}
}

// HostNow reads the switch with the driver's rules, for a program with no
// binding to describe: the mode in force, its state, and a drain's deadline
// from the override, else the drain_deadline_s it is given, else the default.
func TestHostNow(t *testing.T) {
	p := overrideAt(t)
	me := os.Getuid()
	if h, err := HostNow(me, -1); err != nil || h.Mode != modeAuto || h.State != "accepting" || h.DrainSince != "" {
		t.Fatalf("no file: %+v %v", h, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	at := func(d time.Duration) string { return since.Add(d).Format(time.RFC3339) }
	for _, c := range []struct {
		name, body   string
		drainS       int
		state, cutAt string
		until        string
	}{
		{"drain, its own deadline", fmt.Sprintf(`{"mode":"drain","since":%q,"deadline_s":3600}`, at(0)), 600, "draining", at(time.Hour), ""},
		{"drain, the binding's deadline, passed", fmt.Sprintf(`{"mode":"drain","since":%q}`, at(0)), 300, "closed", at(5 * time.Minute), ""},
		{"drain, the default", fmt.Sprintf(`{"mode":"drain","since":%q}`, at(0)), -1, "draining", at(30 * time.Minute), ""},
		{"drain, a deadline out of range is the default", fmt.Sprintf(`{"mode":"drain","since":%q}`, at(0)), 1 << 40, "draining", at(30 * time.Minute), ""},
		{"open until", fmt.Sprintf(`{"mode":"open","since":%q,"until":%q}`, at(0), at(2*time.Hour)), -1, "accepting", "", at(2 * time.Hour)},
		{"closed", fmt.Sprintf(`{"mode":"closed","since":%q}`, at(0)), -1, "closed", "", ""},
	} {
		write(c.body)
		h, err := HostNow(me, c.drainS)
		if err != nil || h.State != c.state || h.DrainDeadline != c.cutAt || h.Until != c.until || h.DrainCut != nil || !h.Valid() {
			t.Errorf("%s: %+v %v, want state %s cut at %q until %q", c.name, h, err, c.state, c.cutAt, c.until)
		}
	}
	write("{")
	if h, err := HostNow(me, -1); err == nil || h.Mode != "unreadable" || h.State != "holding" {
		t.Errorf("an unreadable override: %+v %v", h, err)
	}
}
