// Package config resolves the home directory (in the order of the
// program's Names), this machine's identity and the config file.
// Owned by the integrator.
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Environment variables.
const (
	EnvHuman   = "ZM_HUMAN"   // override the human name (default: OS user)
	EnvMachine = "ZM_MACHINE" // override the machine name (default: config.json, persisted at the first daemon start)
	EnvHost    = "ZM_HOST"    // client: talk to this base URL (a daemon's tailnet listener) instead of the Unix socket
	EnvAs      = "ZM_AS"      // client: the caller's mail address (set inside zm sessions)
	EnvToken   = "ZM_TOKEN"   // client: bearer token (required with ZM_HOST)
	EnvProject = "ZM_PROJECT" // client: the project for writes (slug or ULID), below --project
)

// File names inside Home.
const (
	FileConfig   = "config.json"
	FileRegistry = "registry.db" // projects, identities, grants, tokens, sessions, leases, machine events
	FileLog      = "daemon.log"
	DirProjects  = "projects" // projects/<ulid>/{project.db,project.toml}
	DirScratch   = "scratch"  // scratch/<session8>/ (session module)
)

// The machine name a binary uses: MachineEnv overrides it (ZM_MACHINE), and
// MachineSuffix ends a name taken from the host name. A second product on a
// machine that also runs zm sets both (cmd/potluck: POTLUCK_MACHINE,
// "-potluck"), so its tailnet node, zm-<machine>, never takes the name of
// zm's node there. The defaults are zm's: no suffix.
var (
	MachineEnv    = EnvMachine
	MachineSuffix = ""
)

// The other variables a binary reads under its own names: the human
// (ZM_HUMAN) and the remote daemon a client talks to (ZM_HOST; "" when the
// binary has no remote client). The defaults are zm's (cmd/potluck:
// POTLUCK_HUMAN and none).
var (
	HumanEnv = EnvHuman
	HostEnv  = EnvHost
)

// Config is the resolved configuration. File holds what config.json says;
// the other fields are derived and never written.
type Config struct {
	Home    string `json:"-"`
	Human   string `json:"-"` // this machine's human (human:<Human>)
	Machine string `json:"-"` // this machine's name (machine:<Machine>)
	// MachineFrom says where Machine came from (the MachineFrom* values).
	MachineFrom string `json:"-"`
	File        File   `json:"-"`
	// SyncPause, when set, replaces every pause between a tracker mirror's
	// background syncs. Never read from disk: tests in other packages set it
	// so mirrors sync only on an explicit kick.
	SyncPause time.Duration `json:"-"`
	// UserHome is the OS user's home, where agent runtimes keep their state
	// (~/.claude.json, ~/.claude, ~/.codex) unless a session names its own.
	// Load sets it (LoadFrom does not); "" reads none of it.
	UserHome string `json:"-"`
}

// Where the machine name came from (Config.MachineFrom).
const (
	MachineFromEnv      = "env"      // ZM_MACHINE
	MachineFromFile     = "file"     // config.json: persisted at the daemon's first start
	MachineFromHost     = "host"     // a stable host name (macOS LocalHostName; elsewhere the host name)
	MachineFromUnstable = "unstable" // macOS without a LocalHostName: the DHCP-changeable host name, never persisted
	MachineFromFallback = "fallback" // no name at all: "localhost", never persisted
	MachineFromPrevious = "previous" // the name this home's daemon last ran as (AdoptMachine)
)

// File is config.json. Every field is optional.
type File struct {
	Human   string  `json:"human,omitempty"`
	Machine string  `json:"machine,omitempty"`
	Tailnet Tailnet `json:"tailnet,omitzero"`
	// Web is phone alerts' Web Push settings.
	Web Web `json:"web,omitzero"`
	// Tunnel is the listener a Cloudflare Tunnel forwards to (SPEC 17.11);
	// off unless set.
	Tunnel Tunnel `json:"tunnel,omitzero"`
	// GitHub is "Sign in with GitHub" (SPEC 22, the owners brick).
	GitHub GitHub `json:"github,omitzero"`
	// Runners is the GitHub runner broker (SPEC 20, runners); off unless set.
	Runners Runners `json:"runners,omitzero"`
	// Secrets is the secrets door's authority side: the issuers that mint
	// credentials for boxes (package issuers); off unless set.
	Secrets Secrets `json:"secrets,omitzero"`
	// Door is the Z3 door's public listener (SPEC 18.4, internal/door); off unless set.
	Door Door `json:"door,omitzero"`
	// Agents overrides the command used to start an agent kind (session module), e.g.
	// {"claude": ["claude", "--model", "opus"]}.
	Agents map[string][]string `json:"agents,omitempty"`
	// WorkAdapters lists extra adapter executables by path, besides zm-work-* on PATH.
	WorkAdapters []string `json:"work_adapters,omitempty"`
	// DefaultProject (slug) is the last resolution rule, below the session's
	// project. Never set by installation; "zm project why" reports it.
	DefaultProject string `json:"default_project,omitempty"`
	// Admission are the thresholds below which spawns are refused.
	Admission Admission `json:"admission,omitzero"`
	// LeaseTickSeconds is how often the daemon expires leases (default 30).
	LeaseTickSeconds int `json:"lease_tick_seconds,omitempty"`
	// BoundLeaseTTLSeconds is a bound lease's deadline past its session's
	// last running tick (default 3600): the ticker renews it only while the
	// managed session is running, so an idle or needs_human holder expires
	// like any TTL lease.
	BoundLeaseTTLSeconds int `json:"bound_lease_ttl_seconds,omitempty"`
	// LimitedTokenMaxTTLSeconds is the longest a limited token may live
	// (POST /v0/tokens with a limit; default 30 days).
	LimitedTokenMaxTTLSeconds int64 `json:"limited_token_max_ttl_seconds,omitempty"`
	// IdleAfterSeconds: a managed session with no PTY output for this long is
	// set idle by the session module (default 300), so a hung agent stops
	// renewing its bound lease.
	IdleAfterSeconds int `json:"idle_after_seconds,omitempty"`
	// TaskDeadlineSeconds sets the default overall task deadline (default four hours).
	TaskDeadlineSeconds int `json:"task_deadline_seconds,omitempty"`
	// AgentNice is the nice value an agent session (every kind but shell)
	// and so its builds and tests start at (default 10; -1 keeps the
	// daemon's own priority), so the owner's UIs stay responsive under load.
	AgentNice int `json:"agent_nice,omitempty"`
	// UpdateKeys are the release-signing public keys `zm daemon update
	// --release` trusts (ed25519:<base64>; SPEC 2 "Release channel"). More
	// than one during a key rotation.
	UpdateKeys []string `json:"update_keys,omitempty"`
	// RemoteTasks lets the owner of a team project this machine joined (or
	// hosts) start one-shot tasks here from another machine, through the
	// authority (SPEC 8 "Machine choice"). Default off: the machine owner opts in.
	RemoteTasks bool `json:"remote_tasks,omitempty"`
	// RemoteTaskSlots is how many of those run here at once: the authority's
	// waiting line places no more (SPEC 8 "Waiting line"; default 1).
	RemoteTaskSlots int `json:"remote_task_slots,omitempty"`
	// SandboxTaskCmd is the task driver for sandbox tasks: an absolute path
	// the daemon runs once per sandbox task placed here, with the task on
	// stdin (SPEC 8 "Where it runs"). Unset: the machine takes none. Only
	// meaningful with RemoteTasks.
	SandboxTaskCmd string `json:"sandbox_task_cmd,omitempty"`
	// SandboxTaskSlots is how many sandbox tasks run here at once, apart
	// from RemoteTaskSlots (1..16, default 1). Only meaningful with
	// SandboxTaskCmd.
	SandboxTaskSlots int `json:"sandbox_task_slots,omitempty"`
	// T3 lets the owner of a team project this machine joined (or hosts)
	// start a Z3 server here from a pinned checkout and mint a one-time
	// pairing credential for it, through the authority (SPEC 8 "Z3 on a
	// machine"). Off unless Checkout is set.
	T3 T3 `json:"t3,omitzero"`
}

// MaxSandboxTaskSlots is the most sandbox_task_slots may be.
const MaxSandboxTaskSlots = 16

// SandboxTaskWarning says what is wrong with the task driver settings, for
// the daemon's log; "" when nothing is. A bad value never fails the load
// (every CLI call and hook loads this file): a command that is not an
// absolute path, which would resolve against whatever directory the daemon
// started in, means no task driver, and slots out of 1..MaxSandboxTaskSlots
// are clamped (SandboxTasks).
func (f File) SandboxTaskWarning() string {
	if f.SandboxTaskCmd == "" {
		return ""
	}
	if !filepath.IsAbs(f.SandboxTaskCmd) {
		return fmt.Sprintf("sandbox_task_cmd %q is not an absolute path: this machine takes no sandbox tasks", f.SandboxTaskCmd)
	}
	if f.SandboxTaskSlots < 0 || f.SandboxTaskSlots > MaxSandboxTaskSlots {
		return fmt.Sprintf("sandbox_task_slots %d is not 1..%d: using %d", f.SandboxTaskSlots, MaxSandboxTaskSlots,
			min(max(1, f.SandboxTaskSlots), MaxSandboxTaskSlots))
	}
	return ""
}

// SandboxTasks is the task driver and its slots: "" and 0 when the machine
// takes no sandbox tasks, including when the command is not an absolute
// path. Slots default to 1 and are clamped to 1..MaxSandboxTaskSlots.
func (f File) SandboxTasks() (cmd string, slots int) {
	if f.SandboxTaskCmd == "" || !filepath.IsAbs(f.SandboxTaskCmd) {
		return "", 0
	}
	return f.SandboxTaskCmd, min(max(1, f.SandboxTaskSlots), MaxSandboxTaskSlots)
}

// T3 is the Z3 server a t3_ensure op starts here: `node
// <checkout>/apps/server/src/bin.ts serve --lan` at Commit, its state in
// Home. Paths may start with "~/".
type T3 struct {
	Checkout string `json:"checkout,omitempty"` // a clone of the server's repository, with an origin remote
	Commit   string `json:"commit,omitempty"`   // the full main commit it runs (40 hex)
	Home     string `json:"home,omitempty"`     // its --base-dir (default ~/.z3)
	Port     int    `json:"port,omitempty"`     // default 3773
	Node     string `json:"node,omitempty"`     // default "node" on PATH
	VP       string `json:"vp,omitempty"`       // Vite+ for `vp i` (default "vp" on PATH)
}

// Enabled: the block names a checkout.
func (t T3) Enabled() bool { return t.Checkout != "" }

// Admission thresholds (session module). Zero = the default.
type Admission struct {
	// MinMemoryFreePercent: refuse when macOS kern.memorystatus_level (or
	// Linux MemAvailable %) is below this. Default 20.
	MinMemoryFreePercent int `json:"min_memory_free_percent,omitempty"`
	// MaxSwapUsedMB: refuse when swap in use is above this. Default 4096.
	MaxSwapUsedMB int64 `json:"max_swap_used_mb,omitempty"`
	// MinDiskFreeMB on the home's and the session cwd's volume. Default 5120.
	MinDiskFreeMB int64 `json:"min_disk_free_mb,omitempty"`
	// MaxManagedSessions alive at once. Default 0: no count limit, because the
	// memory, swap and disk checks measure the machine and a count does not. A
	// box that wants a hard ceiling sets one.
	MaxManagedSessions int `json:"max_managed_sessions,omitempty"`
}

// WithDefaults fills zero thresholds.
func (a Admission) WithDefaults() Admission {
	if a.MinMemoryFreePercent == 0 {
		a.MinMemoryFreePercent = 20
	}
	if a.MaxSwapUsedMB == 0 {
		a.MaxSwapUsedMB = 4096
	}
	if a.MinDiskFreeMB == 0 {
		a.MinDiskFreeMB = 5120
	}
	if a.MaxManagedSessions < 0 {
		a.MaxManagedSessions = 0 // nonsense means no limit, and never reaches a heartbeat
	}
	return a
}

// Tailnet is the optional tsnet door (tailnet module).
type Tailnet struct {
	Enabled    bool   `json:"enabled,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	ControlURL string `json:"control_url,omitempty"`
	Port       int    `json:"port,omitempty"` // default 7433
	// Every tailnet request still needs a bearer token or a WhoIs login that
	// maps to a registry identity (zm identity add --tailscale).
}

// Web configures phone alerts (SPEC 16 "Phone alerts").
type Web struct {
	VAPIDSubject string `json:"vapid_subject,omitempty"` // optional public HTTPS contact URL
}

// Tunnel configures the tunnel listener (SPEC 17.11): loopback-only HTTP
// that cloudflared forwards to. API requests must be signed by a member's
// key; only redemption goes without a key-bound token. Native public HTTP
// modules authorize requests on their own capability hostnames.
type Tunnel struct {
	PreviewDomain string `json:"preview_domain,omitempty"` // Existing owned wildcard HTTPS domain; empty disables public previews.
	Listen        string `json:"listen,omitempty"`         // e.g. "127.0.0.1:7435"; loopback addresses only
	URL           string `json:"url,omitempty"`            // the public https URL invites name
	// EdgeSecret, when set, must arrive in X-Zm-Edge-Secret (a Cloudflare
	// Transform Rule adds it) before CF-Connecting-IP is believed for
	// redemption's rate limits; without it, cloudflared's loopback peer is.
	EdgeSecret string `json:"edge_secret,omitempty"`
	// Key, when set, is a secret reference (keychain:, file:, env:) holding
	// this daemon's key outside the home (SPEC 17.11 "The home key across a
	// restore"). A reference, never the key, so a backup set may carry it.
	Key string `json:"key,omitempty"`
}

// GitHub configures the owners brick's GitHub driver (SPEC 22). Empty URLs
// mean github.com; an empty ClientID turns the device flow off (a token on
// stdin still links).
type GitHub struct {
	// ClientID is the OAuth app's client id, with "Enable Device Flow" set.
	// The device flow needs no client secret, so none is configured.
	ClientID string `json:"client_id,omitempty"`
	APIURL   string `json:"api_url,omitempty"` // default https://api.github.com
	WebURL   string `json:"web_url,omitempty"` // default https://github.com (device flow pages)
	// LookupToken is a secret reference (file:PATH, keychain:SERVICE,
	// env:NAME; kit/secrets) to the authority's own GitHub token, for
	// lookups by login and id and for re-checking org memberships: an org
	// grant needs the token of one of the org's members. Never a person's
	// login token.
	LookupToken string `json:"lookup_token,omitempty"`
}

// Door configures the Z3 door (SPEC 18.4): Listen is a loopback address
// serving only POST /v0/door/hook/{token}, which `zm tunnel setup` routes
// to unsigned; PublicURL is where task URLs start (default Tunnel.URL).
type Door struct {
	Listen    string `json:"listen,omitempty"`     // e.g. "127.0.0.1:7437"; loopback addresses only
	PublicURL string `json:"public_url,omitempty"` // https; default tunnel.url
}

// Runners configures the GitHub runner broker (SPEC 20 "GitHub runner
// broker"). Secrets are references (kit/secrets), never values.
type Runners struct {
	GitHub    RunnersGitHub `json:"github,omitzero"`
	Pool      RunnersPool   `json:"pool,omitzero"`
	Label     string        `json:"label,omitempty"`      // default: the pool project's slug
	PublicURL string        `json:"public_url,omitempty"` // https, forwarding the broker's public routes unsigned (not the zm tunnel)
	Listen    string        `json:"listen,omitempty"`     // loopback addr serving only those routes, for public_url's proxy
	// AllowForkPRs serves a private repository's runs from forks and of every
	// event. A public repository's never are. Which repositories are served
	// at all is the broker's own record (potluck runners allow), not config.
	AllowForkPRs bool `json:"allow_fork_prs,omitempty"`
	JobTTLS      int  `json:"job_ttl_s,omitempty"` // a job's box lease, renewed while it runs (default 3600)
	// Accounts are the GitHub users and organizations whose installations of
	// the App the broker hears and links. A public App can be installed by
	// anyone; deliveries from other accounts are dropped unstored. Empty: only
	// the installations linked here are heard.
	Accounts []string `json:"accounts,omitempty"`
	// RetiredAllowPublicRepos is the old global switch for serving public
	// repositories. It does nothing now (each public repository needs its
	// owner's own yes). It is read only so the broker can say so while a
	// config.json still has it set, and is kept on save until the owner
	// deletes it.
	RetiredAllowPublicRepos bool `json:"allow_public_repos,omitempty"`
}

// RunnersGitHub is the broker's GitHub App.
type RunnersGitHub struct {
	AppID         int64  `json:"app_id,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	Slug          string `json:"slug,omitempty"`
	PrivateKey    string `json:"private_key,omitempty"`    // secret reference
	ClientSecret  string `json:"client_secret,omitempty"`  // secret reference
	WebhookSecret string `json:"webhook_secret,omitempty"` // secret reference
	APIURL        string `json:"api_url,omitempty"`        // default https://api.github.com
	WebURL        string `json:"web_url,omitempty"`        // default https://github.com
}

// Secrets configures the secrets door's issuers (docs/daemon.md "Secrets
// issuers"). Keys are secret references (kit/secrets), never values.
//
// The section is decoded strictly: a key it does not know, at any level (a
// "workflow" for "workflows"), would otherwise be dropped and could widen a
// rule. Such a section is kept as written (Save writes it back unchanged)
// and Err answers why; the issuers then refuse to run.
type Secrets struct {
	// Issuers by name. Each mints one kind of credential per box.
	Issuers map[string]SecretsIssuer `json:"issuers,omitempty"`

	raw json.RawMessage // as written, when it did not decode strictly
	err error
}

// secretsPlain is Secrets without its methods, for encoding/json.
type secretsPlain struct {
	Issuers map[string]SecretsIssuer `json:"issuers,omitempty"`
}

// UnmarshalJSON decodes strictly. It never fails the whole config: a section
// that does not decode is kept as written, with Err set.
func (s *Secrets) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p secretsPlain
	err := dec.Decode(&p)
	if err == nil && dec.More() {
		err = errors.New("trailing data")
	}
	if err != nil {
		*s = Secrets{raw: slices.Clone(b), err: fmt.Errorf("config.json secrets: %w", err)}
		return nil
	}
	*s = Secrets{Issuers: p.Issuers}
	return nil
}

// MarshalJSON writes a section that did not decode back as it was written.
func (s Secrets) MarshalJSON() ([]byte, error) {
	if s.err != nil && s.raw != nil {
		return s.raw, nil
	}
	return json.Marshal(secretsPlain{Issuers: s.Issuers})
}

// Err is why the section did not decode strictly (nil when it did).
func (s Secrets) Err() error { return s.err }

// SecretsIssuer is one issuer. Kind "github-app" mints github-token: one
// repository, the requested permissions, GitHub's hour, from a GitHub App
// named here (app_id, private_key, api_url) or by Use ("runners.github": the
// runner broker's App). Kind "potluck" mints potluck-token: a token of this
// pool's own, capped to one role on a box's project and area, for at most the
// box's lease; As names the identity a GitHub job's token acts as.
type SecretsIssuer struct {
	Kind       string `json:"kind,omitempty"`
	As         string `json:"as,omitempty"` // potluck: the identity a job's token acts as
	Use        string `json:"use,omitempty"`
	AppID      int64  `json:"app_id,omitempty"`
	PrivateKey string `json:"private_key,omitempty"` // secret reference
	APIURL     string `json:"api_url,omitempty"`     // default https://api.github.com
	// Ceiling is the most the issuer ever mints; nothing is the default.
	Ceiling []SecretsCeiling `json:"ceiling,omitempty"`
}

// SecretsCeiling is one rule of an issuer's ceiling: a repository
// (owner/name, or owner/* for every repository of owner) and the most each
// permission may be. With Workflows, the rule serves only a GitHub job of one
// of those workflow files on the repository's default branch.
//
// Events are the job events the rule serves (default push and schedule;
// workflow_dispatch and repository_dispatch only when listed). Elevated lists
// the elevated permissions (package issuers) the rule may grant at write;
// a rule with any needs a Reason.
//
// A potluck issuer's rule is instead a project (slug or ULID), optionally one
// area of it (none: any), and the most a token's role may be (Role); Repo,
// Permissions, Elevated and Reason are left unset.
type SecretsCeiling struct {
	Repo        string            `json:"repo,omitempty"`
	Permissions map[string]string `json:"permissions,omitempty"`
	Project     string            `json:"project,omitempty"`
	Area        string            `json:"area,omitempty"`
	Role        string            `json:"role,omitempty"`
	Workflows   []string          `json:"workflows,omitempty"`
	Events      []string          `json:"events,omitempty"`
	Elevated    []string          `json:"elevated,omitempty"`
	Reason      string            `json:"reason,omitempty"`
}

// RunnersPool is where served jobs run: box bindings of one project, by size.
type RunnersPool struct {
	Project     string            `json:"project,omitempty"`
	Sizes       map[string]string `json:"sizes,omitempty"` // optional: size -> the one box binding for it (else placed on any fitting box)
	DefaultSize string            `json:"default_size,omitempty"`
}

// Load resolves Home (HomeDir), reads config.json if present
// and fills identity defaults. It does not create Home; see EnsureHome.
func Load() (*Config, error) {
	home, err := HomeDir()
	if err != nil {
		return nil, err
	}
	c, err := LoadFrom(home)
	if err == nil {
		c.UserHome, _ = os.UserHomeDir()
	}
	return c, err
}

// LoadClient is Load for a CLI call: the daemon decides who the human is
// (its own Load), so Human here is only $ZM_HUMAN, config.json or $USER,
// never the user database (os/user costs about 1 ms per call on macOS, on
// every hook and every command).
func LoadClient() (*Config, error) {
	home, err := HomeDir()
	if err != nil {
		return nil, err
	}
	host := HostName
	if HostEnv != "" && os.Getenv(HostEnv) != "" {
		// A remote client ($ZM_HOST) never names a session on this machine:
		// no subprocess on its prompt path.
		host = shortHostName
	}
	return load(home, func() string { return os.Getenv("USER") }, host)
}

// LoadFrom is Load with an explicit Home (tests).
func LoadFrom(home string) (*Config, error) { return load(home, osUser, HostName) }

func load(home string, user func() string, host func() (string, bool)) (*Config, error) {
	c := &Config{Home: home}
	b, err := os.ReadFile(filepath.Join(home, FileConfig))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &c.File); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(home, FileConfig), err)
		}
	}
	if c.Human = first(os.Getenv(HumanEnv), c.File.Human); c.Human == "" {
		c.Human = user()
	}
	// The host name is looked up only when nothing else names the machine:
	// on macOS it is a subprocess, and hooks load the config on every prompt.
	switch {
	case Sanitize(os.Getenv(MachineEnv)) != "":
		c.Machine, c.MachineFrom = os.Getenv(MachineEnv), MachineFromEnv
	case Sanitize(c.File.Machine) != "":
		c.Machine, c.MachineFrom = c.File.Machine, MachineFromFile
	default:
		h, stable := host()
		c.Machine, c.MachineFrom = h, MachineFromHost
		if !stable {
			c.MachineFrom = MachineFromUnstable
		}
	}
	c.Human, c.Machine = Sanitize(c.Human), Sanitize(c.Machine)
	if MachineSuffix != "" && c.Machine != "" && (c.MachineFrom == MachineFromHost || c.MachineFrom == MachineFromUnstable) {
		c.Machine = Sanitize(strings.TrimRight(c.Machine[:min(len(c.Machine), 63-len(MachineSuffix))], "-._") + MachineSuffix)
	}
	if c.Human == "" {
		c.Human = "me"
	}
	if c.Machine == "" {
		c.Machine, c.MachineFrom = "localhost", MachineFromFallback
	}
	return c, nil
}

// HomeDir is the home in the order of the program's Names (ResolveHome),
// as an absolute path. A warning (two variables, two homes) goes to WarnTo
// once per process.
func HomeDir() (string, error) {
	uh, _ := os.UserHomeDir()
	r, err := ResolveHome(Wired(), os.Getenv, uh, isDir)
	if err != nil {
		return "", err
	}
	if r.Warning != "" && WarnTo != nil {
		warnOnce.Do(func() { fmt.Fprintln(WarnTo, "warning:", r.Warning) })
	}
	return r.Dir, nil
}

// EnsureHome creates Home with mode 0700.
func (c *Config) EnsureHome() error { return os.MkdirAll(c.Home, 0o700) }

// Save writes File to config.json (0600), creating Home. Keys File does
// not know (another zm version's) are kept.
func (c *Config) Save() error {
	b, err := json.Marshal(c.File)
	if err != nil {
		return err
	}
	set := map[string]any{}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(b, &known); err != nil {
		return err
	}
	for k, v := range known {
		set[k] = v
	}
	// File's keys left empty (omitempty) are removed, not kept from before.
	return c.writeKeys(set, fileKeys())
}

// fileKeys are the JSON names of File's fields.
func fileKeys() []string {
	t := reflect.TypeOf(File{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// Path joins name onto Home.
func (c *Config) Path(name string) string { return filepath.Join(c.Home, name) }

// RegistryPath is <home>/registry.db.
func (c *Config) RegistryPath() string { return c.Path(FileRegistry) }

// ProjectDir is <home>/projects/<ulid>.
func (c *Config) ProjectDir(id string) string { return filepath.Join(c.Home, DirProjects, id) }

// LeaseTick is how often leases are expired.
func (c *Config) LeaseTick() time.Duration {
	if c.File.LeaseTickSeconds > 0 {
		return time.Duration(c.File.LeaseTickSeconds) * time.Second
	}
	return 30 * time.Second
}

// BoundLeaseTTL is how long a bound lease outlives its session's last
// running tick.
func (c *Config) BoundLeaseTTL() time.Duration {
	if c.File.BoundLeaseTTLSeconds > 0 {
		return time.Duration(c.File.BoundLeaseTTLSeconds) * time.Second
	}
	return time.Hour
}

// LimitedTokenMaxTTL is the longest a limited token may live.
func (c *Config) LimitedTokenMaxTTL() time.Duration {
	if s := c.File.LimitedTokenMaxTTLSeconds; s > 0 {
		return time.Duration(min(s, int64(math.MaxInt64/time.Second))) * time.Second
	}
	return 30 * 24 * time.Hour
}

// IdleAfter is how long a managed session may print nothing before it is idle.
func (c *Config) IdleAfter() time.Duration {
	if c.File.IdleAfterSeconds > 0 {
		return time.Duration(c.File.IdleAfterSeconds) * time.Second
	}
	return 5 * time.Minute
}

// AgentNiceness is the nice value agent sessions start at: 0 none, else
// 1..19.
func (c *Config) AgentNiceness() int {
	switch n := c.File.AgentNice; {
	case n < 0:
		return 0
	case n == 0:
		return 10
	case n > 19:
		return 19
	default:
		return n
	}
}

// SocketPath is the socket a client dials in Home: the first of the
// program's socket names there, else its first (SocketIn).
func (c *Config) SocketPath() string { return SocketIn(Wired(), c.Home, exists) }

// ListenSocketPath is the socket the daemon listens on: the program's first
// socket name in Home.
func (c *Config) ListenSocketPath() string { return ListenSocketIn(Wired(), c.Home) }

// HumanAddress is "human:<Human>".
func (c *Config) HumanAddress() string { return "human:" + c.Human }

// MachineAddress is "machine:<Machine>".
func (c *Config) MachineAddress() string { return "machine:" + c.Machine }

var unsafeRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// Sanitize lowercases s and turns it into a valid name (api.ValidName),
// e.g. "Host-A-Laptop" -> "host-a-laptop".
func Sanitize(s string) string {
	s = unsafeRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-._")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-._")
	}
	return s
}

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// HostName is this machine's name as zm first sees it, and whether it is
// stable: on macOS the Bonjour name (scutil --get LocalHostName), which a
// network's DHCP never changes, unlike the host name (a Wi-Fi can rename a
// Mac to unknown42a411d299bc.attlocal.net); a Mac without one answers the
// short host name as unstable (never persisted). Elsewhere the short host
// name, stable. The daemon persists what it used in config.json at its first
// start (PersistMachine), so a later rename never changes the machine's name.
var HostName = func() (string, bool) {
	if runtime.GOOS == "darwin" {
		if h := localHostName(); Sanitize(h) != "" {
			return h, true
		}
		h, _ := shortHostName()
		return h, false
	}
	return shortHostName()
}

// localHostName is macOS's LocalHostName ("" when unset or scutil fails).
var localHostName = func() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", "LocalHostName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var osHostname = os.Hostname

func shortHostName() (string, bool) {
	h, _ := osHostname()
	h, _, _ = strings.Cut(h, ".")
	return h, true
}

// dhcpName matches the names macOS takes from a network that hands out
// host names (unknown + the interface's MAC): never a machine's name.
var dhcpName = regexp.MustCompile(`^unknown[0-9a-f]{12}$`)

// AdoptMachine is the daemon's first start on a home with no persisted name:
// prev is the name this home's daemon last ran as (its last daemon.started
// event). An install upgraded from a zm that named the machine after the host
// name keeps that name (its sessions, tailnet node and team joins use it),
// unless it was a DHCP name; a Mac whose stable name is unknown right now
// takes it rather than an unstable one. It reports whether it changed the
// name. ZM_MACHINE and a persisted name always win.
func (c *Config) AdoptMachine(prev string) bool {
	prev = Sanitize(prev)
	if prev == "" || prev == "localhost" || dhcpName.MatchString(prev) {
		return false // never a name to keep
	}
	if prev == c.Machine {
		if c.MachineFrom == MachineFromUnstable || c.MachineFrom == MachineFromFallback {
			c.MachineFrom = MachineFromPrevious // what it already ran as, and not a DHCP name: safe to keep
		}
		return false
	}
	switch c.MachineFrom {
	case MachineFromHost, MachineFromUnstable, MachineFromFallback:
		c.Machine, c.MachineFrom = prev, MachineFromPrevious
		return true
	}
	return false
}

// PersistMachine keeps the machine name stable (the daemon calls it at
// start). When config.json names no machine it records the name in use,
// unless that name is unstable (a Mac with no LocalHostName right now) or a
// fallback: those are tried again at the next start. ZM_MACHINE overriding
// a different persisted name wins for this run with a warning (sessions
// named under the persisted name will not match). note says what it did,
// for the daemon's log; warn marks it a warning.
func (c *Config) PersistMachine() (note string, warn bool, err error) {
	persisted := Sanitize(c.File.Machine)
	if persisted != "" {
		if c.MachineFrom == MachineFromEnv && persisted != c.Machine {
			return fmt.Sprintf("%s=%s overrides the machine name %q persisted in %s; sessions named %s/... will not match",
				MachineEnv, c.Machine, persisted, c.Path(FileConfig), persisted), true, nil
		}
		return "", false, nil
	}
	switch c.MachineFrom {
	case MachineFromUnstable, MachineFromFallback:
		return fmt.Sprintf("machine name %q is not stable (%s): not persisted; set %s or the Mac's local hostname (System Settings > General > Sharing)",
			c.Machine, c.MachineFrom, MachineEnv), true, nil
	}
	if err := c.writeKeys(map[string]any{"machine": c.Machine}, nil); err != nil {
		return "", false, err
	}
	c.File.Machine = c.Machine
	return fmt.Sprintf("machine name %q persisted in %s (from %s)", c.Machine, c.Path(FileConfig), c.MachineFrom), false, nil
}

// AddUpdateKey adds key to config update_keys unless it is there already,
// keeping every other key of config.json; added: it wrote.
func (c *Config) AddUpdateKey(key string) (added bool, err error) {
	if slices.Contains(c.File.UpdateKeys, key) {
		return false, nil
	}
	keys := append(slices.Clone(c.File.UpdateKeys), key)
	if err := c.writeKeys(map[string]any{"update_keys": keys}, nil); err != nil {
		return false, err
	}
	c.File.UpdateKeys = keys
	return true, nil
}

// writeKeys rewrites config.json keeping every key it has (an older or
// newer zm's included): set adds or replaces keys, del removes them. 0600,
// atomically.
func (c *Config) writeKeys(set map[string]any, del []string) error {
	if err := c.EnsureHome(); err != nil {
		return err
	}
	path := c.Path(FileConfig)
	doc := map[string]json.RawMessage{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	for _, k := range del {
		delete(doc, k)
	}
	for k, v := range set {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		doc[k] = raw
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func first(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// agentEnv are the variables an agent runtime sets in what it starts. They
// describe one running session, so anything that outlives or leaves that
// session must drop them. CLAUDE_CODE_CHILD_SESSION is the costly one: a Claude
// Code that inherits it believes it is a child and saves no transcript.
var agentEnv = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_EFFORT": true,
	"CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_ENTRYPOINT": true,
	"CLAUDE_CODE_EXECPATH": true, "CLAUDE_CODE_SESSION_ATTENDED": true,
	"CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true,
	"CODEX_THREAD_ID": true,
}

// IsAgentEnv reports whether k belongs to the agent session or terminal host
// that started this process, not to the machine. Claude Code's own settings
// (CLAUDE_CONFIG_DIR, CLAUDE_CODE_USE_BEDROCK and the like) are not on the list.
func IsAgentEnv(k string) bool { return agentEnv[k] || strings.HasPrefix(k, "CMUX_") }
