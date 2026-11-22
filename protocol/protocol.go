// Package protocol holds the wire types of the potluck/1 driver protocol
// (docs/protocol.md): the request a driver reads on stdin, the error shape it
// answers with, and the capability and state names the protocol defines.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Protocol is the wire value of potluck/1, sent under the key "potluck".
// ProtocolOld is its value before the rename, "zm/v0", still sent beside it
// under the key "zm" (a driver from before reads only that key) and read as
// the same major (SameProtocol).
const (
	Protocol    = "potluck/1"
	ProtocolOld = "zm/v0"
)

// ProtocolNext is Protocol (its step 1 name).
const ProtocolNext = Protocol

// SameProtocol reports whether two protocol values have one major, taking
// "potluck/1" and "zm/v0" as one.
func SameProtocol(a, b string) bool { return family(a) == family(b) }

func family(p string) string {
	if m := ProtocolMajor(p); m != ProtocolMajor(ProtocolOld) {
		return m
	}
	return ProtocolMajor(Protocol)
}

// ProtocolMajor is "zm/v0" -> "zm/v0" (until v1, every v0 is its own major).
func ProtocolMajor(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '.'); i > 0 {
		return p[:i]
	}
	return p
}

// Request is the one JSON object a driver reads on stdin. Its protocol is
// under the key "potluck" (Next: Protocol) and, for drivers from before the
// rename, under "zm" (Spec: ProtocolOld); NewRequest fills both, and
// Protocol() reads either.
type Request struct {
	Spec   string            `json:"zm,omitempty"`      // ProtocolOld
	Next   string            `json:"potluck,omitempty"` // Protocol
	Verb   string            `json:"verb"`
	ID     string            `json:"id,omitempty"`
	Args   json.RawMessage   `json:"args,omitempty"`
	Config map[string]string `json:"config,omitempty"`
}

// NewRequest is a request as a daemon sends it: the protocol under both keys.
func NewRequest(verb, id string, args json.RawMessage, config map[string]string) Request {
	return Request{Spec: ProtocolOld, Next: Protocol, Verb: verb, ID: id, Args: args, Config: config}
}

// Protocol is the request's protocol value: under "potluck", else "zm".
func (r Request) Protocol() string {
	if r.Next != "" {
		return r.Next
	}
	return r.Spec
}

// Error codes a driver answers with.
const (
	CodeBadRequest     = "bad_request"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeNotImplemented = "not_implemented"
	CodeUnavailable    = "unavailable"
	CodeInternal       = "internal"
)

// Error is the one error shape on the wire: {"error": {...}}.
type Error struct {
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Hint       string          `json:"hint,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	RetryAfter int             `json:"retry_after,omitempty"`
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Hint)
	}
	return e.Code + ": " + e.Message
}

// Box capabilities a driver lists in describe.
const (
	// CapPersist keeps a box's disk across stop and start.
	CapPersist = "persist"
	// CapRoot: the box's user has working sudo; CapDocker: it can run
	// docker (implies CapRoot).
	CapRoot   = "root"
	CapDocker = "docker"
	// CapRootVM (with CapRoot): the box is a whole VM of its own, so root in
	// it holds every capability of the guest's kernel, and what fences the
	// box sits outside the guest.
	CapRootVM = "root-vm"
	// CapNetOff honours an acquire's net "off".
	CapNetOff = "net-off"
	// CapVolumes mounts acquire's named volumes (SPEC 20 "Boxes", Named volumes).
	CapVolumes = "volumes"
	// CapVolumesSeal takes volume mode build, release's seal and a volume's
	// sealed (protocol/volume.go).
	CapVolumesSeal = "volumes-seal"
	// CapVolumesKeep takes release's keep beside seal (protocol/volume.go,
	// KeepPathsOK; docs/protocol.md, Sealed volumes).
	CapVolumesKeep = "volumes-keep"
	// CapShelf: every box sees the package shelf, read-only, at
	// describe.shelf.path (docs/protocol.md, The package shelf).
	CapShelf = "shelf"
	// CapGitHubJIT takes acquire's jit_config and starts that runner;
	// CapGitHubJITGuard takes jit_guard: the runner serves only those jobs.
	CapGitHubJIT      = "github-jit"
	CapGitHubJITGuard = "github-jit-guard"
	// CapJITGuardPrivate: jit_guard takes open_if_private (with events): a
	// job its events refuse (another event's run, a fork's) is served only
	// while the job's own event payload says the repository is private.
	CapJITGuardPrivate = "github-jit-guard-private"
	// CapHostLend: the driver holds the boxes it starts to the lender's
	// dials (the cores, memory and disk kept free in the host's
	// host-lend.json, Go package vmhost lend.go), read at every acquire. A
	// driver without it ignores the dials (docs/menubar.md "Memory, and the
	// dials").
	CapHostLend = "host-lend"
)

// ShelfWants is where a box names what it wants on the shelf: a file
// relative to the box user's HOME, one want per line ("npm <name> <version>
// sha512-<base64>"). A driver reads it only as data from inside a box.
const ShelfWants = ".potluck/wants"

// A box acquire's net.
const (
	NetOn  = "on"
	NetOff = "off"
)

// StateStopped is a persistent box that is stopped: its disk kept until
// expires_at, nothing running, startable by its holder.
const StateStopped = "stopped"
