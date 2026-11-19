package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/roomwait"
)

// fakeRoomClock makes roomwait's waits instant for this test: each moves a
// fake clock on and is recorded.
func fakeRoomClock(t *testing.T) *[]time.Duration {
	t.Helper()
	now := time.Date(2026, 11, 19, 5, 37, 0, 0, time.Local)
	var slept []time.Duration
	oldSleep, oldNow := roomSleep, roomNow
	t.Cleanup(func() { roomSleep, roomNow = oldSleep, oldNow })
	roomNow = func() time.Time { return now }
	roomSleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		now = now.Add(d)
		return ctx.Err()
	}
	return &slept
}

// noRoomOn is the daemon's no-room refusal of binding on (runtarget
// driverRefused): the admission controller's reason and retry time.
func noRoomOn(on string) *api.Error {
	return api.Errorf(api.CodeUnavailable, "potluck-driver-oci acquire: the run did not start: no room on this host now: network busy (128/90 Mbit/s) (retry in 240 s)").
		WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedNoRoom, On: on, Why: "network busy (128/90 Mbit/s)", RetryS: 240})
}

// potluck run on a host that has no room now waits (zm-eakh.12.24.18.5): it
// asks again at the controller's retry time, with the same request id, says
// why and when on stderr, and runs once room comes. --wait 0 fails at once.
func TestRunWaitsWhileTheHostHasNoRoom(t *testing.T) {
	slept := fakeRoomClock(t)
	stop := errors.New("stop")
	start := func(context.Context, acquiredBox) (string, error) { return "", stop }
	var errw strings.Builder
	o := boxJobOptions{timeout: time.Minute, out: io.Discard, errw: &errw, p: "potluck", room: roomwait.Default}

	d := &prepDaemon{onlySmall: true, fail: map[int]*api.Error{1: noRoomOn("small"), 2: noRoomOn("small")}}
	r, err := boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if !errors.Is(err, stop) || !slices.Equal(d.seen(), []string{"acquire small", "acquire small", "acquire small", "release small"}) {
		t.Fatalf("%+v %v, asked %q; want two waits, then the box", r, err, d.seen())
	}
	if !slices.Equal(*slept, []time.Duration{4 * time.Minute, 4 * time.Minute}) {
		t.Errorf("waited %v, want the controller's 240 s twice", *slept)
	}
	if ids := []string{d.asks[0].RequestID, d.asks[1].RequestID, d.asks[2].RequestID}; ids[0] == "" || ids[1] != ids[0] || ids[2] != ids[0] {
		t.Errorf("request ids %q: want one id for every ask", ids)
	}
	if want := "potluck: small has no room: network busy (128/90 Mbit/s); asking again at 05:41:00 (waited 0s of 30m)\n"; !strings.HasPrefix(errw.String(), want) {
		t.Errorf("stderr %q, want it to start %q", errw.String(), want)
	}

	*slept, errw = nil, strings.Builder{}
	o.room = 0
	d = &prepDaemon{onlySmall: true, fail: map[int]*api.Error{1: noRoomOn("small")}}
	if _, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start); !api.IsCode(err, api.CodeUnavailable) ||
		len(d.seen()) != 1 || len(*slept) != 0 || errw.Len() != 0 {
		t.Errorf("--wait 0: %v, asked %q, waited %v, said %q; want it at once", err, d.seen(), *slept, errw.String())
	}
}

// The bound ends the wait with the last no room, nothing made or released,
// and the reason said; a hard refusal never waits; another host with room
// takes the box before any wait.
func TestRunWaitEndsAndNeverWaitsOnHardRefusals(t *testing.T) {
	slept := fakeRoomClock(t)
	start := func(context.Context, acquiredBox) (string, error) { return "", errors.New("stop") }
	var errw strings.Builder
	o := boxJobOptions{timeout: time.Minute, out: io.Discard, errw: &errw, p: "potluck", room: 10 * time.Minute}

	always := map[int]*api.Error{}
	for i := 1; i <= 10; i++ {
		always[i] = noRoomOn("small")
	}
	d := &prepDaemon{onlySmall: true, fail: always}
	r, err := boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if _, ok := computeapi.NoRoom(err); !ok || r.Released || slices.Contains(d.seen(), "release small") {
		t.Errorf("bound: %+v %v, asked %q; want the last no room, nothing released", r, err, d.seen())
	}
	if got := len(d.seen()); got != 4 { // at 0, 4m, 8m and the bound (10m)
		t.Errorf("bound: %d asks over %v, want 4", got, *slept)
	}
	if !strings.Contains(errw.String(), "potluck: gave up waiting for room after 10m: small still has no room: network busy (128/90 Mbit/s)\n") {
		t.Errorf("bound: stderr %q", errw.String())
	}

	*slept, errw = nil, strings.Builder{}
	forbidden := api.Errorf(api.CodeForbidden, "potluck-driver-oci cannot isolate boxes now, so it makes no box")
	d = &prepDaemon{onlySmall: true, fail: map[int]*api.Error{1: forbidden}}
	if _, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start); !api.IsCode(err, api.CodeForbidden) || len(*slept) != 0 || errw.Len() != 0 {
		t.Errorf("isolation refusal: %v, waited %v, said %q; want it at once", err, *slept, errw.String())
	}

	d = &prepDaemon{fail: map[int]*api.Error{1: noRoomOn("small")}} // small and std
	if r, _ = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start); r.Target != "std" || len(*slept) != 0 ||
		!slices.Equal(d.seen(), []string{"acquire small", "acquire std", "release std"}) {
		t.Errorf("two hosts: %q, asked %q, waited %v; want std at once", r.Target, d.seen(), *slept)
	}
}

// After a wait, an ask that may have made the sandbox (a host gone quiet) is
// asked again to end it with the very request the last ask sent, short
// lease and self_renew included, with no base plan (--no-base, --full):
// anything else the daemon answers conflict to, and the sandbox would hold
// its slot until its lease ends.
func TestRunAfterAWaitEndsWhatItMayHaveMade(t *testing.T) {
	fakeRoomClock(t)
	quiet := api.Errorf(api.CodeUnavailable, "host has not answered")
	d := &prepDaemon{onlySmall: true, verbs: []string{"renew"}, fail: map[int]*api.Error{1: noRoomOn("small"), 2: quiet}}
	o := boxJobOptions{timeout: time.Minute, out: io.Discard, errw: io.Discard, p: "potluck", room: roomwait.Default}
	r, err := boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, func(context.Context, acquiredBox) (string, error) { return "", nil })
	if !api.IsCode(err, api.CodeUnavailable) || !r.Released || !slices.Equal(d.seen(), []string{"acquire small", "acquire small", "acquire small", "release small"}) {
		t.Fatalf("%+v %v, asked %q; want the wait, the quiet ask, the re-ask and its end", r, err, d.seen())
	}
	want := int(runLeaseTTL / time.Second)
	for i, a := range d.asks {
		if a.TTLS != want || !a.SelfRenew {
			t.Errorf("ask %d: ttl_s %d self_renew %t, want %d true on every ask", i+1, a.TTLS, a.SelfRenew, want)
		}
	}
}

// potluck sandbox create waits the same way, through the same helper: once
// room comes, the sandbox it was asked for is made under the one request id.
func TestSandboxCreateWaitsWhileTheHostHasNoRoom(t *testing.T) {
	slept := fakeRoomClock(t)
	var errw strings.Builder
	a := &app{host: Host{Prefix: "zm", Stdout: io.Discard, Stderr: &errw}}
	d := &prepDaemon{onlySmall: true, fail: map[int]*api.Error{1: noRoomOn("small")}}
	req := computeapi.AcquireRunTargetRequest{RequestID: "cli-1", TTLS: 300}
	h, _, err := a.sandboxCreate(context.Background(), client.WithHandler(d), computeclient.BoxWant{}, req, pushFlags{}, roomWait(roomwait.Default, a.host.Stderr, a.name()))
	if err != nil || h.Handle != "box:small/h1" || len(*slept) != 1 || d.asks[1].RequestID != "cli-1" {
		t.Fatalf("%+v %v, waited %v, asked %q", h, err, *slept, d.seen())
	}
	if !strings.HasPrefix(errw.String(), "zm: small has no room: network busy (128/90 Mbit/s); asking again at ") {
		t.Errorf("stderr %q", errw.String())
	}
}

// A named target that answered no room and then drops out of the listing for
// one ask is waited for, with its own line, not refused as missing
// (zm-9xzw.20); one never listed in the wait is refused at once, as before.
func TestSandboxCreateWaitsWhileANamedTargetIsBrieflyUnlisted(t *testing.T) {
	slept := fakeRoomClock(t)
	var errw strings.Builder
	a := &app{host: Host{Prefix: "zm", Stdout: io.Discard, Stderr: &errw}}
	d := &prepDaemon{fail: map[int]*api.Error{1: noRoomOn("small")}, unlisted: map[int]bool{2: true}} // std stays listed
	req := computeapi.AcquireRunTargetRequest{RequestID: "cli-1", TTLS: 300}
	h, _, err := a.sandboxCreate(context.Background(), client.WithHandler(d), computeclient.BoxWant{On: "small"}, req, pushFlags{}, roomWait(roomwait.Default, a.host.Stderr, a.name()))
	if err != nil || h.Handle != "box:small/h1" || len(*slept) != 2 || !slices.Equal(d.seen(), []string{"acquire small", "acquire small"}) {
		t.Fatalf("%+v %v, waited %v, asked %q; want a wait on the no room, one on the gap, then the box", h, err, *slept, d.seen())
	}
	if !strings.Contains(errw.String(), "zm: small is not listed now (it answered earlier in this wait; its host may be out of touch for a moment); asking again at ") {
		t.Errorf("stderr %q: want a line for the gap", errw.String())
	}

	*slept, errw = nil, strings.Builder{}
	d = &prepDaemon{onlySmall: true}
	_, _, err = a.sandboxCreate(context.Background(), client.WithHandler(d), computeclient.BoxWant{On: "nosuch"}, req, pushFlags{}, roomWait(roomwait.Default, a.host.Stderr, a.name()))
	if !api.IsCode(err, api.CodeNotFound) || !strings.Contains(err.Error(), "no run target nosuch") || len(*slept) != 0 || errw.Len() != 0 {
		t.Errorf("never listed: %v, waited %v, said %q; want 'no run target' at once", err, *slept, errw.String())
	}
}

// A wait that runs out while the named target is unlisted says how long it
// waited and the last reason, never 'no run target'.
func TestSandboxCreateWaitEndsWhileTheTargetIsUnlisted(t *testing.T) {
	fakeRoomClock(t)
	var errw strings.Builder
	a := &app{host: Host{Prefix: "zm", Stdout: io.Discard, Stderr: &errw}}
	unlisted := map[int]bool{}
	for i := 2; i <= 50; i++ {
		unlisted[i] = true
	}
	d := &prepDaemon{fail: map[int]*api.Error{1: noRoomOn("small")}, unlisted: unlisted}
	req := computeapi.AcquireRunTargetRequest{RequestID: "cli-1", TTLS: 300}
	_, _, err := a.sandboxCreate(context.Background(), client.WithHandler(d), computeclient.BoxWant{On: "small"}, req, pushFlags{}, roomWait(10*time.Minute, a.host.Stderr, a.name()))
	if err == nil || strings.Contains(err.Error(), "no run target") || !strings.Contains(err.Error(), "small is not listed now") ||
		!strings.Contains(err.Error(), "before that, small had no room: network busy (128/90 Mbit/s)") {
		t.Fatalf("err %v: want the gap and the last no room, never 'no run target'", err)
	}
	if e := (*api.Error)(nil); !errors.As(err, &e) || !strings.Contains(e.Hint, "std") {
		t.Errorf("err %v: want the miss's hint, naming the targets listed", err)
	}
	if !strings.Contains(errw.String(), "zm: gave up waiting for room after 10m: small is not listed now") ||
		!strings.Contains(errw.String(), "; before that, small had no room: network busy (128/90 Mbit/s)\n") || strings.Contains(errw.String(), "no run target") {
		t.Errorf("stderr %q", errw.String())
	}
}
