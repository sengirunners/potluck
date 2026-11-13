package protocol

import "regexp"

// Acquire progress (docs/protocol.md, Acquire progress and cancel): while an
// acquire is in flight, before it has a handle, progress says how far it got
// and cancel stops it. Both name the acquire by its request_id. A driver
// without the capability is still served: the daemon then reports only that
// the acquire is starting, and cannot cancel it.
const (
	// CapAcquireProgress: the driver serves progress and cancel.
	CapAcquireProgress = "acquire-progress"
	// VerbProgress answers an acquire's state by its request_id.
	VerbProgress = "progress"
	// VerbCancel stops an acquire in flight by its request_id.
	VerbCancel = "cancel"
)

// An acquire's progress states.
const (
	// AcquireStarting: the acquire is in flight; Phase says where.
	AcquireStarting = "starting"
	// AcquireReady: the acquire made its resource; an acquire with the same
	// request_id answers its handle.
	AcquireReady = "ready"
	// AcquireFailed: the acquire was refused or failed and holds nothing.
	AcquireFailed = "failed"
	// AcquireCancelled: cancel stopped it and nothing is held. An acquire
	// with the same request_id answers conflict and makes nothing.
	AcquireCancelled = "cancelled"
)

// AcquirePhaseQueued is the daemon's own phase for an acquire waiting its
// turn on the binding, before the driver has been asked.
const AcquirePhaseQueued = "queued"

// AcquireProgress is progress's answer.
type AcquireProgress struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	// Phase is the driver's word for the step a starting acquire is in
	// ("queued", "booting", "checkout"); required while starting.
	Phase string `json:"phase,omitempty"`
	// Message is one line for a person, never a secret.
	Message string `json:"message,omitempty"`
}

// CancelAnswer is cancel's answer: Cancelled when the acquire is stopped, or
// will be refused when it comes (now or by an earlier cancel, after a failed
// acquire too); false only when it already made its resource. State is its
// progress state after the call.
type CancelAnswer struct {
	Cancelled bool   `json:"cancelled"`
	State     string `json:"state"`
}

var acquirePhase = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// AcquirePhase reports whether p is a phase: a lowercase word of at most 32
// characters (letters, digits, -).
func AcquirePhase(p string) bool { return acquirePhase.MatchString(p) }

// AcquireState reports whether s is one of progress's states.
func AcquireState(s string) bool {
	switch s {
	case AcquireStarting, AcquireReady, AcquireFailed, AcquireCancelled:
		return true
	}
	return false
}
