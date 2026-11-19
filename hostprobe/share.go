package hostprobe

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/conformance"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
)

// Binder is the client's one call share needs (computeclient.Owner has it; the
// machine's owner only, SPEC 20).
type Binder interface {
	BindRunTarget(ctx context.Context, req computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error)
}

// Pinned reports whether an image is digest-pinned.
func Pinned(image string) bool { return imagepins.Pinned(image) }

// NotReadyError is a layer that cannot lend yet: what is wrong and the one
// root command that fixes what root can fix.
type NotReadyError struct {
	Layer       string
	Problems    []string
	RootCommand string
}

func (e *NotReadyError) Error() string {
	s := fmt.Sprintf("layer %s is not ready: %s", e.Layer, strings.Join(e.Problems, "; "))
	if e.RootCommand != "" {
		s += "\nrun as root, then try again:\n  " + e.RootCommand
	}
	return s
}

// ShareOptions is `zm host share`.
type ShareOptions struct {
	// Pool, when set, binds each size machine-level (run_targets.toml) and
	// shares it with that team project, instead of binding it in the
	// resolved project (SPEC 20 "Across machines").
	Pool   string
	Report Report
	// StateDir overrides the driver's state directory; empty keeps its default.
	// Report.StateDir is only the disk-space probe path.
	StateDir   string
	Layer      string // "" = the best available supported layer
	Images     []string
	With       string   // named image, selected alone instead of the default
	Sizes      []string // names; none = every size that fits
	Limits     Limits
	NamePrefix string // required: bindings are <prefix>-<size>
	OCIRuntime string // override the detected runtime
	OCICLI     string // override the detected CLI
	OCINetwork string // optional: a network the owner made for zm
	// Docker (oci) lends boxes with root (sudo) and rootless Docker inside
	// (sudo.go): bound with docker=on on the layer's Sudo runtime, whose
	// root steps (SudoSteps) share runs first. Unless DockerSet (the flag
	// was given: --docker=false turns it off), a re-run keeps an earlier
	// share's (Earlier, sudo.go docker()).
	Docker, DockerSet bool
	// Root (tart) lends boxes whose user keeps the image's passwordless
	// sudo, bound root=on (zm-eakh.12.54.14): root inside a throwaway VM,
	// as GitHub's own macOS runners give, which conformance's root group
	// then checks is still fenced. Unless RootSet (--root=false turns it
	// off), a re-run keeps an earlier share's root=on; otherwise the image pin decides.
	Root, RootSet bool
	// RunnerDir is where the images keep GitHub's runner, bound as each
	// box's runner_dir so it serves Actions jobs: "" takes it from
	// images.pin for an image zm pins (ImageRunnerDir), "none" binds no
	// runner. On oci a runner box needs a network (OCINetwork, else zm's
	// filtered one when doctor finds it): with none, share binds jobs only
	// and says why.
	RunnerDir string
	Driver    string // the zm-run-<driver> to bind; default "tart"
	// DriverDirs, when set, are the directories the daemon runs drivers
	// from (protocol.DriverDirs): conformance then runs the driver file
	// found there, the one the daemon will pin, not a name looked up on
	// PATH. Unset, the suite gets the name (zm's callers before pins).
	DriverDirs  []string
	Conformance string // the suite's binary; default potluck-conformance, else zm-run-conformance (defaultSuite)
	DryRun      bool
	// ShowCommands: a dry run's text also prints each conformance command line.
	ShowCommands bool
	// Full runs the whole conformance suite on every size. Without it share
	// runs QuickGroups once, on the first size, and SizeGroups on each other
	// size: the security checks do not depend on the size, only the boot does.
	Full bool
	// ReclaimIdleS > 0 binds each size with reclaim_idle_s: a box ends as
	// reclaimed (never billed) when the owner touches the keyboard or pointer,
	// and none starts until the Mac has been idle this long. macOS only.
	// Unless ReclaimSet (the owner chose it for this run), a re-run keeps
	// what an earlier share of these bindings bound (reclaim.go).
	ReclaimIdleS int
	ReclaimSet   bool
	// ConformanceTimeout bounds one size's suite run (default 15 minutes).
	ConformanceTimeout time.Duration
	// PullTimeout bounds one image pull (default: the pin's pull_minutes,
	// else 60 minutes). A pull streams its output to Progress while it runs.
	PullTimeout time.Duration
	// Progress, when set, hears each step in plain words.
	Progress func(string)
	// NetAllow and NetForbid (host:port) go to each size's conformance run
	// as --net-allow and --net-forbid; share adds --net-peers and, for each
	// of HostAddrs (this machine's own addresses, HostAddrs()), a
	// --net-forbid on a port it listens on during the runs. NetMail (an
	// outside mail server: a host, or host:port) goes as --net-mail: no box
	// may open it (zm-eakh.12.5.13).
	NetAllow, NetForbid, HostAddrs, NetMail []string
	// NetMbit, with NetMbitSet (--net-mbit was given), caps what all boxes
	// on zm's filtered network download together, in Mbit/s (0: no cap), for
	// a machine whose owner guards its uplink (boxnet.go). It is a root
	// step (RootSteps) kept by the network's unit, so unset keeps what is
	// installed.
	NetMbit    int
	NetMbitSet bool
	// ShelfGB (--shelf-gb), ShelfFill (--shelf-fill) and VolumeMaxGB
	// (--volume-max-gb) set the oci storage keys shelf_max_gb, shelf_fill and
	// volume_max_gb on every size bound (ocistorage.go). Unset (nil, ""), an
	// earlier binding's value is kept.
	ShelfGB, VolumeMaxGB *int
	ShelfFill            string
	// LinuxSlots (--linux-slots) is how many Linux boxes the share runs at
	// once, bound as linux_slots on every size (slots.go). Unset (nil), an
	// earlier binding's is kept, else the driver's default, Slots.
	LinuxSlots *int
	// AdmissionURL, AdmissionTokenFile and AdmissionTTLS are zm-run-tart's
	// admission keys for a host that runs other work beside zm (admission.go):
	// every box lent takes a lease first; share's own conformance boxes do
	// not. "" keeps an earlier binding's (Earlier); NoAdmission removes them.
	AdmissionURL, AdmissionTokenFile string
	OCIHostProfile                   string // absolute owner profile; the driver verifies runtime identity and host budgets
	AdmissionTTLS                    int
	// Earlier are this machine's bindings now, which a re-run reads its
	// admission keys from (zm host share lists them).
	Earlier []computeapi.RunTarget
	// EarlierUnread is why Earlier could not be read in full ("": it was).
	EarlierUnread string
}

// QuickGroups are the conformance groups share runs by default, once: every
// security check (a new box sees nothing of the last one, renter secrets and
// their refusals, refused images, shell strings and foreign handles, the
// network: peers, this machine's addresses, --net-forbid, outbound mail, and
// box secrets' isolation: modes, a memory filesystem, no swap, the owner, a
// planted path, no value in env, argv or the driver's state, gone after
// release), port-channel isolation and full-duplex progress, and the package
// shelf (no box writes it or reaches another through it; it skips where the
// host has no shelf), and database recipe identity refusals. What it
// drops is slow and the same on every size: renew and
// expiry wait out short leases (the host's TTL is the driver's own code, the
// same for each size), box-secrets-ops waits out a 5 s reader, and code,
// ended and reclaim are skipped for a share's boxes anyway.
const QuickGroups = "handles,lifecycle,refuse,net,root,box-secrets,ports,shelf,database"

// SizeGroups is what every other size runs by default: a box of that size
// boots, answers its size and exec, and releases.
const SizeGroups = "boot"

// DefaultReclaimIdleS is how long a Mac's owner must leave the keyboard and
// pointer alone before a lent box may start (`zm host share` on macOS).
const DefaultReclaimIdleS = 300

// Step is one planned or finished action.
type Step struct {
	What    string   `json:"what"`
	Command []string `json:"command,omitempty"`
}

// SizeResult is one size's outcome.
type SizeResult struct {
	Size        Size              `json:"size"`
	Binding     string            `json:"binding"`
	Config      map[string]string `json:"config"`
	Conformance []string          `json:"conformance_command"`
	Passed      bool              `json:"passed"`
	Bound       bool              `json:"bound"`
	Error       string            `json:"error,omitempty"` // why it stayed unbound
	// StillBound: this run failed, but a binding of the name from an earlier
	// run remains.
	StillBound bool   `json:"still_bound,omitempty"`
	Output     string `json:"output,omitempty"`
}

// ShareResult is what Share did, or in a dry run would do.
type ShareResult struct {
	Layer  string `json:"layer"`
	Driver string `json:"driver"`
	DryRun bool   `json:"dry_run"`
	// ShowCommands is the dry run's text output only; the JSON always has
	// every size's conformance_command.
	ShowCommands bool `json:"-"`
	Full         bool `json:"full"`
	// ReclaimIdleS is the reclaim policy bound (0: lent all the time).
	ReclaimIdleS int `json:"reclaim_idle_s,omitempty"`
	// ReclaimFrom names the earlier binding ReclaimIdleS was kept from ("":
	// this run's choice).
	ReclaimFrom string `json:"reclaim_idle_s_kept_from,omitempty"`
	// Images are the pins pulled, checked and bound: the owner's --image, else
	// the layer's default from images.pin (DefaultImage true).
	Images       []string `json:"images"`
	DefaultImage bool     `json:"default_image,omitempty"`
	NamedImage   string   `json:"named_image,omitempty"`
	// RunnerDir is the runner_dir bound (the boxes serve GitHub Actions
	// jobs), "" for boxes that serve jobs only; Network the oci network
	// bound ("" or "none": no network).
	RunnerDir string `json:"runner_dir,omitempty"`
	Network   string `json:"oci_network,omitempty"`
	// Admission is the admission keys bound, AdmissionFrom the earlier
	// binding they were kept from ("" for the flags).
	Admission     map[string]string `json:"admission,omitempty"`
	AdmissionFrom string            `json:"admission_kept_from,omitempty"`
	// Notes are what the owner should know of the result, in plain words
	// (why the boxes serve no Actions, and the root step that would fix it).
	Notes []string `json:"notes,omitempty"`
	// OneAtATime: no size fits twice inside zm's default share, so the
	// sizes are those that fit once, each bound to run one box at a time.
	OneAtATime bool `json:"one_at_a_time,omitempty"`
	// Docker: the boxes have root and docker (sudo.go); DockerFrom names the
	// earlier binding it was kept from ("" for the flag).
	Docker     bool   `json:"docker,omitempty"`
	DockerFrom string `json:"docker_kept_from,omitempty"`
	// Root: the tart boxes keep their image's passwordless sudo (root=on);
	// RootFrom names the earlier binding it was kept from ("" for the flag).
	Root     bool   `json:"root,omitempty"`
	RootFrom string `json:"root_kept_from,omitempty"`
	// LinuxSlots is the linux_slots bound on every size (0: none, the
	// driver's default of 2); LinuxSlotsFrom names the earlier binding it was
	// kept from ("" for the flag). LinuxSlotsByAdmission: the admission
	// controller decides room, so the sizes did not have to fit LinuxSlots
	// times inside the share.
	LinuxSlots            int    `json:"linux_slots,omitempty"`
	LinuxSlotsFrom        string `json:"linux_slots_kept_from,omitempty"`
	LinuxSlotsByAdmission bool   `json:"linux_slots_by_admission,omitempty"`
	// Storage is the shelf and volume keys bound on every oci size, nil when
	// none is set (ocistorage.go).
	Storage *OCIStorage  `json:"storage,omitempty"`
	Pulls   []Step       `json:"pulls"`
	Sizes   []SizeResult `json:"sizes"`
}

// RootSteps is the root steps a share needs first: doctor's
// (Report.RootSteps) for the layer it uses (o.Layer, else the best
// available, else each supported one), less a layer's network steps when
// no box would use zm's network (runnerOn). zm host share runs them with
// sudo, RootScript, at a terminal.
func (o ShareOptions) RootSteps() []string {
	layer := o.Layer
	if b, ok := o.Report.Best(); ok && layer == "" {
		layer = b.Name
	}
	steps := o.Report.RootSteps(layer)
	for _, l := range o.Report.Layers {
		l = o.dockerRuntime(l)
		if len(l.NetworkSteps) > 0 && !o.runnerOn(l.Name) {
			steps = slices.DeleteFunc(steps, func(s string) bool { return slices.Contains(l.NetworkSteps, s) })
		}
		if l.Name == "oci" && (layer == "" || layer == "oci") {
			if capSteps := o.netCapSteps(l); len(capSteps) > 0 { // in place of the network's own: they make it capped
				steps = append(slices.DeleteFunc(steps, func(s string) bool { return slices.Contains(l.NetworkSteps, s) }), capSteps...)
			}
		}
		if on, _ := o.docker(); on && l.Name == "oci" && (layer == "" || layer == "oci") {
			steps = append(steps, l.SudoSteps...)
			if o.kataGuardNeeded(l) {
				steps = append(steps, l.GuardSteps...)
			}
		}
	}
	return dedupe(steps)
}

// Precheck is what Check refuses that no root step can fix, for asking
// root only when share will go on: Check's dry run, and when that stops at
// a layer that is not ready (what the root step may fix), Check's dry run
// again with that layer as the root step would leave it (ready; on oci with
// the CLI detected, else asked for, else docker, and the runtime asked for,
// else runsc), so every later
// refusal (the default image, --oci-network without --net-mail, the sizes)
// still comes first. run is Check's: a dry run only reads with it (Tart's
// image list, so a cached image is not counted as a pull), so Precheck and
// Share give the same answer.
func Precheck(ctx context.Context, run Runner, o ShareOptions) error {
	o.DryRun, o.Progress = true, nil
	_, err := Check(ctx, run, o)
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		return err
	}
	if l, ok := o.Report.Layer(nr.Layer); ok && l.Name == "tart" && len(l.RootSteps) == 0 {
		return err // executable, OS and capability blockers have no root repair
	}
	o.Report.Layers = slices.Clone(o.Report.Layers)
	for i := range o.Report.Layers {
		l := &o.Report.Layers[i]
		if l.Name != nr.Layer {
			continue
		}
		l.Available, l.Problems = true, nil
		if l.Name == "oci" {
			l.CLI = cmp.Or(l.CLI, o.OCICLI, "docker") // a detected CLI stays: Check refuses another
			l.Runtime = cmp.Or(o.OCIRuntime, l.Runtime, "runsc")
			l.Runtimes = append(slices.Clone(l.Runtimes), l.Runtime)
			if l.Sudo == "" && len(l.SudoSteps) > 0 {
				l.Sudo = SudoRuntime
			}
			l.Guard = true // as the root step would leave it
		}
	}
	o.Layer = nr.Layer
	_, err = Check(ctx, run, o)
	return err
}

// netCapSteps are the root steps --net-mbit needs on layer l: none unless it
// was given, boxes would use zm's network, and that network is not made yet
// or its unit sets another cap (a unit that failed with a cap too: a kernel
// without the queues fails it, and --net-mbit 0 is the way back).
func (o ShareOptions) netCapSteps(l Layer) []string {
	installed := IsBoxNet(l.Network) || l.NetworkMbit > 0
	if !o.NetMbitSet || !o.runnerOn(l.Name) || len(l.NetworkSteps) == 0 && (!installed || l.NetworkMbit == o.NetMbit) {
		return nil
	}
	return boxNetOf(l).capSteps(o.NetMbit)
}

// runnerOn is Check's rule for whether a layer's boxes serve a GitHub
// runner on zm's network: a runner_dir (--runner-dir, else the images'
// pin) and no network of the owner's own.
func (o ShareOptions) runnerOn(layer string) bool {
	switch {
	case o.RunnerDir == "none" || o.OCINetwork != "":
		return false
	case o.RunnerDir != "":
		return true
	case len(o.Images) > 0:
		return ImageRunnerDir(o.Images) != ""
	}
	p, err := selectedPin(o.Report, layer, o.With)
	return err == nil && p.RunnerDir != ""
}

// Failed reports whether any chosen size stayed unbound.
func (r ShareResult) Failed() bool {
	if r.DryRun {
		return false
	}
	for _, s := range r.Sizes {
		if !s.Bound {
			return true
		}
	}
	return false
}

// lister is optional on a Binder: it lets share say that an older binding stays.
type lister interface {
	RunTargets(ctx context.Context, describe bool) (computeapi.RunTargetList, error)
}

// Share pulls the pinned images through the layer's CLI, runs conformance for
// each chosen size and binds a box target for each size that passed. It
// refuses before running anything when an image is not pinned or the layer is
// not ready.
func Share(ctx context.Context, run Runner, b Binder, o ShareOptions) (ShareResult, error) {
	res, err := Check(ctx, run, o)
	if err != nil || o.DryRun {
		return res, err
	}
	return Bind(ctx, b, o, res), nil
}

// Check is Share up to binding: it refuses what Share refuses, then pulls the
// images and runs conformance, marking each size that passed. Nothing is
// bound, so a caller can do what binding needs (zm pool host joins the pool)
// only once the machine passed.
func Check(ctx context.Context, run Runner, o ShareOptions) (ShareResult, error) {
	say := func(s string) {
		if o.Progress != nil {
			o.Progress(s)
		}
	}
	if o.With != "" && len(o.Images) > 0 {
		return ShareResult{}, errors.New("--with and --image cannot be combined: lend one named image per host")
	}
	res := ShareResult{DryRun: o.DryRun, Full: o.Full, ShowCommands: o.ShowCommands}
	for _, im := range o.Images {
		if !Pinned(im) {
			return res, fmt.Errorf("image %q is not pinned: want name@sha256:<64 hex digits>", im)
		}
	}
	if o.NamePrefix == "" {
		return res, errors.New("no binding name prefix: pass --name-prefix")
	}
	if err := checkAddrs(o.NetAllow, o.NetForbid); err != nil {
		return res, err
	}
	for _, m := range o.NetMail {
		if checkAddrs([]string{m}) != nil && (m == "" || strings.ContainsAny(m, " :/[]") || strings.HasPrefix(m, "-")) {
			return res, fmt.Errorf("mail server %q: want a host, or host:port", m)
		}
	}
	o.ReclaimIdleS, res.ReclaimFrom = o.reclaim()
	if o.ReclaimIdleS < 0 {
		return res, errors.New("reclaim idle time: want 0 (off) or more seconds")
	}
	if o.ReclaimIdleS > 0 && o.Report.OS != "darwin" {
		return res, fmt.Errorf("reclaim on owner activity reads a Mac's keyboard and pointer idle time, not %s's: pass --reclaim-idle-s 0", o.Report.OS)
	}
	if o.NetMbitSet && (o.NetMbit < 0 || o.NetMbit > 100000) {
		return res, errors.New("--net-mbit: want Mbit/s, 1..100000, or 0 for no cap")
	}
	if r := o.RunnerDir; r != "" && r != "none" && !PlainRunnerDir(r) {
		return res, fmt.Errorf("--runner-dir %q: want a plain path (no quote, $, backslash or blank), or none", r)
	}
	var err error
	if res.Admission, res.AdmissionFrom, err = admissionConfig(o); err != nil {
		return res, err
	}
	res.ReclaimIdleS = o.ReclaimIdleS
	if res.Docker, res.DockerFrom = o.docker(); res.DockerFrom != "" {
		res.Notes = append(res.Notes, "sandboxes with root and docker, kept from "+res.DockerFrom+" (--docker=false turns it off)")
	}
	res.Root, res.RootFrom = o.root()
	driver, suite := o.Driver, o.Conformance
	if driver == "" {
		driver = "tart"
	}
	if suite == "" {
		suite = defaultSuite(SystemFacts{}.LookPath)
	}

	layer, err := chooseLayer(o)
	if err != nil {
		return res, err
	}
	layer = o.dockerRuntime(layer)
	res.Layer, res.Driver = layer.Name, driver
	suiteDriver := driver
	if len(o.DriverDirs) > 0 {
		p, err := protocol.FindDriver(o.DriverDirs, driver)
		switch {
		case err == nil:
			suiteDriver = p
		case !o.DryRun: // a plan may name a driver not installed yet
			return res, err
		}
	}
	res.Images = o.Images
	// The template images share added: a failed pull drops one, never the default.
	templates := map[string]string{}
	if len(res.Images) == 0 { // zm's default for the layer, pinned in images.pin
		p, err := selectedPin(o.Report, layer.Name, o.With)
		if err != nil {
			return res, err
		}
		res.Images, res.DefaultImage, res.NamedImage = []string{p.Image}, o.With == "", o.With
		if o.With == "" {
			say(fmt.Sprintf("no --image: lending zm's default %s image for %s, %s", osName[p.OS], layer.Name, p.Image))
		} else {
			say(fmt.Sprintf("--with %s: lending %s", o.With, p.Image))
		}
		for _, tp := range LayerTemplates(layer.Name) { // zm-eakh.12.5.20: after the default, which stays the binding's first
			if o.With != "" || tp.OS != p.OS {
				continue
			}
			if tp.Image == "" {
				res.Notes = append(res.Notes, "zm's "+tp.Template+" template image is not published yet: boxes lend the default image only")
				continue
			}
			res.Images = append(res.Images, tp.Image)
			templates[tp.Image] = tp.Template
			say(fmt.Sprintf("and zm's %s template image, %s (zm sandbox create --template %s)", tp.Template, tp.Image, tp.Template))
		}
	}
	if o.With == "" && len(o.Images) == 0 {
		if o.EarlierUnread != "" {
			return res, fmt.Errorf("cannot safely choose the default image: earlier bindings could not be read: %s; start the daemon and retry, or pass --image explicitly", o.EarlierUnread)
		}
		// An image this version cannot name (the owner's own, or one from a
		// repository Potluck no longer publishes) is replaced by the default,
		// and the result says so with the flags that keep it: re-lending is
		// one command with no flags.
		replaced := map[string][]string{} // the flags that keep an image: the bindings that lent it
		var keeps []string
		for _, old := range o.Earlier {
			if !strings.HasPrefix(old.Name, o.NamePrefix+"-") || cmp.Or(old.Config["vm"], "tart") != layer.Name || len(old.Images) == 0 {
				continue
			}
			name, known := earlierImage(old)
			if name != "" {
				return res, fmt.Errorf("%s previously lent --with %s: repeat --with %s to keep it, or pass --image explicitly to replace it", old.Name, name, name)
			}
			if !known {
				k := keepFlags(old)
				if replaced[k] == nil {
					keeps = append(keeps, k)
				}
				replaced[k] = append(replaced[k], old.Name)
			}
		}
		for _, k := range keeps {
			note := fmt.Sprintf("%s lent an image this version does not know: lending the default replaces it (to keep that image instead, run this again with %s)", strings.Join(replaced[k], ", "), k)
			res.Notes = append(res.Notes, note)
			say(note)
		}
	}
	if layer.Name == "tart" { // before any pull: a disk too small for the images refuses here
		if err := CheckTartImages(o.Report, res.Images); err != nil {
			return res, err
		}
		note, err := tartDiskCheck(ctx, run, o.Report, res.Images)
		if err != nil {
			return res, err
		}
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
	}
	if !o.RootSet && !o.Root && !res.Root {
		res.Root = imageRoot(res.Images)
	}
	runner := o.RunnerDir // zm-eakh.12.24.5: the box serves GitHub Actions jobs only with a runner_dir
	switch {
	case runner == "none":
		runner = ""
	case runner == "":
		if runner = ImageRunnerDir(res.Images); runner == "" {
			res.Notes = append(res.Notes, "boxes serve jobs only, not GitHub Actions: zm knows of no GitHub runner in "+strings.Join(res.Images, ", ")+
				"; if the image has one, pass --runner-dir PATH (zm's Linux image keeps it in /home/runner)")
		}
	}
	base := map[string]string{"vm": layer.Name}
	if o.OCIHostProfile != "" {
		base["oci_host_profile"] = o.OCIHostProfile
	}
	if o.StateDir != "" {
		p, err := filepath.Abs(o.StateDir) // the driver runs elsewhere
		if err != nil {
			return res, fmt.Errorf("--state-dir: %v", err)
		}
		base["state_dir"] = p
	}
	var pull func(image string) []string
	if o.NetMbitSet && layer.Name != "oci" { // never accepted and ignored (zm-eakh.12.53)
		return res, fmt.Errorf("--net-mbit is for the oci layer: it caps downloads on Potluck's own network %s, which a %s box is not on (a %s box is a full VM on its layer's own network: shape that yourself)",
			BoxNet, layer.Name, layer.Name)
	}
	switch layer.Name {
	case "oci":
		if o.Root {
			return res, errors.New("--root is for the tart layer (a Mac's VMs): on oci, --docker gives sandboxes root and rootless Docker")
		}
		res.Root, res.RootFrom = false, "" // an earlier tart share's, not this layer's
		cli, rt := layer.CLI, layer.Runtime
		if o.OCICLI != "" {
			cli = o.OCICLI
		}
		if o.OCIRuntime != "" {
			rt = o.OCIRuntime
		}
		if !vmhost.OCIEngineVerificationSupported(cli) {
			return res, fmt.Errorf("isolated OCI boxes require a verified local Docker engine; %s is unavailable", cli)
		}
		if !IsolatingRuntime(rt) {
			return res, fmt.Errorf("oci runtime %q is not isolation: use Kata or runsc, never runc, crun or youki", rt)
		}
		if cli != layer.CLI {
			return res, fmt.Errorf("oci cli %q: doctor detected %q; runtimes of another CLI are unknown", cli, layer.CLI)
		}
		if res.Docker { // sudo.go: root and docker need a runtime that honours setuid
			kept := "" // an inherited --docker says where it came from
			if res.DockerFrom != "" {
				kept = " (--docker kept from " + res.DockerFrom + " (--docker=false turns it off))"
			}
			switch {
			case layer.Sudo == "" && len(layer.SudoSteps) > 0:
				return res, &NotReadyError{Layer: layer.Name, Problems: []string{layer.SudoProblem + kept}, RootCommand: RootCommand(layer.SudoSteps)}
			case layer.Sudo == "":
				return res, fmt.Errorf("--docker: %s%s", cmp.Or(layer.SudoProblem, "no runtime here honours setuid in a box"), kept)
			case o.OCIRuntime != "" && o.OCIRuntime != layer.Sudo:
				return res, fmt.Errorf("--docker with --oci-runtime %s: sudo in a box needs a runtime that honours setuid (%s here)", o.OCIRuntime, layer.Sudo)
			}
			rt = layer.Sudo
			base["docker"] = "on"
		}
		if !slices.Contains(layer.Runtimes, rt) && !(res.Docker && rt == layer.Sudo) {
			return res, fmt.Errorf("oci runtime %q is not registered with %s (usable: %s)", rt, cli, strings.Join(layer.Runtimes, ", "))
		}
		base["oci_cli"], base["oci_runtime"] = cli, rt
		if n := o.OCINetwork; n != "" && n != "none" && len(o.NetMail) == 0 {
			return res, fmt.Errorf("oci network %q: name an outside mail server with --net-mail, so conformance checks no box can send mail through it. Tart and Incus block outbound mail in code; zm cannot read a named network's firewall rules", n)
		}
		network := o.OCINetwork
		if runner != "" && (network == "" || network == "none") { // a runner must reach GitHub (zm-eakh.12.24.6)
			if network == "" && layer.Network != "" {
				network = layer.Network
				if len(o.NetMail) == 0 {
					o.NetMail = []string{DefaultNetMail}
				}
				say(fmt.Sprintf("GitHub runners and sandboxes: Potluck's filtered network %s (outbound mail checked against %s)", network, strings.Join(o.NetMail, ", ")))
			} else {
				if capSteps := o.netCapSteps(layer); len(capSteps) > 0 {
					layer.NetworkSteps = capSteps // the one root line makes the network with its cap
				}
				res.Notes = append(res.Notes, runnerNetNote(layer, network))
				runner = ""
			}
		}
		switch capSteps := o.netCapSteps(layer); {
		case o.NetMbitSet && o.NetMbit > 0 && network == "" && o.OCINetwork == "" && o.runnerOn(layer.Name) && len(layer.NetworkSteps) == 0 && layer.NetworkProblem != "":
			return res, fmt.Errorf("--net-mbit: Potluck's network %s is not ready, so there is nothing to cap yet: %s", boxNetOf(layer).Net, layer.NetworkProblem)
		case o.NetMbitSet && o.NetMbit > 0 && !IsBoxNet(network) && (len(layer.NetworkSteps) == 0 || !o.runnerOn(layer.Name)):
			return res, fmt.Errorf("--net-mbit caps downloads on Potluck's own network %s, which these boxes are not on (oci network %q): shape your own network's bridge (sengirunners/potluck cmd/potluck-driver-tart/README.md)", boxNetOf(layer).Net, cmp.Or(network, "none"))
		case IsBoxNet(network) && len(capSteps) > 0:
			res.Notes = append(res.Notes, fmt.Sprintf("boxes' downloads are not capped at %d Mbit/s yet (--net-mbit; the network's unit says %d, 0 is no cap): run as root once:\n  %s",
				o.NetMbit, layer.NetworkMbit, RootCommand(capSteps)))
		}
		if network != "" {
			base["oci_network"] = network
		}
		res.Network = network
		if err := kataDockerGuard(layer, res.Docker, rt, network); err != nil {
			return res, err
		}
		if runner != "" && network != "" && network != "none" { // what the runner's network must and must not reach
			if !slices.Contains(o.NetAllow, RunnerNetAllow) {
				o.NetAllow = append(slices.Clone(o.NetAllow), RunnerNetAllow)
			}
			o.NetForbid = forbidForward(o, &res)
		}
		pull = func(im string) []string { return []string{cli, "pull", "--", im} }
	case "tart":
		if layer.CLI != "" {
			base["tart"] = layer.CLI
		}
		if layer.SoftnetPath != "" {
			base["softnet_path"] = layer.SoftnetPath
		}
		if res.Docker {
			return res, errors.New("--docker is for the oci layer: a tart box is a full VM, whose image decides sudo and docker (--root keeps its sudo)")
		}
		if res.Root {
			base["root"] = "on"
			kept := "--root=false turns it off"
			if res.RootFrom != "" {
				kept = "kept from " + res.RootFrom + "; " + kept
			}
			res.Notes = append(res.Notes, "sandboxes have root: their user keeps the image's passwordless sudo, inside a throwaway VM deleted after each job, "+
				"as on GitHub's own macOS runners. Root reaches only that VM: the checks below prove root in it still cannot reach this Mac or your network ("+kept+")")
		}
		// Softnet gives every Tart box egress, so every Tart box's checks
		// forbid the forward path: the LAN's router and the tailnet.
		o.NetForbid = forbidForward(o, &res)
		pull = func(im string) []string { return []string{cmp.Or(layer.CLI, "tart"), "pull", "--", im} }
	}
	res.RunnerDir = runner

	if res.LinuxSlots, res.LinuxSlotsFrom, err = linuxSlots(o, macOSImages(layer.Name, res.Images)); err != nil {
		return res, err
	}
	res.LinuxSlotsByAdmission = res.LinuxSlots > 0 && res.Admission["admission_url"] != ""
	if !res.LinuxSlotsByAdmission {
		o.Limits.Boxes = res.LinuxSlots // that many of the largest size may run together
	}
	sizes, one, err := chooseSizes(o, res.Images)
	if err != nil {
		return res, err
	}
	if o.OCIHostProfile != "" {
		p, err := vmhost.LoadOCIHostProfile(o.OCIHostProfile, base["oci_runtime"])
		if err != nil {
			return res, err
		}
		cpuCap := int(math.Floor(float64(o.Report.Cores) * o.Limits.Fraction()))
		memCap := int(math.Floor(float64(o.Report.RAMMB) * o.Limits.Fraction()))
		if o.Limits.MaxCores > 0 {
			cpuCap = min(cpuCap, o.Limits.MaxCores)
		}
		if o.Limits.MaxRAMMB > 0 {
			memCap = min(memCap, int(o.Limits.MaxRAMMB))
		}
		base["host_cpu_limit"], base["host_memory_limit_mb"] = strconv.Itoa(cpuCap), strconv.Itoa(memCap)
		fitted := sizes[:0]
		for _, size := range sizes {
			b, err := p.HostBudget(size.CPU, size.MemoryMB)
			if err != nil {
				return res, err
			}
			if n := o.Limits.boxes(); b.CPU <= cpuCap/n && b.MemoryMiB <= memCap/n {
				fitted = append(fitted, size)
			} else if len(o.Sizes) > 0 {
				return res, fmt.Errorf("size %s including host overhead does not fit %s inside the owner's limits (the peer checks run two; --linux-slots asks more)", size.Name, timesWord(o.Limits.boxes()))
			}
		}
		sizes = fitted
		if len(sizes) == 0 {
			return res, fmt.Errorf("no size including host overhead fits %s inside the owner's limits (the peer checks run two; --linux-slots asks more)", timesWord(o.Limits.boxes()))
		}
		one = false
	}
	if res.OneAtATime = one; one {
		if res.LinuxSlots >= Slots {
			return res, fmt.Errorf("--linux-slots %d%s: no size fits twice inside %s, so Potluck would run one box at a time: raise --share, or pass --linux-slots 1",
				res.LinuxSlots, keptFrom(res.LinuxSlotsFrom), o.Limits.Within())
		}
		res.Notes = append(res.Notes, "one box at a time: no size fits twice inside "+o.Limits.Within()+", so zm lends what fits once and runs one box at a time (--share to choose more of it)")
	}
	if err := slotsAlike(o, res.LinuxSlots, res.LinuxSlotsFrom, sizes); err != nil {
		return res, err
	}
	if n := res.LinuxSlots; n > 0 && !one {
		what := fmt.Sprintf("up to %d Linux boxes at once", n)
		if n == 1 {
			what = "one Linux box at a time"
		}
		how := "every size lent fits " + timesWord(o.Limits.boxes()) + " inside " + o.Limits.Within()
		if res.LinuxSlotsByAdmission {
			how = "the admission controller decides room for each box"
		}
		res.Notes = append(res.Notes, what+" on this machine, across every size: "+how+keptFrom(res.LinuxSlotsFrom))
	}

	if res.Storage, err = ociStorage(o, layer.Name, base["oci_cli"], sizes); err != nil {
		return res, err
	}
	if res.Storage != nil {
		maps.Copy(base, res.Storage.Keys) // conformance tests what the binding carries
	}
	for _, im := range res.Images {
		res.Pulls = append(res.Pulls, Step{What: "pull " + im, Command: pull(im)})
	}
	nets, stop, err := netArgs(o)
	if err != nil {
		return res, fmt.Errorf("listen for the host-address checks: %v", err)
	}
	defer stop()
	for i, s := range sizes {
		cfg := map[string]string{}
		for k, v := range base {
			cfg[k] = v
		}
		cfg["size"], cfg["cpu"], cfg["memory"] = s.Name, strconv.Itoa(s.CPU), strconv.Itoa(s.MemoryMB)
		cfg["images"] = strings.Join(res.Images, ",")
		if res.LinuxSlots > 0 { // conformance's boxes count against the same slots as the live ones; its peers check runs two
			cfg["linux_slots"] = strconv.Itoa(max(res.LinuxSlots, Slots))
		}
		args := conformanceArgs(suite, suiteDriver, cfg)
		switch {
		case o.Full:
			args = append(args, nets...)
		case i == 0:
			args = append(append(args, "--only", QuickGroups), nets...)
		default:
			args = append(args, "--only", SizeGroups)
		}
		// Conformance runs without the reclaim policy or the admission keys
		// (the owner is at the keyboard, running share, and joining comes
		// first: the controller still counts these boxes, lease or not); the
		// binding carries both. The binding's
		// images are its pins, which the daemon sends as config images: a
		// config key of that name too could not be read back from
		// run_targets.toml (`images` set twice), so --pool lent nothing.
		bound := map[string]string{}
		for k, v := range cfg {
			if k != "images" {
				bound[k] = v
			}
		}
		if res.DefaultImage {
			bound["image_default"] = "true"
		}
		if p, ok := PinForImage(res.Images[0]); ok && p.Name != "" {
			bound["image_name"] = p.Name
		}
		maps.Copy(bound, res.Admission)
		if o.ReclaimIdleS > 0 {
			bound["reclaim_idle_s"] = strconv.Itoa(o.ReclaimIdleS)
		}
		if res.LinuxSlots > 0 {
			bound["linux_slots"] = strconv.Itoa(res.LinuxSlots)
		}
		// Nor one at a time: the peers check runs two boxes at once, and two
		// that fit once inside the default half fit inside the whole machine.
		if res.OneAtATime {
			for k, v := range OneAtATimeConfig {
				bound[k] = v
			}
		}
		// Nor without runner_dir: on oci a runner binding gives its network
		// only to a box that brought a jit_config or net on, and an older
		// driver's conformance boxes bring neither, so without it its boxes
		// have exactly the network a runner box (or a trusted host's sandbox)
		// gets, and the net checks test it. The binding carries runner_dir.
		if res.RunnerDir != "" {
			bound["runner_dir"] = res.RunnerDir
		}
		res.Sizes = append(res.Sizes, SizeResult{Size: s, Binding: o.NamePrefix + "-" + s.Name, Config: bound, Conformance: args})
	}
	if o.DryRun {
		return res, nil
	}

	pulled := res.Images[:0:0]
	for i, p := range res.Pulls {
		if res.Layer == "tart" && tartImagePresent(ctx, run, res.Images[i], layer.CLI) {
			say("reuse " + res.Images[i])
			pulled = append(pulled, res.Images[i])
			continue
		}
		say(p.What)
		pullOptions := o
		if pullOptions.PullTimeout <= 0 {
			pin, _ := PinForImage(res.Images[i])
			pullOptions.PullTimeout = time.Duration(pin.PullMinutes) * time.Minute
		}
		r, err := runPull(ctx, run, pullOptions, p.Command)
		why := ""
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			why = fmt.Sprintf("%s: timed out after %s (--pull-timeout; a pull that stopped partway resumes where it left off when you run this again)", p.What, pullTimeout(pullOptions))
		case err != nil:
			why = fmt.Sprintf("%s: %v", p.What, err)
		case r.Exit != 0:
			why = fmt.Sprintf("%s: exit %d: %s", p.What, r.Exit, strings.TrimSpace(r.Stderr))
		}
		im := res.Images[i]
		switch tmpl, isTmpl := templates[im]; {
		case why == "":
			pulled = append(pulled, im)
		case isTmpl: // a private package, a full disk, the network: the default still lends
			res.Notes = append(res.Notes, "zm's "+tmpl+" template image did not pull, so boxes lend the default image only ("+why+")")
		default:
			return res, errors.New(why)
		}
	}
	if res.Storage != nil {
		say("shelf volume " + vmhost.ShelfVolume)
		if err := ensureShelfVolume(ctx, run, res.Storage); err != nil {
			return res, err
		}
	}
	if len(pulled) < len(res.Images) { // conformance and the binding take the images pulled
		old, now := "images="+strings.Join(res.Images, ","), "images="+strings.Join(pulled, ",")
		for i := range res.Sizes {
			for j, a := range res.Sizes[i].Conformance {
				if a == old {
					res.Sizes[i].Conformance[j] = now
				}
			}
		}
		res.Images = pulled
	}
	// A named OCI network's filter is unreadable: it binds only once a mail
	// check really passed, not when every one was a SKIP.
	namedNet := res.Layer == "oci" && res.Network != "" && res.Network != "none"
	jobsOnly := ""
	if res.RunnerDir != "" {
		jobsOnly = ", or lend for jobs only (no network) with --runner-dir none"
	}
	timeout := o.ConformanceTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	busy := false // the first size failed only because every slot was in use
	for i := range res.Sizes {
		sr := &res.Sizes[i]
		if !o.Full && i > 0 && !res.Sizes[0].Passed {
			sr.Error = "not checked: the security checks failed on " + res.Sizes[0].Binding
			if busy {
				sr.Error = "not checked: every slot was in use when " + res.Sizes[0].Binding + " was checked; try again when a box ends"
			}
			continue
		}
		say("conformance on " + sr.Binding)
		cctx, cancel := context.WithTimeout(ctx, timeout)
		r, err := run.Run(cctx, sr.Conformance[0], sr.Conformance[1:]...)
		cancel()
		sr.Output = strings.TrimSpace(r.Stdout + "\n" + r.Stderr)
		switch {
		case err != nil:
			sr.Error = "conformance did not run: " + err.Error()
			continue
		case r.Exit != 0 && slotsFullOS(r.Stdout) != "":
			total, os := Slots, slotsFullOS(r.Stdout)
			if os == "linux" && res.LinuxSlots > 0 {
				total = max(res.LinuxSlots, Slots) // what conformance ran with
			}
			sr.Error, busy = slotsFullError(os, total), i == 0
			continue
		case r.Exit != 0:
			sr.Error = fmt.Sprintf("conformance failed (exit %d)", r.Exit)
			f := conformance.FirstFailure(r.Stdout)
			setup, report := vmhost.StartAdvice(f) // read before the cut: the words end in them
			if f != "" {
				if len(f) > 400 {
					f = strings.ToValidUTF8(f[:400], "") + "..."
				}
				sr.Error = fmt.Sprintf("conformance failed at %s (exit %d)", f, r.Exit)
			}
			switch {
			case setup != "": // the same every time: never "run this again"
				sr.Error += "; running this again unchanged fails the same way: " + setup
			case conformance.Late(r.Stdout):
				sr.Error += "; " + busyHint
			}
			if report != "" && !strings.Contains(f, report) {
				sr.Error += "; the box's report: " + report
			}
			continue
		case namedNet && (o.Full || i == 0) && !strings.Contains(r.Stdout, mailPassed):
			sr.Error = fmt.Sprintf("no outbound-mail check passed on oci network %q: none of --net-mail %s opened from this machine, so nothing was proven; try a mail server this machine reaches on port 25, 465 or 587 (your mail provider's submission server, e.g. smtp.<provider>:587)%s",
				res.Network, strings.Join(o.NetMail, ", "), jobsOnly)
			continue
		}
		sr.Passed = true
	}
	return res, nil
}

// busyHint follows a conformance failure in which a driver call ran past its
// time limit: on a machine busy with other work that is a timing failure, not
// a fault of the driver, and a quiet run passes.
const busyHint = "a driver call ran past its time limit, which happens when the machine is busy with other work (another share, an image pull): run this again once the machine is quiet"

// Bind binds each size Check passed (o.Pool: machine-level, shared with it)
// and says of each size that stayed unbound whether an older binding remains.
// An earlier size no box fits on this machine any more is unbound
// (dropUnfit), with a note.
func Bind(ctx context.Context, b Binder, o ShareOptions, res ShareResult) ShareResult {
	existing := map[string]bool{}
	if l, ok := b.(lister); ok {
		if list, err := l.RunTargets(ctx, false); err == nil {
			for _, t := range list.Targets {
				existing[t.Name] = true
			}
		}
	}
	for i := range res.Sizes {
		sr := &res.Sizes[i]
		sr.StillBound = existing[sr.Binding]
		if !sr.Passed {
			continue
		}
		if o.Progress != nil {
			o.Progress("bind " + sr.Binding)
		}
		req := computeapi.BindRunTargetRequest{Replace: true, Target: computeapi.RunTarget{
			Name: sr.Binding, Driver: res.Driver, Kind: "box", Isolation: "ephemeral", Credentials: false,
			Images: res.Images, Config: sr.Config,
		}}
		if o.Pool != "" {
			req.Host, req.Share = true, []string{o.Pool}
		}
		if _, err := b.BindRunTarget(ctx, req); err != nil {
			sr.Error = "bind: " + err.Error()
			continue
		}
		sr.Bound = true
	}
	for i := range res.Sizes {
		if sr := &res.Sizes[i]; !sr.Bound && sr.Error != "" {
			if note, ok := dropKeyless(ctx, b, o, res, sr.Binding); ok {
				sr.Error, sr.StillBound = sr.Error+note, !strings.Contains(note, "was unbound")
				continue
			}
			if sr.StillBound {
				sr.Error += "; " + sr.Binding + " is still bound from before"
				if res.Storage != nil { // it keeps the shelf limits it had, which may not be the ones just bound
					sr.Error += ", with the shelf and volume limits it had (every size of an engine should carry the same shelf_max_gb and shelf_fill: run this again)"
				}
			} else {
				sr.Error += "; " + sr.Binding + " is not bound"
			}
		}
	}
	if !res.DryRun {
		res.Notes = append(res.Notes, dropUnfit(ctx, b, o, res.Layer)...)
	}
	return res
}

// runnerNetNote says why oci boxes whose image has a runner are bound for
// jobs only: they would have no network, and the root step that fixes it.
func runnerNetNote(l Layer, asked string) string {
	s := "boxes serve jobs only, not GitHub Actions, and sandboxes have no internet: they would have no network, and a runner must reach GitHub"
	switch {
	case asked == "none":
		return s + " (--oci-network none)"
	case len(l.NetworkSteps) > 0:
		return s + ". Potluck's filtered network " + boxNetOf(l).Net + " is not set up: run as root once, then this again:\n  " + RootCommand(l.NetworkSteps)
	case l.NetworkProblem != "":
		return s + ": " + l.NetworkProblem
	}
	return s + ": pass --oci-network NAME (a network you filtered) --net-mail HOST"
}

func conformanceArgs(suite, driver string, cfg map[string]string) []string {
	args := []string{suite, driver, "--kind", "box"}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--config", k+"="+cfg[k])
	}
	return args
}

func chooseLayer(o ShareOptions) (Layer, error) {
	var l Layer
	if o.Layer == "" {
		best, ok := o.Report.Best()
		if !ok {
			// Name the layer closest to ready so the owner gets its root command.
			for _, n := range LayerOrder {
				if c, found := o.Report.Layer(n); found && c.Supported && len(c.RootSteps) > 0 {
					return l, notReady(c)
				}
			}
			// A Mac has one layer, Tart: say what it lacks (Softnet, Tart itself) instead of pointing at doctor.
			if c, found := o.Report.Layer("tart"); found && c.Supported && o.Report.OS == "darwin" && len(c.Problems) > 0 {
				return l, notReady(c)
			}
			// Linux lends through oci today: say what it lacks (Kata without
			// passwordless sudo has no root step, only a fix to name).
			if c, found := o.Report.Layer("oci"); found && c.Supported && o.Report.OS == "linux" && len(c.Problems) > 0 {
				return l, notReady(c)
			}
			return l, errors.New("no layer can lend this machine: run `zm host doctor` for what is missing")
		}
		return best, nil
	}
	c, ok := o.Report.Layer(o.Layer)
	if !ok {
		return l, fmt.Errorf("unknown layer %q (incus, oci or tart)", o.Layer)
	}
	if !c.Supported {
		return l, fmt.Errorf("layer %s is detected but zm-run-tart cannot bind vm=%s yet", c.Name, c.Name)
	}
	if !c.Available {
		return l, notReady(c)
	}
	return c, nil
}

func notReady(c Layer) error {
	return &NotReadyError{Layer: c.Name, Problems: c.Problems, RootCommand: RootCommand(c.RootSteps)}
}

// chooseSizes is the sizes asked for (default: Lend's, every size that fits
// twice, or the owner's --linux-slots times, inside the share, else, at zm's
// default share, once, one box at a time), none below the images' own floor
// (ImageMinimums): a VM smaller than its image allows never boots.
func chooseSizes(o ShareOptions, images []string) ([]Size, bool, error) {
	l := Lend(o.Report, o.Limits, images)
	minCPU, minMem := ImageMinimums(images)
	small := func(s Size) bool { return s.CPU < minCPU || s.MemoryMB < minMem }
	floor := fmt.Sprintf("at least %d vCPU and %d MB", minCPU, minMem)
	n := o.Limits.boxes()
	auto := o.Limits.Share <= 0 && n == Slots // the one-at-a-time fallback (Lend)
	times, raise := timesWord(n), "raise --share or the caps"
	if n > Slots {
		raise += ", or lower --linux-slots"
	}
	within := times + " inside the share"
	if auto {
		within = "inside the share, even one box at a time"
	}
	if len(l.Sizes) == 0 {
		if auto && len(fitting(o.Report, o.Limits, n)) > 0 {
			within = times + " inside the share"
		}
		if len(fitting(o.Report, o.Limits, n)) == 0 && (!auto || len(fitting(o.Report, o.Limits, 1)) == 0) {
			return nil, false, fmt.Errorf("no size fits %s: %s", within, raise)
		}
		return nil, false, fmt.Errorf("no size that fits %s is big enough for the image (%s): %s; nothing was downloaded", within, floor, raise)
	}
	if len(o.Sizes) == 0 {
		return l.Sizes, l.OneAtATime, nil
	}
	if l.OneAtATime {
		within = "inside the share"
	}
	var out []Size
	for _, n := range o.Sizes {
		s, err := SizeByName(n)
		if err != nil {
			return nil, false, err
		}
		switch {
		case slices.Contains(l.Sizes, s):
			out = append(out, s)
		case small(s):
			names := make([]string, len(l.Sizes))
			for i, s := range l.Sizes {
				names[i] = s.Name
			}
			return nil, false, fmt.Errorf("size %s is too small for the image, which needs %s: pass --sizes %s instead; nothing was downloaded", n, floor, strings.Join(names, ","))
		default:
			return nil, false, fmt.Errorf("size %s does not fit %s", n, within)
		}
	}
	return out, l.OneAtATime, nil
}

// defaultSuite is the conformance suite's program: potluck-conformance when
// look finds it, else zm-run-conformance (its name before the rename).
func defaultSuite(look func(string) (string, bool)) string {
	if _, ok := look("potluck-conformance"); ok {
		return "potluck-conformance"
	}
	return "zm-run-conformance"
}
