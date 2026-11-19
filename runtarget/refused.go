package runtarget

import (
	"encoding/json"
	"errors"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
)

// refusal says whether an acquire's err is a refusal, which made nothing:
// an error with a code other than internal (the driver's own answer, the
// daemon's before it asked the driver, a lending host's). An internal one
// (the driver failed, answered no JSON, a bad result or past a limit) may
// come after the driver made the box, and so may an error with no code.
func refusal(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code != api.CodeInternal
}

// markRefused marks a refusal made before the request is recorded (or after
// it was forgotten): nothing was made, so a caller may take its request id to
// another binding.
func markRefused(e *api.Error, reason string) error {
	return e.WithData(computeapi.AcquireRefused{Reason: reason})
}

// driverRefused marks an unavailable refusal a driver (or a lending host's)
// made of a box acquire on binding name, called once the daemon has
// forgotten the request: nothing was made and the request id is free, so a
// caller may ask another binding with it (computeapi.AcquireRefused). Only a
// box's, whose drivers answer unavailable before they make anything (a run's
// driver may answer it while its run starts), and only unavailable (no
// capacity, not ready): the other codes include refusals about a request id
// the driver already made a box for (a reused id with other settings), which
// stay unmarked so the walk stops there. A driver's no-room answer
// (protocol.ReasonNoRoom) is marked RefusedNoRoom with its why and retry
// time, and said from them (AcquireRefused.NoRoomWords). Any other error
// that already carries data keeps it: a driver's
// "building" answer is the daemon's own signal (deps.go).
func driverRefused(kind, name string, err error) error {
	var e *api.Error
	if kind != computeapi.RunTargetKindBox || !errors.As(err, &e) || e.Code != api.CodeUnavailable {
		return err
	}
	var bz protocol.Busy
	switch {
	case len(e.Data) == 0:
		return markRefused(e, computeapi.RefusedDriver)
	case json.Unmarshal(e.Data, &bz) == nil && bz.Reason == protocol.ReasonNoRoom:
		r := computeapi.AcquireRefused{Reason: computeapi.RefusedNoRoom, On: name, Why: protocol.RefusalReason(bz.Why), RetryS: min(max(bz.RetryS, 0), 3600)}
		c := *e
		c.Message = r.NoRoomWords() // the driver's words for it may name its program or a path on its machine
		return c.WithData(r)
	}
	return err
}

// notAvailable is the daemon's refusal of a new box acquire on binding b,
// whose describe d says it is not available, before it asks the driver:
// RefusedNoRoom when that is only for now (d.FullNow), else
// RefusedUnavailable.
func notAvailable(b computeapi.RunTarget, d *computeapi.RunTargetDescribe) error {
	e := api.Errorf(api.CodeUnavailable, "run target %s is not available: %s", b.Name, oneLine(d.Why))
	if b.Kind == computeapi.RunTargetKindBox && d.FullNow() {
		return e.WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedNoRoom, On: b.Name, Why: protocol.RefusalReason(d.Why)})
	}
	return markRefused(e, computeapi.RefusedUnavailable)
}
