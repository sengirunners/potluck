package api

import (
	"encoding/json"
	"time"
)

// Projects, areas, identities, grants and tokens (the shared core; owned by
// the integrator). See SPEC.md "Projects and areas" and "Who is calling".

// ProjectNone is the project of things that belong to no project: shell
// sessions launched outside any project, daemon events. Only the machine's
// owner (the local OS user) sees them.
const ProjectNone = "none"

// Roles (the relation in a grant tuple), weakest first.
const (
	RoleGuest  = "guest"
	RoleMember = "member"
	RoleOwner  = "owner"
)

// ProjectRef names a project: the ULID is stored, the slug is shown.
type ProjectRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// RepoRules match a working directory or a git repository to a project or area.
// Paths are absolute (a leading ~ is expanded); a directory matches itself and
// everything under it, longest match wins. Remotes are normalised
// "host/owner/repo" (any of https://, ssh://, git@host: forms match).
type RepoRules struct {
	Paths   []string `json:"paths,omitempty"`
	Remotes []string `json:"remotes,omitempty"`
}

// Area is a scope inside a project.
type Area struct {
	Name string `json:"name"`
	RepoRules
	// Work are id globs ("*" matches anything) that put adapter work items in
	// this area, e.g. "bd:sprout-*".
	Work []string `json:"work,omitempty"`
	// Labels put adapter work items that carry any of these tracker labels
	// in this area (after Work globs), e.g. "sprout". Whoever can label items
	// in the tracker can move them into or out of the area.
	Labels []string `json:"labels,omitempty"`
}

// Tracker binds a work adapter to a project: ids with Prefix in this project
// go to zm-work-<Adapter>, which gets Config with every request.
type Tracker struct {
	Adapter string            `json:"adapter"` // "native", or <name> -> zm-work-<name>
	Prefix  string            `json:"prefix"`  // "bd"
	Area    string            `json:"area,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
	// Env is "" (the adapter gets its allowlisted environment, SPEC 17.2)
	// or "inherit" (the daemon's whole environment; refused for a beads
	// --server binding).
	Env string `json:"env,omitempty"`
}

// TrackerEnvInherit opts a tracker out of the environment allowlist.
const TrackerEnvInherit = "inherit"

// Project is a top-level boundary. A caller sees only the areas (and repo
// rules) it may read; Role is the caller's strongest relation in it.
type Project struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	RepoRules
	Areas    []Area    `json:"areas,omitempty"`
	Trackers []Tracker `json:"trackers,omitempty"`
	// GuestIssues lets the project's guests write native issues in what they
	// may write (SPEC "Native issues": guests are read-only otherwise).
	GuestIssues bool      `json:"guest_issues,omitempty"`
	Role        string    `json:"role,omitempty"`
	Created     time.Time `json:"created"`
}

// Ref is the project's ProjectRef.
func (p Project) Ref() ProjectRef { return ProjectRef{ID: p.ID, Slug: p.Slug} }

// WorkSkipped is one tracker or authority left out of a list: a partial
// list never looks complete.
type WorkSkipped struct {
	Project string `json:"project"` // project ULID
	Source  string `json:"source"`  // tracker prefix ("bd")
	Error   string `json:"error"`   // sanitised for the caller
}

// ProjectList is GET /v0/projects.
type ProjectList struct {
	Projects []Project `json:"projects"`
	// Skipped names joined team projects whose authority did not answer
	// (a replica's list; SPEC 17.3): the list is partial.
	Skipped []WorkSkipped `json:"skipped,omitempty"`
}

// NewProjectRequest is POST /v0/projects.
type NewProjectRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
	RepoRules
}

// UpdateProjectRequest is PATCH /v0/projects/{project}. Nil fields are kept.
type UpdateProjectRequest struct {
	Slug     *string    `json:"slug,omitempty"`
	Name     *string    `json:"name,omitempty"`
	Repos    *RepoRules `json:"repos,omitempty"`
	Trackers *[]Tracker `json:"trackers,omitempty"`
	// GuestIssues: admin on the project (SPEC "Native issues").
	GuestIssues *bool `json:"guest_issues,omitempty"`
}

// AddAreaRequest is POST /v0/projects/{project}/areas.
type AddAreaRequest = Area

// AreaList is GET /v0/projects/{project}/areas.
type AreaList struct {
	Areas []Area `json:"areas"`
}

// UpdateAreaRequest is PATCH /v0/projects/{project}/areas/{area}. A nil
// field is kept; a present one replaces that rule list (empty clears it).
// The daemon applies it at once (no restart, no hand edit of project.toml).
type UpdateAreaRequest struct {
	Paths   *[]string `json:"paths,omitempty"`
	Remotes *[]string `json:"remotes,omitempty"`
	Work    *[]string `json:"work,omitempty"`
	Labels  *[]string `json:"labels,omitempty"`
}

// AreaView is GET /v0/projects/{project}/areas/{area} (and what PATCH
// answers): one area as the caller sees it.
type AreaView struct {
	Project ProjectRef `json:"project"`
	Area
	// Role is the caller's strongest relation on the area.
	Role string `json:"role,omitempty"`
	// Trackers are the trackers the caller sees that serve this area: the
	// area's own, and project-level ones whose items can reach it (the area
	// takes labels or a work glob names the prefix; config only for callers
	// who see the whole project, who also get every project-level tracker).
	Trackers []Tracker `json:"trackers"`
}

// MarshalJSON writes every list as an array, cleared or never set ("work": []).
func (v AreaView) MarshalJSON() ([]byte, error) {
	arr := func(xs []string) []string {
		if xs == nil {
			return []string{}
		}
		return xs
	}
	ts := v.Trackers
	if ts == nil {
		ts = []Tracker{}
	}
	return json.Marshal(struct {
		Project  ProjectRef `json:"project"`
		Name     string     `json:"name"`
		Paths    []string   `json:"paths"`
		Remotes  []string   `json:"remotes"`
		Work     []string   `json:"work"`
		Labels   []string   `json:"labels"`
		Role     string     `json:"role,omitempty"`
		Trackers []Tracker  `json:"trackers"`
	}{v.Project, v.Name, arr(v.Paths), arr(v.Remotes), arr(v.Work), arr(v.Labels), v.Role, ts})
}

// ExportRequest is POST /v0/projects/{project}/export (local callers only).
type ExportRequest struct {
	// Dir is created and receives project.db (a consistent copy) and project.toml.
	Dir string `json:"dir"`
}

// ExportResponse lists the files written.
type ExportResponse struct {
	Files []string `json:"files"`
}

// ResolveStep is one rule tried during project resolution.
type ResolveStep struct {
	Rule   string `json:"rule"`             // flag, env, cwd-file, cwd-path, cwd-remote, session, default_project
	Input  string `json:"input,omitempty"`  // what the rule looked at
	Result string `json:"result,omitempty"` // matched slug, or why not
}

// Resolution is GET /v0/projects/resolve ("zm project why"): which project a
// write from this caller would go to and which rule fired.
type Resolution struct {
	Project *ProjectRef   `json:"project,omitempty"` // nil: no rule matched (writes are refused)
	Area    string        `json:"area,omitempty"`
	Rule    string        `json:"rule,omitempty"`
	Steps   []ResolveStep `json:"steps"`
}

// Identity is a principal: a person, a remote agent or a machine.
type Identity struct {
	ID   string `json:"id"` // ULID
	Name string `json:"name"`
	Kind string `json:"kind"` // human | agent | machine
	// TailscaleLogin maps a tailnet caller (WhoIs login) to this identity.
	TailscaleLogin string    `json:"tailscale_login,omitempty"`
	Created        time.Time `json:"created,omitzero"`
}

// Identity kinds.
const (
	IdentityHuman   = "human"
	IdentityAgent   = "agent"
	IdentityMachine = "machine"
)

// IdentityList is GET /v0/identities.
type IdentityList struct {
	Identities []Identity `json:"identities"`
}

// NewIdentityRequest is POST /v0/identities.
type NewIdentityRequest struct {
	Name           string `json:"name"`
	Kind           string `json:"kind,omitempty"` // default human
	TailscaleLogin string `json:"tailscale_login,omitempty"`
}

// Grant is one tuple (identity, role, project[/area]).
type Grant struct {
	Identity string `json:"identity"` // identity name
	Role     string `json:"role"`
	Project  string `json:"project"` // slug
	Area     string `json:"area,omitempty"`
	// Object is the stored tuple object: "project:<ulid>" or "area:<ulid>/<name>".
	Object  string    `json:"object"`
	Created time.Time `json:"created,omitzero"`
	// AlsoLowered names the other identities of the same GitHub account
	// whose grant on the object a removal or lowering took down with it.
	AlsoLowered []string `json:"also_lowered,omitempty"`
}

// GrantRequest is POST /v0/grants and DELETE /v0/grants (Role ignored).
type GrantRequest struct {
	Identity string `json:"identity"` // name or ULID; POST creates a human identity if missing
	Role     string `json:"role,omitempty"`
	Project  string `json:"project"` // slug or ULID
	Area     string `json:"area,omitempty"`
}

// GrantList is GET /v0/grants.
type GrantList struct {
	Grants []Grant `json:"grants"`
	// Ceilings are the owners' lowerings and removals on the listed objects.
	Ceilings []Ceiling `json:"ceilings"`
}

// Ceiling is an owner's lowering or removal of an identity's grant: a
// GitHub join (any GitHub grant, user or org) gives the identity, or any
// identity of the same GitHub account, at most Role here (a whole-project
// ceiling never takes an area join below that area's own grant, nor an
// area ceiling a whole-project join below the whole-project grant). It
// outlives the grant row. An admin lifts it by a grant to the identity on
// the same project or area, or an invite there, at LoweredFrom or above; a
// GitHub grant (user or org) never lifts it.
type Ceiling struct {
	Identity string `json:"identity"` // identity name
	Project  string `json:"project"`  // slug
	Area     string `json:"area,omitempty"`
	// Role is the most a GitHub join gives; "" means removed (joins refused).
	Role string `json:"role"`
	// LoweredFrom is the highest role it was lowered from: a grant to the
	// identity at this role or above removes the ceiling.
	LoweredFrom string    `json:"lowered_from"`
	SetBy       string    `json:"set_by"` // who lowered or removed it
	SetAt       time.Time `json:"set_at"`
	// Accounts are the GitHub accounts ("github:<id>") the identity is or
	// was linked to; their joins under any identity meet this ceiling.
	// Shown to a whole-project admin only.
	Accounts []string `json:"accounts,omitempty"`
}

// Token is a bearer token's record. The secret is shown once, at mint.
// The registry stores only its sha256.
type Token struct {
	ID       string     `json:"id"` // ULID
	Identity string     `json:"identity"`
	Project  string     `json:"project,omitempty"` // slug; "" = every project the identity is granted
	Area     string     `json:"area,omitempty"`
	Role     string     `json:"role"` // the cap: the token never acts above this role
	Note     string     `json:"note,omitempty"`
	Created  time.Time  `json:"created"`
	Expires  *time.Time `json:"expires,omitempty"`
	Revoked  *time.Time `json:"revoked,omitempty"`
	LastUsed *time.Time `json:"last_used,omitempty"`
	// Node and Machine are set on a node-bound token (a team replica's, from
	// zm join): it works only from that tailnet node.
	Node    string `json:"node,omitempty"`
	Machine string `json:"machine,omitempty"`
}

// MintTokenRequest is POST /v0/tokens.
type MintTokenRequest struct {
	As         string `json:"as"`                // identity name; created (kind human) if missing
	Project    string `json:"project,omitempty"` // slug or ULID
	Area       string `json:"area,omitempty"`
	Role       string `json:"role"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"` // 0 = no expiry
	Note       string `json:"note,omitempty"`
	// NoGrant mints without adding the matching grant (the identity must already have one).
	NoGrant bool `json:"no_grant,omitempty"`
	// BoxKey binds the token to one box's key (its key id, "zmkey:<fp>"):
	// it then works only over the tunnel, on requests that key signed. Set
	// by the potluck-token kind in process; never read from the wire.
	BoxKey string `json:"-"`
}

// MintTokenResponse carries the secret, once.
type MintTokenResponse struct {
	Token  Token  `json:"token"`
	Secret string `json:"secret"`
}

// TokenList is GET /v0/tokens.
type TokenList struct {
	Tokens []Token `json:"tokens"`
}
