// Package issuers holds the secrets door's minted kinds (docs/daemon.md
// "Secrets issuers"): credentials the authority mints for one box, cut down to
// what the request names and inside the issuer's ceiling, renewed while the box
// lives and revoked at the issuer on every way the box ends. The ledger records
// who got what, never a value.
//
// Minted is one issuer: the lifecycle every minted kind shares (admission,
// holding, renewal, revocation on every end, the ledger, restarts) over a
// backend that holds the kind's own part (what it checks, how it mints and
// revokes at its source, its grant ceilings). The github-token kind is an
// installation token of a GitHub App for exactly one repository and the
// requested permissions (github.go).
//
// The policy (grants) and the way a value reaches a box (sealed transport, the
// guest's landing) are other packages'. This one takes them as interfaces:
// Deliverer, ClassOf and Ledger. The grants check's side of a kind
// (grants.Issuer: Kind, Ceiling, Within) is Minted's too; Register hooks an
// issuer to the grants package (grants.go).
package issuers

import (
	"context"
	"regexp"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/secrets"
)

// KindGitHubToken is the credential kind a github-app issuer mints.
const KindGitHubToken = computeapi.SecretKindGitHubToken

// Issuer is a minted kind. The lifecycle owner calls Issue when a box is
// granted a credential, End on every way the box ends, Tick on a schedule
// (renewal and revocation retries), Start once at authority start and Close
// at authority stop.
type Issuer interface {
	Kind() string
	// Issue checks req, mints, records and delivers through the Deliverer. It
	// answers what was issued, never the value.
	Issue(ctx context.Context, req Request) (Issued, error)
	// End revokes every credential issued for box, at the issuer.
	End(ctx context.Context, box Box, why EndReason) error
	// EndGrant revokes every credential issued under grant id (the grant was
	// deleted).
	EndGrant(ctx context.Context, id string) error
	// Tick renews what is due, revokes renewed-away credentials after their
	// grace and retries failed revocations; it answers when it is next due.
	Tick(ctx context.Context) time.Time
	// Sweep ends every box live does not answer true for.
	Sweep(ctx context.Context, live func(Box) bool)
	// EndUntrusted ends every box (held or being issued) whose machine
	// ClassOf no longer answers trusted for: the owner calls it as soon as a
	// machine's class changes, and on a schedule.
	EndUntrusted(ctx context.Context)
	// Start settles what a previous run issued and could not revoke, and
	// issues again to every box still assigned one that its owner says lives.
	Start(ctx context.Context) error
	// Close revokes everything still held and refuses every later Issue. It
	// keeps the assignments, so Start issues again after a restart.
	Close(ctx context.Context) error
}

// Deliverer puts a credential into a box under name, replacing what is there
// (the first value and each renewal). The transport and landing lanes
// implement it; this package never writes into a box itself.
type Deliverer interface {
	Deliver(ctx context.Context, box Box, name string, value secrets.Value) error
}

// DeliverFunc adapts a function to Deliverer.
type DeliverFunc func(ctx context.Context, box Box, name string, value secrets.Value) error

func (f DeliverFunc) Deliver(ctx context.Context, box Box, name string, v secrets.Value) error {
	return f(ctx, box, name, v)
}

// ClassOf answers the class of the machine that runs box: ClassTrusted or
// anything else. It is asked at every issue and every renewal, never taken
// from the request. The daemon passes the run-target module's host class,
// the source the grants check uses too.
type ClassOf func(ctx context.Context, box Box) (string, error)

// Live answers whether box still exists, as its owner (the run-target
// module) knows it. Start asks it for every assignment kept across a restart.
type Live func(ctx context.Context, box Box) (bool, error)

// JobHolds answers whether the GitHub job req's subject names still holds
// req.Box, as the runner broker and GitHub answer now: the broker's record
// of the box (a job that completed is recorded so, across restarts) and
// GitHub's job (in progress, on the box's runner). It is asked at every
// renewal and every re-issue after a restart of a job's credential, never
// at its first issue (the broker asks that itself, just before). The daemon
// passes the broker's (ProvideJobHolds); without one a job's credential is
// never renewed or issued again.
type JobHolds func(ctx context.Context, req Request) (bool, error)

// JobBox answers whether req.Box is the runner broker's box for the GitHub
// job req's subject names, as the broker records it now: a box of its pool
// project, in the area of the link of the installation that serves the
// job's repository, guarded to the job's run. A potluck-token rule naming
// another project serves a job only on such a box, and it is asked at every
// issue, renewal and re-issue of one. The daemon passes the broker's
// (ProvideJobBox); without one no such rule serves anything.
type JobBox func(ctx context.Context, req Request) (bool, error)

// Check is the grant check (GrantCheck: the grants package), required. It
// is asked at every issue, renewal and re-issue after a restart, and its
// Decision names the grant the credential is issued under (Request.Grant is
// always set from it, never taken from the caller), so removing that grant
// ends the credential (EndGrant).
type Check func(ctx context.Context, req Request) (grants.Decision, error)

// ClassTrusted is the one class a credential goes to (docs/daemon.md "Machine
// class").
const ClassTrusted = api.MachineTrusted

// Box is where a credential lands: a box (its handle) of a project (its
// ULID) and area, on a host ("" this machine).
type Box struct {
	Project string `json:"project"`
	Area    string `json:"area,omitempty"`
	ID      string `json:"id"`
	Host    string `json:"host,omitempty"`
}

func (b Box) key() string { return b.Project + "/" + b.ID }

// Subject is who the credential is for: an identity (a person or a
// controller), by its id, or a GitHub job the broker serves. Exactly one is
// set.
type Subject struct {
	IdentityID string `json:"identity_id,omitempty"`
	Job        *Job   `json:"job,omitempty"`
}

// Job is a GitHub Actions job as GitHub answered it; its rules are
// jobrule's, shared with the grants check.
type Job = jobrule.Job

// Request asks for one credential for one box. Repo and Permissions are
// github-token's params, Role potluck-token's: the same schema a grant's
// ceiling is read with (ParamsFor).
type Request struct {
	Kind         string            `json:"kind,omitempty"` // KindGitHubToken or KindPotluckToken; set by Issue for the issuer's own
	Name         string            `json:"name"`           // the file name in the box, [A-Z][A-Z0-9_]{0,63}
	Subject      Subject           `json:"subject"`
	Box          Box               `json:"box"`
	Grant        string            `json:"grant,omitempty"`        // set by Issue from Check's Decision; a caller's is ignored
	Repo         string            `json:"repo"`                   // owner/name
	Permissions  map[string]string `json:"permissions"`            // permission -> read|write; metadata read is added
	Installation int64             `json:"installation,omitempty"` // the App's installation on Repo's account; 0 asks GitHub
	Reason       string            `json:"reason,omitempty"`       // required for an elevated permission at write
	Role         string            `json:"role,omitempty"`         // potluck-token: guest or member
	// Caller is who asks for an identity's credential, as authenticated
	// (its token's cap included): the grant check judges it, never the
	// identity's whole rights. Kept in memory only: an identity's assignment
	// re-issued after a restart has none, and the grant check refuses it.
	Caller *authz.Subject `json:"-"`
}

// Issued is what Issue answers: everything but the value.
type Issued struct {
	Name        string
	Repo        string
	Permissions map[string]string
	ExpiresAt   time.Time
	HashedToken string // as GitHub's audit log names it (ghapp.Token.HashedToken)
}

// EndReason is why a box's credentials are revoked.
type EndReason string

// The ways a box ends, and the other reasons to revoke.
const (
	EndReleased     EndReason = "released"
	EndExpired      EndReason = "expired"
	EndReclaimed    EndReason = "reclaimed"
	EndLost         EndReason = "lost"
	EndHostDemoted  EndReason = "host_demoted"
	EndSwapUnsafe   EndReason = "swap_unsafe" // the box's host swaps where a secret could reach a disk (renewal)
	EndJobCompleted EndReason = "job_completed"
	EndJobTaken     EndReason = "job_taken"
	EndGone         EndReason = "gone"     // a sweep found no such box
	EndShutdown     EndReason = "shutdown" // the authority stops
	EndGrant        EndReason = "grant_deleted"
)

var secretNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// Publisher is the pool's event log (*events.Bus).
type Publisher interface {
	Publish(ctx context.Context, in events.Input) (api.Event, error)
}

// Event types this package publishes (computeapi's).
const (
	EventSecretRenewed = computeapi.EventSecretRenewed
	EventSecretRevoked = computeapi.EventSecretRevoked
	EventSecretRefused = computeapi.EventSecretRefused
)

// SecretEvent is a published event's data: policy and the token's hash,
// never a value.
type SecretEvent = computeapi.SecretEvent
