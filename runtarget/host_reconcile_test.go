package runtarget_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
)

// TestLentAcquireFailedAfterMaking (zm-eakh.12.5.44): a lending host's
// driver that made the box and then failed without refusing (no JSON
// answer) leaves a box the host never recorded for the project. The
// authority's reconcile still finds it through the host's list, so it keeps
// the request: a retry of the same request id on another binding is sent
// back to the lent one, which answers that box, and no second box is made.
// The adopted box is the project's for status and release too.
func TestLentAcquireFailedAfterMaking(t *testing.T) {
	ctx := context.Background()
	fail := filepath.Join(t.TempDir(), "fail-next-acquire")
	rp := newRelayPairWrapped(t, func(drv string) string {
		return inFront(t, drv, fmt.Sprintf(`case "$in" in *'"verb":"acquire"'*) if [ -e %q ]; then rm -f %q; echo 'not an answer'; exit 1; fi ;; esac`, fail, fail))
	})
	if _, err := computeclient.BindRunTarget(ctx, rp.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "here", Driver: "ref", Kind: "box",
		Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
		t.Fatal(err)
	}
	touch(t, fail)
	req := computeapi.AcquireRunTargetRequest{RequestID: "k", TTLS: 120}
	if h, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, req); err == nil || computeclient.MadeNothing(err) {
		t.Fatalf("the failed acquire on %s: %+v %v, want an unmarked failure", lent, h, err)
	}
	if exists(fail) {
		t.Fatal("the host's driver was not asked")
	}
	if _, err := computeclient.AcquireRunTarget(ctx, rp.owner, "here", req); err == nil {
		t.Error("the retry on another binding made a second box")
	} else if on, ok := computeapi.RequestUsedOn(err); !ok || on != lent {
		t.Errorf("the retry on another binding: %v, want it sent to %s", err, lent)
	}
	h, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, req)
	if err != nil {
		t.Fatalf("the retry on %s: %v, want the box it made", lent, err)
	}
	if hs := liveOn(ctx, t, rp.owner, "here"); len(hs) != 0 {
		t.Errorf("here's live handles: %+v, want none", hs)
	}
	if st, err := rp.status(h); err != nil {
		t.Errorf("status of the adopted box: %q %v", st, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, rp.owner, lent, idOf(h.Handle)); err != nil {
		t.Errorf("release of the adopted box: %v", err)
	}
}
