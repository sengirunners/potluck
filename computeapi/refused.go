package computeapi

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sengirunners/potluck/kit/api"
)

// AcquireRefused is the data of an acquire refusal the daemon made before it
// recorded the request, or a driver's refusal after which it forgot the
// request: nothing was made and the request id is still free, so a caller
// may ask another binding with it. Reason names the rule (Refused*). Every
// other failure (a timeout, a failure that is no refusal, a cut-off
// connection) carries none: the box may still come up there.
type AcquireRefused struct {
	Reason string `json:"refused"`
	// On, Why and RetryS are a RefusedNoRoom refusal's: the binding that is
	// full, what is full in its host's words (untrusted text), and the
	// host's admission controller's retry time in seconds (0: none given).
	On     string `json:"on,omitempty"`
	Why    string `json:"why,omitempty"`
	RetryS int    `json:"retry_s,omitempty"`
}

// The rules an AcquireRefused names.
const (
	RefusedUnavailable = "unavailable" // the binding's driver says it is not available
	RefusedHandlesMax  = "handles_max" // the caller holds the binding's most live handles
	RefusedBudget      = "budget"      // the binding's budget for the period is spent
	RefusedLifetime    = "lifetime"    // ttl_s is past the binding's lifetime cap
	RefusedDriver      = "driver"      // the binding's driver refused the acquire (no capacity, a size it cannot place)
	RefusedImage       = "image"       // the binding lends no pin of the image the acquire needs (a database recipe's)
	RefusedNoRoom      = "no_room"     // the binding is only full for now (RunTargetDescribe.FullNow, protocol.ReasonNoRoom): asking again later may get the box
)

// NoRoomWords is a RefusedNoRoom refusal as a caller reads it, said from
// its fields: "<on> has no room now: <why>; it says to ask again in 600 s".
// The daemon that marks the refusal makes it the refusal's message, so no
// reader shows a driver's or a host's own text for it (a driver program's
// name, a path on the host).
func (r AcquireRefused) NoRoomWords() string {
	on, why := r.On, r.Why
	if on == "" {
		on = "the pool"
	}
	if why == "" {
		why = "no reason given"
	}
	s := on + " has no room now: " + why
	if r.RetryS > 0 {
		s += fmt.Sprintf("; it says to ask again in %d s", r.RetryS)
	}
	return s
}

// NoRoom says whether err is an acquire refusal whose only reason is that
// the binding is full for now (RefusedNoRoom), and if so what it said.
func NoRoom(err error) (AcquireRefused, bool) {
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeUnavailable || len(e.Data) == 0 {
		return AcquireRefused{}, false
	}
	var r AcquireRefused
	if json.Unmarshal(e.Data, &r) != nil || r.Reason != RefusedNoRoom {
		return AcquireRefused{}, false
	}
	return r, true
}

// RefusedBeforeRecord says whether err is an acquire refusal marked
// AcquireRefused, and if so its reason.
func RefusedBeforeRecord(err error) (string, bool) {
	var e *api.Error
	if !errors.As(err, &e) || len(e.Data) == 0 {
		return "", false
	}
	var r AcquireRefused
	if json.Unmarshal(e.Data, &r) != nil {
		return "", false
	}
	switch r.Reason {
	case RefusedUnavailable, RefusedHandlesMax, RefusedBudget, RefusedLifetime, RefusedDriver, RefusedImage, RefusedNoRoom:
		return r.Reason, true
	}
	return "", false
}

// AcquireUsedOn is the data of a box acquire the daemon refused because its
// request id already names a box acquire on another of the project's
// bindings, Target: that binding answers it (a retry, never a second box).
type AcquireUsedOn struct {
	Target string `json:"used_on"`
}

// RequestUsedOn says whether err is a refusal marked AcquireUsedOn, and if
// so the binding that holds the request.
func RequestUsedOn(err error) (string, bool) {
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConflict || len(e.Data) == 0 {
		return "", false
	}
	var u AcquireUsedOn
	if json.Unmarshal(e.Data, &u) != nil || u.Target == "" {
		return "", false
	}
	return u.Target, true
}

// TargetNotListed is the data of an answer that found no run target Name
// listed now: the caller's resolution of --on found it in no listing, or
// the daemon has no advert from the host that serves it. Either may be a
// host out of touch for a moment, so a wait that saw the target answer
// earlier asks again (roomwait); a name never listed is still a mistake.
type TargetNotListed struct {
	Name string `json:"not_listed"`
}

// NotListed says whether err is an answer marked TargetNotListed, and if so
// the name it found no target for.
func NotListed(err error) (string, bool) {
	var e *api.Error
	if !errors.As(err, &e) || (e.Code != api.CodeNotFound && e.Code != api.CodeUnavailable) || len(e.Data) == 0 {
		return "", false
	}
	var n TargetNotListed
	if json.Unmarshal(e.Data, &n) != nil || n.Name == "" {
		return "", false
	}
	return n.Name, true
}
