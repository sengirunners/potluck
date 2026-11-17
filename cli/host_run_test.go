package cli

import (
	"encoding/json"
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
	hostSpawn = func(args []string, log, lock *os.File) (int, error) {
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
	// --detach, never asking for a password.
	want := []string{"host", "--image=" + refPin, "--name-prefix=solo", "--reclaim-idle-s=0", "--sizes=c1m2", "--no-sudo"}
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
// root (a mounted one's words too), its arguments and the flags given,
// the host's own included, without --detach and --json.
func TestDetachArgs(t *testing.T) {
	root := &cobra.Command{Use: "supper"}
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().StringP("project", "P", "", "")
	group := &cobra.Command{Use: "pool"}
	var got []string
	host := &cobra.Command{Use: "host", RunE: func(cmd *cobra.Command, _ []string) error {
		got = detachArgs(cmd, true)
		return nil
	}}
	host.Flags().Bool("detach", false, "")
	host.Flags().Bool("no-sudo", false, "")
	host.Flags().StringArray("image", nil, "")
	group.AddCommand(host)
	root.AddCommand(group)
	root.SetArgs([]string{"--json", "-P", "acme", "pool", "host", "trial", "--detach", "--image", "a@sha256:1", "--image", "b@sha256:2", "--no-sudo"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{"pool", "host", "--image=a@sha256:1", "--image=b@sha256:2", "--project=acme", "--no-sudo", "trial"}
	if !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
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
