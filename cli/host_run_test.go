package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/menubar"
)

// host --detach checks here and goes on in the background, its words in
// the home's host.log; one host run at a time, and host status says when
// one goes on.
func TestHostDetachAndOneRunAtATime(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	old := hostSpawn
	t.Cleanup(func() { hostSpawn = old })
	var spawned [][]string
	hostSpawn = func(args []string, arg string, log, lock *os.File) (int, error) {
		if arg != "trial" {
			t.Errorf("the background run's stdin %q, want the pool", arg)
		}
		if lock == nil || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			t.Error("the background run was not handed the lock, held")
		}
		spawned = append(spawned, args)
		_, err := log.WriteString("pulling ref-image: 40%\n")
		return 4242, err
	}
	share := []string{"host", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"}
	out := m.ok(t, append(share, "--detach")...)
	if len(spawned) != 1 || !strings.Contains(out, "sharing goes on in the background (process 4242): potluck host status says how it goes") ||
		!strings.Contains(out, filepath.Join(m.home, "host.log")) {
		t.Fatalf("host --detach: spawned %v\n%s", spawned, out)
	}
	// The background run is this command, its flags as given, without
	// --detach, never asking for a password, its pool on stdin.
	want := []string{"host", "--image=" + refPin, "--name-prefix=solo", "--reclaim-idle-s=0", "--sizes=c1m2", "--no-sudo", "--invite-stdin"}
	if !slices.Equal(spawned[0], want) {
		t.Errorf("spawned %q, want %q", spawned[0], want)
	}
	if _, err := os.Stat(filepath.Join(m.home, "host.pid")); !os.IsNotExist(err) {
		t.Errorf("the detaching run left its pid behind: %v", err)
	}
	// A dry run does nothing to go on with: it never detaches.
	m.ok(t, append(share, "--detach", "--dry-run")...)
	if len(spawned) != 1 {
		t.Fatalf("a dry run detached: %v", spawned)
	}
	// Marked as a background run before it reaches its own lock.
	if b, err := os.ReadFile(filepath.Join(m.home, "host.bgpid")); err != nil || string(b) != "4242\n" {
		t.Fatalf("host.bgpid after --detach: %q %v", b, err)
	}

	bg := func() *menubar.Background {
		t.Helper()
		var st menubar.Status
		if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
			t.Fatal(err)
		}
		return st.Background
	}
	if b := bg(); b == nil || b.State != "ended" || b.Last != "pulling ref-image: 40%" {
		t.Fatalf("background after the run: %+v", b)
	}
	// A run that takes the lock itself knows a marker left behind is stale
	// (here the background run never ran; else it was killed).
	m.ok(t, share...)
	if _, err := os.Stat(filepath.Join(m.home, "host.bgpid")); !os.IsNotExist(err) {
		t.Fatalf("a stale host.bgpid outlived a run that took the lock: %v", err)
	}

	// Another run holds the lock: host is refused, naming it; status says
	// it goes on.
	f, err := os.OpenFile(filepath.Join(m.home, "host.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.home, "host.pid"), []byte("777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, e, code := m.run(t, share...); code != 1 || !strings.Contains(e, "another host run is going on (process 777): potluck host status says how it goes") {
		t.Fatalf("host while another runs: exit %d\n%s", code, e)
	}
	if b := bg(); b == nil || b.State != "ended" {
		t.Fatalf("a run in the foreground is no background run: %+v", b)
	}
	if err := os.WriteFile(filepath.Join(m.home, "host.bgpid"), []byte("777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b := bg(); b == nil || b.State != "running" {
		t.Fatalf("background while a background run holds the lock: %+v", b)
	}
	if out := m.ok(t, "host", "status"); !strings.Contains(out, "sharing goes on in the background: pulling ref-image: 40%") {
		t.Fatalf("host status:\n%s", out)
	}
	os.Remove(filepath.Join(m.home, "host.bgpid"))

	// The run that started a background one hands it the lock, held: the
	// background run takes it as its own, marks itself, and lets it go.
	dup, err := syscall.Dup(int(f.Fd())) // as exec hands it: another fd, the same lock
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envHostLockFD, strconv.Itoa(dup))
	m.ok(t, share...)
	if os.Getenv(envHostLockFD) != "" {
		t.Error("the handed lock's variable outlived the run")
	}
	for _, n := range []string{"host.pid", "host.bgpid"} {
		if _, err := os.Stat(filepath.Join(m.home, n)); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", n, err)
		}
	}
	f.Close()         // the test's own hold on it
	m.ok(t, share...) // the lock is free again
}

// A handed lock's variable is read once and cleared, its fd kept from the
// processes the run starts; a variable naming stdin, stdout or stderr, or
// no open fd, is ignored and closes nothing.
func TestHandedLock(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dup, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(dup)
	t.Setenv(envHostLockFD, strconv.Itoa(dup))
	if n := handedLock(); n != dup || os.Getenv(envHostLockFD) != "" {
		t.Fatalf("handed %d (want %d), variable %q", n, dup, os.Getenv(envHostLockFD))
	}
	if flags, err := unix.FcntlInt(uintptr(dup), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Errorf("the handed fd is not close-on-exec: %d %v", flags, err)
	}
	for _, v := range []string{"1", "nope", "987654"} {
		t.Setenv(envHostLockFD, v)
		if n := handedLock(); n != -1 {
			t.Errorf("%q: handed %d", v, n)
		}
	}
	if _, err := os.Stdout.Stat(); err != nil {
		t.Errorf("stdout closed: %v", err)
	}
}

// The background run is the command as the program spells it below its
// root (a mounted one's words too) and the flags given, the host's own
// included, without --detach and --json; its argument goes on its stdin
// (--invite-stdin), never in its argv.
func TestDetachArgs(t *testing.T) {
	detached := func(arg string, asJSON bool, args ...string) []string {
		t.Helper()
		root := &cobra.Command{Use: "supper"}
		root.PersistentFlags().Bool("json", false, "")
		root.PersistentFlags().StringP("project", "P", "", "")
		group := &cobra.Command{Use: "pool"}
		var got []string
		host := &cobra.Command{Use: "host", RunE: func(cmd *cobra.Command, _ []string) error {
			got = detachArgs(cmd, arg, asJSON)
			return nil
		}}
		host.Flags().Bool("detach", false, "")
		host.Flags().Bool("no-sudo", false, "")
		host.Flags().StringArray("image", nil, "")
		group.AddCommand(host)
		root.AddCommand(group)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := detached("trial", true, "--json", "-P", "acme", "pool", "host", "trial", "--detach", "--image", "a@sha256:1", "--image", "b@sha256:2", "--no-sudo")
	if want := []string{"pool", "host", "--image=a@sha256:1", "--image=b@sha256:2", "--project=acme", "--no-sudo", "--invite-stdin"}; !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
	// An invite given as the pool flag's value goes on stdin alone.
	const invite = "potluck-mini:abcdefabcdefabcdefabcdefabcdefab:ABCD-EFGH"
	got = detached(invite, false, "-P", invite, "pool", "host", "--detach", "--image", "a@sha256:1")
	if want := []string{"pool", "host", "--image=a@sha256:1", "--no-sudo", "--invite-stdin"}; !slices.Equal(got, want) {
		t.Errorf("an invite as the pool flag: %q, want %q", got, want)
	}
}

// A host program that gives no Stdin still reads --invite-stdin's line
// from the process's own stdin, where the background run gets it.
func TestStdinInviteWithoutHostStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	old := os.Stdin
	t.Cleanup(func() { os.Stdin = old })
	os.Stdin = r
	if _, err := w.WriteString("trial\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	a := newApp(Host{Stdout: new(strings.Builder), Stderr: new(strings.Builder)})
	if got, err := a.stdinInvite(&cobra.Command{Use: "host"}); err != nil || got != "trial" {
		t.Fatalf("read %q, %v", got, err)
	}
}

// --when is when sandboxes may run, in words: any (--reclaim-idle-s 0) or
// away (the Mac's idle rule); never both it and --reclaim-idle-s.
func TestHostWhen(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	share := []string{"--json", "host", "--dry-run", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--no-sudo"}
	var res struct {
		ReclaimIdleS int `json:"reclaim_idle_s"`
	}
	if err := json.Unmarshal([]byte(m.ok(t, append(share, "--when", "any")...)), &res); err != nil || res.ReclaimIdleS != 0 {
		t.Fatalf("--when any: %+v %v", res, err)
	}
	if _, e, code := m.run(t, append(share, "--when", "away")...); code != 1 || !strings.Contains(e, "reads a Mac's keyboard and pointer idle time") {
		t.Fatalf("--when away on linux: exit %d %s", code, e)
	}
	if _, e, code := m.run(t, append(share, "--when", "never")...); code != 2 || !strings.Contains(e, `--when "never": want any or away`) {
		t.Fatalf("--when never: exit %d %s", code, e)
	}
	if _, e, code := m.run(t, append(share, "--when", "any", "--reclaim-idle-s", "0")...); code != 2 || !strings.Contains(e, "--when or --reclaim-idle-s, not both") {
		t.Fatalf("--when with --reclaim-idle-s: exit %d %s", code, e)
	}
}

// A quiet run says now and then that it still goes on, and what it did
// last; its own lines reset the wait.
func TestHostBeat(t *testing.T) {
	old := hostBeat
	t.Cleanup(func() { hostBeat = old })
	hostBeat = 40 * time.Millisecond
	var mu sync.Mutex
	var said []string
	o := hostprobe.ShareOptions{Progress: func(s string) { mu.Lock(); said = append(said, s); mu.Unlock() }}
	stop := startBeat(&o)
	o.Progress("pull ref-image")
	time.Sleep(150 * time.Millisecond)
	stop()
	stop() // twice is fine
	mu.Lock()
	defer mu.Unlock()
	if len(said) < 2 || said[0] != "pull ref-image" || !strings.HasPrefix(said[1], "still going (") || !strings.HasSuffix(said[1], "; last: pull ref-image") {
		t.Fatalf("said: %q", said)
	}
	n := len(said)
	time.Sleep(100 * time.Millisecond)
	if len(said) != n {
		t.Fatalf("the beat went on after stop: %q", said)
	}
	if o := (hostprobe.ShareOptions{}); startBeat(&o) == nil || o.Progress != nil {
		t.Fatal("no Progress (--json): the beat must say nothing")
	}
}

// The setup check's button runs: after a host run that stopped before it
// ended, where this machine already shares sizes with the pool, host
// status's setup do is that run again, and run as the app runs it, it goes
// on in the background (not refused as a bare re-run: the press asked).
func TestHostSetupDoRuns(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	old := hostSpawn
	t.Cleanup(func() { hostSpawn = old })
	var spawned [][]string
	var pools []string
	hostSpawn = func(args []string, arg string, log, lock *os.File) (int, error) {
		spawned, pools = append(spawned, args), append(pools, arg)
		return 4242, nil
	}
	// A run of host trial --pull-timeout 5m that stopped in preflight: no
	// flag that changes the share, so without --yes it is a bare re-run.
	hostsetup.Start(m.home, hostsetup.Command{Prefix: "potluck host", Path: []string{"host"}, Pool: "trial",
		Flags: []hostsetup.Flag{{Name: "pull-timeout", Text: " --pull-timeout 5m", Args: []string{"--pull-timeout=5m0s"}}}}).Begin(hostsetup.Preflight, "checking")
	var st menubar.Status
	if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
		t.Fatal(err)
	}
	var do *menubar.Do
	for _, c := range st.Setup.Checks {
		if c.ID == "setup" {
			do = c.Do
		}
	}
	if st.Setup.State != menubar.SetupInterrupted || do == nil || do.Invite || st.Setup.NextID != "setup" {
		t.Fatalf("setup %s: do %+v, next_id %q\n%+v", st.Setup.State, do, st.Setup.NextID, st.Setup.Checks)
	}
	// Without --yes the same run only says what it would do.
	if _, _, code := m.run(t, slices.DeleteFunc(slices.Clone(do.Args), func(a string) bool { return a == "--yes" })...); code == 0 && len(spawned) > 0 {
		t.Fatalf("a bare re-run went on without --yes: %q", spawned)
	}
	spawned, pools = nil, nil
	m.ok(t, do.Args...)
	if len(spawned) != 1 || !slices.Equal(pools, []string{"trial"}) || !slices.Contains(spawned[0], "--pull-timeout=5m0s") {
		t.Fatalf("the button's run %q, stdin %q (do %q)", spawned, pools, do.Args)
	}
}

// host '<invite>' --detach hands the invite to the background run on its
// stdin, never in its argv (which any user here reads with ps for the whole
// run); that run joins with it, and one told an invite comes on stdin that
// gets none refuses, spending nothing. No record, log or status holds it.
func TestHostDetachInviteOffArgv(t *testing.T) {
	tn := tailnettest.New()
	owner := startMachine(t, tn, "owner", "owner@example.com")
	lender := soloMachineOn(t, tn, "")
	owner.ok(t, "create", "friends")
	invite := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "judy", "--host"))
	if invite == nil {
		t.Fatal("no host invite")
	}
	code := invite[1]
	old := hostSpawn
	t.Cleanup(func() { hostSpawn = old })
	var args []string
	var stdin string
	hostSpawn = func(a []string, arg string, log, lock *os.File) (int, error) {
		args, stdin = a, arg
		return 4242, nil
	}
	share := []string{"--image", refPin, "--sizes", "c1m2", "--name-prefix", "lender", "--reclaim-idle-s", "0", "--no-sudo"}
	out := lender.ok(t, append([]string{"host", code, "--detach"}, share...)...)
	if args == nil || stdin != code {
		t.Fatalf("host --detach: spawned %q, stdin %q\n%s", args, stdin, out)
	}
	secret := code[strings.LastIndex(code, ":")+1:]
	for _, a := range args {
		if strings.Contains(a, code) || strings.Contains(a, secret) {
			t.Fatalf("the background run's argv holds the invite: %q", args)
		}
	}
	if !slices.Contains(args, "--invite-stdin") {
		t.Fatalf("the background run is not told its invite comes on stdin: %q", args)
	}

	// Told an invite comes on stdin, and none came: refused, nothing spent.
	if o, e, c := lender.runIn(t, "", args...); c == 0 || !strings.Contains(e, "--invite-stdin: no invite code (or pool) came on stdin") {
		t.Fatalf("no invite on stdin: exit %d\n%s%s", c, o, e)
	}
	// An argument as well is a usage error.
	if _, e, c := lender.runIn(t, code+"\n", append(args, "friends")...); c != 2 || !strings.Contains(e, "give it no argument as well") {
		t.Fatalf("--invite-stdin with an argument: exit %d\n%s", c, e)
	}

	// The background run, as hostSpawn starts it, joins with the invite.
	o, e, c := lender.runIn(t, stdin+"\n", args...)
	if c != 0 || !strings.Contains(o, "shared lender-c1m2") || !strings.Contains(o, "friends") {
		t.Fatalf("the background run: exit %d\n%s%s", c, o, e)
	}
	if strings.Contains(o+e, code) {
		t.Fatalf("the background run's words (host.log) hold the invite:\n%s%s", o, e)
	}
	raw, err := os.ReadFile(filepath.Join(lender.home, hostsetup.FileName))
	if err != nil || strings.Contains(string(raw), code) || strings.Contains(string(raw), "invite-stdin") {
		t.Fatalf("setup record (%v):\n%s", err, raw)
	}
	if st := lender.ok(t, "--json", "host", "status"); strings.Contains(st, code) || strings.Contains(st, "invite-stdin") {
		t.Fatalf("host status:\n%s", st)
	}
}

// hostSpawn's own process: the invite comes whole on its stdin, then the
// end, and is in neither its argv nor its environment.
func TestHostSpawnHandsTheInviteOnStdin(t *testing.T) {
	const invite = "potluck-mini:abcdefabcdefabcdefabcdefabcdefab:ABCD-EFGH"
	if os.Getenv("POTLUCK_TEST_SPAWNED") != "" { // the spawned process
		in, err := io.ReadAll(os.Stdin)
		fmt.Printf("stdin=%q err=%v argv-or-env=%v done\n", in, err, strings.Contains(strings.Join(append(os.Environ(), os.Args...), "\n"), "ABCD-EFGH"))
		return
	}
	t.Setenv("POTLUCK_TEST_SPAWNED", "1")
	dir := t.TempDir()
	log, err := os.Create(filepath.Join(dir, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	lock, err := os.Create(filepath.Join(dir, "host.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	pid, err := hostSpawn([]string{"-test.run=^TestHostSpawnHandsTheInviteOnStdin$"}, invite, log, lock)
	if err != nil {
		t.Fatal(err)
	}
	var ws syscall.WaitStatus
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if got == pid {
			break
		}
		if err != nil && err != syscall.EINTR || time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the spawned process did not end: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(log.Name())
	want := fmt.Sprintf("stdin=%q err=<nil> argv-or-env=false done", invite+"\n")
	if err != nil || !strings.Contains(string(b), want) {
		t.Fatalf("the spawned process said (%v):\n%s\nwant %s", err, b, want)
	}
}
