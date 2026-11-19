package roomwait_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/roomwait"
)

// clock is a fake clock: Sleep moves it on and records each wait.
type clock struct {
	t      time.Time
	slept  []time.Duration
	cancel func() // set: Sleep cancels the context at its nth call
	at     int
}

func (c *clock) now() time.Time { return c.t }

func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	if c.cancel != nil && len(c.slept) == c.at {
		c.cancel()
		return ctx.Err()
	}
	c.t = c.t.Add(d)
	return nil
}

func (c *clock) wait(bound time.Duration, lines *[]string) roomwait.Wait {
	return roomwait.Wait{For: bound, Now: c.now, Sleep: c.sleep, Say: func(l string) { *lines = append(*lines, l) }}
}

// noRoom is the daemon's no-room refusal of binding on.
func noRoom(on, why string, retryS int) error {
	return api.Errorf(api.CodeUnavailable, "potluck-driver-tart acquire: the run did not start: no room on this host now: %s", why).
		WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedNoRoom, On: on, Why: why, RetryS: retryS})
}

// A no-room answer is asked again, at the host's retry time, until room
// comes; each change is one line naming the host, the reason and the next ask.
func TestWaitAsksAgainUntilRoom(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 19, 5, 37, 0, 0, time.Local)}
	var lines []string
	answers := []error{noRoom("thefactory-c2m4", "network busy (128/90 Mbit/s)", 240), noRoom("thefactory-c2m4", "network busy (128/90 Mbit/s)", 240), nil}
	asks := 0
	err := c.wait(30*time.Minute, &lines).Do(context.Background(), func(context.Context) error {
		asks++
		return answers[asks-1]
	})
	if err != nil || asks != 3 {
		t.Fatalf("err %v after %d asks, want nil after 3", err, asks)
	}
	if want := []time.Duration{4 * time.Minute, 4 * time.Minute}; !equal(c.slept, want) {
		t.Errorf("waited %v, want %v (the controller's retry time)", c.slept, want)
	}
	if len(lines) != 1 || lines[0] != "thefactory-c2m4 has no room: network busy (128/90 Mbit/s); asking again at 05:41:00 (waited 0s of 30m)" {
		t.Errorf("lines %q: want one line for the one reason", lines)
	}
}

// With no retry time it backs off; a new reason is a new line.
func TestWaitBacksOffAndSaysEachChange(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 19, 5, 0, 0, 0, time.Local)}
	var lines []string
	answers := []error{noRoom("a", "no free linux slot (8 in use)", 0), noRoom("a", "no free linux slot (8 in use)", 0), noRoom("b", "CPU busy", 0), nil}
	asks := 0
	if err := c.wait(time.Hour, &lines).Do(context.Background(), func(context.Context) error { asks++; return answers[asks-1] }); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second}; !equal(c.slept, want) {
		t.Errorf("waited %v, want %v", c.slept, want)
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "b has no room: CPU busy; asking again at 05:01:10 (waited 30s of 1h)") {
		t.Errorf("lines %q", lines)
	}
}

// The bound ends the wait with the last no room, after a line saying so; the
// last ask comes at the bound, not past it.
func TestWaitEndsAtTheBound(t *testing.T) {
	c := &clock{t: time.Now()}
	var lines []string
	asks := 0
	last := noRoom("thefactory-c2m4", "network busy", 600)
	err := c.wait(15*time.Minute, &lines).Do(context.Background(), func(context.Context) error { asks++; return last })
	if err != last {
		t.Fatalf("err %v, want the last no room", err)
	}
	if want := []time.Duration{10 * time.Minute, 5 * time.Minute}; !equal(c.slept, want) || asks != 3 {
		t.Errorf("waited %v over %d asks, want %v over 3", c.slept, asks, want)
	}
	if l := lines[len(lines)-1]; l != "gave up waiting for room after 15m: thefactory-c2m4 still has no room: network busy" {
		t.Errorf("last line %q", l)
	}
}

// Ctrl-C while waiting ends it at once with the last no room, nothing held.
func TestWaitStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &clock{t: time.Now(), cancel: cancel, at: 1}
	var lines []string
	asks := 0
	err := c.wait(30*time.Minute, &lines).Do(ctx, func(context.Context) error { asks++; return noRoom("h", "full", 0) })
	if _, ok := roomwait.Of(err); !ok || asks != 1 {
		t.Fatalf("err %v after %d asks, want the no room after 1", err, asks)
	}
	if l := lines[len(lines)-1]; !strings.HasPrefix(l, "stopped waiting for room (nothing held): h has no room: full") {
		t.Errorf("last line %q", l)
	}
}

// A hard refusal, any other failure and a wait of 0 answer at once.
func TestWaitNeverWaitsOnAHardRefusal(t *testing.T) {
	isolation := api.Errorf(api.CodeForbidden, "potluck-driver-tart cannot isolate boxes now, so it makes no box")
	budget := api.Errorf(api.CodeUnavailable, "budget spent").WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedBudget})
	driver := api.Errorf(api.CodeUnavailable, "the run did not start: boot failed").WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedDriver})
	for name, c := range map[string]struct {
		bound time.Duration
		err   error
	}{
		"bad_request":       {time.Hour, api.Errorf(api.CodeBadRequest, "no box template")},
		"forbidden":         {time.Hour, isolation},
		"budget":            {time.Hour, budget},
		"driver":            {time.Hour, driver},
		"plain error":       {time.Hour, errors.New("connection refused")},
		"no room, --wait 0": {0, noRoom("h", "full", 60)},
	} {
		clk := &clock{t: time.Now()}
		var lines []string
		asks := 0
		err := clk.wait(c.bound, &lines).Do(context.Background(), func(context.Context) error { asks++; return c.err })
		if err != c.err || asks != 1 || len(clk.slept) != 0 || len(lines) != 0 {
			t.Errorf("%s: %v after %d asks, %d waits, lines %q: want it at once", name, err, asks, len(clk.slept), lines)
		}
	}
}

// A caller's own Full (potluck host's check box found every slot taken)
// waits as an acquire's does.
func TestWaitOnACallersFull(t *testing.T) {
	c := &clock{t: time.Now()}
	var lines []string
	asks := 0
	err := c.wait(time.Minute, &lines).Do(context.Background(), func(context.Context) error {
		if asks++; asks < 2 {
			return &roomwait.Full{Where: "this machine", Why: "every Linux slot is in use"}
		}
		return nil
	})
	if err != nil || asks != 2 || len(lines) != 1 || !strings.HasPrefix(lines[0], "this machine has no room: every Linux slot is in use;") {
		t.Errorf("err %v after %d asks, lines %q", err, asks, lines)
	}
}

func equal(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
