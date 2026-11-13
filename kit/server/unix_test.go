package server_test

import (
	"context"
	. "github.com/sengirunners/potluck/kit/server"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

func unixTransport(sock string) *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
}

// Dogfood: the old daemon's exit removed the socket of a daemon started
// right after `zm daemon stop` returned.
func TestRemoveIfOurs(t *testing.T) {
	path := t.TempDir() + "/s.sock"
	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	ours, _ := os.Stat(path)
	ln.Close() // leaves a stale socket file: the next ListenUnix replaces it
	ln2, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	RemoveIfOurs(path, ours)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed the next daemon's socket")
	}
	// On a VM the next socket can get the freed inode and, within one clock
	// tick, the same time: the stat matches, and the socket still answers.
	theirs, _ := os.Stat(path)
	RemoveIfOurs(path, theirs)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed a live daemon's socket whose stat matched")
	}
	ln2.Close()
	RemoveIfOurs(path, theirs)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("kept its own socket")
	}
}

// Go's UnixListener.Close unlinks its path whoever's socket is there now.
// A daemon that took the path while the old one still listened (its liveness
// dial failed: a full backlog, a 1 s timeout) lost its socket when the old
// one closed. Only removeIfOurs, which checks, removes it.
func TestClosingTheOldListenerKeepsTheNewSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := dir + "/s.sock"
	old, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil { // the next daemon judged it stale
		t.Fatal(err)
	}
	next, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	old.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the next daemon's socket after the old listener closed: %v", err)
	}
	c.Close()
}

func TestLockHome(t *testing.T) {
	home := t.TempDir()
	release, err := LockHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockHome(home); err == nil || !strings.Contains(err.Error(), "already runs") || !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	r2, err := LockHome(home)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	r2()
}

// The real socket server marks only Unix connections local; every other
// listener is the network (TestUnmarkedListenerFailsClosed walks every
// route over one, in internal/wiring/fulltest).
func TestMarkUnixMarksOnlyUnix(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if ctx := MarkUnix(context.Background(), a); ctx != context.Background() {
		t.Fatal("a non-Unix connection was marked local")
	}
}
