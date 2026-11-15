package core

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// KeyLogin binds a signing key to a person through a GitHub login of
// purpose "key" (Deps.KeyLogin). The owners brick runs the login and
// carries the key payload without reading it; the brick that keeps
// approvers implements this. Nil: the authority answers purpose "key" as
// an unknown purpose.
type KeyLogin interface {
	// Check runs when the login starts, before anything is reserved: the
	// payload's proof and nonce, that the identity answers the project, and
	// where the call came from (hdr is the request's headers; nil when the
	// start did not come over HTTP). An error refuses the start.
	Check(ctx context.Context, sub *authz.Subject, hdr http.Header, start KeyStart) error
	// Bind runs inside the registry transaction that links the identity to
	// the approving account, after the link; an error rolls both back. It
	// re-checks what Check checked that can change while the person
	// approves (the identity still answers the project), and ends the
	// identity's earlier key from the same node, so the last bind to
	// commit is the one that answers. An error coded
	// unavailable or internal leaves the outcome collectable again; any
	// other error spends it.
	Bind(ctx context.Context, tx *sql.Tx, b KeyBind) (api.KeyBinding, error)
}

// KeyStart is a key login as the authority sees it when it starts.
type KeyStart struct {
	Project api.ProjectRef // resolved in the starter's scope
	// Node is where the key will answer from: the node-bound token's
	// tailnet node, or KeyNodeLocal for the authority's own socket owner.
	// Machine is that node's machine name (the authority's own for local).
	Node    string
	Machine string
	Key     api.KeyAttach // as the client sent it, unread
}

// KeyNodeLocal is KeyStart.Node for a key login started on the
// authority's own socket.
const KeyNodeLocal = "local"

// KeyBind is a collected key login: who started it and the provider
// account GitHub says approved it (the authority's view, never the
// client's).
type KeyBind struct {
	KeyStart
	Identity api.Identity
	Account  string // "github:<numeric id>"
	Login    string
}
