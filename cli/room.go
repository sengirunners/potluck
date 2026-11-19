package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/pushbase"
	"github.com/sengirunners/potluck/roomwait"
)

// roomFlag adds --wait, the bound of a command's wait for room (roomwait)
// while what it asked for is full for now; def is its default.
func roomFlag(f *pflag.FlagSet, d *time.Duration, def time.Duration, while string) {
	f.DurationVar(d, "wait", def, "while "+while+", ask again for up to this long, saying why and when it asks next (0: fail at once)")
}

// noRoomWhile is --wait's words for a command that acquires a box.
const noRoomWhile = "every machine that fits has no room now (every slot in use, its reclaim policy or admission controller saying no for now)"

// roomSleep and roomNow stand in for roomwait's clock (tests; nil: the real one).
var (
	roomSleep func(ctx context.Context, d time.Duration) error
	roomNow   func() time.Time
)

// roomWait waits up to d, its lines on w as "<name>: <line>" (nil w: none).
func roomWait(d time.Duration, w io.Writer, name string) roomwait.Wait {
	rw := roomwait.Wait{For: d, Sleep: roomSleep, Now: roomNow}
	if w != nil {
		rw.Say = func(line string) { fmt.Fprintf(w, "%s: %s\n", name, line) }
	}
	return rw
}

// acquireRoom is pushbase.Acquire, asked again through w while its only
// answer is no room now. Each ask starts from req as given (prep and the
// base fill rewrite it per binding); req comes back as the last ask sent it.
// Every ask has the same request id: a no room made nothing, so the id is
// still free.
func acquireRoom(ctx context.Context, w roomwait.Wait, c *client.Client, want computeclient.BoxWant, req *computeapi.AcquireRunTargetRequest,
	prep func(context.Context, string, *computeapi.AcquireRunTargetRequest) error, plan *pushbase.Plan) (target string, h computeapi.RunTargetHandle, err error) {
	given := *req
	err = w.Do(ctx, func(ctx context.Context) error {
		r := given
		var err error
		target, h, err = pushbase.Acquire(ctx, c, want, &r, prep, plan)
		*req = r
		return err
	})
	return target, h, err
}
