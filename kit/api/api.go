// Package api holds the JSON types of the zm/v0 HTTP API (see API.md).
//
// Shared files (api.go, address.go, event.go, project.go) belong to the
// integrator. Each module owns exactly one file: mail.go, work.go,
// session.go, tailnet.go.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Protocol is the protocol's old value, "zm/v0", kept for importers: zm's
// work adapters report and compare it (AdapterProtocol is its name there).
// A daemon and its clients send WireProtocol ("potluck/1"), which a reader
// takes as the same major (SameProtocol); a client and daemon whose major
// differs refuse to talk.
const Protocol = "zm/v0"

// Spec is the old name of Protocol (adapters report it as "zm").
const Spec = Protocol

// EnvUserHome is set in an adapter's environment to the user's home: a
// leading ~ in tracker config means this, not the adapter's own HOME.
const EnvUserHome = "ZM_USER_HOME"

// Version and Commit are set by the build:
//
//	-ldflags "-X github.com/sengirunners/potluck/kit/api.Version=v0.1.0 -X ...api.Commit=abc1234"
var (
	Version = "v0.0.0-dev"
	Commit  = "unknown"
)

// Header names. Every client request carries HeaderProtocol; the daemon
// answers with it too, and each side refuses a different protocol. These are
// the old (Zm-*) names, read still; every sender sends each under its new
// name, Potluck-* (HeaderNew, SetHeader), and every reader reads the new name
// first (GetHeader).
const (
	HeaderProtocol = "Zm-Protocol"
	// HeaderAs is the caller's mail address: a session address on this
	// machine or this machine's human. It never grants anything: who the
	// caller IS comes from the transport (socket = the OS user) or the token.
	HeaderAs = "Zm-As"
	// HeaderProject is an explicit project (slug or ULID) from --project or
	// $ZM_PROJECT; HeaderProjectFrom says which ("flag" or "env").
	HeaderProject     = "Zm-Project"
	HeaderProjectFrom = "Zm-Project-From"
	// HeaderCwd is the client's working directory, used for project
	// resolution on the Unix socket only (ignored from remote callers).
	HeaderCwd = "Zm-Cwd"
	// HeaderAuthorization carries "Bearer <token>" (zm token mint).
	HeaderAuthorization = "Authorization"
	// HeaderScopeVersion is the scope_version a replica last saw for its
	// identity (request) and the authority's current one (response); a
	// mismatch is 409 rescope (SPEC 17.4).
	HeaderScopeVersion = "Zm-Scope-Version"
	// HeaderTeam is the authority's project header for a replica (base64url
	// JSON TeamHeader), on every answer to a node-bound token (SPEC 17.3).
	HeaderTeam = "Zm-Team"
	// HeaderWriteID is a replica's id for one queued write (a ULID it
	// minted): the authority stores a comment or mail under it, so a replay
	// never duplicates (SPEC 17.6). Only a node-bound token's is read.
	HeaderWriteID = "Zm-Write-Id"
	// HeaderNoQueue ("1") on a claim, lease or release asks a replica never
	// to queue it: unreachable or unanswered, it fails (503) with an outcome
	// in the error's data (zm SPEC 17.6, "No-queue lease writes").
	HeaderNoQueue = "Zm-No-Queue"
	// HeaderWriteReplayed ("1") marks an answer the authority recorded for a
	// write id it had already run (a live write delivered twice).
	HeaderWriteReplayed = "Zm-Write-Replayed"
	// HeaderBase is a replica's base for a queued field write (a change id
	// it had seen for the item); only a node-bound token's is read.
	HeaderBase = "Zm-Base"
	// HeaderSeal (any value) asks the tunnel's transport to seal the whole
	// body to the home's key; it never leaves the machine (SPEC 17.11
	// "Sealed secrets").
	HeaderSeal = "Zm-Seal"
	// HeaderSealed ("1") says the body is {"zm_sealed": <the body
	// sealed>}, which the tunnel's door opens before routing.
	HeaderSealed = "Zm-Sealed"
	// HeaderSealKey is the sender's seal key certified by its tunnel key,
	// on every signed request and answer over the tunnel, and on every
	// request and answer between daemons over the tailnet.
	HeaderSealKey = "Zm-Seal-Key"
	// HeaderSealID is the identity public key (base64url) that certified
	// HeaderSealKey, on the tailnet (the tunnel's own Zm-Key names it there).
	HeaderSealID = "Zm-Seal-Id"
	// HeaderSealDate and HeaderSealNonce bind a body sealed over the tailnet
	// to its one request (the tunnel's signed Zm-Date and Zm-Nonce do that
	// there).
	HeaderSealDate  = "Zm-Seal-Date"
	HeaderSealNonce = "Zm-Seal-Nonce"
	// HeaderSealSig is the sender's ed25519 signature (by the Zm-Seal-Id key)
	// over a tailnet-sealed body's purpose and the sealed body's hash: proof
	// that the sender holds the identity it names.
	HeaderSealSig = "Zm-Seal-Sig"
	// HeaderSealing is the sealing mode a home reports beside its key on the
	// tailnet: "required" when any pool it serves requires sealing.
	HeaderSealing = "Zm-Sealing"
)

// Error codes. Each maps to one HTTP status (see Status).
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthenticated = "unauthenticated"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeNotImplemented  = "not_implemented"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
	// CodeRescope (409): the caller's identity's grants changed since the
	// scope_version it sent; a replica refreshes what it holds (SPEC 17.4).
	CodeRescope = "rescope"
	// CodeResync (410): a sync cursor older than the authority's changelog
	// keeps; the replica takes a snapshot (SPEC 17.6).
	CodeResync = "resync"
	// CodeQueued is never sent as an error: the client turns a replica's 202
	// queued answer (QueuedResponse) into an *Error with this code, so a
	// caller expecting a lease cannot mistake a queued claim for one.
	CodeQueued = "queued"
	// CodeRateLimited (429): too many requests; RetryAfter (and the
	// Retry-After header) says when to try again.
	CodeRateLimited = "rate_limited"
	// CodeTooLarge (413): a request body over a route's limit; nothing ran.
	CodeTooLarge = "too_large"
)

// Error is the one error shape on the wire: {"error": {...}}. It is also a Go
// error, so services return it and handlers write it unchanged.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	// Data carries structured detail, e.g. the current lease on a claim
	// conflict. It must be redacted for the caller (see SPEC "Leaks").
	Data json.RawMessage `json:"data,omitempty"`
	// RetryAfter, when set, is how many seconds the caller should wait
	// before trying again (a tracker's rate limit: an adapter passes it on,
	// and the mirror's sync waits at least that long).
	RetryAfter int `json:"retry_after,omitempty"`
	// HTTPStatus is filled in by the client from the response; never sent.
	HTTPStatus int `json:"-"`
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Hint)
	}
	return e.Code + ": " + e.Message
}

// Status is the HTTP status for the error's code.
func (e *Error) Status() int {
	switch e.Code {
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeRescope:
		return http.StatusConflict
	case CodeNotImplemented:
		return http.StatusNotImplemented
	case CodeResync:
		return http.StatusGone
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusInternalServerError
	}
}

// WithHint returns a copy with Hint set.
func (e *Error) WithHint(h string) *Error { c := *e; c.Hint = h; return &c }

// WithRetryAfter returns a copy of e asking the caller to wait secs seconds.
func (e *Error) WithRetryAfter(secs int) *Error { c := *e; c.RetryAfter = secs; return &c }

// WithData returns a copy with Data set to v marshalled.
func (e *Error) WithData(v any) *Error {
	c := *e
	c.Data, _ = json.Marshal(v)
	return &c
}

// Errorf builds an *Error.
func Errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// NotFound is THE not-found error. Out-of-scope objects must be reported with
// exactly this call, so a caller cannot tell "exists but hidden" from
// "missing": api.NotFound("project", ref) is byte-identical either way.
func NotFound(kind, id string) *Error {
	return Errorf(CodeNotFound, "no %s %q", kind, id)
}

// ErrNotImplemented is what module stubs return until a builder fills them.
func ErrNotImplemented(what string) *Error {
	return Errorf(CodeNotImplemented, "%s is not implemented yet", what)
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error *Error `json:"error"`
}

// WriteJSON writes v as JSON with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError writes err as {"error": ...}; non-*Error values become internal
// errors whose message is NOT the Go error text (it may name paths or other
// projects); the daemon log has the detail.
func WriteError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: CodeInternal, Message: "internal error"}
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	WriteJSON(w, e.Status(), ErrorResponse{Error: e})
}

// NotImplemented is an http.HandlerFunc for stub routes.
func NotImplemented(what string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { WriteError(w, ErrNotImplemented(what)) }
}

// MaxBody bounds request bodies (1 MiB).
const MaxBody = 1 << 20

// DecodeJSON reads a JSON request body into v. An empty body leaves v unchanged.
// Unknown fields are refused so typos surface.
func DecodeJSON(r *http.Request, v any) error { return DecodeJSONWithin(r, v, MaxBody) }

// DecodeJSONWithin is DecodeJSON for a route whose bodies may be larger than
// MaxBody: at most limit bytes are read.
func DecodeJSONWithin(r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return Errorf(CodeBadRequest, "invalid JSON body: %v", err)
	}
	return nil
}

// ProtocolMajor is "zm/v0" -> "zm/v0" (until v1, every v0 is its own major).
func ProtocolMajor(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '.'); i > 0 {
		return p[:i]
	}
	return p
}

// Health is GET /v0/health. Remote and unauthenticated callers get only
// OK, Protocol, Version and Commit.
type Health struct {
	OK       bool      `json:"ok"`
	Protocol string    `json:"protocol"`
	Version  string    `json:"version"`
	Commit   string    `json:"commit"`
	Machine  string    `json:"machine,omitempty"`
	PID      int       `json:"pid,omitempty"`
	Started  time.Time `json:"started,omitzero"`
	// Hedges: a team replica's hedged reads (SPEC 17.3), local callers only.
	Hedges *HedgeStats `json:"hedges,omitempty"`
	// SelfCheck is the start-up self-check (SPEC 2, "Swaps"), local callers
	// only; absent until the first check ends.
	SelfCheck *SelfCheck `json:"selfcheck,omitempty"`
	// LastSwap is the last recorded stop, restart or upgrade of this home's
	// daemon, local callers only.
	LastSwap *SwapRecord `json:"last_swap,omitempty"`
	// Parts are the sections modules add to a local caller's answer
	// (core.Deps.AddHealthPart), encoded, by their JSON name: MarshalJSON
	// writes each at the top level, after the fields above, in key order.
	Parts map[string]json.RawMessage `json:"-"`
}

// HedgeStats counts a replica's hedged forwarded reads: over the last
// minute (the budget's window) and since the daemon started. Won: hedges
// that answered first.
type HedgeStats struct {
	Reads1m   int   `json:"reads_1m"`
	Hedges1m  int   `json:"hedges_1m"`
	Refused1m int   `json:"refused_1m"`
	Reads     int64 `json:"reads"`
	Hedges    int64 `json:"hedges"`
	Won       int64 `json:"won"`
	Refused   int64 `json:"refused"`
}

// Whoami is GET /v0/whoami: who the daemon thinks the caller is.
type Whoami struct {
	// As is the caller's mail address: "<machine>/<session8>", "human:<name>"
	// or "machine:<host>". Replies to the caller land here.
	As       string      `json:"as"`
	Kind     AddressKind `json:"kind"`
	Identity Identity    `json:"identity"`
	// Via is how the caller was authenticated: local (Unix socket), token, tailnet.
	Via     string `json:"via"`
	Session string `json:"session,omitempty"`
	Machine string `json:"machine"`
	// Protocol and Commit let a client check it talks to the daemon it expects.
	Protocol string `json:"protocol"`
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	// Home and Socket are only reported to local callers.
	Home   string `json:"home,omitempty"`
	Socket string `json:"socket,omitempty"`
}

// DaemonStopRequest is POST /v0/daemon/stop.
type DaemonStopRequest struct {
	// Force also ends every managed session (outcome infra); without it they
	// keep running in their holders and the next daemon adopts them.
	Force bool `json:"force,omitempty"`
	// Swap is the nonce in $ZM_HOME/swap.lock: while that lock is held, only
	// its holder may stop the daemon.
	Swap string `json:"swap,omitempty"`
	// What says why (restart, upgrade); recorded in the stopping event.
	What string `json:"what,omitempty"`
	// DryRun checks the guards and stops nothing (a daemon older than
	// swaps ignores it and stops: send it only to one with a manifest).
	DryRun bool `json:"dry_run,omitempty"`
}

// DaemonStopResponse reports actual managed-session outcomes when forced.
type DaemonStopResponse struct {
	Stopping        bool                   `json:"stopping"`
	ManagedSessions int                    `json:"managed_sessions"`
	Ended           bool                   `json:"ended,omitempty"`
	NotEnded        []DaemonSessionFailure `json:"not_ended,omitempty"`
}

type DaemonSessionFailure struct {
	Session string `json:"session"`
	Reason  string `json:"reason"`
}

// HeaderPersonalMail is a replica's declaration that it enforces personal
// audience on its local copy. Older replicas receive no personal rows/events.
const HeaderPersonalMail = "Zm-Personal-Mail"

const PersonalMailProtocol = "participants/1"
