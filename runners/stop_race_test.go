package runners_test

import (
	"net/http"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
)

// Stopping the broker while it has a job read in flight is not misuse of
// GitHub: the read is abandoned when the broker's context ends, Close revokes
// the installation's tokens, and the fake then gets the request with a revoked
// token. It answers 401, as GitHub does, and records nothing (the cleanup
// fails the test on any recorded misuse). TestParseLabels used to fail this
// way, rarely, when a case ended with the broker still reading its waiting job.
func TestStopWithJobReadInFlight(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.Label = "MyPool" })
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	gate := make(chan struct{})
	f.gh.mu.Lock()
	f.gh.gate, f.gh.gateOn = gate, "/actions/jobs/9100"
	f.gh.mu.Unlock()
	if code := f.deliver(t, "workflow_job", jobEvent("queued", 9100, 910, "acme/app", true, "mypool:c1m2")); code != http.StatusAccepted {
		t.Fatalf("deliver: %d", code)
	}
	eventually(t, "the job read held at GitHub", func() bool { f.gh.mu.Lock(); defer f.gh.mu.Unlock(); return f.gh.waiting > 0 })
	f.stop()
	f.stop = nil
	f.gh.mu.Lock()
	f.gh.gate = nil
	f.gh.mu.Unlock()
	close(gate) // the held read now reaches the fake, its token revoked
}
