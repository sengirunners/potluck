package computeapi

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Run targets (SPEC 20): a binding lends a throwaway database or a run of a
// named script, through a zm-run-<driver> process.

// Run target kinds and isolation classes.
const (
	RunTargetKindDB  = "db"
	RunTargetKindRun = "run"
	RunTargetKindBox = "box"

	RunTargetIsolationNone      = "none"
	RunTargetIsolationEphemeral = "ephemeral"
)

// RunTarget is a [[run_targets]] binding in project.toml. Only the machine's
// owner on the socket without a token sets one.
type RunTarget struct {
	Name        string           `json:"name"`
	Driver      string           `json:"driver"`
	Kind        string           `json:"kind"`
	Isolation   string           `json:"isolation"`
	Credentials bool             `json:"credentials"`
	Repo        string           `json:"repo"`
	Branch      string           `json:"branch"`
	AllowAnyRef bool             `json:"allow_any_ref,omitempty"`
	Scripts     []string         `json:"scripts,omitempty"`
	Budget      *RunTargetBudget `json:"budget,omitempty"`
	TTLMaxS     int              `json:"ttl_max_s,omitempty"`
	HandlesMax  int              `json:"handles_max,omitempty"`
	CreateIn    []string         `json:"create_in,omitempty"`
	// Images are a box binding's owner-set images, each pinned by digest
	// (name@sha256:<hex>); the first is the default.
	Images []string `json:"images,omitempty"`
	// Config is the driver's own config (strings; a credential only as a
	// *_file key naming a 0600 file).
	Config map[string]string `json:"config,omitempty"`
	// Host, HostBinding and HostProject are set only on a binding at a team
	// authority that another machine serves (SPEC 20 "Across machines"): the
	// machine, its binding's own name, and the project's ULID. Name is
	// <HostBinding>@<Host>. Never in a project.toml.
	Host        string `json:"host,omitempty"`
	HostBinding string `json:"host_binding,omitempty"`
	HostProject string `json:"-"`
}

// HostRunTarget is a machine-level box binding in $ZM_HOME/run_targets.toml
// (SPEC 20 "Across machines"): a binding the machine's owner shares with the
// listed team projects (the authorities' project ids), advertised to each
// project's authority.
type HostRunTarget struct {
	RunTarget
	Share []string `json:"share"`
}

// RunTargetAdvert is what a host's heartbeat for one team project says of a
// binding shared with it: the binding's name and its describe, refreshed on
// every beat. The authority keeps the latest per machine.
type RunTargetAdvert struct {
	Binding  string            `json:"binding"`
	Describe RunTargetDescribe `json:"describe"`
}

// RunTargetOp is a machine op of kind run_target: one driver verb on a
// binding of the machine's, with its args, sent by a team authority.
type RunTargetOp struct {
	Binding string          `json:"binding"`
	Verb    string          `json:"verb"`
	ID      string          `json:"id,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	// Sealed replaces Args when Args hold secrets or a jit_config: Args
	// sealed to the host's certified key (SPEC 17.11 "Sealed secrets"),
	// over every transport; only the host opens it.
	Sealed string `json:"sealed,omitempty"`
	// SealedBy is the key id of the authority that sealed Sealed. A host
	// joined over the tailnet opens with it (the pinned node vouches for
	// the op); one joined over the tunnel uses the home key it pinned.
	SealedBy string `json:"sealed_by,omitempty"`
	// Sealing is 1 from an authority that seals every op holding secrets or
	// a jit_config: such an op's Args in the clear are refused by the host,
	// never run. An authority from before sealing on every leg sends none.
	Sealing int `json:"sealing,omitempty"`
}

// RunTargetBudget caps a binding's spend per calendar period (UTC).
type RunTargetBudget struct {
	Unit   string `json:"unit"`
	Limit  int64  `json:"limit"`
	Period string `json:"period"` // day | week | month
}

// Refusal is why a binding that spent spent this period takes no new handle
// ("" while under the limit). Acquire refuses with it; placement passes the
// binding over with it.
func (b RunTargetBudget) Refusal(spent float64) string {
	if spent < float64(b.Limit) {
		return ""
	}
	return fmt.Sprintf("spent %g of its %d %s this %s (budget)", spent, b.Limit, b.Unit, b.Period)
}

// RunTargetCost is describe's cost.
type RunTargetCost struct {
	Unit     string  `json:"unit"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency,omitempty"`
}

// RunTargetSpend is a handle's spend so far, in describe's cost unit.
type RunTargetSpend struct {
	Unit   string  `json:"unit"`
	Amount float64 `json:"amount"`
}

// RunTargetDescribe is a driver's describe answer for one binding.
type RunTargetDescribe struct {
	// ImageName is the host's named pin for the default image, independent of digest versions.
	ImageName string `json:"image_name,omitempty"`
	// Images (an advert's only) are the host binding's pinned images
	// (name@sha256:<hex>, its default first), so its authority keys a
	// dependency layer by the digest the host's box boots (runtarget/deps.go).
	Images []string `json:"images,omitempty"`
	// Potluck is the driver's protocol ("potluck/1"); Zm the same under its
	// old key ("zm/v0"), which a driver from before the rename sends alone.
	Potluck      string        `json:"potluck,omitempty"`
	Zm           string        `json:"zm,omitempty"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	Version      string        `json:"version"`
	Verbs        []string      `json:"verbs"`
	Available    bool          `json:"available"`
	Why          string        `json:"why,omitempty"`
	Isolation    string        `json:"isolation"`
	Credentials  bool          `json:"credentials"`
	Cost         RunTargetCost `json:"cost"`
	ReportsSpend bool          `json:"reports_spend"`
	TTLMaxS      int           `json:"ttl_max_s"`
	Capabilities []string      `json:"capabilities"`
	// Size is a box target's one size.
	Size *RunTargetSize `json:"size,omitempty"`
	// Slots are the binding's concurrent boxes for its OS as the driver
	// counts them; nil from a driver that does not say.
	Slots *RunTargetSlots `json:"slots,omitempty"`
	// Runners is what the driver's newest GitHub runner refresh found, one
	// entry per image of a binding whose boxes start a GitHub runner; empty
	// before a box has started one, and from a driver that does not say.
	Runners []protocol.RunnerStatus `json:"runners,omitempty"`
	// Host is the host switch as the driver reads it now; nil from a driver
	// that has none.
	Host *protocol.HostStatus `json:"host,omitempty"`
	// Swap (an advert's only) is the state of the host machine's swap, read
	// by its daemon, never a driver's word (hostswap: off, zram, encrypted,
	// plain-disk or unknown). The authority gives box secrets only to a host
	// whose state is safe; "" is a host on a build from before the field.
	Swap string `json:"swap,omitempty"`
}

// RunTargetSize is a box binding's size.
type RunTargetSize struct {
	Name     string `json:"name"`
	OS       string `json:"os"`
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb,omitempty"`
}

// FullNow says whether d is not available only for now: every slot is in
// use, or the host's own reclaim policy (host mode auto) holds new work. A
// box may be had there later by asking again (RefusedNoRoom). Any other
// reason it is not available (paused, closed, a driver that cannot read its
// image) is not. Full slots count only when Why is the driver's word for
// them (protocol.NoFreeSlot): a driver may report no free slot while some
// other reason is what keeps it unavailable, which waiting does not lift.
func (d *RunTargetDescribe) FullNow() bool {
	if d == nil || d.Available {
		return false
	}
	if h := d.Host; h != nil && h.State != "" && h.State != protocol.HostAccepting {
		return h.Mode == protocol.HostAuto // the reclaim policy's hold passes; the owner's switch (open, drain, closed) is theirs to lift
	}
	s := d.Slots
	return s != nil && s.Total > 0 && s.Free <= 0 && strings.HasPrefix(d.Why, protocol.NoFreeSlot(s.OS))
}

// RunTargetSlots is describe's slots: how many boxes of OS may run at
// once (Total) and how many more may start now (Free). Bindings that share
// a driver state directory share these slots.
type RunTargetSlots struct {
	OS    string `json:"os"`
	Total int    `json:"total"`
	Free  int    `json:"free"`
}

// RunTargetView is a binding as GET answers it: the binding, the class zm
// uses (the stricter of the binding's and describe's) and describe itself
// when the driver answered.
type RunTargetView struct {
	RunTarget
	Effective RunTargetClass     `json:"effective"`
	Describe  *RunTargetDescribe `json:"describe,omitempty"`
	Error     string             `json:"error,omitempty"`
	// HostClass is the binding's host operator class (trusted or public,
	// SPEC 8 "Operator class"): trusted for a binding on this machine.
	// Placement counts anything but trusted as public.
	HostClass string `json:"host_class,omitempty"`
	// BudgetSpent is a budgeted binding's spend this period, in its budget's
	// unit, as this machine recorded it (a remote binding's budget is its
	// host's: none here). Acquire refreshes live handles' figures first and
	// decides; placement reads this to pass a spent binding over.
	BudgetSpent *float64 `json:"budget_spent,omitempty"`
	// Host is the machine that serves the binding (a local binding: this
	// machine, trusted), for placement.
	Host string `json:"host,omitempty"`
	// Shared is a machine-level binding's share list as the owner reads it:
	// the slug of each project this machine is in, else the project's id.
	Shared []string `json:"share,omitempty"`
	// SetupNotes are what the driver's setup said it did (bind with setup).
	SetupNotes []string `json:"setup_notes,omitempty"`
	// Live is a binding's handles its driver lists as not ended, for the
	// machine's owner (GET /v0/run_targets?live=1, with host=1 for the
	// machine-level ones; a binding another machine serves has none).
	Live []string `json:"live,omitempty"`
	// Stopped are the handles in Live that are stopped (a kept disk, no
	// VM running), so they hold no cores.
	Stopped []string `json:"stopped,omitempty"`
	// ImagesBehind is set on a box binding of this machine, machine-level or
	// a project's own, that lends other digests of Potluck's own images than
	// the daemon's build pins (the owner's list, with or without host=1),
	// until the daemon rebinds it.
	ImagesBehind *ImagesBehind `json:"images_behind,omitempty"`
}

// ImagesBehind is a binding that lends other pins than its daemon's build:
// what it lends, what this build pins, and why it still lends what it has
// (being pulled, waiting for its boxes to end, a failed pull, or a move only
// its owner's run of host makes: a Mac's images, or one that would change
// more than the images).
type ImagesBehind struct {
	Lends   []string `json:"lends"`
	Current []string `json:"current"`
	Why     string   `json:"why"`
}

// RunTargetClass is an isolation class.
type RunTargetClass struct {
	Isolation   string `json:"isolation"`
	Credentials bool   `json:"credentials"`
}

// UntrustedCapable reports a class that may run anyone's code: each piece of
// work in a throwaway machine walled off from the host, its LAN and its
// tailnet, holding no credentials. Anything else (an isolation not exactly
// ephemeral included) may not.
func (c RunTargetClass) UntrustedCapable() bool {
	return c.Isolation == RunTargetIsolationEphemeral && !c.Credentials
}

// Guarded reports a class whose acquire is local-only and whose run target
// runs only its listed scripts: it holds credentials or has no isolation.
func (c RunTargetClass) Guarded() bool {
	return c.Credentials || c.Isolation == RunTargetIsolationNone
}

// NotIsolated says in plain words why c may not run untrusted code, "" when
// it may. Refusals and doctor say this, so they say it the same way.
func (c RunTargetClass) NotIsolated() string {
	var why []string
	if c.Isolation != RunTargetIsolationEphemeral {
		why = append(why, "work on it is not walled off from this machine, its LAN and its tailnet")
	}
	if c.Credentials {
		why = append(why, "work on it could read the credentials it holds")
	}
	return strings.Join(why, ", and ")
}

// CannotIsolate is the plain-words start of every refusal of untrusted work
// on a class that cannot isolate it: who refuses, and why.
func (c RunTargetClass) CannotIsolate(who string) string {
	return who + " cannot isolate untrusted code (" + c.NotIsolated() + ")"
}

// RunTargetList is GET /v0/run_targets.
type RunTargetList struct {
	Project string          `json:"project"`
	Targets []RunTargetView `json:"targets"`
}

// BindRunTargetRequest is POST /v0/run_targets.
type BindRunTargetRequest struct {
	Target RunTarget `json:"target"`
	// Replace allows rebinding an existing name (refused while it has live handles).
	Replace bool `json:"replace,omitempty"`
	// Setup runs the driver's one-time setup (a dry run first) before binding.
	Setup bool `json:"setup,omitempty"`
	// Host binds Target as a machine-level box binding in
	// $ZM_HOME/run_targets.toml instead of the project's project.toml,
	// shared with the team projects in Share: each a project id, or the slug
	// of a team project this machine is in (stored as its id).
	Host  bool     `json:"host,omitempty"`
	Share []string `json:"share,omitempty"`
}

// AcquireRunTargetRequest is POST /v0/run_targets/{target}/acquire. A run
// names ref (a pushed branch or tag) and script; zm resolves the commit.
type AcquireRunTargetRequest struct {
	Database  string   `json:"database,omitempty"` // box: project-scoped database recipe
	RequestID string   `json:"request_id"`
	Actor     string   `json:"actor,omitempty"`
	TTLS      int      `json:"ttl_s,omitempty"`
	Ref       string   `json:"ref,omitempty"`
	Commit    string   `json:"commit,omitempty"`
	Script    string   `json:"script,omitempty"`
	Args      []string `json:"args,omitempty"`
	Name      string   `json:"name,omitempty"`  // db
	Image     string   `json:"image,omitempty"` // box: one of the binding's images, by name
	// Template (box): one of zm's box templates (BoxTemplateImage), the
	// binding's image of that template's name; not with Image.
	Template string `json:"template,omitempty"`
	// JITConfig (box): a just-in-time GitHub runner config the box starts as
	// its first command (SPEC 20). A single-use credential: passed to the
	// driver, never stored, logged or answered.
	JITConfig string `json:"jit_config,omitempty"`
	// JITGuard (box, with JITConfig): which jobs the runner may serve; the
	// box stops any other before its first step (SPEC 20). Not a secret.
	JITGuard *JITGuard `json:"jit_guard,omitempty"`
	// Net (box): NetOff makes the box with no network at all, on a driver listing CapNetOff; NetOn or unset is
	// the default, the binding's filtered network on a trusted host and none
	// on a public one (SPEC 20 "Boxes", Network).
	Net string `json:"net,omitempty"`
	// Persist (box): keep the box's disk when it stops (SPEC 20 "Boxes",
	// Kept disks; zm-eakh.12.5.23), on a driver listing CapPersist and a
	// trusted host only. Unset is a throwaway box, wiped when it ends.
	Persist bool `json:"persist,omitempty"`
	// Volumes (box): named volumes of the holder's that outlive the box,
	// mounted at their paths (SPEC 20 "Boxes", Named volumes;
	// zm-eakh.12.32.1), on a driver listing CapVolumes and a trusted host
	// only. zm sends the driver the holder's identity as their owner.
	Volumes []BoxVolume `json:"volumes,omitempty"`
	// Deps (box): dependency layers (docs/daemon.md, Dependency layers): for
	// each, the daemon mounts the holder's sealed volume built from exactly
	// these files, cow at Path, building it first in a fresh box of the
	// holder's when there is none. Trusted hosts only; not with persist or
	// jit_config.
	Deps []BoxDeps `json:"deps,omitempty"`
	// Base asks for the holder's weekly Git push base on this binding.
	Base *BoxBase `json:"base,omitempty"`
	// DepsWaitS (with Deps) is how long the acquire waits while another box
	// builds a layer it needs (default DepsWaitDefaultS). 0 waits not at
	// all: the box comes without that layer (the answer's deps say so).
	DepsWaitS *int `json:"deps_wait_s,omitempty"`
	// SelfRenew: the caller renews this handle itself (renew, before its
	// TTL runs out), so the daemon's sweep never renews it for a live
	// holder. A client that dies then lets the handle end at its TTL, though
	// the session it ran under lives on. Unset keeps the sweep's renewal.
	SelfRenew bool `json:"self_renew,omitempty"`
}

// BoxBase identifies a repository by the SHA256 of its sorted root commit IDs.
// Fill asks for a restricted builder instead of a reader of the sealed base.
type BoxBase struct {
	Repo string `json:"repo"`
	Fill bool   `json:"fill,omitempty"`
}

// BoxBaseAnswer is present when an acquire asked for a base. Hit means a
// sealed, readonly base was mounted at Path. A fill also answers Path;
// otherwise Why explains why the box has no base.
type BoxBaseAnswer struct {
	Path string `json:"path,omitempty"`
	Hit  bool   `json:"hit"`
	Why  string `json:"why,omitempty"`
}

// BaseMissing is the acquire refusal reason when its base is not sealed.
const BaseMissing = "base_missing"

// BaseRefusal is a push-base acquire refusal made before a box exists.
type BaseRefusal struct {
	Reason string `json:"reason"`
}

// ReleaseRunTargetRequest optionally seals a base builder owned by the caller.
// The daemon chooses the files kept; ordinary handles cannot be sealed.
type ReleaseRunTargetRequest struct {
	Seal bool `json:"seal,omitempty"`
}

// DepsLayersLack is the first capability caps (a binding's describe) lacks
// for building dependency layers ("" when it has them all): named volumes
// that seal and are kept, and base64 exec to write the layer's files. Both
// placement (computeclient.PlaceBox) and the daemon's acquire judge by it.
func DepsLayersLack(caps []string) string {
	for _, c := range []string{protocol.CapVolumes, protocol.CapVolumesSeal, protocol.CapVolumesKeep, EncodingBase64} {
		if !slices.Contains(caps, c) {
			return c
		}
	}
	return ""
}

// BoxDeps is one dependency layer an acquire asks for. Kind names what is
// built and from which files (npm: package-lock.json, package.json and the
// package.json of every workspace the lockfile names, all relative to
// Path). Scripts DepsScriptsOff skips dependencies' install scripts; it is
// part of the layer's key.
type BoxDeps struct {
	Kind    string     `json:"kind"`
	Path    string     `json:"path"` // relative to the box user's HOME, as a volume's
	Scripts string     `json:"scripts,omitempty"`
	Files   []DepsFile `json:"files"`
}

// DepsFile is one input file of a layer, its path relative to the layer's.
type DepsFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

// BoxDepsAnswer is one layer in an acquire's answer: its key (the sha256
// its volume is named by), Hit when the box mounts a layer that was already
// built, Built when this acquire built it. Neither: the box came without it
// (DepsWaitS 0 while another box was building it).
type BoxDepsAnswer struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Hit   bool   `json:"hit"`
	Built bool   `json:"built,omitempty"`
}

// Dependency layer limits and settings.
const (
	DepsScriptsOff = "off"
	// DepsMax is how many layers one box may ask for; DepsFilesMax and
	// DepsBytesMax bound all their files together.
	DepsMax          = 4
	DepsFilesMax     = 2000
	DepsBytesMax     = 16 << 20
	DepsWaitDefaultS = 600
	DepsWaitMaxS     = 3600
)

// BoxVolume is one of a box acquire's named volumes. Mode is VolumeRW (the
// default: one box at a time), VolumeRO or VolumeCOW (both shared).
type BoxVolume struct {
	Name string `json:"name"`
	Path string `json:"path"` // relative to the box user's HOME (never absolute)
	Mode string `json:"mode,omitempty"`
	// New (an acquire's answer only): this acquire made the volume, empty.
	New bool `json:"new,omitempty"`
	// Sealed is a dependency layer's volume, read only once sealed
	// (protocol.CapVolumesSeal). The daemon asks it; a caller never does.
	Sealed bool `json:"sealed,omitempty"`
	// Checkout is a dependency layer's cow volume that holds the box's
	// checkout of the acquire's commit (protocol.CapVolumesSeal), so code
	// sits beside the layer's node_modules. The daemon asks it; a caller
	// never does.
	Checkout bool `json:"checkout,omitempty"`
}

// Named volume modes, and the capability of a driver that mounts them
// (protocol/volume.go, protocol/protocol.go).
const (
	VolumeRW   = protocol.VolumeRW
	VolumeRO   = protocol.VolumeRO
	VolumeCOW  = protocol.VolumeCOW
	CapVolumes = protocol.CapVolumes
	// VolumesMax is how many volumes one box may mount.
	VolumesMax = 8
)

// CapPersist is the capability of a driver that keeps a box's disk across
// stop and start (verbs stop and start).
const CapPersist = "persist"

// StateStopped is a persistent box that is stopped: its disk kept until
// ExpiresAt, nothing running, startable by its holder.
const StateStopped = "stopped"

// StartRunTargetRequest is POST .../start: a stopped box's new lease.
type StartRunTargetRequest struct {
	TTLS int `json:"ttl_s,omitempty"`
}

// CapRoot is the capability of a box target whose user has working sudo;
// CapDocker of one whose user can run docker (implies CapRoot). SPEC 20
// "Boxes", Root and Docker (zm-eakh.12.5.26).
const (
	CapRoot   = "root"
	CapDocker = "docker"
)

// A box acquire's Net, and the capability of a driver that honours it.
const (
	NetOn     = "on"
	NetOff    = "off"
	CapNetOff = "net-off"
)

// JITGuard is what a broker would serve: a job of Repo (owner/name) whose
// run was started by one of Events and not from a fork; no Events, any.
// OpenIfPrivate (with Events; driver capability
// protocol.CapJITGuardPrivate) also serves a job Events refuse, but only
// while the job's own event payload says the repository is private
// (repository.private true, repository.visibility "private"): the box
// checks, at the job's start, what the broker could only check at the mint.
// Run, when set, narrows it to that one run and holds the job until the
// door's ready marker (protocol.JITRun; driver capability
// protocol.CapJITGuardRun).
type JITGuard struct {
	Repo          string           `json:"repo"`
	Events        []string         `json:"events,omitempty"`
	OpenIfPrivate bool             `json:"open_if_private,omitempty"`
	Run           *protocol.JITRun `json:"run,omitempty"`
}

// RunTargetHandle is a handle's state as zm records it. URL (a db's) is
// only ever in the acquire answer.
type RunTargetHandle struct {
	Handle    string `json:"handle"`
	Target    string `json:"target"`
	Kind      string `json:"kind"`
	RequestID string `json:"request_id"`
	Actor     string `json:"actor"`
	// Area is the area the acquire was made in ("" = the whole project).
	Area   string `json:"area,omitempty"`
	State  string `json:"state"`
	Commit string `json:"commit,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Script string `json:"script,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	// Publish is the JSON a run's script left in its publish file, on an
	// ended run only (protocol.CapRunPublish); omitted when there was none.
	Publish   json.RawMessage `json:"publish,omitempty"`
	ExpiresAt string          `json:"expires_at"`
	TTLS      int             `json:"ttl_s,omitempty"`
	Log       string          `json:"log,omitempty"`
	URL       string          `json:"url,omitempty"`
	Spend     *RunTargetSpend `json:"spend,omitempty"`
	// Size and ImageDigest are a box's, from its acquire.
	Size        string `json:"size,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	// Net is a box's, in acquire's answer only (never stored): "on" when the
	// driver made it with a network, "off" when with none; unset when the
	// driver does not say.
	Net string `json:"net,omitempty"`
	// Persist is a box acquired with persist: stop keeps its disk. While it
	// is stopped, ExpiresAt is when the disk is deleted.
	Persist bool `json:"persist,omitempty"`
	// Volumes are a box's named volumes, in acquire's answer only (never stored).
	Volumes []BoxVolume `json:"volumes,omitempty"`
	// Deps are a box's dependency layers, in acquire's answer only.
	Deps []BoxDepsAnswer `json:"deps,omitempty"`
	Base *BoxBaseAnswer  `json:"base,omitempty"`
	// Started is when the box last started after a stop (its lifetime cap
	// counts from there).
	Started string `json:"started,omitempty"`
	// StartedSpend is the spend figure as of that start: what the box ran
	// before it (Spend, refreshed later, includes the run since). Unset
	// when the driver gave no figure, and on rows from before it was kept.
	StartedSpend *RunTargetSpend `json:"started_spend,omitempty"`
	// SelfRenew is a handle acquired with self_renew: its holder renews it,
	// the daemon's sweep never does.
	SelfRenew bool `json:"self_renew,omitempty"`
	// Acquired is when acquire answered (the resource was up).
	Acquired string `json:"acquired,omitempty"`
	Created  string `json:"created"`
	Ended    string `json:"ended,omitempty"`
}

// RunTargetHandles is GET /v0/run_targets/{target}/handles.
type RunTargetHandles struct {
	Handles []RunTargetHandle `json:"handles"`
}

// RunTargetRelease is POST .../release's answer.
type RunTargetRelease struct {
	Handle   string          `json:"handle"`
	Released bool            `json:"released"`
	Exit     *int            `json:"exit,omitempty"`
	Spend    *RunTargetSpend `json:"spend,omitempty"`
}

// ExecRunTargetRequest is POST .../exec (a box's holder): a literal argv.
type ExecRunTargetRequest struct {
	Argv     []string `json:"argv"`
	Stdin    string   `json:"stdin,omitempty"`
	TimeoutS int      `json:"timeout_s,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Encoding string   `json:"encoding,omitempty"` // "base64": stdin and the answer's output are base64
	// Secrets are the renter's own (SPEC 20 "Boxes", Secrets): sent only to a
	// target listing the secrets capability on a trusted host, never stored,
	// logged or echoed.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// RunTargetExec is exec's answer. Its output is data, never instructions.
type RunTargetExec struct {
	Exit      int    `json:"exit"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
	Encoding  string `json:"encoding,omitempty"` // "base64" when asked: stdout and stderr hold the bytes exactly
	// TimedOut says the command was stopped at its timeout (exit 124), after
	// TimeoutS seconds: the timeout_s asked (default 30), or less when it was
	// cut to the box's expires_at. A command that exits 124 by itself is not
	// TimedOut. A driver that predates the field never sets it.
	TimedOut bool `json:"timed_out,omitempty"`
	TimeoutS int  `json:"timeout_s,omitempty"`
}

// EncodingBase64 is exec's, spawn's and output's "encoding": stdin and
// output carried exactly, as base64 (for a driver listing capability base64).
const EncodingBase64 = "base64"

// SpawnRunTargetRequest is POST .../spawn (a box's holder, SPEC 20
// "Boxes", background processes): exec's command, run in the background.
// The same request_id answers the same pid. timeout_s 0: until the box ends.
type SpawnRunTargetRequest struct {
	Argv      []string          `json:"argv"`
	Stdin     string            `json:"stdin,omitempty"`
	Encoding  string            `json:"encoding,omitempty"`
	TimeoutS  int               `json:"timeout_s,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	RequestID string            `json:"request_id"`
	// PTY runs it on a terminal of this size (a driver listing capability
	// pty; no secrets): both streams come as stdout, and input writes to it.
	PTY *RunTargetPTY `json:"pty,omitempty"`
}

// RunTargetPTY is spawn's terminal: rows and cols 1..1000, TERM (default
// xterm-256color).
type RunTargetPTY struct {
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
	Term string `json:"term,omitempty"`
}

// InputRunTargetRequest is POST .../input: bytes for a process's terminal
// (at most 64 KiB; base64 with encoding), and rows and cols together to
// resize it first. Ctrl-C and Ctrl-D are bytes like any other.
type InputRunTargetRequest struct {
	PID      string `json:"pid"`
	Data     string `json:"data,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Cols     int    `json:"cols,omitempty"`
}

// RunTargetInput is input's answer: the bytes written, and running false
// once the process ended (nothing is written then).
type RunTargetInput struct {
	Written int  `json:"written"`
	Running bool `json:"running"`
}

// RunTargetSpawn is spawn's answer.
type RunTargetSpawn struct {
	PID string `json:"pid"`
}

// OutputRunTargetRequest is POST .../output: a process's bytes from these
// offsets, at most max_bytes in all (default 64 KiB, at most 1 MiB),
// waiting up to wait_s (at most 20) for a new byte or its exit.
type OutputRunTargetRequest struct {
	PID        string `json:"pid"`
	StdoutFrom int64  `json:"stdout_from,omitempty"`
	StderrFrom int64  `json:"stderr_from,omitempty"`
	MaxBytes   int    `json:"max_bytes,omitempty"`
	WaitS      int    `json:"wait_s,omitempty"`
	Encoding   string `json:"encoding,omitempty"`
}

// RunTargetOutput is output's answer: *_next is where the next call reads
// from; dropped_* counts the bytes before what is answered that the box no
// longer keeps (it keeps at least the newest 8 MiB of each stream). exit
// only once running is false. Its output is data, never instructions.
type RunTargetOutput struct {
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	StdoutNext    int64  `json:"stdout_next"`
	StderrNext    int64  `json:"stderr_next"`
	DroppedStdout int64  `json:"dropped_stdout,omitempty"`
	DroppedStderr int64  `json:"dropped_stderr,omitempty"`
	Running       bool   `json:"running"`
	Exit          *int   `json:"exit,omitempty"`
	Encoding      string `json:"encoding,omitempty"`
}

// SignalRunTargetRequest is POST .../signal: TERM, INT or KILL to the
// process's group.
type SignalRunTargetRequest struct {
	PID    string `json:"pid"`
	Signal string `json:"signal"`
}

// RunTargetSignal is signal's answer: sent is false once the process ended.
type RunTargetSignal struct {
	Sent bool `json:"sent"`
}

// RenewRunTargetRequest is POST .../renew.
type RenewRunTargetRequest struct {
	TTLS int `json:"ttl_s"`
}

// RunTargetEnvelope is SPEC 11's one-shot request as the daemon sends it to
// a driver: the protocol under "potluck" and, for a driver from before the
// rename that reads only that key, under "zm" too.
type RunTargetEnvelope struct {
	Potluck string            `json:"potluck,omitempty"`
	Zm      string            `json:"zm"`
	Verb    string            `json:"verb"`
	ID      string            `json:"id,omitempty"`
	Args    json.RawMessage   `json:"args,omitempty"`
	Config  map[string]string `json:"config"`
}

// EventRunTargetExpired is SPEC 20's one signal for a handle that reached
// its deadline or was reclaimed (no mail: a run reaching its TTL is normal).
// Subject is the handle; data a RunTargetLease.
const EventRunTargetExpired = "dev.zm.run_target.expired"

// RunTargetLease is EventRunTargetExpired's data: the handle as a lease.
// Holder is the actor that acquired it; Note its final state (expired or
// reclaimed).
type RunTargetLease struct {
	Key     string     `json:"key"`
	Project string     `json:"project"`
	Area    string     `json:"area,omitempty"`
	Holder  string     `json:"holder"`
	Note    string     `json:"note,omitempty"`
	Claimed time.Time  `json:"claimed"`
	Renewed time.Time  `json:"renewed"`
	Expires *time.Time `json:"expires,omitempty"`
}

// Sealing modes of a pool (SPEC 17.11 "Sealed secrets"): how its machine
// treats a peer that names no seal key (a Potluck or zm from before sealing
// on the tailnet).
const (
	// SealingPreferred (a pool with no row; a new pool starts required): seal to every peer that can; to one
	// that cannot, over the tailnet, send as before (in the clear) and warn.
	SealingPreferred = "preferred"
	// SealingRequired: refuse secrets and jit_config to and from a peer that
	// cannot seal.
	SealingRequired = "required"
)

// Sealing is GET /v0/sealing: the pool's mode and the machines its machine
// heard from since it started, with whether each names a seal key.
type Sealing struct {
	Project string        `json:"project"`
	Mode    string        `json:"mode"`
	Peers   []SealingPeer `json:"peers"`
	// CannotSeal are the peers that name no seal key (the pre-check for
	// required: empty means every machine heard from can seal).
	CannotSeal []string `json:"cannot_seal"`
	// Since is when this machine started hearing: a machine not heard from
	// since is not listed.
	Since time.Time `json:"since"`
}

// SealingPeer is one machine of the pool as its machine last heard it.
type SealingPeer struct {
	Machine string    `json:"machine"`
	Seals   bool      `json:"seals"`
	Lends   bool      `json:"lends"`
	Seen    time.Time `json:"seen"`
}

// SetSealingRequest is PUT /v0/sealing (admin on the pool). Required is
// refused (409, naming them) while a machine heard from cannot seal, unless
// Force.
type SetSealingRequest struct {
	Mode  string `json:"mode"`
	Force bool   `json:"force,omitempty"`
}

// EventSealingChanged is published on every change of a pool's sealing mode
// (SPEC 17.11): who changed it, from what, to what.
const EventSealingChanged = "dev.zm.sealing.changed"

// SealingChanged is EventSealingChanged's data.
type SealingChanged struct {
	Project string `json:"project"`
	From    string `json:"from"`
	To      string `json:"to"`
	By      string `json:"by"`
	Forced  bool   `json:"forced,omitempty"`
}

// ForgetSealedHomeRequest is POST /v0/sealing/forget (this machine's owner,
// on the socket): forget that the pool's machine of Pool named a seal key,
// after its owner put it back to a build from before sealing on purpose.
type ForgetSealedHomeRequest struct {
	Pool string `json:"pool"`
}

// ForgetSealedHome is its answer.
type ForgetSealedHome struct {
	Pool   string `json:"pool"`
	Forgot bool   `json:"forgot"`
}

// RunTargetAcquireProgress is GET /v0/run_targets/{target}/acquires/{request_id}:
// how far the caller's acquire with that request_id got. State is
// protocol.AcquireStarting, Ready, Failed or Cancelled. Phase is a starting
// acquire's step: "queued" while it waits its turn on the binding, then the
// driver's word, empty from a driver that does not report progress. Message
// is a starting acquire's only. Handle is set once the daemon holds the
// acquire's handle (it may have ended since: its status says).
type RunTargetAcquireProgress struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	Phase     string `json:"phase,omitempty"`
	Message   string `json:"message,omitempty"`
	Handle    string `json:"handle,omitempty"`
}

// RunTargetAcquireCancel is POST /v0/run_targets/{target}/acquires/{request_id}/cancel.
// Cancelled is true when the acquire is stopped (now, or by an earlier
// cancel, before it was sent, or after it failed: it is then refused when
// it comes); false only when it already made its resource (Handle, when the
// daemon holds it: release that if it is still live).
type RunTargetAcquireCancel struct {
	RequestID string `json:"request_id"`
	Cancelled bool   `json:"cancelled"`
	State     string `json:"state"`
	Handle    string `json:"handle,omitempty"`
}
