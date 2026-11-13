package core

import (
	"context"

	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/project"
)

type (
	subjectKey struct{}
	authErrKey struct{}
	remoteKey  struct{}
	localKey   struct{}
	resolveKey struct{}
	teamKey    struct{}
	labelKey   struct{}
	peerPIDKey struct{}
)

// WithPeerPID records kernel-provided Unix peer credentials, never request data.
func WithPeerPID(ctx context.Context, pid int) context.Context {
	return context.WithValue(ctx, peerPIDKey{}, pid)
}
func PeerPIDOf(ctx context.Context) int { pid, _ := ctx.Value(peerPIDKey{}).(int); return pid }

func withTeamSubject(ctx context.Context, s *authz.Subject) context.Context {
	return context.WithValue(ctx, teamKey{}, s)
}

// TeamSubjectOf is the caller when it authenticated with a node-bound token
// (a team replica), even when the request then failed (409 rescope): the
// server puts the project header on every answer to it.
func TeamSubjectOf(ctx context.Context) *authz.Subject {
	s, _ := ctx.Value(teamKey{}).(*authz.Subject)
	return s
}

// WithSubject returns ctx carrying the authenticated caller.
func WithSubject(ctx context.Context, s *authz.Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, s)
}

// SubjectOf is the authenticated caller. Inside any route that is not Public
// it is never nil (the router refuses unauthenticated requests first).
func SubjectOf(ctx context.Context) *authz.Subject {
	s, _ := ctx.Value(subjectKey{}).(*authz.Subject)
	return s
}

func withAuthErr(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, authErrKey{}, err)
}

func authErrOf(ctx context.Context) error {
	e, _ := ctx.Value(authErrKey{}).(error)
	return e
}

// AuthErr is why Authenticate found no subject for ctx's request, or nil
// (the tunnel's door refuses an unauthenticated token before any route,
// public ones included).
func AuthErr(ctx context.Context) error { return authErrOf(ctx) }

// WithLocal marks ctx as arriving on the Unix socket. Only the daemon's Unix
// listener sets it (http.Server.ConnContext on a *net.UnixConn), plus
// in-process test clients via server.Daemon.SocketHandler. A request without
// this mark is a network request, whatever else its context says: it must
// present a token or a mapped WhoIs login, else 401 (fail closed).
func WithLocal(ctx context.Context) context.Context {
	return context.WithValue(ctx, localKey{}, true)
}

// IsLocal reports whether ctx came from the Unix socket. WithRemote wins:
// a context carrying both is remote.
func IsLocal(ctx context.Context) bool {
	l, _ := ctx.Value(localKey{}).(bool)
	return l && RemoteOf(ctx) == nil
}

// Remote carries what a network listener knows about its caller (the
// tailnet module sets it before calling d.Handler). It only carries the
// WhoIs login and node: being remote is the default for any request without
// WithLocal, so forgetting WithRemote can only lose the WhoIs identity,
// never grant owner rights. Login and Node may be empty.
//
// NodeID and NodeKey identify the calling device itself (WhoIs stable node
// ID and node public key). Only Device routes (approvals, SPEC section 16)
// use them: the decide right binds to a node, never to a login, which every
// device of one person (and every agent on the Mac) shares.
type Remote struct {
	Login   string // tailnet user login, e.g. victor@example.com
	Node    string // tailnet node name
	NodeID  string // stable node ID, e.g. nXXXXEXMPL
	NodeKey string // node public key, e.g. nodekey:abcd...
	// Tagged is true for tagged nodes (servers, CI): never a person's device.
	Tagged bool
	// Self is true when the caller is this daemon's own tailnet node or comes
	// from this machine (its Tailscale address is one of the host's).
	Self bool
	// Zm is true when the node is (or names itself as) a zm daemon: its
	// own hostname or its tailnet name starts with zm-. Such nodes run
	// agents, so approvals refuse to pair them.
	Zm bool
	// OS is the node's self-reported OS ("iOS", "android", ...); SameLogin is
	// true when it signed in to the tailnet as the login this daemon's own
	// node did. Approvals' one-tap phone pairing reads both (SPEC 16).
	OS        string
	SameLogin bool
	// Via is "" for the tailnet (WhoIs filled the fields above) and
	// ViaTunnel for the tunnel listener (SPEC 17.11): NodeID is then the key
	// id that signed the request, and Addr the client's address as the
	// tunnel reported it (rate limits only).
	Via  string
	Addr string
	// SealKey is the caller's seal key (SPEC 17.11 "Sealed secrets"): the
	// X25519 public key its Zm-Seal-Key certified. Over the tunnel the door
	// checks it against the key that signed the request; over the tailnet
	// against the identity key the request names (Zm-Seal-Id), the pinned
	// node vouching for both. Nil when the caller sent none (a Potluck or zm
	// from before sealing on that leg).
	SealKey []byte
	// SealID is the key id SealKey is certified under (sealbox.KeyID):
	// NodeID over the tunnel, the Zm-Seal-Id key's over the tailnet.
	SealID string
}

// ViaTunnel marks a Remote that came through the tunnel listener.
const ViaTunnel = "tunnel"

// WithRemote attaches the network caller's WhoIs info (and overrides WithLocal).
func WithRemote(ctx context.Context, r Remote) context.Context {
	return context.WithValue(ctx, remoteKey{}, &r)
}

// RemoteOf returns the WhoIs info a network listener attached, or nil. A nil
// result does NOT mean the socket; use IsLocal.
func RemoteOf(ctx context.Context) *Remote {
	r, _ := ctx.Value(remoteKey{}).(*Remote)
	return r
}

// WithReadProgress attaches f, called whenever a transport reads bytes of
// an answer before handing it over (the tunnel's client reads each answer
// whole to check its signature; a hedge's silence timer must still see it
// move).
func WithReadProgress(ctx context.Context, f func()) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// ReadProgress is the func WithReadProgress attached, or nil.
func ReadProgress(ctx context.Context) func() {
	f, _ := ctx.Value(progressKey{}).(func())
	return f
}

type progressKey struct{}

// WithResolveInput stores what the request said about its project.
func WithResolveInput(ctx context.Context, in project.ResolveInput) context.Context {
	return context.WithValue(ctx, resolveKey{}, in)
}

// ResolveInputOf returns what the request said about its project.
func ResolveInputOf(ctx context.Context) project.ResolveInput {
	in, _ := ctx.Value(resolveKey{}).(project.ResolveInput)
	return in
}

// WithClaimLabel carries a claim door's routing label (bd's actor) through
// the work module to the native adapter's claim, in process, so the claim is
// compared and written under the label. A team project's write-back carries
// it too: if that lands first, the door's claim finds it already made.
func WithClaimLabel(ctx context.Context, label string) context.Context {
	if label == "" {
		return ctx
	}
	return context.WithValue(ctx, labelKey{}, label)
}

// ClaimLabelOf is the label WithClaimLabel set, or "".
func ClaimLabelOf(ctx context.Context) string {
	v, _ := ctx.Value(labelKey{}).(string)
	return v
}

// OverTunnel reports whether a request must be treated as having come over
// a tunnel, whose provider reads answers: it is, unless it came on this
// machine's socket or from a tailnet node (a Remote WhoIs filled: no Via, a
// node id). Anything else, an unknown transport included, counts as a
// tunnel: what is withheld from one is withheld from it.
func OverTunnel(ctx context.Context) bool {
	if IsLocal(ctx) {
		return false
	}
	rm := RemoteOf(ctx)
	return rm == nil || rm.Via != "" || rm.NodeID == ""
}
