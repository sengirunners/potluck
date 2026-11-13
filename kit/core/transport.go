package core

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// Team transports (SPEC 17.11 "Transports"). A transport is one way a
// member's daemon reaches a team project's home: the embedded tailnet
// (kit/tailnet) and the signed HTTPS tunnel (kit/tunnel) today;
// the host's own Tailscale is planned. Each is a driver of TeamTransport,
// registered once in the wiring (internal/wiring) with Deps.AddTransport. The team module
// looks drivers up by Via and knows nothing else about them: joining,
// pinning, signing and verifying, inviting and checking a redemption all
// happen behind the interface.

// ViaTailnet is the embedded tailnet's Via (a Remote with Via "" is one).
const ViaTailnet = "tailnet"

// TeamTransport is one transport's driver.
type TeamTransport interface {
	// Via names it: api.Remote.Via of the remotes joined over it and
	// Remote.Via of the requests it carries.
	Via() string

	// ---- the member's side ----

	// ParseCode reads code when it is this transport's join code (ok false
	// when it is not; an error when it is, but malformed).
	ParseCode(code string) (target JoinTarget, ok bool, err error)
	// Prepare readies this machine to reach target as machine before the
	// code is redeemed (the tailnet: its node up, under its own name). A
	// browser login still needed is login (and note), with no error.
	Prepare(ctx context.Context, req api.JoinRequest, machine string, target JoinTarget) (login, note string, err error)
	// Extra are the headers every call to the home carries, from the join
	// request (the tunnel: a Cloudflare Access service token), or nil. The
	// team module stores them next to the token.
	Extra(req api.JoinRequest) (http.Header, error)
	// RedeemRequest is what this machine sends to redeem target's code.
	RedeemRequest(target JoinTarget, machine string) (api.RedeemRequest, error)
	// Client reaches target's home with the home pinned (and for the
	// tunnel, every request signed and every answer verified). A Client
	// whose Transport has NoReuse() http.RoundTripper hedges on that; a
	// plain *http.Transport is cloned without keep-alives.
	Client(target JoinTarget, opts ClientOptions) *http.Client
	// Explain turns an error from Client into what a caller sees, or nil
	// when it is not this transport's (the team module then says the home
	// is unreachable).
	Explain(url string, err error) error

	// ---- the home's side ----

	// Invite is what an invite made on this machine names. An error with
	// code unavailable means this transport is not serving here now (an
	// invite without a via then tries the next transport); any other error
	// is the answer.
	Invite(ctx context.Context) (InviteTarget, error)
	// RateKey is the redemption rate-limit bucket of a caller of this
	// transport (each transport has its own budget).
	RateKey(rm *Remote) string
	// RedeemInput is a redemption that arrived over this transport, for the
	// registry: the code's secret, or the hash a proof matched. A request
	// it cannot read gives an input that matches no invite.
	RedeemInput(ctx context.Context, rm *Remote, req api.RedeemRequest) project.RedeemInput
}

// Sealer is this daemon's seal key (the tunnel module holds it, SPEC 17.11
// "Sealed secrets"; kit/sealbox is the primitive). Every leg that carries a
// run-target call's secrets or a jit_config finds it by a type assertion on
// Transport(ViaTunnel), whatever transport carries the leg: it seals a
// relayed op's arguments to the host that will run them, opens what its
// authority sealed for this machine, and names this daemon's key on a leg
// whose messages the identity does not sign (the tailnet). Secrets go
// nowhere sealing is not possible.
type Sealer interface {
	// SealTo seals plaintext for the daemon whose key id is peer and whose
	// certified seal key is key (Remote.SealKey of its request), bound to
	// purpose, for ttl. A peer with no key is a 403 saying it is too old.
	SealTo(peer string, key []byte, purpose string, ttl time.Duration, plaintext []byte) (string, error)
	// OpenFrom opens what the daemon with key id from sealed for this one,
	// bound to purpose. Every failure is the same 400.
	OpenFrom(from, purpose, sealed string) ([]byte, error)
	// SealIdentity is this daemon's identity public key (base64url) and its
	// certified seal key (the Zm-Seal-Key value); "", "", nil when sealing
	// is off (a test standing in for a build from before sealing); an error
	// when a key cannot be read (this machine's fault: never a reason to
	// send in the clear).
	SealIdentity() (idPub, cert string, err error)
	// SignSeal signs msg with this daemon's ed25519 identity key (the key
	// SealIdentity names): a body sealed over the tailnet carries it, so a
	// seal id is taken only from whoever holds its key.
	SignSeal(msg []byte) (string, error)
	// HomeSealed is the sealing mode the home pinned as fp last reported with
	// its seal key ("" when it never named one to this machine; kept across
	// restarts). An error when the record cannot be read (fail closed: no
	// secret goes in the clear; the error names the file and the forget step).
	HomeSealed(fp string) (mode string, err error)
	// MarkHomeSealed records that the home pinned as fp named key id keyID,
	// reporting mode.
	MarkHomeSealed(fp, keyID, mode string) error
	// ForgetHomeSealed is the operator's step after putting a home back to
	// a build from before sealing; false when fp was not recorded. On a
	// record that cannot be read it starts a new, empty one.
	ForgetHomeSealed(fp string) (bool, error)
}

// SealingService is the service (Provide) that answers the sealing mode
// this machine reports beside its seal key on the tailnet: "required" when
// any pool it serves requires sealing, else "preferred" (the run-target
// module provides it).
const SealingService = "sealing"

// SealingReporter is SealingService's type.
type SealingReporter interface {
	ReportedSealing(ctx context.Context) string
}

// JoinTarget is a home as a join code names it, and as a remote records it.
type JoinTarget struct {
	Via         string
	URL         string // the base URL the member calls
	Authority   string // the home's machine name, when the code names it
	Fingerprint string // what the member pins (a node's or a key's)
	Secret      string // the code's secret (never stored)
	Code        string // the code as given (never stored)
}

// InviteTarget is what a new invite names (project.InviteInput).
type InviteTarget struct {
	Authority   string
	Host        string // the home's full name to dial when it has one (the tailnet: its MagicDNS name, zm-my5)
	Node        string // the home's tailnet node name (potluck-<machine>, an older node's zm-<machine>); "" elsewhere
	Fingerprint string
	URL         string
}

// ClientOptions are the team module's for one remote.
type ClientOptions struct {
	DialTimeout time.Duration // connecting: an offline home must not stall merged lists
	Extra       http.Header   // headers for every call (Extra)
	// Peer, when set, is told who owns the pinned home behind each new
	// connection, where the transport knows (the tailnet: the node's owner
	// login), so a remote joined by GitHub can warn when it changes.
	Peer func(fingerprint, owner string)
}

// ErrPinMismatch: whatever answered is not the home the join code pinned.
var ErrPinMismatch = errors.New("the home answering is not the one this join code pinned (fingerprint mismatch)")

// transports is the registry (Deps.AddTransport, Deps.Transport).
type transports struct {
	mu   sync.RWMutex
	list []TeamTransport
}

// AddTransport registers a transport driver (the wiring, once each, in
// the order an invite without a via tries them).
func (d *Deps) AddTransport(t TeamTransport) {
	d.tr.mu.Lock()
	defer d.tr.mu.Unlock()
	for _, x := range d.tr.list {
		if x.Via() == t.Via() {
			panic("core: transport " + t.Via() + " registered twice")
		}
	}
	d.tr.list = append(d.tr.list, t)
}

// Transport is the driver for via ("" is the tailnet), or nil.
func (d *Deps) Transport(via string) TeamTransport {
	if via == "" {
		via = ViaTailnet
	}
	d.tr.mu.RLock()
	defer d.tr.mu.RUnlock()
	for _, t := range d.tr.list {
		if t.Via() == via {
			return t
		}
	}
	return nil
}

// Transports is every registered driver, in registration order.
func (d *Deps) Transports() []TeamTransport {
	d.tr.mu.RLock()
	defer d.tr.mu.RUnlock()
	return append([]TeamTransport(nil), d.tr.list...)
}

// TransportVia is the transport a remote request came over.
func (r *Remote) TransportVia() string {
	if r.Via == "" {
		return ViaTailnet
	}
	return r.Via
}
