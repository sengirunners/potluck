package vmhost

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPolicy(t *testing.T) {
	at := func(hm string) time.Time {
		t, err := time.Parse("15:04:05", hm)
		if err != nil {
			t, _ = time.Parse("15:04", hm)
		}
		return t
	}
	for _, c := range []struct {
		name   string
		p      Policy
		now    string
		idle   time.Duration
		allows bool
	}{
		{"off", Policy{}, "12:00", 0, true},
		{"owner idle long enough", Policy{IdleS: 300}, "12:00", 301 * time.Second, true},
		{"owner active", Policy{IdleS: 300}, "12:00", 5 * time.Second, false},
		{"inside a day window", Policy{Window: "09:00-17:00"}, "12:00", 0, true},
		{"window end is exclusive", Policy{Window: "09:00-17:00"}, "17:00", 0, false},
		{"inside a window over midnight", Policy{Window: "22:00-07:00"}, "02:30", 0, true},
		{"outside a window over midnight", Policy{Window: "22:00-07:00"}, "12:00", 0, false},
		{"window and idle both needed", Policy{Window: "22:00-07:00", IdleS: 60}, "23:00", time.Second, false},
		{"a window to the second, open", Policy{Window: "11:00:00-12:00:30"}, "12:00:29", 0, true},
		{"a window to the second, closed", Policy{Window: "11:00:00-12:00:30"}, "12:00:30", 0, false},
	} {
		ok, why := c.p.Allows(at(c.now), c.idle)
		if ok != c.allows || (!ok && why == "") {
			t.Errorf("%s: allows %v (%q), want %v", c.name, ok, why, c.allows)
		}
	}
}

func TestConfig(t *testing.T) {
	base := map[string]string{"target": "t", "kind": "run", "image": "img", "remote": "https://github.com/o/r.git", "state_dir": t.TempDir()}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		name string
		cfg  map[string]string
		bad  string // a word the refusal names; "" = accepted
	}{
		{"defaults", with(), ""},
		{"no image", with("image", ""), "image"},
		{"a db kind", with("kind", "db"), "run and box"},
		{"a box without size", with("kind", "box", "images", "r@sha256:"+strings.Repeat("a", 64), "cpu", "1", "memory", "512"), "size"},
		{"a box image not pinned", with("kind", "box", "size", "s", "images", "ghcr.io/x/y:latest", "cpu", "1", "memory", "512"), "pinned"},
		{"a box without cpu", with("kind", "box", "size", "s", "images", "r@sha256:"+strings.Repeat("a", 64)), "cpu"},
		{"unknown network", with("network", "bridged"), "network"},
		{"guest fetch from a local path", with("remote", "/srv/r.git", "fetch", "guest"), "fetch=guest"},
		{"credentials in a remote the guest fetches", with("remote", "https://tok@github.com/o/r.git"), "credentials"},
		{"scripts_dir escaping the checkout", with("scripts_dir", "../x"), "scripts_dir"},
		{"bad window", with("window", "9-5"), "window"},
		{"boot timeout past acquire's limit", with("boot_timeout_s", "200"), "boot_timeout_s"},
	} {
		_, err := parseConfig(c.cfg)
		switch {
		case c.bad == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)):
			t.Errorf("%s: %v, want a refusal naming %s", c.name, err, c.bad)
		}
	}
	c, _ := parseConfig(with("macos_slots", "5", "remote", "/srv/r.git", "network", "nat"))
	if c.MacSlots != macCap || !c.HostFetch || c.Softnet {
		t.Errorf("macos_slots %d (cap %d), host fetch %v, softnet %v", c.MacSlots, macCap, c.HostFetch, c.Softnet)
	}
}

// cfg is a binding whose tart is missing, so nothing here can make a VM.
func cfg(t *testing.T) map[string]string {
	return map[string]string{"target": "t", "kind": "run", "image": "img", "remote": "/srv/r.git",
		"state_dir": t.TempDir(), "tart": filepath.Join(t.TempDir(), "no-tart")}
}

func code(err error) string {
	var e refusal
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}

// waitState waits up to 15 s for run h to reach state want, and returns its status.
func waitState(t *testing.T, m map[string]string, h, want string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		st, err := call(t, m, "status", h, nil)
		if err == nil && st["state"] == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v %v, want %s", h, st, err, want)
		}
	}
}

func call(t *testing.T, m map[string]string, verb, id string, args any) (map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := dispatch(verb, id, raw, m)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, nil
}

func TestRefusedBeforeAnyVM(t *testing.T) {
	m := cfg(t)
	commit := strings.Repeat("a", 40)
	run := func(kv ...any) map[string]any {
		a := map[string]any{"request_id": "k", "actor": "a", "ttl_s": 60, "ref": "main", "commit": commit, "script": "probe.sh"}
		for i := 0; i < len(kv); i += 2 {
			a[kv[i].(string)] = kv[i+1]
		}
		return a
	}
	for _, c := range []struct {
		name, verb, id string
		args           any
		want           string
	}{
		{"no request_id", "acquire", "", run("request_id", ""), "bad_request"},
		{"ttl past the cap", "acquire", "", run("ttl_s", 4*3600+1), "bad_request"},
		{"a shell string", "acquire", "", run("script", "probe.sh; touch x"), "bad_request"},
		{"a path", "acquire", "", run("script", "../probe.sh"), "bad_request"},
		{"a short commit", "acquire", "", run("commit", "abc123"), "bad_request"},
		{"no image to clone", "acquire", "", run(), "unavailable"},
		{"a malformed handle", "status", "garbage", nil, "bad_request"},
		{"another binding's handle", "status", "run:other/r000000000000", nil, "bad_request"},
		{"a db handle", "release", "db:t/x", nil, "bad_request"},
		{"a handle never issued", "status", "run:t/r000000000000", nil, "not_found"},
		{"an id of another shape", "renew", "run:t/zzz", map[string]int{"ttl_s": 60}, "not_found"},
		{"an unknown verb", "setup", "", nil, "not_implemented"},
	} {
		if _, err := call(t, m, c.verb, c.id, c.args); code(err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
	if ps, _ := filepath.Glob(filepath.Join(m["state_dir"], "runs", "*")); len(ps) > 0 {
		t.Errorf("refusals left records: %v", ps)
	}
}

func deadPID(t *testing.T) int {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func put(t *testing.T, m map[string]string, r record) record {
	t.Helper()
	r.Target, r.Handle = m["target"], "run:"+m["target"]+"/"+r.ID
	if r.Key == "" {
		r.Key = r.ID
	}
	st := store{m["state_dir"]}
	if err := st.locked(func() error { return st.save(r) }); err != nil {
		t.Fatal(err)
	}
	return r
}

// A supervisor that died (killed, the Mac restarted) must not leave a run
// live: the next call to the driver ends it, as expired once past its deadline.
func TestSweepEndsWhatADeadSupervisorLeft(t *testing.T) {
	m := cfg(t)
	now := time.Now().UTC()
	past := put(t, m, record{ID: "r000000000001", State: stRunning, Supervisor: deadPID(t), Start: now.Add(-time.Hour),
		RunStart: now.Add(-time.Hour), Deadline: now.Add(-time.Minute)})
	early := put(t, m, record{ID: "r000000000002", State: stRunning, Supervisor: deadPID(t), Start: now,
		RunStart: now, Deadline: now.Add(time.Hour)})
	held := put(t, m, record{ID: "r000000000003", State: stRunning, Supervisor: os.Getpid(), Start: now,
		RunStart: now, Deadline: now.Add(time.Hour)})
	for _, c := range []struct {
		r     record
		state string
		exit  any
	}{{past, stExpired, 124.0}, {early, stLost, nil}, {held, stRunning, nil}} {
		st, err := call(t, m, "status", c.r.Handle, nil)
		if err != nil {
			t.Fatal(err)
		}
		if st["state"] != c.state || st["exit"] != c.exit {
			t.Errorf("%s: state %v exit %v, want %s %v", c.r.ID, st["state"], st["exit"], c.state, c.exit)
		}
	}
}

func TestRenewAndRelease(t *testing.T) {
	m := cfg(t)
	m["ttl_max_s"] = "600"
	now := time.Now().UTC()
	r := put(t, m, record{ID: "r000000000001", State: stRunning, Supervisor: os.Getpid(), Start: now.Add(-100 * time.Second),
		RunStart: now, Deadline: now.Add(time.Minute)})
	if _, err := call(t, m, "renew", r.Handle, map[string]int{"ttl_s": 550}); code(err) != "bad_request" {
		t.Errorf("renew past the lifetime since acquire: %v, want bad_request", err)
	}
	if _, err := call(t, m, "renew", r.Handle, map[string]int{}); code(err) != "bad_request" {
		t.Errorf("renew without ttl_s: %v, want bad_request", err)
	}
	if _, err := call(t, m, "renew", r.Handle, map[string]int{"ttl_s": 400}); err != nil {
		t.Errorf("renew inside the cap: %v", err)
	}
	for i, want := range []bool{true, false} {
		rel, err := call(t, m, "release", r.Handle, nil)
		if err != nil || rel["released"] != want || rel["exit"] != nil {
			t.Errorf("release %d: %v %v, want released %v and no exit", i+1, rel, err, want)
		}
	}
	seven := 7
	e := put(t, m, record{ID: "r000000000002", State: stEnded, Exit: &seven, Start: now, RunStart: now, End: now, Deadline: now.Add(time.Minute)})
	if rel, err := call(t, m, "release", e.Handle, nil); err != nil || rel["released"] != false || rel["exit"] != 7.0 {
		t.Errorf("release of an ended run: %v %v, want released false, exit 7", rel, err)
	}
	if _, err := call(t, m, "renew", e.Handle, map[string]int{"ttl_s": 60}); code(err) != "bad_request" {
		t.Errorf("renew of an ended run: %v, want bad_request", err)
	}
}

func TestReclaimTiming(t *testing.T) {
	seen := time.Date(2026, 11, 6, 12, 0, 1, 0, time.UTC)
	got := reclaimTiming(seen, seen.Add(200*time.Millisecond), 700*time.Millisecond, true)
	if want := "input 2026-11-06T12:00:00.3Z, seen 700 ms after it, VM gone 900 ms after it"; !strings.Contains(got, want) {
		t.Errorf("by input: %q, want %q in it", got, want)
	}
	if got := reclaimTiming(seen, seen.Add(200*time.Millisecond), 0, false); !strings.Contains(got, "VM gone 200 ms later") {
		t.Errorf("by window: %q", got)
	}
}

// Only owner input inside the window is a reclaim by input: the window
// closing is not, even while the owner is active.
func TestReclaimByInputOnlyWhenTheWindowAllows(t *testing.T) {
	now := time.Now()
	in := now.Add(-time.Minute).Format("15:04:05") + "-" + now.Add(time.Hour).Format("15:04:05")
	out := now.Add(-2*time.Hour).Format("15:04:05") + "-" + now.Add(-time.Hour).Format("15:04:05")
	if now.Hour() < 2 || now.Hour() > 21 {
		t.Skip("windows near midnight wrap; the case is the same, the fixture is not")
	}
	if _, err := hidIdle(); err != nil {
		t.Skip("no HID idle time here")
	}
	// IdleS huge: the owner always counts as active.
	if _, _, rd := (Policy{IdleS: 1 << 30, Window: in}).check(); !rd.byInput {
		t.Error("owner active inside the window: want byInput")
	}
	if _, _, rd := (Policy{IdleS: 1 << 30, Window: out}).check(); rd.byInput {
		t.Error("outside the window: want not byInput")
	}
}
