package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
)

// memSwitch is the host switch in memory, for hostNow and hostSet: the
// modes and the --for/--until rules a test needs, and the state each mode
// puts a machine with no schedule in.
type memSwitch struct {
	mode  string
	until *time.Time
}

func (m *memSwitch) now(int, int) (protocol.HostStatus, error) {
	mode := m.mode
	if mode == "" || m.until != nil && !time.Now().Before(*m.until) {
		mode = protocol.HostAuto
	}
	st := map[string]string{protocol.HostAuto: protocol.HostAccepting, protocol.HostOpen: protocol.HostAccepting,
		protocol.HostDrain: protocol.HostDraining, protocol.HostClosed: protocol.HostShut}[mode]
	h := protocol.HostStatus{Mode: mode, State: st}
	if m.until != nil && mode != protocol.HostAuto {
		h.Until = m.until.UTC().Format(time.RFC3339)
	}
	return h, nil
}

func (m *memSwitch) set(mode, forD, until, deadline string) (vmhost.HostOverride, error) {
	switch mode {
	case protocol.HostAuto, protocol.HostOpen, protocol.HostDrain, protocol.HostClosed:
	default:
		return vmhost.HostOverride{}, fmt.Errorf("mode %q is not open, drain, closed or auto", mode)
	}
	o := vmhost.HostOverride{Mode: mode, Since: time.Now().UTC()}
	if forD != "" {
		d, err := time.ParseDuration(forD)
		if err != nil {
			return vmhost.HostOverride{}, err
		}
		t := o.Since.Add(d)
		o.Until = &t
	}
	m.mode, m.until = mode, o.Until
	return o, nil
}

// useSwitch gives the test a switch of its own, auto, and answers it.
func useSwitch(t *testing.T) *memSwitch {
	t.Helper()
	oldNow, oldSet := hostNow, hostSet
	sw := &memSwitch{}
	hostNow, hostSet = sw.now, sw.set
	t.Cleanup(func() { hostNow, hostSet = oldNow, oldSet })
	return sw
}

// A pause is the host switch closed: host status and the menu show it,
// with Resume lending as the way back; it is refused while work runs
// (state refused, exit 0 with --json), resume hands the switch back to
// auto, and host stop, which leaves nothing shared, takes a pause with it.
func TestHostPauseIsTheHostSwitch(t *testing.T) {
	m := onePoolMachine(t, refPin)
	sw := useSwitch(t)
	status := func() menubar.Status {
		t.Helper()
		var st menubar.Status
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	type answer struct {
		State   string
		Sizes   []string
		Until   string
		Refused *struct{ Reason, Size, Message string }
		Next    string
	}
	ask := func(args ...string) answer {
		t.Helper()
		var a answer
		if err := json.Unmarshal([]byte(m.ok(t, append([]string{"--json", "host"}, args...)...)), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}

	if st := status(); st.Mode != protocol.HostAuto || !strings.HasPrefix(st.Menu.Title, "Lending ") {
		t.Fatalf("before: %+v", st.Menu)
	}
	if a := ask("pause"); a.State != "paused" || len(a.Sizes) != 1 || a.Sizes[0] != "solo-c1m2" || a.Next != "potluck host resume" || a.Refused != nil {
		t.Fatalf("pause: %+v", a)
	}
	st := status()
	if st.Mode != protocol.HostClosed || st.Menu.Title != "Paused: lending nothing" || st.Menu.Items[0].Label != "Resume lending" ||
		strings.Join(st.Menu.Items[0].Args, " ") != "host set auto" {
		t.Fatalf("paused status: %s %+v", st.Mode, st.Menu)
	}
	if out := m.ok(t, "host", "status"); !strings.Contains(out, "Paused: lending nothing") || !strings.Contains(out, "host switch: closed") {
		t.Fatalf("host status:\n%s", out)
	}
	if a := ask("resume"); a.State != "resumed" || sw.mode != protocol.HostAuto {
		t.Fatalf("resume: %+v, switch %s", a, sw.mode)
	}
	if a := ask("resume"); a.State != "not_paused" {
		t.Fatalf("resume again: %+v", a)
	}

	// Work running: refused, nothing changed.
	out := m.ok(t, "sandbox", "create", "--ttl", "5m")
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if a := ask("pause"); a.State != "refused" || a.Refused == nil || a.Refused.Reason != "job_running" || a.Refused.Size != "solo-c1m2" ||
		len(a.Sizes) != 0 || sw.mode != protocol.HostAuto {
		t.Fatalf("pause while work runs: %+v, switch %s", a, sw.mode)
	}
	if _, e, code := m.run(t, "host", "pause"); code != 1 || !strings.Contains(e, "work is running on solo-c1m2") || !strings.Contains(e, "potluck host set closed ends it now") {
		t.Fatalf("pause while work runs, text: exit %d\n%s", code, e)
	}
	m.ok(t, "sandbox", "rm", id)

	// --for resumes by itself; close is the menu's word for closed.
	if a := ask("pause", "--for", "2h"); a.State != "paused" || a.Until == "" {
		t.Fatalf("pause --for: %+v", a)
	}
	m.ok(t, "host", "set", "auto")
	m.ok(t, "host", "set", "close")
	if sw.mode != protocol.HostClosed {
		t.Fatalf("host set close: %s", sw.mode)
	}

	// host stop leaves nothing shared, and the pause goes with it.
	var stop struct {
		Removed   []string
		SwitchWas string `json:"switch_was"`
	}
	if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "stop")), &stop); err != nil || len(stop.Removed) != 1 || stop.SwitchWas != protocol.HostClosed {
		t.Fatalf("host stop: %+v %v", stop, err)
	}
	if st := status(); st.Mode != protocol.HostAuto || st.Menu.Title != "Not lending" {
		t.Fatalf("after stop: %s %+v", st.Mode, st.Menu)
	}
	// A stop that removed nothing leaves a pause as it is: the switch may
	// keep another program's sizes idle.
	m.ok(t, "host", "pause")
	if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "stop")), &stop); err != nil || len(stop.Removed) != 0 || sw.mode != protocol.HostClosed {
		t.Fatalf("host stop with nothing shared: %+v %v, switch %s", stop, err, sw.mode)
	}
	if a := ask("resume"); a.State != "resumed" || a.Next != "potluck host" {
		t.Fatalf("resume with nothing shared: %+v", a)
	}
}

// A share onto a paused machine says the sizes stay idle until resume.
func TestHostSaysItIsPaused(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	m.ok(t, "host", "pause")
	out := m.ok(t, "host", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo")
	if !strings.Contains(out, "note: the host switch is closed (paused): nothing new starts until potluck host resume") {
		t.Fatalf("host while paused:\n%s", out)
	}
}
