// Package rt is the run-target module's published surface (SPEC 20): the
// service other modules reach through core.Deps (From), the interfaces a
// machine's host side and the team relay implement, the shared time limits,
// and the project.toml [[run_targets]] section (projectfile.go) and
// $ZM_HOME/run_targets.toml (hostfile.go). runtarget implements it.
package rt

import (
	"context"
	"errors"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/secrets"
)

// Name is the service runtarget provides on core.Deps.
const Name = "runtarget"

// From is the run-target service on d, or nil when the module is not wired.
func From(d *core.Deps) Service {
	s, _ := d.Service(Name).(Service)
	return s
}

// EndFunc receives a run-target handle once it has ended (released,
// expired, reclaimed, lost, or a run that ended on its own), after zm's
// final status, so its spend is final. Each call runs on its own goroutine
// (a panic is recovered and logged), so it never delays or fails the request
// or sweep that ended the handle.
type EndFunc func(ctx context.Context, project string, h computeapi.RunTargetHandle)

// BoxAcquireFunc is asked before a new box is acquired, or a stopped box
// started again, in area of project (area "" is the project's own). live are that area's live boxes on every
// binding of the project, as last recorded; it reads them only when called,
// so a subscriber with nothing to check costs nothing. A non-nil error
// refuses the acquire and is the caller's answer, so it must say why in
// plain words.
type BoxAcquireFunc func(ctx context.Context, project, area string, live func() ([]computeapi.RunTargetHandle, error)) error

// Service acquires, renews and releases run-target handles as a subject,
// with exactly the checks of the run-target routes (SPEC 20): the runner
// broker (runners) acquires a box as a linked area's zm-runners.
type Service interface {
	Acquire(ctx context.Context, sub *authz.Subject, project, target string, req computeapi.AcquireRunTargetRequest) (computeapi.RunTargetHandle, error)
	Renew(ctx context.Context, sub *authz.Subject, project, target, id string, ttlS int) (computeapi.RunTargetHandle, error)
	Release(ctx context.Context, sub *authz.Subject, project, target, id string) (computeapi.RunTargetRelease, error)
	// List is GET /v0/run_targets as sub (describe: ?describe=1), every host's
	// box bindings with their host_class: the broker places a job from it.
	List(ctx context.Context, sub *authz.Subject, project string, describe bool) (computeapi.RunTargetList, error)
	// Status is GET /v0/run_targets/{target}/handles/{id} as sub: the driver
	// asked, its answer recorded (an end fires OnEnd's subscribers).
	Status(ctx context.Context, sub *authz.Subject, project, target, id string) (computeapi.RunTargetHandle, error)
	// OnEnd subscribes f to every handle's end (the sandbox usage ledger
	// writes its row here); subscribers register before Start.
	OnEnd(f EndFunc)
	// OnBoxAcquire subscribes f to every new box acquire and every start of a
	// stopped box (the usage module's per-area minute limit); subscribers
	// register before Start. A repeat of an acquire already made is not asked.
	OnBoxAcquire(f BoxAcquireFunc)
	// EndedSince answers project's handles matching only that ended at or
	// after since (inclusive, so a reconcile reading from its last watermark
	// sees that row again and must be idempotent), oldest end first, at most
	// limit (1..1000): a subscriber whose hook write failed reconciles from
	// it. Only handles that got one. Daemon-internal with no caller scope:
	// never hand its rows to a caller without the caller's own check (area
	// included).
	EndedSince(ctx context.Context, project string, only EndedOnly, since time.Time, limit int) ([]computeapi.RunTargetHandle, error)
}

// EndedOnly narrows EndedSince; an empty field matches every value.
type EndedOnly struct {
	Area string
	Kind string // computeapi.RunTargetKind*
}

// DoorName is the service the secrets door's side of run targets is
// provided under (runtarget provides it beside Name).
const DoorName = "runtarget.door"

// DoorFrom is the door's side of run targets on d, or nil.
func DoorFrom(d *core.Deps) Door {
	s, _ := d.Service(DoorName).(Door)
	return s
}

// Door is what the secrets door asks of run targets, as the authority and
// with no caller: write a credential it minted into a box (the holder
// check and the reserved names skipped, every other gate of a box secrets
// call kept), the class of the machine serving a box, whether a box lives.
// A box is its project's ULID and its handle. Daemon-internal: never reach
// it from a route.
type Door interface {
	// BoxSecret writes one value under name, replacing what is there. An
	// error wrapping ErrMaybeDelivered means the call may have reached the
	// box; any other error, that nothing was written.
	BoxSecret(ctx context.Context, project, handle, name string, v secrets.Value) error
	BoxClass(ctx context.Context, project, handle string) (string, error)
	BoxLive(ctx context.Context, project, handle string) (bool, error)
}

// ErrMaybeDelivered: a door write failed after it was sent (the driver did
// not answer in time, or the host could not be reached mid-call), so the
// value may be in the box.
var ErrMaybeDelivered = errors.New("the value may have been delivered")

// ValidPeriod reports whether p is a budget period: day, week or month.
func ValidPeriod(p string) bool { return p == "day" || p == "week" || p == "month" }

// PeriodStart is the start of budget period p (UTC calendar) containing
// now: the day, the ISO week (from Monday) or the month.
func PeriodStart(p string, now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch p {
	case "week":
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return day
}

// Run-target time limits (SPEC 20), shared by the driver call and the
// proxy rows that forward a member's call to its authority (it waits for
// them, not its own call timeout) and by the runners broker.
const (
	VerbLimit    = 30 * time.Second  // describe, status, release, renew, list
	AcquireLimit = 120 * time.Second // acquire and setup
	ExecDefaultS = 30                // exec's timeout_s when 0
	ExecMaxS     = 600
	// RelaySlack is what a relayed call may take over the driver's own
	// limit (runtarget remote.go).
	RelaySlack = 20 * time.Second
)

// CodeDriverTimeout is the code a host reports for a driver that did not
// answer in its time limit (the relay returns it as an *api.Error): the
// authority treats it like its own driver's timeout, so the acquire retry
// and the list reconcile apply (SPEC 20).
const CodeDriverTimeout = "driver_timeout"

// noHandle is a not_found that is about the handle itself.
type noHandle struct{ err *api.Error }

func (e *noHandle) Error() string { return e.err.Error() }
func (e *noHandle) Unwrap() error { return e.err }

// NoHandle marks e (a not_found) as the handle's target, or this machine's
// own record, saying it has no such handle. It still is e to errors.As and
// api.IsCode: every check for a not_found reads it as before. Over the relay
// the code stays not_found too, the mark beside it (runtarget relay.go), so
// a machine on a build without the mark does what it always did.
func NoHandle(e *api.Error) error { return &noHandle{e} }

// IsNoHandle reports whether err is a status's not_found about the handle
// itself: the box is gone. A not_found for any other reason is not: whoever
// answered it did not speak about the box.
func IsNoHandle(err error) bool {
	var e *noHandle
	return errors.As(err, &e)
}
