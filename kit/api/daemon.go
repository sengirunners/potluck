package api

import "time"

// Swaps (SPEC 2, "Swaps"): what a daemon serves, what a build can serve,
// the preflight that compares them, the self-check and the swap record.

// EventDaemonUpdateChecked reports a host pull check (including holds/failures).
// Data: SwapRecord with what=update. It does not replace the last swap.
const EventDaemonUpdateChecked = "dev.zm.daemon.update_checked"

// EventDaemonSwapped (machine-level): a stop, restart or upgrade was recorded.
// Data: SwapRecord.
const EventDaemonSwapped = "dev.zm.daemon.swapped"

// EventDaemonSelfCheck (machine-level): the self-check found problems, or
// cleared them. Data: SelfCheck.
const EventDaemonSelfCheck = "dev.zm.daemon.selfcheck"

// BuildManifest is what one zm build can serve. Schema maps a database kind
// (registry, project, issues, replica) to the migration names it registers.
type BuildManifest struct {
	Protocol string              `json:"protocol"`
	Version  string              `json:"version"`
	Commit   string              `json:"commit"`
	Routes   []string            `json:"routes"`
	Proxied  []string            `json:"proxied"` // route patterns its team proxy forwards for a joined project
	Schema   map[string][]string `json:"schema"`
}

// DaemonManifest is GET /v0/daemon/manifest: the running daemon's build and
// what this home has it serve.
type DaemonManifest struct {
	Build BuildManifest `json:"build"`
	// Exe and Args are how the daemon was started (a restart reuses them).
	Exe  string   `json:"exe"`
	Args []string `json:"args"`
	// Used are the route patterns answered since the daemon started.
	Used []string `json:"used"`
	// Joined are the slugs of the team projects this machine joined.
	Joined []string `json:"joined"`
	// Applied maps a database file (relative to ZM_HOME) to its migrations.
	Applied map[string][]string `json:"applied"`
}

// Preflight is `zm daemon preflight --json`: whether this build can take
// over from the running daemon (or start on this home).
type Preflight struct {
	OK       bool     `json:"ok"`
	Build    string   `json:"build"`             // this build: version (commit)
	Running  string   `json:"running,omitempty"` // the running daemon's build, "" when none answers
	Problems []string `json:"problems"`
	Warnings []string `json:"warnings"`
	Notes    []string `json:"notes,omitempty"`
}

// ServeItem is one thing a daemon serves: Kind is joined, issues, work or
// adapter; Name is a project slug, or slug/prefix for an adapter.
type ServeItem struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// Key is the item's identity across two probes.
func (i ServeItem) Key() string { return i.Kind + " " + i.Name }

// ServeCheck is a probe of a daemon: what it served at Checked. Projects
// are every project slug it lists (local and joined), so an item of a
// project that left is not expected again.
type ServeCheck struct {
	Checked  time.Time   `json:"checked"`
	Version  string      `json:"version,omitempty"`
	Commit   string      `json:"commit,omitempty"`
	Projects []string    `json:"projects"`
	Items    []ServeItem `json:"items"`
}

// SelfCheck is the daemon's own check at start (and every minute while it
// finds problems).
type SelfCheck struct {
	OK       bool      `json:"ok"`
	Checked  time.Time `json:"checked"`
	Problems []string  `json:"problems,omitempty"`
}

// SwapRecord says who stopped, restarted or upgraded the daemon, from what
// build to what, and how it ended.
type SwapRecord struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`             // the caller's zm address (the daemon fills it)
	What   string    `json:"what"`           // stop | restart | upgrade | update
	From   string    `json:"from,omitempty"` // version (commit) before
	To     string    `json:"to,omitempty"`   // version (commit) after
	Exe    string    `json:"exe,omitempty"`
	Result string    `json:"result"` // ok | rolled_back | rolled_forward | failed; update also held | unchanged
	Forced bool      `json:"forced,omitempty"`
	// Overridden are the preflight problems --force overrode.
	Overridden []string `json:"overridden,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// MachineZm is the zm build a machine runs and its release channel state
// (SPEC 2 "Release channel"), in its machine numbers: Z3's Machines panel
// shows it, and another machine's updater reads Stable and Failed before it
// takes a release (the canary gate).
type MachineZm struct {
	Version string    `json:"version"`
	Commit  string    `json:"commit"`
	Started time.Time `json:"started"` // this daemon's start
	// Release is the channel release this home installed last, while the
	// running build is it; Installed is when.
	Release   string    `json:"release,omitempty"`
	Installed time.Time `json:"installed,omitzero"`
	// Held is set while the hold switch is on: its reason, else "held".
	Held      string    `json:"held,omitempty"`
	HeldSince time.Time `json:"held_since,omitzero"`
	// Last is the last update check: result held, unchanged, failed or ok.
	Last *UpdateCheck `json:"last,omitempty"`
	// Stable are the releases this machine ran for 20 minutes with no
	// daemon restart (it vouches for them); Failed are those whose install
	// failed here (a veto). Newest first.
	Stable []string `json:"stable,omitempty"`
	Failed []string `json:"failed,omitempty"`
	// Job is the update job this daemon can start on request (the program
	// reports it through core.Deps.UpdateJob). Nil means this daemon knows
	// no update job it can start, its build is from before the field, or
	// the authority reading it is (a lenient one drops the field, a strict
	// one refuses it and the daemon leaves it out for an hour).
	Job *UpdateJob `json:"job,omitempty"`
}

// UpdateJob is a machine's update job as its daemon reads it each time it
// builds its machine numbers.
type UpdateJob struct {
	// MinAgeS is the minimum age, in seconds since publication, a release
	// must reach before the job takes it. Nil means unknown, never zero; a pointer to 0
	// means the job takes a release at once.
	MinAgeS *int64 `json:"min_age_s,omitempty"`
}

// UpdateCheck is the gist of an update check's SwapRecord.
type UpdateCheck struct {
	At     time.Time `json:"at"`
	Result string    `json:"result"`
	To     string    `json:"to,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// SwapHolder is the body of $ZM_HOME/swap.lock while a swap runs.
type SwapHolder struct {
	PID   int       `json:"pid"`
	By    string    `json:"by"`
	What  string    `json:"what"`
	Since time.Time `json:"since"`
	Nonce string    `json:"nonce,omitempty"`
}
