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
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/roomwait"
)

// A lending host's refusal of a box, relayed to the pool's caller, names
// the binding and never the driver program (zm-9xzw.14): a Linux Docker host
// once answered "potluck-driver-tart acquire: ...". A no room keeps its
// structured fields across the relay, and its message is said from them
// by the daemon that marks it, whatever words the host's build wrote.
func TestARelayedRefusalNamesNoDriverProgram(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "answer.json")
	rp := newRelayPairWrapped(t, func(drv string) string {
		return before(t, drv, fmt.Sprintf(`case "$in" in *'"verb":"acquire"'*) if [ -e %q ]; then cat %q; exit 0; fi ;; esac`, answer, answer))
	})
	refuse := func(body string) error {
		t.Helper()
		if err := os.WriteFile(answer, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := computeclient.AcquireRunTarget(context.Background(), rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "k", TTLS: 120})
		return err
	}

	// A host's driver on an older build words it so; the caller never sees those words.
	err := refuse(`{"ok":false,"error":{"code":"unavailable","message":"the run did not start: no room on this host now: lender closed: jobs are waiting for a processor core (retry in 600 s); report: ~/.zm-run-tart/failed/rb2840cf0a757.txt",` +
		`"data":{"reason":"no_room","why":"lender closed: jobs are waiting for a processor core","retry_s":600}}}`)
	r, ok := computeapi.NoRoom(err)
	if !ok || r.On != lent || r.Why != "lender closed: jobs are waiting for a processor core" || r.RetryS != 600 {
		t.Fatalf("relayed no room: %v (%+v %v); want it marked no room on %s with the host's reason and retry time", err, r, ok, lent)
	}
	if strings.Contains(err.Error(), "potluck-driver-") {
		t.Errorf("relayed no room names the driver program: %v", err)
	}
	var e *api.Error
	if !asAPI(err, &e) || e.Code != api.CodeUnavailable ||
		e.Message != lent+" has no room now: lender closed: jobs are waiting for a processor core; it says to ask again in 600 s" {
		t.Errorf("relayed no room as the caller reads it: %v", err)
	}
	if f, ok := roomwait.Of(err); !ok || f.Where != lent || f.Retry != 10*time.Minute {
		t.Errorf("relayed no room as a wait reads it: %+v %v", f, ok)
	}

	err = refuse(`{"ok":false,"error":{"code":"unavailable","message":"the box did not start: the VM exited while booting (exit 1); report: ~/.potluck-driver-tart/failed/r0123456789ab.txt (on lender)"}}`)
	if !asAPI(err, &e) || e.Code != api.CodeUnavailable ||
		e.Message != "sandbox: the box did not start: the VM exited while booting (exit 1); report: ~/.potluck-driver-tart/failed/r0123456789ab.txt (on lender)" {
		t.Errorf("relayed hard refusal: %v; want the binding's name and the host's words", err)
	}
	if _, ok := roomwait.Of(err); ok {
		t.Errorf("a hard refusal read as no room: %v", err)
	}
}

func asAPI(err error, e **api.Error) bool { return errors.As(err, e) }
