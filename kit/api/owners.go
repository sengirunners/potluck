package api

// Owned by the owners brick (SPEC 22): GitHub logins run by the authority,
// and identity links.

import (
	"slices"
	"strings"
	"time"
)

// GitHubLoginRequest is POST /v0/login/github/start: start a GitHub login that
// this authority runs (GitHub's device flow with its own client id).
type GitHubLoginRequest struct {
	Purpose string `json:"purpose,omitempty"` // "link" (the default), "bind", "key" or "invite" (a replica's node-bound token, right after redemption: zm join --github)
	// A bind (zm names bind): @Owner/Name for Project. A key login: the
	// project the key answers in (or ?project= on the route).
	Project string `json:"project,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Name    string `json:"name,omitempty"`
	// A key login: the signing key to bind and its proof.
	KeyAttach
}

// KeyAttach is a key login's payload (purpose "key"). The authority's
// owners brick carries it unread to core.Deps.KeyLogin, which checks it.
type KeyAttach struct {
	Key     string `json:"key,omitempty"`     // the public key, X9.63, base64
	Device  string `json:"device,omitempty"`  // a label for people
	Install string `json:"install,omitempty"` // the client's install id, shown only
	Nonce   string `json:"nonce,omitempty"`   // single use
	Time    string `json:"time,omitempty"`    // when Proof was made, RFC 3339, as signed
	Proof   string `json:"proof,omitempty"`   // the key's signature over the bind statement
}

// KeyBinding is a key login's outcome: the approver the key now is.
// Account, Login and Machine are the authority's: GitHub's answer and the
// node the login came from.
type KeyBinding struct {
	Approver    string `json:"approver"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	ReceiptKey  string `json:"receipt_key"`
	Account     string `json:"account"` // "github:<numeric id>"
	Login       string `json:"login"`
	Machine     string `json:"machine"`
}

// GitHubLoginStart is what the person needs to approve, who is asking,
// and the poll secret: the only copy, for collecting the outcome once.
type GitHubLoginStart struct {
	ID              string    `json:"id"`
	Secret          string    `json:"secret"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	Expires         time.Time `json:"expires"`
	Authority       string    `json:"authority"` // the machine running the login
	For             string    `json:"for"`       // who it signs in: "identity judy", "node host-b (n...)"
}

// GitHubLoginPoll is POST /v0/login/github/{id}: wait up to WaitMS (at
// most 30 s) for the outcome.
type GitHubLoginPoll struct {
	Secret string `json:"secret"`
	WaitMS int    `json:"wait_ms,omitempty"`
}

// GitHubLoginOutcome is a login's outcome: Pending while the person has
// not approved yet; else what the login did (answered once).
type GitHubLoginOutcome struct {
	Pending bool            `json:"pending,omitempty"`
	Link    *GitHubIdentity `json:"link,omitempty"`
	// Join is a GitHub join's admission: the member token for the node
	// that started it, as invite redemption answers it.
	Join *RedeemResponse `json:"join,omitempty"`
	// Bind is a bind's new (or current again) @owner/name.
	Bind *NameBinding `json:"bind,omitempty"`
	// Key is a key login's bound key.
	Key *KeyBinding `json:"key,omitempty"`
}

// GitHubGrantRequest is POST /v0/grants/github (and the body of DELETE):
// a role in a project or area for a GitHub user or org, by login. The
// login is resolved to its numeric id when the grant is made; an org's
// members qualify, checked live at join.
type GitHubGrantRequest struct {
	Owner   string `json:"owner"` // "@alice" or "@acme"
	Role    string `json:"role,omitempty"`
	Project string `json:"project"`
	Area    string `json:"area,omitempty"`
}

// GitHubGrant is a role granted to a GitHub account.
type GitHubGrant struct {
	Account string    `json:"account,omitempty"` // "github:<numeric id>"; empty while Queued
	Login   string    `json:"login"`             // when granted, or last seen
	Kind    string    `json:"kind,omitempty"`    // user | org; empty while Queued
	Role    string    `json:"role"`
	Project string    `json:"project"` // slug
	Area    string    `json:"area,omitempty"`
	Created time.Time `json:"created"`
	// Queued: GitHub's rate limit refused the lookup of the login, so the
	// grant is held and finishes by itself once the limit resets (RetryAt;
	// the granter is mailed how it ended). Account is empty until it is made.
	Queued  bool       `json:"queued,omitempty"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
	// Ceilings are the owners' ceilings in the project on the identities
	// this account (a user grant) is or was linked to: its joins get no
	// more than they allow.
	Ceilings []Ceiling `json:"ceilings,omitempty"`
}

// GitHubGrantList is GET /v0/grants/github.
type GitHubGrantList struct {
	Grants []GitHubGrant `json:"grants"`
	// Queued are grants held for GitHub's rate limit (GitHubGrant.Queued).
	Queued []GitHubGrant `json:"queued,omitempty"`
}

// GitHubAuthority is GET /v0/join/github: "a zm authority", nothing about
// its projects (no project input, no list).
type GitHubAuthority struct {
	Authority bool   `json:"authority"`
	Protocol  string `json:"protocol"`
	Machine   string `json:"machine"`
	GitHub    bool   `json:"github"` // it runs GitHub logins (github.client_id is set)
}

// GitHubJoinRequest is POST /v0/join/github: start a GitHub login that
// joins this node to the project (no code).
type GitHubJoinRequest struct {
	Project string `json:"project"`
	Machine string `json:"machine"`
	// Area and Role scope the admission (SPEC 22): exactly that area, at
	// most that role, when the account's GitHub grants allow it. Over the
	// tunnel Area is required and the join is guest only.
	Area string `json:"area,omitempty"`
	Role string `json:"role,omitempty"`
}

// OwnerStatus is GET /v0/owner?project=: whether the caller's identity owns
// or administers the project (it answers card asks there, POST
// /v0/asks/{id}/choose), and the GitHub login it is linked to ("" when none).
type OwnerStatus struct {
	Project  string `json:"project"` // slug
	Identity string `json:"identity"`
	Owner    bool   `json:"owner"`
	GitHub   string `json:"github,omitempty"`
}

// GitHubIdentity is an identity's link to its GitHub account.
type GitHubIdentity struct {
	Identity string    `json:"identity"` // the zm identity's name
	Account  string    `json:"account"`  // "github:<numeric id>", never the login
	Login    string    `json:"login"`    // the login when last verified (display only)
	Linked   time.Time `json:"linked"`
}

// GitHubUnlink is DELETE /v0/identities/{identity}/github[?revoke=1].
type GitHubUnlink struct {
	Identity string   `json:"identity"`
	Account  string   `json:"account"`
	Login    string   `json:"login"`
	Revoked  []string `json:"revoked"` // node-bound token ids revoked (revoke=1)
}

// GitHubRevoke is DELETE /v0/grants/github: the grant is gone, and so is
// what it gave (Admissions: the joins whose grants and node-bound tokens
// were revoked with it).
type GitHubRevoke struct {
	Revoked    bool `json:"revoked"`
	Admissions int  `json:"admissions"`
}

// ReservedProjectNames are refused as a project name under every owner
// (owners-2026-11-01 Q3), as is any name starting "zm-".
var ReservedProjectNames = []string{"none", "all", "owners", "admin", "root", "system", "api", "www", "app", "web",
	"help", "support", "security", "abuse", "status", "settings", "new", "explore", "search", "login", "logout",
	"signup", "join", "invite"}

// ValidProjectName reports whether s may name a project under an owner
// (@owner/<s>): a slug of at least 2 characters, no blocked word, not
// reserved.
func ValidProjectName(s string) bool {
	return len(s) >= 2 && ValidSlug(s) && !HasBlockedWord(s) && !strings.HasPrefix(s, "zm-") &&
		!slices.Contains(ReservedProjectNames, s)
}

// NameBinding is one @owner/name bound to a project. Bindings are never
// moved or removed; a rename or transfer adds one and makes it current.
type NameBinding struct {
	Path    string    `json:"path"`    // "@<owner's login when last seen>/<name>"
	Account string    `json:"account"` // the owner: "github:<numeric id>"
	Name    string    `json:"name"`
	Project string    `json:"project"` // the project's ULID
	Current bool      `json:"current"`
	Bound   time.Time `json:"bound"`
}

// NameBindingList is GET /v0/projects/{project}/names.
type NameBindingList struct {
	Names []NameBinding `json:"names"`
}

// NameResolution is GET /v0/names/{owner}/{name}: the project the name
// reaches, and its current @owner/name.
type NameResolution struct {
	Project string `json:"project,omitempty"` // the project's ULID
	Slug    string `json:"slug,omitempty"`    // its slug on this authority
	Path    string `json:"path,omitempty"`    // its current @owner/name
	Moved   bool   `json:"moved,omitempty"`   // the name asked is not the current one
	// Frozen: GitHub no longer answers for the owner's account (suspended
	// or deleted): the name resolves read-only, and nothing is bound,
	// renamed or transferred under it.
	Frozen bool `json:"frozen,omitempty"`
}
