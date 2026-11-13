package tailnet

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sengirunners/potluck/kit/core"
)

type fakeNet struct {
	core.NetService
	up atomic.Bool
}

func (n *fakeNet) Self(context.Context) (core.NetSelf, error) {
	return core.NetSelf{Running: n.up.Load()}, nil
}

// A dial right after this daemon starts waits for its tailnet node, so a
// call in those seconds does not fail for nothing; it gives up at NodeUpWait,
// and a node that is up, or a daemon past the wait, never waits.
func TestWaitNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old := NodeUpWait
		NodeUpWait = 300 * time.Millisecond
		t.Cleanup(func() { NodeUpWait = old })
		n := &fakeNet{}
		s := &teamTransport{d: &core.Deps{}, net: n}

		s.started = time.Now()
		go func() { time.Sleep(100 * time.Millisecond); n.up.Store(true) }()
		t0 := time.Now()
		s.waitNode(context.Background())
		if d := time.Since(t0); d < 90*time.Millisecond || d >= 300*time.Millisecond {
			t.Fatalf("waited %v for a node that came up at 100 ms", d)
		}

		t0 = time.Now()
		s.waitNode(context.Background())
		if d := time.Since(t0); d > 50*time.Millisecond {
			t.Fatalf("a running node waited %v", d)
		}

		n.up.Store(false)
		s.started = time.Now()
		t0 = time.Now()
		s.waitNode(context.Background())
		if d := time.Since(t0); d < 250*time.Millisecond || d > 2*time.Second {
			t.Fatalf("a node that never came up waited %v", d)
		}

		t0 = time.Now()
		s.waitNode(context.Background()) // past the wait
		if d := time.Since(t0); d > 50*time.Millisecond {
			t.Fatalf("past the wait it waited %v", d)
		}
	})
}
