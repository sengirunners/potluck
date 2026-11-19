package cli

import (
	"context"
	"errors"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
)

// jobResult is --json's answer. State: exited (Exit is the command's),
// timeout (killed at --timeout, exit 124), lost (it ended with no exit code:
// its sandbox went; never a pass), interrupted (Ctrl-C), failed (potluck
// could not start it; Error says why).
type jobResult struct {
	Handle         string             `json:"handle,omitempty"`
	Target         string             `json:"target,omitempty"`
	Commit         string             `json:"commit,omitempty"`
	PID            string             `json:"pid,omitempty"`
	State          string             `json:"state"`
	Exit           *int               `json:"exit,omitempty"`
	Error          string             `json:"error,omitempty"`
	ErrorCode      string             `json:"error_code,omitempty"`
	SetupS         *float64           `json:"setup_s,omitempty"`
	Phases         map[string]float64 `json:"phases"`
	AcquireDetails *jobAcquireDetails `json:"acquire_details,omitempty"`
	// Deps are the acquire's answer for each layer asked for (--deps).
	Deps     []computeapi.BoxDepsAnswer `json:"deps,omitempty"`
	Kept     bool                       `json:"kept"`
	Released bool                       `json:"released"`
	Seconds  float64                    `json:"seconds"`
}

// code is the command's exit code for r.
func (r jobResult) code() int {
	switch {
	case r.State == "exited" && r.Exit != nil:
		return *r.Exit
	case r.State == "timeout":
		return 124
	case r.State == "interrupted":
		return 130
	}
	return 1
}

var errInterrupted = errors.New("interrupted before the command started")

// stopped is r for an error before the command ran: on Ctrl-C it is
// interrupted (exit 130), whatever the cut-off step said.
func (r jobResult) stopped(ctx context.Context, err error) (jobResult, error) {
	if ctx.Err() != nil {
		r.State = "interrupted"
		return r, errInterrupted
	}
	return r, err
}

// failureCode uses typed causes, never human-readable messages.
func failureCode(err error) string {
	var noTarget *computeclient.NoTarget
	if errors.As(err, &noTarget) {
		return "no_target"
	}
	var e *api.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
