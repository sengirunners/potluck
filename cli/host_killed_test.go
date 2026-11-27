package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/menubar"
)

// envKilledAt makes this test binary a host run that stops for good at its
// nth command (1: the first), or at killedJoined, once it has made the file
// "reached" in envKilledDir.
const (
	envKilledAt  = "POTLUCK_TEST_KILLED_AT"
	envKilledDir = "POTLUCK_TEST_KILLED_DIR"
)

// killedJoined is the moment a run joined its pool and has not noted it.
const killedJoined = 0

// killedRun really starts a host run of args as its own process against m's
// daemon, kills it (SIGKILL) at its nth command, and answers whether it got
// there (false: it succeeded first).
func killedRun(t *testing.T, m *machine, nth int, stdin string, args ...string) bool {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.Marshal(args)
	c := exec.Command(os.Args[0], "-test.run=^TestHostRunChild$")
	c.Env = append(os.Environ(), envKilledAt+"="+strconv.Itoa(nth), envKilledDir+"="+dir, EnvHome+"="+m.home, "POTLUCK_TEST_KILLED_ARGS="+string(b))
	c.Stdin = strings.NewReader(stdin)
	log, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	c.Stdout, c.Stderr = log, log
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { _ = c.Wait(); close(ended) }()
	defer func() { <-ended }()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "reached")); err == nil {
			_ = c.Process.Signal(syscall.SIGKILL)
			return true
		}
		select {
		case <-ended:
			if out, _ := os.ReadFile(log.Name()); c.ProcessState.ExitCode() != 0 {
				t.Fatalf("the run ended before it could be killed, exit %d:\n%s", c.ProcessState.ExitCode(), out)
			}
			return false
		default:
		}
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatal("the run neither got there nor ended")
		}
	}
}

// withFlags is a button's arguments, in the foreground, with the flags a test
// machine's share takes.
func withFlags(do []string) []string {
	args := slices.DeleteFunc(slices.Clone(do), func(a string) bool { return a == "--detach" || a == "--" })
	return append(args, "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0")
}

// TestHostRunChild is killedRun's process.
func TestHostRunChild(t *testing.T) {
	at, err := strconv.Atoi(os.Getenv(envKilledAt))
	if err != nil {
		t.Skip("only as killedRun's process")
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("POTLUCK_TEST_KILLED_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	fakeHost()
	stop := func() {
		if err := os.WriteFile(filepath.Join(os.Getenv(envKilledDir), "reached"), nil, 0o600); err != nil {
			t.Error(err)
		}
		select {}
	}
	if at == killedJoined {
		hostJoined = stop
	}
	n := 0
	shareRunner = runnerFunc(func(_ context.Context, name string, a ...string) (hostprobe.Result, error) {
		n++
		fmt.Println("command", n, name, a)
		if n == at {
			stop()
		}
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	os.Exit(Run(context.Background(), args, os.Stdin, os.Stdout, os.Stderr))
}

// A setup killed at each of its commands, one after another: the checklist
// says it was cut off, never that it goes on or went through, and its one
// button, pressed with the invite again, finishes it.
func TestHostRunKilledAtEachStep(t *testing.T) {
	var at []string
	for nth := 1; ; nth++ {
		tn := tailnettest.New()
		owner := startMachine(t, tn, "owner", "owner@example.com")
		m := soloMachineOn(t, tn, "")
		owner.ok(t, "create", "friends")
		invite := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "judy", "--host"))
		if invite == nil {
			t.Fatal("no host invite")
		}
		useSwitch(t)
		share := []string{"host", "--invite-stdin", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"}
		if !killedRun(t, m, nth, invite[1]+"\n", share...) {
			break
		}
		var st menubar.Status
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
			t.Fatal(err)
		}
		v := st.Setup.View
		stage := st.Setup.Stages[len(st.Setup.Stages)-1].Name
		at = append(at, stage)
		if st.Setup.State != menubar.SetupInterrupted || v.Heading != "Setup stopped" || v.Do == nil || v.Do.Label != "Continue setup" || len(v.Do.InviteArgs) == 0 {
			t.Fatalf("killed at command %d (%s): state %s, %q, do %+v", nth, stage, st.Setup.State, v.Heading, v.Do)
		}
		press := slices.DeleteFunc(slices.Clone(v.Do.InviteArgs), func(a string) bool { return a == "--detach" })
		if o, e, code := m.runIn(t, invite[1]+"\n", press...); code != 0 {
			t.Fatalf("killed at command %d (%s): Continue setup: exit %d\n%s%s", nth, stage, code, o, e)
		}
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
			t.Fatal(err)
		}
		if st.Setup.State != menubar.SetupSucceeded || st.Lending.Sizes == 0 {
			t.Fatalf("killed at command %d (%s), then finished: %s, %d sizes", nth, stage, st.Setup.State, st.Lending.Sizes)
		}
		if r, err := hostsetup.Read(m.home); err != nil || r.Pool() != "friends" || r.PoolID == "" {
			t.Fatalf("the pool joined is not known by its id: %+v %v", r, err)
		}
	}
	if len(at) == 0 || !slices.Contains(at, hostsetup.Images) && !slices.Contains(at, hostsetup.Validation) {
		t.Errorf("killed only at %v", at)
	}
	t.Logf("killed at %v", at)
}

// A killed run whose invite was already spent on joining, and one whose pool
// this machine has since left: the button goes on without the spent invite,
// and asks for a new one, each time the only thing that can work.
func TestHostRunKilledInvite(t *testing.T) {
	tn := tailnettest.New()
	owner := startMachine(t, tn, "owner", "owner@example.com")
	m := soloMachineOn(t, tn, "")
	owner.ok(t, "create", "friends")
	invite := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "judy", "--host"))[1]
	useSwitch(t)
	if !killedRun(t, m, killedJoined, invite+"\n", "host", "--invite-stdin", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo") {
		t.Fatal("the run ended before it could be killed")
	}
	view := func() menubar.Status {
		t.Helper()
		var st menubar.Status
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	do := view().Setup.View.Do
	if do == nil || do.Label != "Continue setup" || len(do.InviteArgs) > 0 {
		t.Fatalf("a spent invite is asked for: %+v", do)
	}
	foreground := func(args []string) []string {
		return slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--detach" })
	}
	m.ok(t, foreground(do.Args)...)
	if st := view(); st.Setup.State != menubar.SetupSucceeded || st.Lending.Sizes != 1 {
		t.Fatalf("%s, %d sizes", st.Setup.State, st.Lending.Sizes)
	}

	run := func() *hostsetup.Run {
		t.Helper()
		r, err := hostsetup.Read(m.home)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	id := run().PoolID
	if id == "" || run().Pool() != "friends" {
		t.Fatalf("the pool named is not known by its id: %+v", run())
	}

	// The record torn: it goes on with the pool it joined to lend.
	m.ok(t, "host", "stop")
	if err := os.WriteFile(filepath.Join(m.home, hostsetup.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := view()
	if do = st.Setup.View.Do; do == nil || !slices.Contains(do.Args, "--instead-of-invite=friends") || !strings.Contains(st.Setup.View.Lead, "the group friends") {
		t.Fatalf("a torn record: %q, %+v", st.Setup.View.Lead, do)
	}
	m.ok(t, withFlags(do.Args)...)

	// Its spent invite pasted again: refused, and with none given it goes on
	// with the pool it joined.
	if _, e, code := m.runIn(t, invite+"\n", "host", "--invite-stdin", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"); code == 0 || run().Joining != nil || run().Else != "friends" || run().ElseID != id || !view().Setup.View.InviteRefused {
		t.Fatalf("a spent invite: exit %d, %+v\n%s", code, run(), e)
	}
	m.ok(t, foreground(view().Setup.View.Do.Args)...)
	if run().Pool() != "friends" || view().Setup.View.InviteRefused {
		t.Fatalf("it did not go on with the pool it joined: %+v", run())
	}
	// Another good one for the pool it is in: refused too, and it says which
	// pool is meant though there are two of that owner's.
	owner.ok(t, "create", "second")
	second := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "judy3", "--host", "-P", "second"))[1]
	m.ok(t, "join", second)
	for _, pool := range []string{"friends", "second"} {
		extra := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "more-"+pool, "--host", "-P", pool))[1]
		if _, e, code := m.runIn(t, extra+"\n", "host", "--invite-stdin", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"); code == 0 || run().Else != pool {
			t.Fatalf("an invite for %s, which it is in: exit %d, %+v\n%s", pool, code, run(), e)
		}
	}
	m.ok(t, "leave", "second")
	// One whose machine does not answer may have been spent: not refused.
	if _, e, code := m.runIn(t, strings.Replace(invite, "owner", "nobody", 1)+"\n", "host", "--invite-stdin", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"); code == 0 || run().Joining == nil || run().InviteRefused {
		t.Fatalf("an invite whose machine does not answer: exit %d, %+v\n%s", code, run(), e)
	}

	// In a pool of its own too, it left the one it lent to: no other is taken.
	m.ok(t, "create", "mine", "--via", "tunnel")
	m.ok(t, "host", "friends", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo")
	if run().PoolID != id {
		t.Fatalf("%+v, want pool %s", run(), id)
	}
	m.ok(t, "host", "stop")
	m.ok(t, "leave", "friends")
	st = view()
	if o, e, code := m.run(t, foreground(st.Setup.View.Do.Args)...); code == 0 || !strings.Contains(e+o, "Paste an invite first, or type the name of a group this Mac is in (mine)") {
		t.Fatalf("a Mac that left its pool, with no invite given: exit %d\n%s%s", code, o, e)
	}
	if st = view(); st.Lending.Sizes != 0 {
		t.Fatalf("%d sizes", st.Lending.Sizes)
	}
	do = st.Setup.View.Do
	if st.Setup.View.Heading != "This Mac isn't lending" || do == nil || do.Label != "Lend this Mac" || len(do.InviteArgs) == 0 || !strings.Contains(st.Setup.View.Lead, "invite") {
		t.Fatalf("a Mac that left its pool: %q, %q, %+v", st.Setup.View.Heading, st.Setup.View.Lead, do)
	}
	again := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "judy2", "--host", "-P", "friends"))[1]
	// An app from before invite_args appends the code to args.
	if o, e, code := m.run(t, append(foreground(do.Args), again)...); code != 0 {
		t.Fatalf("lend again: exit %d\n%s%s", code, o, e)
	}
	if st := view(); st.Setup.State != menubar.SetupSucceeded || st.Lending.Sizes != 1 {
		t.Fatalf("%s, %d sizes", st.Setup.State, st.Lending.Sizes)
	}

	// Its owner ends the pool while it lends: it lends to nobody, and says so.
	owner.ok(t, "disband", "friends", "--yes", "--force")
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if st = view(); st.Lending.Sizes == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a pool that ended: %+v\n%s", st.Setup.View, m.ok(t, "--json", "remotes"))
		}
	}
	if do = st.Setup.View.Do; st.Setup.View.Heading != "This Mac isn't lending" || do == nil || len(do.InviteArgs) == 0 {
		t.Fatalf("a pool that ended: %+v", st.Setup.View)
	}
	owner.ok(t, "create", "new")
	if o, e, code := m.runIn(t, hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "judy4", "--host", "-P", "new"))[1]+"\n", foreground(do.InviteArgs)...); code != 0 {
		t.Fatalf("lend to another: exit %d\n%s%s", code, o, e)
	}
	if st := view(); st.Setup.State != menubar.SetupSucceeded || st.Lending.Sizes != 1 {
		t.Fatalf("%s, %d sizes", st.Setup.State, st.Lending.Sizes)
	}

	// Its owner makes another of the ended one's name: lent to again, it is the
	// new one, whatever order the two are listed in.
	owner.ok(t, "create", "friends")
	if o, e, code := m.runIn(t, hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "judy5", "--host", "-P", "friends"))[1]+"\n", "host", "--invite-stdin", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"); code != 0 {
		t.Fatalf("join one of the same name: exit %d\n%s%s", code, o, e)
	}
	reused := run().PoolID
	if reused == "" || reused == id {
		t.Fatalf("the new one's id is %q, the ended one's %q", reused, id)
	}
	for range 6 {
		m.ok(t, "host", "friends", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo")
		if run().PoolID != reused {
			t.Fatalf("lent to pool %s, want %s", run().PoolID, reused)
		}
	}
}

// Only an answer that refuses says nothing was joined.
func TestJoinRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{api.Errorf(api.CodeForbidden, "spent"), true},
		{fmt.Errorf("join: %w", api.Errorf(api.CodeConflict, "already joined")), true},
		{api.Errorf(api.CodeUnavailable, "busy"), false},
		{api.Errorf(api.CodeInternal, "broken"), false},
		{context.DeadlineExceeded, false},
	} {
		if got := joinRefused(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}

// Only a refusal of the invite itself says a new one is needed.
func TestInviteRefused(t *testing.T) {
	ours := []api.Remote{{Project: api.ProjectRef{ID: "01a", Slug: "a"}}}
	two := append(ours, api.Remote{Project: api.ProjectRef{ID: "01b", Slug: "b"}})
	in := api.Errorf(api.CodeConflict, "already joined").WithData(api.ProjectRef{ID: "01b", Slug: "b"})
	for name, c := range map[string]struct {
		err     error
		ours    []api.Remote
		refused bool
		had     string
	}{
		"spent":                               {api.Errorf(api.CodeForbidden, "used"), ours, true, "a"},
		"spent, two joined there":             {api.Errorf(api.CodeForbidden, "used"), two, true, ""},
		"for one of two joined there":         {fmt.Errorf("join: %w", in), two, true, "b"},
		"for one joined, not from there":      {in, ours, true, ""},
		"another pool here has its name":      {api.Errorf(api.CodeConflict, "this machine already has a project named a"), ours, false, ""},
		"another machine has this one's":      {api.Errorf(api.CodeConflict, "machine name m is taken"), ours, false, ""},
		"no answer":                           {context.DeadlineExceeded, ours, false, ""},
		"this machine's own setting is wrong": {api.Errorf(api.CodeBadRequest, "x"), ours, false, ""},
	} {
		if had, refused := inviteRefused(c.err, c.ours); refused != c.refused || had.Slug != c.had {
			t.Errorf("%s: %q, %v", name, had.Slug, refused)
		}
	}
}

// A record a full disk or a power cut left unreadable, on a Mac that lends
// nothing: one button sets it up again.
func TestHostRecordUnreadable(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	m.ok(t, "host", "stop")
	for _, b := range []string{"", "{\"started\": \"2026"} {
		if err := os.WriteFile(filepath.Join(m.home, hostsetup.FileName), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		var st menubar.Status
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
			t.Fatal(err)
		}
		v := st.Setup.View
		if st.Setup.State != menubar.SetupUnreadable || v.Heading != "Potluck can't tell how setup went" || v.Do == nil || v.Do.Label != "Set up again" || v.Details == "" {
			t.Fatalf("%q: %s %q %+v", b, st.Setup.State, v.Heading, v.Do)
		}
		if st.Lending.Sizes != 0 {
			t.Fatalf("%q: lends already", b)
		}
		m.ok(t, withFlags(v.Do.Args)...)
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
			t.Fatal(err)
		}
		if st.Setup.State != menubar.SetupSucceeded || st.Lending.Sizes != 1 {
			t.Fatalf("%q: set up again: %s, %d sizes", b, st.Setup.State, st.Lending.Sizes)
		}
		if r, err := hostsetup.Read(m.home); err != nil || r.PoolID == "" {
			t.Fatalf("the pool it runs is not known by its id: %+v %v", r, err)
		}
		m.ok(t, "host", "stop")
	}
}
