package daemonrecord_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/daemonrecord"
)

// answer serves GET /v0/health on a Unix socket, saying it is process pid,
// until the test ends or stop is called.
func answer(t *testing.T, pid int) (sock string, stop func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "dr") // short: the socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": pid})
	})}
	go func() { _ = srv.Serve(ln) }()
	stop = func() { _ = srv.Close() }
	t.Cleanup(stop)
	return sock, stop
}

// answerProtocol serves health on a Unix socket under another protocol
// major: a daemon that is up but that this build cannot talk to.
func answerProtocol(t *testing.T, protocol string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.SetHeader(w.Header(), api.HeaderProtocol, protocol)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// deadPID is the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func record(program, home, sock string, pid int) daemonrecord.Record {
	return daemonrecord.Record{Program: program, Exe: "/bin/" + program, Home: home, Socket: sock, PID: pid,
		Protocol: "potluck/1", Version: "v0", Modules: []string{"core", "runtarget"}, Started: time.Now().UTC().Truncate(time.Second)}
}

// A record is written whole, private, one file per program; read back as
// written; removed only by the daemon it names.
func TestWriteReadRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "daemons")
	ctx := context.Background()
	r := record("supper", "/h/supper", "/h/supper/supper.sock", os.Getpid())
	if ok, err := daemonrecord.Write(ctx, dir, r); err != nil || !ok {
		t.Fatalf("write: %v %v", ok, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "supper.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi.Mode(), err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"program":"supper","home":"/x","socket":"/x/s"}`), 0o600); err != nil {
		t.Fatal(err) // a file whose name is not its program is not a record
	}
	got, err := daemonrecord.Read(dir)
	if err != nil || len(got) != 1 || got[0].Program != "supper" || got[0].Home != r.Home || !got[0].Started.Equal(r.Started) || !got[0].Serves("runtarget") || got[0].Serves("runtarget", "usage") {
		t.Fatalf("read: %+v %v", got, err)
	}
	other := r
	other.PID++
	if err := daemonrecord.Remove(dir, other); err != nil {
		t.Fatal(err)
	}
	if got, _ := daemonrecord.Read(dir); len(got) != 1 {
		t.Fatal("removed another process's record")
	}
	if err := daemonrecord.Remove(dir, r); err != nil {
		t.Fatal(err)
	}
	if got, _ := daemonrecord.Read(dir); len(got) != 0 {
		t.Fatalf("kept its own record: %+v", got)
	}
	if _, err := daemonrecord.Write(ctx, dir, record("../x", "/h", "/h/s", 1)); err == nil {
		t.Fatal("a program name that is a path wrote a record")
	}
}

// Live is the records whose process is alive and whose socket answers as
// that process; a dead record never is.
func TestLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(daemonrecord.EnvDir, dir)
	ctx := context.Background()
	me := os.Getpid()
	up, _ := answer(t, me)
	wrongPID, _ := answer(t, me+1)
	silent := filepath.Join(t.TempDir(), "none.sock")
	otherProtocol := answerProtocol(t, "zm/v9")
	for _, r := range []daemonrecord.Record{
		record("up", "/h/up", up, me),
		record("dead", "/h/dead", up, deadPID(t)),      // the process is gone
		record("silent", "/h/silent", silent, me),      // nothing answers
		record("reused", "/h/reused", wrongPID, me),    // another process answers
		record("newer", "/h/newer", otherProtocol, me), // up, but speaks another protocol major
		record("unanswered", "/h/unanswered", up, 0),   // no pid
	} {
		if _, err := daemonrecord.Write(ctx, dir, r); err != nil {
			t.Fatal(err)
		}
	}
	live, err := daemonrecord.Live(ctx, nil)
	if err != nil || len(live) != 2 || live[0].Program != "newer" || live[1].Program != "up" {
		t.Fatalf("live: %+v %v", live, err)
	}
}

// A program's record stays the first live daemon's: a second daemon of the
// same program on another home does not take it, and takes it once the
// first is gone.
func TestWriteKeepsTheLiveDaemonsRecord(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	sock, stop := answer(t, os.Getpid())
	first := record("supper", "/h/one", sock, os.Getpid())
	if ok, err := daemonrecord.Write(ctx, dir, first); err != nil || !ok {
		t.Fatalf("first: %v %v", ok, err)
	}
	second := record("supper", "/h/two", "/h/two/supper.sock", os.Getpid())
	if ok, err := daemonrecord.Write(ctx, dir, second); err != nil || ok {
		t.Fatalf("second beside a live first: %v %v", ok, err)
	}
	again := first
	again.Started = again.Started.Add(time.Second)
	if ok, err := daemonrecord.Write(ctx, dir, again); err != nil || !ok {
		t.Fatalf("the first again (a restart on its home): %v %v", ok, err)
	}
	stop()
	if ok, err := daemonrecord.Write(ctx, dir, second); err != nil || !ok {
		t.Fatalf("second after the first stopped: %v %v", ok, err)
	}
	if got, _ := daemonrecord.Read(dir); len(got) != 1 || got[0].Home != "/h/two" {
		t.Fatalf("records: %+v", got)
	}
}

// In a test binary only POTLUCK_DAEMONS_DIR names the directory: with it
// unset, records are off, whatever HOME is.
func TestDirInTests(t *testing.T) {
	t.Setenv(daemonrecord.EnvDir, "")
	if d := daemonrecord.Dir(); d != "" {
		t.Fatalf("Dir() = %q in a test binary with %s unset", d, daemonrecord.EnvDir)
	}
	if ok, err := daemonrecord.Write(context.Background(), daemonrecord.Dir(), record("supper", "/h", "/h/s", 1)); ok || err != nil {
		t.Fatalf("write with records off: %v %v", ok, err)
	}
	if live, err := daemonrecord.Live(context.Background(), nil); len(live) != 0 || err != nil {
		t.Fatalf("live with records off: %v %v", live, err)
	}
	t.Setenv(daemonrecord.EnvDir, "rel")
	if d := daemonrecord.Dir(); !filepath.IsAbs(d) {
		t.Fatalf("Dir() = %q", d)
	}
}

// Only this user may write a record: a directory open to others is refused
// (and Write makes this user's own private), and a file open to others, a
// symlink or a record of another user's process counts for nothing.
func TestRecordsAreThisUsersAlone(t *testing.T) {
	ctx := context.Background()
	me := os.Getpid()
	sock, _ := answer(t, me)
	dir := filepath.Join(t.TempDir(), "daemons")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonrecord.Read(dir); err == nil {
		t.Fatal("read a records directory open to other users")
	}
	if ok, err := daemonrecord.Write(ctx, dir, record("supper", "/h/supper", sock, me)); err != nil || !ok {
		t.Fatalf("write into this user's own directory: %v %v", ok, err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("Write left the directory %v", fi.Mode().Perm())
	}
	// A file open to others, and a symlink to a good record, are skipped.
	b, _ := os.ReadFile(filepath.Join(dir, "supper.json"))
	if err := os.WriteFile(filepath.Join(dir, "open.json"), []byte(`{"program":"open","home":"/h","socket":"`+sock+`","pid":`+strconv.Itoa(me)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "linked.json")
	if err := os.WriteFile(outside, []byte(strings.Replace(string(b), `"supper"`, `"linked"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if got, err := daemonrecord.Read(dir); err != nil || len(got) != 1 || got[0].Program != "supper" {
		t.Fatalf("records: %+v %v", got, err)
	}
	// A symlinked directory is refused.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonrecord.Write(ctx, link, record("supper", "/h/supper", sock, me)); err == nil {
		t.Fatal("wrote through a symlinked directory")
	}
	if _, err := daemonrecord.Read(link); err == nil {
		t.Fatal("read through a symlinked directory")
	}
	// Another user's process (pid 1), whatever its socket answers.
	t.Setenv(daemonrecord.EnvDir, dir)
	root, _ := answer(t, 1)
	if _, err := daemonrecord.Write(ctx, dir, record("init", "/h/init", root, 1)); err != nil {
		t.Fatal(err)
	}
	if live, _ := daemonrecord.Live(ctx, func(r daemonrecord.Record) bool { return r.Program == "init" }); len(live) != 0 {
		t.Fatalf("another user's process counted as live: %+v", live)
	}
}
