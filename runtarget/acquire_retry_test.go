package runtarget_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runtarget"
)

// inFront writes a script in front of the driver at ref that runs it on
// the envelope and then does what step says (shell, with $in the envelope
// and $out the driver's answer) before printing the answer.
func inFront(t *testing.T, ref, step string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "front")
	script := fmt.Sprintf("#!/bin/sh\nin=$(cat)\nout=$(printf '%%s' \"$in\" | %q)\nst=$?\n%s\nprintf '%%s\\n' \"$out\"\nexit $st\n", ref, step)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// before is inFront with step run before the driver.
func before(t *testing.T, ref, step string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "front")
	script := fmt.Sprintf("#!/bin/sh\nin=$(cat)\n%s\nprintf '%%s' \"$in\" | exec %q\n", step, ref)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func liveOn(ctx context.Context, t *testing.T, c *client.Client, target string) []computeapi.RunTargetHandle {
	t.Helper()
	hs, err := computeclient.RunTargetHandles(ctx, c, target, true)
	if err != nil {
		t.Fatal(err)
	}
	return hs
}

// TestAcquireDriverFailedAfterMaking (zm-eakh.12.5.41): a driver that made
// the box and then failed without refusing (no JSON answer) leaves the
// request recorded with what list finds: the box counts against
// handles_max, and a retry with the same request id answers that box on
// its binding, never a second box on the next.
func TestAcquireDriverFailedAfterMaking(t *testing.T) {
	ctx := context.Background()
	fail := filepath.Join(t.TempDir(), "fail-next-acquire")
	owner := boxDaemon(t, func(driver, ref string) string {
		if driver != "ref" {
			return ref
		}
		return inFront(t, ref, fmt.Sprintf(`case "$in" in *'"verb":"acquire"'*) if [ -e %q ]; then rm -f %q; echo 'not an answer'; exit 1; fi ;; esac`, fail, fail))
	},
		computeapi.RunTarget{Name: "a", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, HandlesMax: 1},
		computeapi.RunTarget{Name: "b", Driver: "refb", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}})
	touch(t, fail)
	req := computeapi.AcquireRunTargetRequest{RequestID: "k", TTLS: 120}
	if target, h, err := computeclient.AcquireBox(ctx, owner, computeclient.BoxWant{}, req, nil); err == nil || computeclient.MadeNothing(err) || target != "a" {
		t.Fatalf("the failed acquire: %q %+v %v, want a's unmarked failure", target, h, err)
	}
	if hs := liveOn(ctx, t, owner, "a"); len(hs) != 1 {
		t.Errorf("a's live handles after the failure: %+v, want the box it made", hs)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "a", computeapi.AcquireRunTargetRequest{RequestID: "other", TTLS: 120}); err == nil {
		t.Error("a second box on a past handles_max")
	} else if why, ok := computeapi.RefusedBeforeRecord(err); !ok || why != computeapi.RefusedHandlesMax {
		t.Errorf("a second box on a: %v, want handles_max", err)
	}
	target, h, err := computeclient.AcquireBox(ctx, owner, computeclient.BoxWant{}, req, nil)
	if err != nil || target != "a" {
		t.Errorf("the retry: %q %+v %v, want a's box", target, h, err)
	}
	if err == nil {
		t.Cleanup(func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, target, idOf(h.Handle)) })
	}
	if hs := liveOn(ctx, t, owner, "b"); len(hs) != 0 {
		t.Errorf("b's live handles: %+v, want none", hs)
	}
}

// TestAcquireOneRequestTwoBindings (zm-eakh.12.5.42): two acquires of one
// request id on two box bindings at once, the first held in its driver's
// describe, end with one box: the second waits for the first, then is
// sent to it.
func TestAcquireOneRequestTwoBindings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gate, entered := filepath.Join(dir, "gate"), filepath.Join(dir, "entered")
	owner := boxDaemon(t, func(driver, ref string) string {
		if driver != "ref" {
			return ref
		}
		return before(t, ref, fmt.Sprintf(`case "$in" in *'"verb":"describe"'*) if [ -e %q ]; then : > %q; while [ -e %q ]; do sleep 0.05; done; fi ;; esac`, gate, entered, gate))
	},
		computeapi.RunTarget{Name: "a", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}},
		computeapi.RunTarget{Name: "b", Driver: "refb", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}})
	touch(t, gate)
	t.Cleanup(func() { os.Remove(gate) })
	type res struct {
		h   computeapi.RunTargetHandle
		err error
	}
	req := computeapi.AcquireRunTargetRequest{RequestID: "k", TTLS: 120}
	on := func(target string) <-chan res {
		ch := make(chan res, 1)
		go func() {
			h, err := computeclient.AcquireRunTarget(ctx, owner, target, req)
			ch <- res{h, err}
		}()
		return ch
	}
	first := on("a")
	for deadline := time.Now().Add(20 * time.Second); !exists(entered); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a's describe was never asked")
		}
	}
	second := on("b")
	var b res
	select {
	case b = <-second: // it did not wait: the race is lost (a second box comes)
		second = nil
	case <-time.After(time.Second):
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	a := <-first
	if second != nil {
		b = <-second
	}
	for _, r := range []res{a, b} {
		if r.err == nil {
			target, id, _ := computeclient.SplitBoxHandle(r.h.Handle)
			t.Cleanup(func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, target, id) })
		}
	}
	if a.err != nil {
		t.Errorf("k on a: %v", a.err)
	}
	if on, ok := computeapi.RequestUsedOn(b.err); !ok || on != "a" {
		t.Errorf("k on b: %+v %v, want refused naming a", b.h, b.err)
	}
	if n := len(liveOn(ctx, t, owner, "a")) + len(liveOn(ctx, t, owner, "b")); n != 1 {
		t.Errorf("%d live boxes, want 1", n)
	}
}

// TestAcquireRefusedMarks (zm-eakh.12.5.42): the daemon marks an acquire
// refused before it recorded the request when the driver describes the
// binding unavailable, and when the binding's budget is spent; nothing is
// made and the request id stays free.
func TestAcquireRefusedMarks(t *testing.T) {
	ctx := context.Background()
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	owner := boxDaemon(t, func(driver, ref string) string {
		if driver != "down" {
			return ref
		}
		state := filepath.Join(fix, "down", "state") // MkdirAll: the driver's directory too
		p, err := refdriver.Install(filepath.Join(fix, "down"), state, "unavailable")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { refdriver.KillAll(state) })
		return p
	},
		computeapi.RunTarget{Name: "u", Driver: "down", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}},
		computeapi.RunTarget{Name: "spent", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
			Budget: &computeapi.RunTargetBudget{Unit: "cpu_s", Limit: 1, Period: "day"}})
	spend, err := computeclient.AcquireRunTarget(ctx, owner, "spent", computeapi.AcquireRunTargetRequest{RequestID: "spend", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // the box runs past the budget's one cpu_s
	if _, err := computeclient.ReleaseRunTarget(ctx, owner, "spent", idOf(spend.Handle)); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]struct{ code, why string }{
		"u":     {api.CodeUnavailable, computeapi.RefusedUnavailable},
		"spent": {api.CodeForbidden, computeapi.RefusedBudget},
	} {
		_, err := computeclient.AcquireRunTarget(ctx, owner, target, computeapi.AcquireRunTargetRequest{RequestID: "k-" + target, TTLS: 120})
		if why, ok := computeapi.RefusedBeforeRecord(err); !api.IsCode(err, want.code) || !ok || why != want.why {
			t.Errorf("%s: %v (refused before record %q %v), want %s marked %s", target, err, why, ok, want.code, want.why)
		}
		if hs := liveOn(ctx, t, owner, target); len(hs) != 0 {
			t.Errorf("%s's live handles: %+v, want none", target, hs)
		}
	}
}

// TestDriverRefusalMarked (zm-eakh.12.5.45): a driver's own refusal of an
// acquire (no capacity: its describe says available) makes nothing and the
// daemon forgets the request, so it marks the refusal made-nothing: a sandbox
// walks on to the next binding with the same request id, and exactly one box
// is made. A driver refusal that is not unavailable stays unmarked.
func TestDriverRefusalMarked(t *testing.T) {
	ctx := context.Background()
	owner := boxDaemon(t, nil, // the same size: ranked by name, a first
		computeapi.RunTarget{Name: "a", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"refuse": "1"}},
		computeapi.RunTarget{Name: "b", Driver: "refb", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}})
	req := computeapi.AcquireRunTargetRequest{RequestID: "k", TTLS: 120}
	_, err := computeclient.AcquireRunTarget(ctx, owner, "a", req)
	if why, ok := computeapi.RefusedBeforeRecord(err); !api.IsCode(err, api.CodeUnavailable) || !ok || why != computeapi.RefusedDriver {
		t.Errorf("a's refusal: %v (refused before record %q %v), want unavailable marked %s", err, why, ok, computeapi.RefusedDriver)
	}
	target, h, err := computeclient.AcquireBox(ctx, owner, computeclient.BoxWant{}, req, nil)
	if err != nil || target != "b" {
		t.Fatalf("a refusing first choice: %q %+v %v, want b", target, h, err)
	}
	t.Cleanup(func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, "b", idOf(h.Handle)) })
	if n := len(liveOn(ctx, t, owner, "a")) + len(liveOn(ctx, t, owner, "b")); n != 1 {
		t.Errorf("%d live boxes, want 1", n)
	}
}

// TestRelayedNoResult (zm-eakh.12.5.41): a host that ran an op and reported
// no result may have made a box: the authority takes it as internal, never
// a refusal, so an acquire reconciles from list instead of forgetting it.
func TestRelayedNoResult(t *testing.T) {
	if _, err := runtarget.AsRelayedFromOld(nil); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("no result: %v, want internal", err)
	}
	// A failure with a message and no code is internal too, its words kept.
	if _, err := runtarget.AsRelayedFromOld(errors.New("it fell over")); !api.IsCode(err, api.CodeInternal) || !strings.Contains(err.Error(), "it fell over") {
		t.Errorf("a failure with no code: %v, want internal naming it", err)
	}
	// A code a host reported is kept, never made internal.
	if _, err := runtarget.AsRelayedFromOld(api.Errorf(api.CodeUnavailable, "no capacity")); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("a failure with a code: %v, want unavailable", err)
	}
}
