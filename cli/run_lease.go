package cli

// A run's sandbox lease: asked short and renewed by this client while it
// follows the command, so a client that dies (kill -9, a lost laptop, a
// dropped pool) leaves a sandbox that ends within runLeaseTTL, not at the
// end planned for --timeout; and the end of the sandbox, asked again while
// the pool does not answer.

import (
	"context"
	"errors"
	"time"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// runLeaseTTL is the lease a renewed sandbox is asked for, renewed every
// runRenewEvery; runReleaseFor is how long the end of a sandbox is asked
// again for.
var (
	runLeaseTTL   = 5 * time.Minute
	runRenewEvery = time.Minute
	runReleaseFor = 3 * time.Minute
)

// releaseBox ends the sandbox, asking again for up to runReleaseFor (and
// while ctx lasts) when the pool does not answer: a blip of the network must
// not leave a sandbox up. A caller that ends it whatever happened, Ctrl-C
// included, passes a ctx that a Ctrl-C does not cancel. again, when set,
// runs once before the first ask again; the asks after it go on the context
// it returns (one another Ctrl-C cancels), so a Ctrl-C while the first ask
// is answered cuts nothing short.
func releaseBox(ctx context.Context, c *client.Client, target, id string, again func(context.Context) (context.Context, context.CancelFunc)) error {
	rctx, cancel := context.WithTimeout(ctx, runReleaseFor)
	defer cancel()
	for wait := time.Second; ; wait = min(wait*2, 15*time.Second) {
		actx, acancel := context.WithTimeout(rctx, 60*time.Second)
		_, err := computeclient.ReleaseRunTarget(actx, c, target, id)
		acancel()
		if err == nil || !askAgain(err) || rctx.Err() != nil {
			return err
		}
		if again != nil {
			var stop context.CancelFunc
			rctx, stop = again(rctx)
			defer stop()
			again = nil
		}
		select {
		case <-rctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

// askAgain says whether a release that failed is sent again: no answer, a
// dropped relay or a call that outlasted its own time limit; not a refusal,
// and not a driver's failure (internal), which is an answer.
func askAgain(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || boxpush.Transient(err) && !api.IsCode(err, api.CodeInternal)
}

// keepLease renews the sandbox's short lease every runRenewEvery until
// stopped (the returned func, which waits for the last renewal and answers
// why renewing stopped early, nil if it did not), by runLeaseTTL from now
// and never past end. A renewal that fails is tried again at the next tick,
// while the failure may pass (no answer, a dropped relay, a driver's
// failure), and stops at a refusal (the sandbox gone or ended, the lease
// past its cap): the lease outlasts a few missed renewals, and when none
// gets through the sandbox ends at the lease, which is the point. It writes
// nothing itself: the caller's streams are not its to share.
func keepLease(ctx context.Context, c *client.Client, target, id string, end time.Time) (stop func() error) {
	kctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	var last error // the last renewal's failure, nil once one got through
	go func() {
		defer close(done)
		t := time.NewTicker(runRenewEvery)
		defer t.Stop()
		for {
			select {
			case <-kctx.Done():
				return
			case <-t.C:
			}
			ttl := min(runLeaseTTL, time.Until(end))
			if ttl < time.Second {
				return // the planned end: the lease runs out there
			}
			rctx, rcancel := context.WithTimeout(kctx, 60*time.Second) // the daemon allows a driver's renew up to 50 s
			_, err := computeclient.RenewRunTarget(rctx, c, target, id, int(ttl/time.Second))
			rcancel()
			if kctx.Err() != nil {
				last = nil // stopped while it was asked: no failure to report
				return
			}
			last = err
			if err != nil && !boxpush.Transient(err) && !errors.Is(err, context.DeadlineExceeded) {
				return
			}
		}
	}()
	return func() error { cancel(); <-done; return last }
}
