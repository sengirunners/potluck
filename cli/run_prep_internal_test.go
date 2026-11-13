package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// prepDaemon answers two box bindings, small (a lifetime cap of 10
// minutes) and std (two hours), logging each acquire and release as
// "<verb> <binding>"; describe runs onDescribe first.
type prepDaemon struct {
	mu         sync.Mutex
	asked      []string
	onDescribe func()
	// fail answers the nth acquire (1-based) with that error, nothing made.
	fail map[int]*api.Error
	// hook runs on the nth acquire before it answers (it may block on the
	// request's context, as a daemon making a box does).
	hook         map[int]func(r *http.Request)
	releaseFails bool // a release is refused
	onlySmall    bool // the daemon lists the small binding alone
	acquires     int
}

func (d *prepDaemon) log(s string) { d.mu.Lock(); d.asked = append(d.asked, s); d.mu.Unlock() }

func (d *prepDaemon) seen() []string { d.mu.Lock(); defer d.mu.Unlock(); return slices.Clone(d.asked) }

func (d *prepDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(api.HeaderProtocol, api.Protocol)
	view := func(name string, cpu, ttlMax int) computeapi.RunTargetView {
		return computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: "box", TTLMaxS: ttlMax}, HostClass: api.MachineTrusted,
			Describe: &computeapi.RunTargetDescribe{Kind: "box", Available: true, Size: &computeapi.RunTargetSize{Name: name, OS: "linux", CPU: cpu, MemoryMB: 1024 * cpu}}}
	}
	views := map[string]computeapi.RunTargetView{"small": view("small", 1, 600), "std": view("std", 2, 7200)}
	if d.onlySmall {
		delete(views, "std")
	}
	p := strings.TrimPrefix(r.URL.Path, "/v0/run_targets")
	switch {
	case p == "":
		l := computeapi.RunTargetList{}
		for _, n := range []string{"small", "std"} {
			if v, ok := views[n]; ok {
				l.Targets = append(l.Targets, v)
			}
		}
		api.WriteJSON(w, 200, l)
	case strings.HasSuffix(p, "/acquire"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/acquire")
		var req computeapi.AcquireRunTargetRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.log("acquire " + name)
		d.mu.Lock()
		d.acquires++
		n := d.acquires
		fail, hook := d.fail[n], d.hook[n]
		d.mu.Unlock()
		if hook != nil {
			hook(r)
		}
		if fail != nil {
			api.WriteError(w, fail)
			return
		}
		api.WriteJSON(w, 200, computeapi.RunTargetHandle{Handle: "box:" + name + "/h1", RequestID: req.RequestID, State: "ready"})
	case strings.HasSuffix(p, "/release"):
		d.log("release " + strings.Split(strings.TrimPrefix(p, "/"), "/")[0])
		if d.releaseFails {
			api.WriteError(w, api.Errorf(api.CodeInternal, "potluck-driver-x release: crashed"))
			return
		}
		api.WriteJSON(w, 200, computeapi.RunTargetRelease{Released: true})
	default:
		v, ok := views[strings.TrimPrefix(p, "/")]
		if !ok {
			api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
			return
		}
		if d.onDescribe != nil {
			d.onDescribe()
		}
		api.WriteJSON(w, 200, v)
	}
}

// TestBoxJobPrep (zm-eakh.12.5.43): a --timeout past the first binding's
// lifetime cap passes to the next binding, nothing asked of the first; with
// every binding too short, the error names each and nothing is asked; Ctrl-C
// while prep describes a binding asks nothing, so nothing is made and ended.
// An acquire that failed inside (the daemon kept the request) is asked again
// with its key and what it made ended: run shows no key to retry with.
func TestBoxJobPrep(t *testing.T) {
	stop := errors.New("stop")
	start := func(context.Context, acquiredBox) (string, error) { return "", stop }
	o := boxJobOptions{timeout: 30 * time.Minute, out: io.Discard, errw: io.Discard}

	d := &prepDaemon{}
	r, err := boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if !errors.Is(err, stop) || r.Target != "std" || !slices.Equal(d.seen(), []string{"acquire std", "release std"}) {
		t.Errorf("too long for small: %q %v, asked %q; want std", r.Target, err, d.seen())
	}

	d = &prepDaemon{}
	o.timeout = 10 * time.Hour
	_, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if err == nil || !strings.Contains(err.Error(), "small (does not fit)") || !strings.Contains(err.Error(), "std (does not fit)") || len(d.seen()) > 0 {
		t.Errorf("too long for both: %v, asked %q", err, d.seen())
	}

	d = &prepDaemon{fail: map[int]*api.Error{1: api.Errorf(api.CodeInternal, "potluck-driver-x acquire: no JSON answer")}}
	o.timeout = time.Minute
	r, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if err == nil || !api.IsCode(err, api.CodeInternal) || strings.Contains(err.Error(), "--request-id") || !r.Released ||
		!slices.Equal(d.seen(), []string{"acquire small", "acquire small", "release small"}) {
		t.Errorf("failed inside: %+v %v, asked %q; want it asked again and ended", r, err, d.seen())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d = &prepDaemon{onDescribe: cancel}
	o.timeout = time.Minute
	r, err = boxJob(ctx, client.WithHandler(d), o, jobResult{}, start)
	if !errors.Is(err, errInterrupted) || r.State != "interrupted" || len(d.seen()) > 0 {
		t.Errorf("Ctrl-C in prep: %+v %v, asked %q; want nothing asked", r, err, d.seen())
	}
}

// TestBoxJobEndsWhatItMayHaveMade (zm-eakh.12.5.45): after a failure the
// daemon may have kept (unmarked unavailable: a host gone quiet), boxJob asks
// again with the key and ends what that made; a driver's own refusal the
// daemon marked as made nothing is no such failure (the walk passes to the
// next binding, no re-ask); a re-ask refused before the daemon recorded it
// made nothing, and says so; a re-ask that fails says the sandbox may still
// come up; a release that fails says it ends at its lease.
func TestBoxJobEndsWhatItMayHaveMade(t *testing.T) {
	stop := errors.New("stop")
	start := func(context.Context, acquiredBox) (string, error) { return "", stop }
	var errw strings.Builder
	o := boxJobOptions{timeout: time.Minute, out: io.Discard, errw: &errw}
	quiet := api.Errorf(api.CodeUnavailable, "host has not answered")
	marked := api.Errorf(api.CodeUnavailable, "potluck-driver-x acquire: no capacity").WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedDriver})

	d := &prepDaemon{fail: map[int]*api.Error{1: quiet}}
	r, err := boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if !api.IsCode(err, api.CodeUnavailable) || !r.Released || !strings.Contains(err.Error(), "is ended") ||
		!slices.Equal(d.seen(), []string{"acquire small", "acquire small", "release small"}) {
		t.Errorf("an unmarked unavailable: %+v %v, asked %q; want it asked again and ended", r, err, d.seen())
	}

	d = &prepDaemon{fail: map[int]*api.Error{1: marked}}
	r, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if !errors.Is(err, stop) || r.Target != "std" || !slices.Equal(d.seen(), []string{"acquire small", "acquire std", "release std"}) {
		t.Errorf("a marked driver refusal: %q %v, asked %q; want it passed to std with no re-ask", r.Target, err, d.seen())
	}

	d = &prepDaemon{fail: map[int]*api.Error{1: quiet, 2: marked}}
	r, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if err == nil || strings.Contains(err.Error(), "may still come up") || !strings.Contains(err.Error(), "it made nothing") || r.Released || r.Handle != "" ||
		!slices.Equal(d.seen(), []string{"acquire small", "acquire small"}) {
		t.Errorf("a re-ask refused before it recorded: %+v %v, asked %q; want nothing made and no \"may still come up\"", r, err, d.seen())
	}

	d = &prepDaemon{fail: map[int]*api.Error{1: quiet, 2: quiet}}
	r, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if err == nil || !strings.Contains(err.Error(), "may still come up") || !strings.Contains(err.Error(), "request id job-") || r.Released ||
		!slices.Equal(d.seen(), []string{"acquire small", "acquire small"}) {
		t.Errorf("a failed re-ask: %+v %v, asked %q; want the sandbox may still come up", r, err, d.seen())
	}

	d = &prepDaemon{fail: map[int]*api.Error{1: quiet}, releaseFails: true}
	r, err = boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, start)
	if err == nil || !strings.Contains(err.Error(), "could not end ") || !strings.Contains(err.Error(), "ends at its lease") || r.Released || r.Handle == "" ||
		!slices.Equal(d.seen(), []string{"acquire small", "acquire small", "release small"}) {
		t.Errorf("a failed release: %+v %v, asked %q; want could not end it", r, err, d.seen())
	}
	if !strings.Contains(errw.String(), "Ctrl-C to stop waiting") {
		t.Errorf("the cleanup said nothing of how to stop it: %q", errw.String())
	}
}

// A second Ctrl-C while boxJob asks again to end a sandbox that may have
// been made stops the wait at once: the sandbox ends at its lease and the
// error says it may still come up.
func TestBoxJobCleanupHonoursCtrlCAgain(t *testing.T) {
	stop := errors.New("stop")
	start := func(context.Context, acquiredBox) (string, error) { return "", stop }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	again := make(chan struct{})
	quiet := api.Errorf(api.CodeUnavailable, "host has not answered")
	d := &prepDaemon{fail: map[int]*api.Error{1: quiet, 2: quiet}, hook: map[int]func(*http.Request){
		1: func(*http.Request) { cancel() },                             // Ctrl-C while the sandbox is being made
		2: func(r *http.Request) { close(again); <-r.Context().Done() }, // the daemon is slow to answer the re-ask; Ctrl-C again
	}}
	var errw strings.Builder
	o := boxJobOptions{timeout: time.Minute, out: io.Discard, errw: &errw, again: func(ctx context.Context) (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-again:
				cancel()
			case <-c.Done():
			}
		}()
		return c, cancel
	}}
	type res struct {
		r   jobResult
		err error
	}
	done := make(chan res, 1)
	go func() { r, err := boxJob(ctx, client.WithHandler(d), o, jobResult{}, start); done <- res{r, err} }()
	select {
	case got := <-done:
		if got.err == nil || !strings.Contains(got.err.Error(), "may still come up") || got.r.State != "interrupted" || got.r.Released {
			t.Errorf("Ctrl-C again: %+v %v; want the sandbox may still come up", got.r, got.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cleanup ignored Ctrl-C again")
	}
	if !strings.Contains(errw.String(), "Ctrl-C again to stop waiting") {
		t.Errorf("the cleanup said nothing of how to stop it: %q", errw.String())
	}
}

// A single binding's lifetime cap that --timeout is past is a Misfit with
// the API's code (bad_request), as it is when several bindings all misfit.
func TestBoxJobSingleMisfitHasCode(t *testing.T) {
	d := &prepDaemon{onlySmall: true}
	o := boxJobOptions{timeout: 10 * time.Hour, out: io.Discard, errw: io.Discard}
	_, err := boxJob(context.Background(), client.WithHandler(d), o, jobResult{}, func(context.Context, acquiredBox) (string, error) { return "", nil })
	if !api.IsCode(err, api.CodeBadRequest) || !computeclient.MadeNothing(err) || len(d.seen()) > 0 {
		t.Errorf("one binding too short: %v (code %v), asked %q; want bad_request and nothing asked", err, api.IsCode(err, api.CodeBadRequest), d.seen())
	}
}
