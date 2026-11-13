package tunnel

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/core"
)

// A missing or unreadable journal is rebuilt to keep reads working. That
// rewrite must not turn a second restart into proof that old writes are safe.
func TestReplayRecoverySurvivesRestart(t *testing.T) {
	for _, damage := range []string{"missing", "unreadable"} {
		t.Run(damage, func(t *testing.T) {
			h := newService(t)
			t.Cleanup(func() { h.Close() })
			var seen atomic.Pointer[core.Remote]
			srv := httptest.NewServer(h.Handler(echo(&seen)))
			t.Cleanup(srv.Close)
			fp, _ := h.Fingerprint()
			_, c := member(t, fp)
			var cp capture
			proxy := cp.server(t, srv.URL)
			if resp, _, err := do(t, c, "POST", proxy.URL+"/v0/work/x/comment", "zm_tok", "once"); err != nil || resp.StatusCode != 201 {
				t.Fatalf("original write: %v", err)
			}
			old, body := cp.last.Load(), *cp.body.Load()
			srv.Close()
			h.Close()
			path := h.journal.path
			if damage == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 2*maxLine), 0o600); err != nil {
				t.Fatal(err)
			}

			h2 := restartService(t, h)
			t.Cleanup(func() { h2.Close() })
			srv2 := httptest.NewServer(h2.Handler(echo(&seen)))
			t.Cleanup(srv2.Close)
			if resp, b, err := do(t, c, "GET", srv2.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
				t.Fatalf("read during recovery: %v %s", err, b)
			}
			seen.Store(nil)
			if resp, b, err := do(t, legacyClient(c), "POST", srv2.URL+"/v0/work/x/comment", "zm_tok", "new write"); err != nil || resp.StatusCode != 503 || seen.Load() != nil {
				t.Fatalf("write during recovery: %v %s", err, b)
			}
			srv2.Close()
			h2.Close()

			h3 := restartService(t, h)
			t.Cleanup(func() { h3.Close() })
			srv3 := httptest.NewServer(h3.Handler(echo(&seen)))
			t.Cleanup(srv3.Close)
			again, _ := http.NewRequest(old.Method, srv3.URL+old.URL.RequestURI(), bytes.NewReader(body))
			again.Header = old.Header.Clone()
			resp, err := http.DefaultClient.Do(again)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 503 || seen.Load() != nil {
				t.Fatalf("captured write after a second restart: %d %s", resp.StatusCode, b)
			}
		})
	}
}

func TestReplayRecoverySurvivesCompactionAndClockChange(t *testing.T) {
	h := newService(t)
	t.Cleanup(func() { h.Close() })
	j := h.journal
	j.load(time.Now()) // an existing key's missing journal, not a fresh key
	if !j.degraded() {
		t.Fatal("missing journal did not hold writes")
	}
	if got := j.add("zmkey:member", "read-during-recovery", time.Now().Unix(), time.Now()); got != nonceNew {
		t.Fatalf("recording a read during recovery: %d", got)
	}
	j.mu.Lock()
	snap := j.snapshotLocked()
	j.compacting = true
	j.mu.Unlock()
	j.compact(snap)
	if j.compactions != 1 {
		t.Fatal("compaction did not run")
	}
	j.close()

	h2 := restartService(t, h)
	t.Cleanup(func() { h2.Close() })
	// Even a large wall-clock change cannot expire the durable hold: each
	// restart must wait a full local monotonic interval.
	h2.journal.load(time.Now().Add(24 * time.Hour))
	if !h2.journal.degraded() {
		t.Fatal("compaction or clock change erased the recovery hold")
	}
}

func TestReplayRecoveryMarkerRetry(t *testing.T) {
	h := newService(t)
	t.Cleanup(func() { h.Close() })
	j := h.journal
	if err := os.MkdirAll(j.path+".hold.tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	j.load(time.Now())
	if _, err := os.Stat(j.path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rewrote uncertain journal without a durable hold: %v", err)
	}
	if got := j.add("zmkey:member", "blocked-read-nonce", time.Now().Unix(), time.Now()); got != nonceNoJournal {
		t.Fatalf("append created a clean-looking journal without a durable hold: %d", got)
	}
	if err := os.Remove(j.path + ".hold.tmp"); err != nil {
		t.Fatal(err)
	}
	if got := j.add("zmkey:member", "allowed-read-nonce", time.Now().Unix(), time.Now()); got != nonceNew {
		t.Fatalf("append did not retry recording the recovery hold: %d", got)
	}
	j.close()
	h2 := restartService(t, h)
	t.Cleanup(func() { h2.Close() })
	if !h2.journal.degraded() {
		t.Fatal("append retry hid the recovery hold from the next run")
	}
}

func TestReplayRecoveryClearsAfterLocalWindow(t *testing.T) {
	h := newService(t)
	t.Cleanup(func() { h.Close() })
	j := h.journal
	j.load(time.Now())
	if !j.degraded() {
		t.Fatal("missing journal did not hold writes")
	}
	// Simulate the monotonic interval elapsing, without an eleven-minute test.
	j.mu.Lock()
	j.degradedTo = time.Now().Add(-time.Second)
	j.mu.Unlock()
	if j.degraded() {
		t.Fatal("completed local recovery still holds writes")
	}
	if _, err := os.Stat(j.path + ".hold"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("completed recovery left a durable hold: %v", err)
	}
	j.close()
	h2 := restartService(t, h)
	t.Cleanup(func() { h2.Close() })
	if h2.journal.degraded() {
		t.Fatal("clean journal restarted recovery after the local window completed")
	}
}
