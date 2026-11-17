package computeapi

import (
	"encoding/json"

	"github.com/sengirunners/potluck/jobrule"
)

// Secret grants (docs/daemon.md, "Secret grants"): the policy table on a
// pool's authority that says who may put which kind of secret, up to what
// ceiling, into which boxes. The grants package implements it.

// Secret kinds. KindValue is a value the caller holds; the others are
// credentials the authority mints per box through an issuer.
const (
	SecretKindValue        = "value"
	SecretKindGitHubToken  = "github-token"
	SecretKindPotluckToken = "potluck-token"
	SecretKindGitHubJIT    = "github-jit"
)

// Secret scopes: how long a secret lives in the box. SecretScopeExec is one
// command (exec or spawn: the command's own directory); SecretScopeBox is
// the box's life (a fixed directory every later command sees).
const (
	SecretScopeExec = "exec"
	SecretScopeBox  = "box"
)

// The built-in grants' ids. GrantDefaultID keeps renter secrets on exec and
// spawn working as before grants existed; GrantDefaultJITID keeps a
// caller's jit_config on a box acquire working the same way. One switch
// (POST /v0/secret_grants/default) turns both off or on.
const (
	GrantDefaultID    = "default"
	GrantDefaultJITID = "default-jit"
)

// SecretNameJIT is the name a jit_config goes by in grants and the ledger.
const SecretNameJIT = "JIT_CONFIG"

// GrantTo says who may use a grant. Exactly one field is set.
type GrantTo struct {
	// Identity is an identity's name; IdentityID is its ULID, resolved when
	// the grant is added (a rename keeps the grant on the same identity).
	Identity   string `json:"identity,omitempty"`
	IdentityID string `json:"identity_id,omitempty"`
	// Role is "member" or "owner": anyone holding at least that role on
	// the box's area.
	Role string `json:"role,omitempty"`
	// Job is a GitHub job the runner broker serves, matched on GitHub's own
	// answers about the run.
	Job *GrantJob `json:"job,omitempty"`
}

// GrantJob is a job subject: the run's repository, workflow file and ref
// must be these and its event one of Events (jobrule's rules: push and
// schedule by default; a dispatch event only when listed).
type GrantJob struct {
	Repo     string   `json:"repo"`
	Workflow string   `json:"workflow"`
	Ref      string   `json:"ref"`
	Events   []string `json:"events"`
}

// SecretGrant is one row of the table (GET /v0/secret_grants).
type SecretGrant struct {
	ID string `json:"id"`
	// Area is the pool area whose boxes it covers ("" = every area). Only
	// an admin of that area adds or removes it.
	Area string  `json:"area,omitempty"`
	To   GrantTo `json:"to"`
	Kind string  `json:"kind"`
	// Name is the file name in the box, [A-Z][A-Z0-9_]{0,63}, or "*" (any
	// name: kind value only).
	Name string `json:"name"`
	// Ceiling is the most one request may ask, by kind: for value
	// {"max_bytes": n}; for a minted kind, what its issuer reads.
	Ceiling   json.RawMessage `json:"ceiling,omitempty"`
	HostClass string          `json:"host_class"`
	Scopes    []string        `json:"scopes"`
	// Targets narrows it to these run targets (box bindings, as listed);
	// empty = every box of the area.
	Targets   []string `json:"targets,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	Created   string   `json:"created,omitempty"`
	// Builtin marks a default grant; Off says the pool's owner turned the
	// defaults off.
	Builtin bool `json:"builtin,omitempty"`
	Off     bool `json:"off,omitempty"`
	// Corrupt: the stored row does not decode. Stale: its identity left the
	// pool after it was made (coming back does not revive it). Either
	// matches nothing; an admin removes it.
	Corrupt bool `json:"corrupt,omitempty"`
	Stale   bool `json:"stale,omitempty"`
}

// AddSecretGrantRequest is POST /v0/secret_grants. HostClass defaults to
// trusted, Scopes to ["exec"], a value grant's ceiling to 64 KiB.
type AddSecretGrantRequest struct {
	Area      string          `json:"area,omitempty"`
	To        GrantTo         `json:"to"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Ceiling   json.RawMessage `json:"ceiling,omitempty"`
	HostClass string          `json:"host_class,omitempty"`
	Scopes    []string        `json:"scopes,omitempty"`
	Targets   []string        `json:"targets,omitempty"`
}

// SecretGrantDefaultRequest is POST /v0/secret_grants/default.
type SecretGrantDefaultRequest struct {
	On bool `json:"on"`
}

// SecretGrantList is GET /v0/secret_grants: the default first, then the
// rows in the areas the caller reads, oldest first. Over a tunnel the rows'
// names are withheld (Note says so): the tunnel's provider reads answers.
type SecretGrantList struct {
	Project string        `json:"project"`
	Grants  []SecretGrant `json:"grants"`
	Note    string        `json:"note,omitempty"`
}

// SecretNamesWithheld is the note an answer over a tunnel carries in place
// of secret names.
const SecretNamesWithheld = "secret names are shown on the pool's machine or over the tailnet, never over a tunnel"

// SecretEventWithheld are the keys of a secret.* event a reader over a
// tunnel does not receive; SecretGrantWithheld a secret_grant.* event's.
var (
	SecretEventWithheld = []string{"names", "sizes", "grants"}
	SecretGrantWithheld = []string{"name"}
)

// Ledger events. secret.* are per request: granted when a request passed
// the grant check and its secrets were handed to the transport, delivered
// when the call that carried them answered, refused when the check said no.
// For a kind an issuer mints, the issuer writes granted, delivered, renewed
// and revoked; the check writes only refusals. secret_grant.* are edits of
// the table. None ever carries a value. secret.* need operate on the box's
// area to read, secret_grant.* admin.
const (
	EventSecretGranted   = "dev.zm.secret.granted"
	EventSecretDelivered = "dev.zm.secret.delivered"
	EventSecretRefused   = "dev.zm.secret.refused"
	EventSecretRenewed   = "dev.zm.secret.renewed"
	EventSecretRevoked   = "dev.zm.secret.revoked"

	EventSecretGrantAdded   = "dev.zm.secret_grant.added"   // data SecretGrant
	EventSecretGrantRemoved = "dev.zm.secret_grant.removed" // data SecretGrant
	EventSecretGrantDefault = "dev.zm.secret_grant.default" // data SecretGrant (the default, Off set)
)

// SecretEvent is a secret.* event's data: names, sizes and policy, never a
// value. Grants maps each name to the grant that allowed it.
type SecretEvent struct {
	By string `json:"by"`
	// Identity is who asked, by identity id; Job the GitHub job, as GitHub
	// answered about its run.
	Identity string       `json:"identity,omitempty"`
	Job      *jobrule.Job `json:"job,omitempty"`
	Kind     string       `json:"kind"`
	Scope    string       `json:"scope"`
	Verb     string       `json:"verb,omitempty"`
	Names    []string     `json:"names"`
	// Count is how many names: what a reader over a tunnel gets in place
	// of the names, sizes and grants (withheld there).
	Count     int               `json:"count,omitempty"`
	Sizes     map[string]int    `json:"sizes,omitempty"`
	Grants    map[string]string `json:"grants,omitempty"`
	Target    string            `json:"target"`
	Box       string            `json:"box,omitempty"`
	Host      string            `json:"host,omitempty"`
	HostClass string            `json:"host_class"`
	Params    json.RawMessage   `json:"params,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	// A minted credential's (an issuer writes them): the issuer, the grant
	// it was minted under, its expiry, GitHub's audit-log hashed_token
	// (base64 SHA-256), and whether it held elevated permissions.
	Issuer      string   `json:"issuer,omitempty"`
	Grant       string   `json:"grant,omitempty"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
	HashedToken string   `json:"hashed_token,omitempty"`
	Elevated    []string `json:"elevated,omitempty"`
	// TokenProject and TokenArea are where a minted potluck-token acts: the
	// box's project (the event's own) and area, or for a GitHub job under a
	// rule naming another project, that project's ULID and the rule's area.
	TokenProject string `json:"token_project,omitempty"`
	TokenArea    string `json:"token_area,omitempty"`
	// Why is a refusal's cause, in words that name no secret.
	Why string `json:"why,omitempty"`
	// Failed: the call that carried the secrets returned an error (they may
	// or may not have reached the box).
	Failed bool `json:"failed,omitempty"`
}
