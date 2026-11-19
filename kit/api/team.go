package api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Teams (SPEC 17.3, 17.4; API.md "Teams"): invites and redemption on a
// project's authority, join and leave on a member's replica, and the
// project header a replica caches.

// Invite is one join invitation (zm invite). No identity, grant or token
// exists until it is redeemed.
type Invite struct {
	ID          string `json:"id"`
	Project     string `json:"project"` // slug
	ProjectID   string `json:"project_id"`
	Area        string `json:"area,omitempty"`
	As          string `json:"as"` // the identity it creates or reuses
	Role        string `json:"role"`
	AllowTagged bool   `json:"allow_tagged,omitempty"`
	// Class is the operator class (SPEC 8) the redeeming machine gets in the
	// project: trusted or public, "" for the default of its role.
	Class     string     `json:"class,omitempty"`
	CreatedBy string     `json:"created_by,omitempty"`
	Created   time.Time  `json:"created"`
	Expires   time.Time  `json:"expires"`
	State     string     `json:"state"`             // live | redeemed | expired
	Machine   string     `json:"machine,omitempty"` // redeemed: the replica's machine name
	Node      string     `json:"node,omitempty"`    // redeemed: the replica's stable node ID
	Redeemed  *time.Time `json:"redeemed,omitempty"`
	Used      bool       `json:"used,omitempty"` // redeemed and the token has made its first call
	// Revoked: redeemed, and the machine that redeemed it holds no live
	// token in the project now (it left, or its token was revoked or
	// expired): no longer in use.
	Revoked bool `json:"revoked,omitempty"`
}

// Invite states.
const (
	InviteLive     = "live"
	InviteRedeemed = "redeemed"
	InviteExpired  = "expired"
)

// InviteRequest is POST /v0/invites.
type InviteRequest struct {
	Project     string `json:"project"` // slug or ULID
	Area        string `json:"area,omitempty"`
	As          string `json:"as"`
	Role        string `json:"role"`                  // member | guest
	TTLSeconds  int64  `json:"ttl_seconds,omitempty"` // default 24 h
	AllowTagged bool   `json:"allow_tagged,omitempty"`
	// Via picks the code's transport: "tailnet" or "tunnel" (SPEC 17.11).
	// Default: the tailnet while this machine's node runs, else the tunnel
	// when tunnel.url is set.
	Via string `json:"via,omitempty"`
	// Class is the operator class (SPEC 8 "Operator class") the machine
	// that redeems the code gets: trusted or public; "" leaves the default
	// of its role. Only an owner or admin of the whole project sets it.
	Class string `json:"class,omitempty"`
	// AddArea makes Area in the project first when it has none by that
	// name (admin on the whole project): an invite into an area its program
	// always has, such as a pool's, completes a project made before the
	// area was.
	AddArea bool `json:"add_area,omitempty"`
}

// Transports a team remote uses (SPEC 17.11).
const (
	ViaTailnet = "tailnet"
	ViaTunnel  = "tunnel"
)

// InviteResponse carries the join code, once.
type InviteResponse struct {
	Invite Invite `json:"invite"`
	Code   string `json:"code"` // potluck-<machine>:<fingerprint>:XXXX-XXXX (an older node's zm-<machine>:...), or <url>#<fingerprint>:<secret>
}

// InviteList is GET /v0/invites.
type InviteList struct {
	Invites []Invite `json:"invites"`
}

// RedeemRequest is POST /v0/invites/redeem (from the replica's tailnet node).
type RedeemRequest struct {
	Code    string `json:"code,omitempty"` // the whole join code, or its XXXX-XXXX secret (tailnet)
	Machine string `json:"machine"`        // the replica's machine name, unique per authority
	// Proof replaces Code over the tunnel (SPEC 17.11): the secret never
	// crosses it (project.RedeemProof).
	Proof string `json:"proof,omitempty"`
}

// TeamCap is what a replica's token may do: at most Role, inside Project
// (and Area).
type TeamCap struct {
	Project string `json:"project"` // ULID on the authority
	Slug    string `json:"slug"`
	Area    string `json:"area,omitempty"`
	Role    string `json:"role"`
}

// RedeemResponse is the answer to a redemption. The token is node-bound: it
// works only from the redeeming node, never on a Unix socket.
type RedeemResponse struct {
	Token        string  `json:"token"`
	Cap          TeamCap `json:"cap"`
	ScopeVersion int64   `json:"scope_version"`
	Machine      string  `json:"machine"`
	Identity     string  `json:"identity"`
	Authority    string  `json:"authority"` // the authority's machine name
}

// TeamHeader is the project header a replica caches (SPEC 17.3), sent on
// every answer to a node-bound token (HeaderTeam).
type TeamHeader struct {
	Project      ProjectRef `json:"project"`
	Areas        []string   `json:"areas,omitempty"` // the areas the member sees
	Role         string     `json:"role"`            // the member's strongest role in the project
	Cap          TeamCap    `json:"cap"`
	ScopeVersion int64      `json:"scope_version"`
	Identity     string     `json:"identity"`
	Machine      string     `json:"machine"`           // the replica's machine name at the authority
	Remotes      []string   `json:"remotes,omitempty"` // repo remotes the member may see (to find checkouts)
	// Repos and AreaRepos are the project's path and remote rules (whole-
	// project members) and those of each area the member sees: the
	// replica's cwd resolution matches them like a local project's (SPEC
	// "Which project"). Fresh with every answer.
	Repos     RepoRules    `json:"repos"`
	AreaRepos []NamedRepos `json:"area_repos,omitempty"`
}

// NamedRepos is one area's path and remote rules.
type NamedRepos struct {
	Area string `json:"area"`
	RepoRules
}

// ProjectModeResponse is POST /v0/projects/{project}/team and .../solo.
type ProjectModeResponse struct {
	Project ProjectRef `json:"project"`
	Team    bool       `json:"team"`
	Moved   int        `json:"moved"` // leases moved between registry.db and project.db
}

// JoinRequest is POST /v0/team/join on the member's own daemon.
type JoinRequest struct {
	Code    string   `json:"code"`
	Machine string   `json:"machine,omitempty"`  // default: this daemon's machine name
	AuthKey string   `json:"auth_key,omitempty"` // for zm tailnet up; never saved
	Paths   []string `json:"paths,omitempty"`    // the member's checkouts of the project
	Cwd     string   `json:"cwd,omitempty"`      // found by remote when no paths are given
	WaitMS  int      `json:"wait_ms,omitempty"`  // how long to wait for the tailnet (default 10 s)
	// Cloudflare Access service token for a tunnel home behind Access
	// (optional, SPEC 17.11): stored next to the token, sent on every call.
	AccessID     string `json:"access_id,omitempty"`
	AccessSecret string `json:"access_secret,omitempty"`
	// A join with no code (SPEC 22): Project names the team project; From
	// the authority's machine when several answer; Login continues a
	// GitHub login this daemon started (JoinResponse.GitHub.Login).
	Project string `json:"project,omitempty"`
	From    string `json:"from,omitempty"`
	Login   string `json:"login,omitempty"`
	// Area and Role scope a GitHub join (SPEC 22): one area, at most that
	// role (guest only over https). From may then be an https URL
	// with the home's key fingerprint: https://<host>#<fingerprint>.
	Area string `json:"area,omitempty"`
	Role string `json:"role,omitempty"`
}

// Remote is a team project this machine has joined (its replica's record).
type Remote struct {
	Project      ProjectRef `json:"project"` // the authority's ULID and slug
	Authority    string     `json:"authority"`
	URL          string     `json:"url"`
	Fingerprint  string     `json:"fingerprint"`
	Via          string     `json:"via"` // tailnet | tunnel (SPEC 17.11)
	Cap          TeamCap    `json:"cap"`
	ScopeVersion int64      `json:"scope_version"`
	Machine      string     `json:"machine"`
	Identity     string     `json:"identity"`
	Areas        []string   `json:"areas,omitempty"`
	Role         string     `json:"role,omitempty"`
	Paths        []string   `json:"paths,omitempty"`
	Joined       time.Time  `json:"joined"`
	State        string     `json:"state"` // ok | purged | revoked
	LastError    string     `json:"last_error,omitempty"`
	TokenFile    string     `json:"token_file,omitempty"`
	// AuthorityOwner is the tailnet owner (WhoIs login) of the authority's
	// node when this machine joined by GitHub (SPEC 22); OwnerNow is set
	// when a later connection found another owner (zm remotes warns).
	AuthorityOwner string `json:"authority_owner,omitempty"`
	OwnerNow       string `json:"owner_now,omitempty"`
	// Replica is the local copy (round C): its state, the cursor's age, the
	// outbox and the last error.
	Replica *ReplicaStatus `json:"replica,omitempty"`
	// Unsent is the file a dropped copy's still-pending writes were saved in
	// (JSON Lines, one write per line; never re-sent); UnsentCount is its
	// line count. Both are set only while the file exists.
	Unsent      string `json:"unsent,omitempty"`
	UnsentCount int    `json:"unsent_count,omitempty"`
}

// EventTeamJoined (machine-level, owner only): a replica redeemed an invite.
// Data: TeamJoined.
const EventTeamJoined = "dev.zm.team.joined"

// TeamJoined is EventTeamJoined's data.
type TeamJoined struct {
	Project  ProjectRef `json:"project"`
	Area     string     `json:"area,omitempty"`
	Role     string     `json:"role"`
	Identity string     `json:"identity"`
	Machine  string     `json:"machine"`
	Node     string     `json:"node"`
}

// Remote states.
const (
	RemoteOK      = "ok"
	RemotePurged  = "purged"  // the authority refused the token 3 times over 10 minutes
	RemoteRevoked = "revoked" // the identity has no grant left in the project
)

// JoinResponse is what zm join prints.
type JoinResponse struct {
	Remote  *Remote  `json:"remote,omitempty"`
	AuthURL string   `json:"auth_url,omitempty"` // the tailnet needs a browser login first
	Items   int      `json:"items"`              // open work items the member sees
	Notes   []string `json:"notes,omitempty"`
	// GitHub is a join with no code waiting for the person to approve its
	// GitHub login: show the code and URL, then call again with Login.
	GitHub *JoinGitHub `json:"github,omitempty"`
	// Authorities answers a join with no code and no From: the zm
	// authorities on the tailnet that run GitHub logins, with their tailnet
	// owners, for the person to choose (and confirm) one; Skipped are the
	// other zm peers and why. Nothing is started until From names one.
	Authorities []JoinAuthority `json:"authorities,omitempty"`
	Skipped     []JoinSkipped   `json:"skipped,omitempty"`
}

// JoinAuthority is a zm authority a join with no code could use.
type JoinAuthority struct {
	Machine string `json:"machine"`
	Host    string `json:"host,omitempty"` // its full MagicDNS name (what is dialled)
	Owner   string `json:"owner"`          // its node's tailnet owner (WhoIs login)
	Node    string `json:"node"`           // its stable node id
}

// JoinSkipped is a zm peer a join with no code did not use, and why.
type JoinSkipped struct {
	Machine string `json:"machine"`
	Reason  string `json:"reason"` // "no answer", "not a zm authority", "runs no GitHub logins"
}

// JoinGitHub is a GitHub join in flight (SPEC 22).
type JoinGitHub struct {
	Login           string    `json:"login"` // this daemon's handle for it (never the poll secret)
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	Expires         time.Time `json:"expires"`
	Authority       string    `json:"authority"` // the machine running the login
	Owner           string    `json:"owner"`     // its node's tailnet owner (WhoIs login)
	For             string    `json:"for"`       // who it admits: this node, as machine ...
	// Resumed: the same code an earlier run started, still waiting (a
	// client killed or timed out does not end a join; asking again resumes).
	Resumed bool `json:"resumed,omitempty"`
}

// RemoteList is GET /v0/team/remotes.
type RemoteList struct {
	Remotes []Remote `json:"remotes"`
}

// LeaveResponse is DELETE /v0/team/remotes/{project}.
type LeaveResponse struct {
	Project ProjectRef `json:"project"`
	Removed []string   `json:"removed"` // files deleted
	Revoked bool       `json:"revoked"` // the authority revoked the token too
}

// EncodeTeamHeader is HeaderTeam's value: base64url (no padding) of h's JSON.
func EncodeTeamHeader(h TeamHeader) string {
	js, _ := json.Marshal(h)
	return base64.RawURLEncoding.EncodeToString(js)
}

// DecodeTeamHeader reads HeaderTeam's value.
func DecodeTeamHeader(s string) (TeamHeader, error) {
	var h TeamHeader
	js, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return h, err
	}
	return h, json.Unmarshal(js, &h)
}

// PresenceRequest is POST /v0/team/presence: a member machine's heartbeat
// for one team project (SPEC 17.3), every 15 s and soon after a change a
// part asks a beat for. The machine-op reports are common to every beat;
// the rest is the registered parts' (kit/teamlink BeatPart), one raw
// JSON value per part name ("sessions": zm's orchapi.SessionsBeat, "run_targets": a
// JSON array of computeapi.RunTargetAdvert).
type PresenceRequest struct {
	Project string `json:"project"` // the authority's ULID
	// Ops reports this machine's ops (SPEC 8 "Machine choice"): results and
	// started tasks' status, until an answer took them.
	Ops []MachineOpReport `json:"ops,omitempty"`
	// Took are the ids of the ops this machine has taken and not had a
	// final report taken for: no later answer carries them (or their
	// payload, which may hold stdin or secrets); an answer to a beat sent
	// before the machine had an op may carry it once more. An authority that
	// does not read it sends a pending op with every answer.
	Took  []string                   `json:"took,omitempty"`
	Parts map[string]json.RawMessage `json:"parts,omitempty"`
}

// MachineReport is POST /v0/team/machine: a replica's numbers for one team
// project (SPEC 8 "Machine numbers"), sent apart from presence (at most
// every 30 s, after a heartbeat went through) so an authority without the
// route still takes the heartbeat.
type MachineReport struct {
	Project string       `json:"project"` // the authority's ULID
	Stats   MachineStats `json:"stats"`
}

// MachineOpList is GET /v0/team/ops's answer: the ops pending for the
// calling machine in the project, oldest first (zm-eakh.12.5.10.1).
type MachineOpList struct {
	Ops []MachineOp `json:"ops"`
}

// MachineOpReports is POST /v0/team/ops/report: a machine's reports on its
// ops in Project, sent as each finishes.
type MachineOpReports struct {
	Project string            `json:"project"`
	Ops     []MachineOpReport `json:"ops"`
}

// PresenceResponse answers a heartbeat: the ops pending for the machine
// and each registered part's answer by part name ("sessions":
// zm's orchapi.SessionsBeatAnswer).
type PresenceResponse struct {
	// Ops are the ops pending for this machine in the project, oldest first
	// (SPEC 8 "Machine choice"): each runs at most once, reported in Ops.
	Ops   []MachineOp                `json:"ops,omitempty"`
	Parts map[string]json.RawMessage `json:"parts,omitempty"`
}

// TunnelStatus is GET /v0/tunnel (SPEC 17.11).
type TunnelStatus struct {
	PreviewDomain string `json:"preview_domain,omitempty"`
	Listen        string `json:"listen,omitempty"` // configured, or the running listener's address
	URL           string `json:"url,omitempty"`    // the public URL invites name
	Running       bool   `json:"running"`
	Fingerprint   string `json:"fingerprint,omitempty"` // this daemon's key, as invites pin it
	Error         string `json:"error,omitempty"`
	// EdgeSecret: tunnel.edge_secret is set; EdgeSeen: when a request last
	// arrived carrying it (cloudflared is forwarding).
	EdgeSecret bool       `json:"edge_secret,omitempty"`
	EdgeSeen   *time.Time `json:"edge_seen,omitempty"`
	// KeyEscrow is the secret reference holding this daemon's key outside
	// the home (tunnel.key), "" when none: a restore then mints a new key.
	KeyEscrow string `json:"key_escrow,omitempty"`
}

// TunnelKeyRequest is POST /v0/tunnel/key (zm tunnel key): a secret
// reference (keychain:, file:, env:) that holds this daemon's key, "" to
// clear it. Never the key.
type TunnelKeyRequest struct {
	Ref string `json:"ref"`
}

// TunnelUpRequest is POST /v0/tunnel/up (zm tunnel setup): the listener,
// the public URL invites name, and the edge secret cloudflared sends.
type TunnelUpRequest struct {
	PreviewDomain string `json:"preview_domain,omitempty"`
	Listen        string `json:"listen"`
	URL           string `json:"url"`
	EdgeSecret    string `json:"edge_secret,omitempty"`
}

// Person is one teammate in GET /v0/people: an identity holding a grant in a
// project the caller shares with it. ID is the stable person id (bead
// zm-rpjh): the verified account the identity is linked to at its
// authority ("github:<numeric id>", the owner id of the owners decision),
// else the identity's ULID there. A member's socket merges authorities by
// ID, never by name: two authorities' identities are one person only when
// both are linked to the same account.
type Person struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Kind string `json:"kind"` // IdentityHuman or IdentityAgent
	// GitHub is the linked account's login (display only; ID is the key).
	GitHub string `json:"github,omitempty"`
	// Names are every identity name the person holds at the authorities
	// merged in (one GitHub account may own several), sorted; Name is the
	// least of them.
	Names []string `json:"names,omitempty"`
	// You is the caller's own row: one of its identities is the caller's,
	// or shares the caller's linked account.
	You bool `json:"you,omitempty"`
	// Aliases are the exact label names the person also answers to
	// (owner:judy for judyexample; zm identity alias), at every
	// authority merged in, sorted.
	Aliases  []string        `json:"aliases,omitempty"`
	Projects []PersonProject `json:"projects"`
	// Machines are the machines of the person's live sessions the caller
	// sees; Seen is the latest change among those sessions.
	Machines []string   `json:"machines,omitempty"`
	Seen     *time.Time `json:"seen,omitempty"`
}

// PersonProject is a project the caller shares with a person, under one of
// the person's identities: a person with two identities in a project has a
// row for each.
type PersonProject struct {
	Project  string `json:"project"` // ULID
	Slug     string `json:"slug,omitempty"`
	Identity string `json:"identity"` // the person's identity ULID at the project's authority
	// Name is the identity's name at the project's authority: the
	// human:<name> that reaches the person in this project.
	Name string `json:"name,omitempty"`
	// Answers is whether this identity owns or administers the whole
	// project: it answers the project's asks. Shown only to a reader of
	// the whole project.
	Answers bool `json:"answers,omitempty"`
}

// PeopleList is GET /v0/people. Skipped names an authority that did not
// answer (a partial list never looks complete).
type PeopleList struct {
	People  []Person      `json:"people"`
	Skipped []WorkSkipped `json:"skipped,omitempty"`
}

// Operator classes of a machine in a team project (SPEC 8 "Operator class").
const (
	// MachineTrusted: whoever runs the machine is trusted with the
	// project's secrets, private code and the parent's authority.
	MachineTrusted = "trusted"
	// MachinePublic: a lender's or a stranger's machine: secret-free work
	// only, and no remote task or Z3 placed there.
	MachinePublic = "public"
)

// MachineClassRequest is POST /v0/team/machines/{machine}/class. Class ""
// clears an explicit class, so the default applies again.
type MachineClassRequest struct {
	Project string `json:"project,omitempty"`
	Class   string `json:"class"`
}

// MachineClass is one machine's operator class in a project, as its
// authority decides it (never from the machine's own report).
type MachineClass struct {
	Machine string `json:"machine"`
	Class   string `json:"class"`
	// Why: "set by <identity>" for an explicit class, else the default's
	// reason (the authority's own machine; joined as an owner or admin; as a
	// member or guest).
	Why   string     `json:"why"`
	SetBy string     `json:"set_by,omitempty"`
	SetAt *time.Time `json:"set_at,omitempty"`
	// FromInvite: the class is the one the invite the machine joined with
	// carried (the owner's choice when they made it), not one set since.
	FromInvite bool `json:"from_invite,omitempty"`
}

// MachineClassList is GET /v0/team/machines/class.
type MachineClassList struct {
	Project  string         `json:"project"`
	Machines []MachineClass `json:"machines"`
}

// TunnelPreviewDomainRequest configures existing wildcard ingress; it creates no DNS or certificate.
type TunnelPreviewDomainRequest struct {
	Domain string `json:"domain"`
}

// ProjectKey is the authority key a team project's id is bound to on this
// machine (GET /v0/team/project-keys/{project}): the first join of the id
// that worked bound it, and only its owner's explicit forget removes it.
type ProjectKey struct {
	Project     string    `json:"project"`
	Via         string    `json:"via"`
	Fingerprint string    `json:"fingerprint"`
	Authority   string    `json:"authority"` // its machine name when bound, for people only
	Bound       time.Time `json:"bound"`
}

// What a forget does with what this machine kept for the project's id
// (a host's shares, handle ledger and key tag): kept for the next authority
// that joins the id, or dropped.
const (
	ForgetKeepHostState = "keep"
	ForgetDropHostState = "drop"
)

// ForgetProjectKeyRequest is POST /v0/team/project-keys/{project}/forget.
// HostState is ForgetKeepHostState or ForgetDropHostState; nothing else
// is taken. Fingerprint must be the bound key's: the forget removes the
// binding the owner was shown, not one bound since.
type ForgetProjectKeyRequest struct {
	HostState   string `json:"host_state"`
	Fingerprint string `json:"fingerprint"`
}

// ForgetProjectKeyResponse is what a forget removed.
type ForgetProjectKeyResponse struct {
	Key       ProjectKey `json:"key"`
	HostState string     `json:"host_state"`
	Dropped   []string   `json:"dropped"` // what went with ForgetDropHostState
}
