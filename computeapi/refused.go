package computeapi

import (
	"encoding/json"
	"errors"

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
}

// The rules an AcquireRefused names.
const (
	RefusedUnavailable = "unavailable" // the binding's driver says it is not available
	RefusedHandlesMax  = "handles_max" // the caller holds the binding's most live handles
	RefusedBudget      = "budget"      // the binding's budget for the period is spent
	RefusedLifetime    = "lifetime"    // ttl_s is past the binding's lifetime cap
	RefusedDriver      = "driver"      // the binding's driver refused the acquire (no capacity, a size it cannot place)
)

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
	case RefusedUnavailable, RefusedHandlesMax, RefusedBudget, RefusedLifetime, RefusedDriver:
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
