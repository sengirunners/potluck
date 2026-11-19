package pushbase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/protocol"
)

// Plan is the repository to seed if placement reports a missing base.
// Prep may fill it after learning that a binding needs a client-side push.
type Plan struct {
	Max        int64 // bundle limit for the initial base fill; zero uses boxpush.DefaultMax
	Repository *Repository
	Dir        string
	// ObserveFill receives the elapsed base-fill attempt, including waiting,
	// transfer, sealing and cleanup. It is a subset of Acquire's wall time.
	ObserveFill func(time.Duration)
}

// Acquire places a box with a base when repo identifies this checkout.
// A miss fills the base on the selected host, then retries the same task
// request. A failed fill falls back to a whole push on that host. The
// daemon guarantees that a structured base_missing refusal made no box.
// With no plan it only places and acquires. Either way req comes back as
// the last acquire sent it, prep's changes included (unchanged when none
// was sent): asked again under the same request id, the daemon answers the
// same request only when everything else is the same too.
func Acquire(ctx context.Context, c *client.Client, want computeclient.BoxWant, req *computeapi.AcquireRunTargetRequest,
	prep func(context.Context, string, *computeapi.AcquireRunTargetRequest) error, plan *Plan) (string, computeapi.RunTargetHandle, error) {
	last := *req
	prepared := false
	capture := func(ctx context.Context, target string, r *computeapi.AcquireRunTargetRequest) error {
		prepared = false
		if prep != nil {
			if err := prep(ctx, target, r); err != nil {
				return err
			}
		}
		if plan != nil {
			r.Base = nil
			if plan.Repository != nil {
				r.Base = &computeapi.BoxBase{Repo: plan.Repository.Key}
			}
		}
		last = *r
		prepared = true
		return nil
	}
	target, h, err := computeclient.AcquireBox(ctx, c, want, *req, capture)
	*req = last
	if !prepared || plan == nil {
		return target, h, err
	}
	if last.Base != nil && ctx.Err() == nil && (api.IsCode(err, api.CodeBadRequest) || (api.IsCode(err, api.CodeNotFound) && !missing(err))) {
		// Older daemons may reject the field, and older drivers may report
		// an untyped missing volume. Do not infer a fill: retry the same target
		// with the hint off. An unrelated refusal is still returned.
		last.Base = nil
		*req = last
		h, err = computeclient.AcquireRunTarget(ctx, c, target, last)
		return target, h, err
	}
	if !missing(err) || ctx.Err() != nil || plan.Repository == nil {
		return target, h, err
	}
	fillStart := time.Now()
	fillErr := fill(ctx, c, target, last, plan.Repository, plan.Dir, plan.Max)
	if plan.ObserveFill != nil {
		plan.ObserveFill(time.Since(fillStart))
	}
	if err := fillErr; err != nil {
		if ctx.Err() != nil {
			return target, h, ctx.Err()
		}
		last.Base = nil
	}
	*req = last
	h, err = computeclient.AcquireRunTarget(ctx, c, target, last)
	if missing(err) && ctx.Err() == nil {
		// Eviction after sealing cannot make the task depend on the cache.
		last.Base = nil
		*req = last
		h, err = computeclient.AcquireRunTarget(ctx, c, target, last)
	}
	return target, h, err
}

func missing(err error) bool {
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeNotFound {
		return false
	}
	var data computeapi.BaseRefusal
	return json.Unmarshal(e.Data, &data) == nil && data.Reason == computeapi.BaseMissing
}

func fill(ctx context.Context, c *client.Client, target string, task computeapi.AcquireRunTargetRequest, repo *Repository, dir string, limit int64) error {
	// This is a different id from the task's: its request is a builder, not
	// the task acquire that was refused before a box existed.
	req := computeapi.AcquireRunTargetRequest{RequestID: "base-" + strings.ToLower(ids.New()), Image: task.Image, Template: task.Template,
		TTLS: 900, SelfRenew: true, Net: computeapi.NetOff, Base: &computeapi.BoxBase{Repo: repo.Key, Fill: true}}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var h computeapi.RunTargetHandle
	for {
		var err error
		h, err = computeclient.AcquireRunTarget(waitCtx, c, target, req)
		if err == nil {
			break
		}
		var e *api.Error
		if !errors.As(err, &e) {
			discardUncertainBuilder(ctx, c, target, req)
			return err
		}
		if e.Code == api.CodeConflict {
			return nil
		} // another builder sealed it
		var busy protocol.Busy
		if e.Code != api.CodeUnavailable || json.Unmarshal(e.Data, &busy) != nil || busy.Reason != protocol.ReasonBuilding {
			if !computeclient.MadeNothing(err) && (e.Code == api.CodeInternal || e.Code == api.CodeUnavailable) {
				discardUncertainBuilder(ctx, c, target, req)
			}
			return err
		}
		delay := time.Duration(busy.RetryS) * time.Second
		if delay < time.Second {
			delay = time.Second
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return waitCtx.Err()
		case <-timer.C:
		}
	}
	_, id, splitErr := computeclient.SplitBoxHandle(h.Handle)
	if splitErr != nil {
		return fmt.Errorf("base builder returned an invalid box handle")
	}
	sealed := false
	defer func() {
		if sealed {
			return
		}
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = computeclient.ReleaseRunTarget(endCtx, c, target, id)
	}()
	if h.Base == nil || h.Base.Path == "" {
		return fmt.Errorf("base builder returned no mount")
	}
	src, err := boxpush.Resolve(ctx, boxpush.MakeOptions{Dir: dir, Ref: repo.Commit, Max: limit})
	if err != nil {
		return err
	}
	ex := boxpush.Exec(c, target, id)
	pushed, err := boxpush.PushSource(ctx, ex, src, boxpush.PushOptions{}, nil)
	if err != nil {
		return err
	}
	x, err := ex(ctx, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", seed, "potluck-base", pushed.Dir, h.Base.Path, repo.Commit}})
	if err != nil {
		return err
	}
	if x.Exit != 0 {
		return fmt.Errorf("seed push base: exit %d: %s", x.Exit, x.Stderr)
	}
	var released computeapi.RunTargetRelease
	err = c.Do(ctx, http.MethodPost, "/v0/run_targets/"+client.Seg(target)+"/handles/"+client.Seg(id)+"/release", computeapi.ReleaseRunTargetRequest{Seal: true}, &released)
	if err != nil {
		return err
	}
	sealed = true
	return nil
}

// A lost acquire response may hide a live builder. Reconcile its id before
// falling back, so it does not keep the host's only slot occupied.
func discardUncertainBuilder(ctx context.Context, c *client.Client, target string, req computeapi.AcquireRunTargetRequest) {
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	h, err := computeclient.AcquireRunTarget(endCtx, c, target, req)
	if err != nil {
		return
	} // its non-renewable lease remains the final bound
	_, id, err := computeclient.SplitBoxHandle(h.Handle)
	if err == nil {
		_, _ = computeclient.ReleaseRunTarget(endCtx, c, target, id)
	}
}

// The builder is fresh and its push's git directory is ours. The ref is
// written last so an interrupted copy never advertises an incomplete base.
const seed = `set -eu
case "$1" in /*) co=$1 ;; *) co=$HOME/$1 ;; esac
case "$2" in /*) base=$2 ;; *) base=$HOME/$2 ;; esac
objects=$(git -C "$co" rev-parse --path-format=absolute --git-path objects)
cp -R "$objects" "$base/objects"
mkdir -p "$base/refs/heads"
printf '%s\n' "$3" > "$base/refs/heads/base"
`
