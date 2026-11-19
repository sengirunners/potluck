// Package hostprobe is what `zm host doctor` and `zm host share` stand on
// (SPEC 20, "Host onboarding"): it detects what a machine can lend as run
// target boxes (cores, RAM, disk, KVM, and the VM layers incus, oci and tart),
// proposes the sizes that fit twice inside the owner's share (once, one box at
// a time, when none does at zm's default share), folds every
// pending root step into one command, and (share.go) pulls digest-pinned
// images, runs the conformance suite per size and binds only what passed.
//
// Share takes a Binder the client satisfies; vmhost supplies the common native
// resource/engine policy. Detection runs
// every command through a Runner and reads every fact through Facts, so tests
// use fakes and never touch a real incus, docker, podman or tart. Nothing
// here names a machine, address, user or label; host facts are detected at run
// time or given by the owner in flags.
//
// Owner: run targets (zm-compute-runs). The second driver we expect: the
// incus layer (zm-eakh.12.7.1) flips Supported["incus"]; a cloud Linux
// driver's layer would be one more entry in Detect.
package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/vmhost"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// Result is a finished command.
type Result struct {
	Stdout, Stderr string
	Exit           int
}

// Runner runs one command. A command that ran and exited non-zero is a Result
// with Exit set and a nil error; the error is for "could not start".
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

// ExecRunner runs real commands.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Exit = ee.ExitCode()
		return res, nil
	}
	return res, err
}

// Facts is where a machine's static numbers come from.
type Facts interface {
	OS() string   // "linux", "darwin"
	Arch() string // "arm64", "amd64"
	Cores() int
	RAMMB() int64
	FreeDiskGB(dir string) (float64, error)
	// Exists reports whether a path exists (/dev/kvm).
	Exists(path string) bool
	// OpenRW opens a path read/write and closes it: nil when this user can
	// use a device node such as /dev/kvm (existence alone is not usability).
	OpenRW(path string) error
	// LookPath finds an executable on PATH.
	LookPath(name string) (string, bool)
	// OCIEngine runs the driver's own check of the local Docker engine and
	// runtime (vmhost.VerifyOCIEngine): nil when lending would accept it.
	OCIEngine(cli, runtime string) error
}

// Supported is the one place that says which layers `zm host share` may bind:
// zm-run-tart serves tart and oci. zm-eakh.12.7.1 flips "incus" when the
// driver supports vm=incus.
var Supported = map[string]bool{"tart": true, "oci": true, "incus": false}

// LayerOrder is the preference when the owner names no layer.
var LayerOrder = []string{"incus", "oci", "tart"}

// Layer is one way to run a VM on this machine.
type Layer struct {
	Name           string `json:"name"` // incus, oci or tart
	Available      bool   `json:"available"`
	Supported      bool   `json:"supported"` // zm-run-tart can bind it today
	Version        string `json:"version,omitempty"`
	Isolation      string `json:"isolation,omitempty"` // vm, sandboxed-kernel, or "" for none
	CLI            string `json:"cli,omitempty"`       // oci: docker, podman or nerdctl
	SoftnetPath    string `json:"softnet_path,omitempty"`
	SoftnetVersion string `json:"softnet_version,omitempty"`
	Runtime        string `json:"runtime,omitempty"` // oci: kata or runsc, by name
	// Runtimes is every isolating runtime this CLI can use, in preference order.
	Runtimes []string `json:"runtimes,omitempty"`
	Problems []string `json:"problems,omitempty"` // plain words
	// Warnings are what the owner should know but do not stop the layer
	// (tart: low disk, a Mac that sleeps).
	Warnings  []string `json:"warnings,omitempty"`
	RootSteps []string `json:"root_steps,omitempty"`
	// Network (oci) is Potluck's filtered network for boxes that serve GitHub
	// runners, set when it is ready (boxnet.go); else NetworkProblem says
	// why, and NetworkSteps are the root steps that make it. Neither makes
	// the layer unavailable: without it, boxes serve jobs only. NetworkMbit
	// is the cap its unit puts on all boxes' downloads together (0: none).
	Network        string   `json:"network,omitempty"`
	NetworkMbit    int      `json:"network_mbit,omitempty"`
	NetworkProblem string   `json:"network_problem,omitempty"`
	NetworkSteps   []string `json:"network_root_steps,omitempty"`
	// NetworkLegacy says the machine has the network under the names zm gave
	// it (zm-box, br-zmbox, zm-box-net.service), and Network, NetworkMbit and
	// NetworkSteps are about those (boxNetOf).
	NetworkLegacy bool `json:"network_legacy,omitempty"`
	// Sudo (oci) is the runtime a box with root and docker uses
	// (sudo.go): runsc-root, or the Kata runtime; else SudoProblem says why
	// and SudoSteps add it. Neither makes the layer unavailable, and doctor's
	// root line leaves them out: only zm host share --docker runs them.
	Sudo        string   `json:"sudo_runtime,omitempty"`
	SudoProblem string   `json:"sudo_problem,omitempty"`
	SudoSteps   []string `json:"sudo_root_steps,omitempty"`
	// Guard (oci) is whether zm's per-port anti-spoofing on its network's
	// bridge runs as zm installs it (boxguard.go); else GuardProblem says
	// why and GuardSteps install it. A Kata box with docker on zm's network
	// needs it (its sudo runtime waits for it); nothing else does.
	Guard        bool     `json:"box_guard,omitempty"`
	GuardProblem string   `json:"box_guard_problem,omitempty"`
	GuardSteps   []string `json:"box_guard_root_steps,omitempty"`
}

// Report is Detect's answer.
type Report struct {
	OS         string  `json:"os"`
	MacOS      string  `json:"macos,omitempty"` // actual product version, never inferred from the kernel
	Arch       string  `json:"arch"`
	Cores      int     `json:"cores"`
	RAMMB      int64   `json:"ram_mb"`
	FreeDiskGB float64 `json:"free_disk_gb"`
	StateDir   string  `json:"state_dir,omitempty"`
	// TartDir is where Tart stores its images (macOS), and TartFreeGB the
	// free disk there: not StateDir's volume, which can be another one.
	TartDir    string  `json:"tart_dir,omitempty"`
	TartFreeGB float64 `json:"tart_free_gb,omitempty"`
	// TartDiskUnread is why TartFreeGB could not be read ("": it was).
	TartDiskUnread string `json:"tart_disk_unread,omitempty"`
	KVM            bool   `json:"kvm"` // /dev/kvm exists and this user can open it read/write
	// KVMProblem is why KVM is false on a Linux box, in plain words.
	KVMProblem string `json:"kvm_problem,omitempty"`
	// KVMRootSteps fixes KVMProblem when root can (the kvm group).
	KVMRootSteps []string `json:"kvm_root_steps,omitempty"`
	Layers       []Layer  `json:"layers"`
	// Swap is the state of this machine's swap (hostswap: off, zram,
	// encrypted, plain-disk or unknown), and SwapNote says in plain words
	// when it is not safe for box secrets: a box's memory can be paged out
	// to it.
	Swap     string `json:"swap,omitempty"`
	SwapNote string `json:"swap_note,omitempty"`
}

// Layer returns the named layer.
func (r Report) Layer(name string) (Layer, bool) {
	for _, l := range r.Layers {
		if l.Name == name {
			return l, true
		}
	}
	return Layer{}, false
}

// Best is the first available, supported layer in LayerOrder.
func (r Report) Best() (Layer, bool) {
	for _, n := range LayerOrder {
		if l, ok := r.Layer(n); ok && l.Available && l.Supported {
			return l, true
		}
	}
	return Layer{}, false
}

// RootSteps is every pending root step of the layers worth fixing: those of
// the named layer, or, with none named, of each layer that is supported on
// this OS and not yet available (deduplicated, in order), each layer's
// network steps after its own.
func (r Report) RootSteps(layer string) []string {
	var steps []string
	for _, l := range r.Layers {
		if layer != "" && l.Name != layer {
			continue
		}
		if layer == "" && (l.Available || !l.Supported) {
			continue
		}
		steps = append(append(steps, l.RootSteps...), l.NetworkSteps...)
	}
	return dedupe(steps)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// isolatingRuntime mirrors zm-run-tart's rule (vmhost):
// Kata or gVisor by name, never runc, crun or youki. Keep the two in step.
var isolatingRuntime = regexp.MustCompile(`^(runsc|runsc-root|io\.containerd\.runsc\.v1|kata|kata-runtime|kata-(qemu|clh|fc|stratovirt|dragonball)|io\.containerd\.kata(-(qemu|clh|fc|stratovirt|dragonball))?\.v2)$`)

// runtimePreference is the fixed order a runtime is chosen in (runsc needs no
// KVM; Kata is picked ahead of it only where KVM is usable and it ranks first).
var runtimePreference = []string{"runsc", "io.containerd.runsc.v1", "kata", "kata-qemu", "kata-clh", "kata-fc",
	"kata-stratovirt", "kata-dragonball", "kata-runtime", "io.containerd.kata.v2", "io.containerd.kata-qemu.v2",
	"io.containerd.kata-clh.v2", "io.containerd.kata-fc.v2", "io.containerd.kata-stratovirt.v2", "io.containerd.kata-dragonball.v2"}

func runtimeRank(n string) int {
	for i, p := range runtimePreference {
		if p == n {
			return i
		}
	}
	return len(runtimePreference)
}

// IsolatingRuntime reports whether an OCI runtime name is a boundary.
func IsolatingRuntime(name string) bool { return isolatingRuntime.MatchString(name) }

// Detect reads the machine. stateDir is where the driver keeps its records and
// images (its free disk is reported); "" skips the disk.
func Detect(ctx context.Context, run Runner, f Facts, stateDir string) Report {
	r := Report{OS: f.OS(), Arch: f.Arch(), Cores: f.Cores(), RAMMB: f.RAMMB(), StateDir: stateDir}
	if stateDir != "" {
		if g, err := f.FreeDiskGB(stateDir); err == nil {
			r.FreeDiskGB = g
		}
	}
	if r.OS == "linux" {
		switch {
		case !f.Exists("/dev/kvm"):
			r.KVMProblem = "/dev/kvm is missing: enable hardware virtualization in the firmware and load the kvm module"
		default:
			if err := f.OpenRW("/dev/kvm"); err != nil {
				r.KVMProblem = "/dev/kvm exists but this user cannot open it read/write (" + err.Error() + "): add the user to the kvm group, then log in again"
				r.KVMRootSteps = []string{`usermod -aG kvm "${SUDO_USER:-$USER}"`}
			} else {
				r.KVM = true
			}
		}
	}
	pm := packageManager(f)
	r.Layers = []Layer{detectIncus(ctx, run, f, r, pm), detectOCI(ctx, run, f, r, pm), detectTart(ctx, run, f, &r)}
	for i := range r.Layers {
		r.Layers[i].Supported = Supported[r.Layers[i].Name]
	}
	sw := readSwap(ctx, run, r.OS)
	r.Swap, r.SwapNote = sw.State, sw.Note
	return r
}

func detectIncus(ctx context.Context, run Runner, f Facts, r Report, pm string) Layer {
	l := Layer{Name: "incus", Isolation: "vm"}
	if r.OS != "linux" {
		l.Problems = []string{"incus runs on Linux"}
		return l
	}
	if _, ok := f.LookPath("incus"); !ok {
		l.Problems = append(l.Problems, "incus is not installed")
		l.RootSteps = append(l.RootSteps, installStep(pm, "incus"))
	} else if res, err := run.Run(ctx, "incus", "version"); err == nil && res.Exit == 0 {
		l.Version = incusVersion(res.Stdout)
	} else {
		l.Problems = append(l.Problems, "incus is installed but did not answer `incus version` (is its daemon running?)")
	}
	if !r.KVM {
		l.Problems = append(l.Problems, r.KVMProblem)
		l.RootSteps = append(l.RootSteps, r.KVMRootSteps...)
	}
	l.Available = len(l.Problems) == 0
	l.RootSteps = dedupe(l.RootSteps)
	return l
}

// incusVersion takes the server's version from `incus version`
// ("Client version: 6.0.0\nServer version: 6.0.0"), else the client's.
func incusVersion(out string) string {
	client := ""
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "server version":
			if v != "" && v != "unreachable" {
				return v
			}
		case "client version":
			client = v
		}
	}
	return client
}

var ociCLIs = []string{"docker", "podman", "nerdctl"}

func detectOCI(ctx context.Context, run Runner, f Facts, r Report, pm string) Layer {
	l := Layer{Name: "oci"}
	if r.OS != "linux" {
		l.Problems = []string{"the oci layer runs on Linux"}
		return l
	}
	for _, c := range ociCLIs {
		if _, ok := f.LookPath(c); ok {
			l.CLI = c
			break
		}
	}
	registered := map[string]bool{}
	daemonDown, socketDenied := false, false
	runtimesJSON := "" // docker info's Runtimes (sudo.go reads runsc-root's entry)
	if l.CLI == "" {
		l.Problems = append(l.Problems, "no container CLI (docker, podman or nerdctl) is installed (Docker: https://docs.docker.com/engine/install/)")
		l.RootSteps = append(l.RootSteps, installStep(pm, "docker"))
		l.RootSteps = dedupe(l.RootSteps)
		return l
	}
	if res, err := run.Run(ctx, l.CLI, "--version"); err == nil && res.Exit == 0 {
		l.Version = firstVersion(res.Stdout)
	}
	if !vmhost.OCIEngineVerificationSupported(l.CLI) {
		l.Problems = append(l.Problems, "isolated OCI boxes require a verified local Docker engine; this adapter is unavailable")
		l.NetworkProblem = l.Problems[0]
		return l
	}
	if l.CLI == "docker" {
		res, err := run.Run(ctx, "docker", "info", "--format", "{{json .Runtimes}}")
		if err != nil || res.Exit != 0 {
			// A running daemon whose socket this user may not open answers
			// "permission denied" (the state right after usermod -aG docker,
			// before the next login); anything else is a daemon that is down.
			daemonDown = true
			socketDenied = err == nil && strings.Contains(strings.ToLower(res.Stderr+res.Stdout), "permission denied")
		} else {
			runtimesJSON = res.Stdout
			for _, n := range runtimeNames(res.Stdout) {
				registered[n] = true
			}
		}
	}
	if socketDenied {
		// Nothing about runtimes can be known: no runsc or kata steps.
		l.Problems = append(l.Problems, "docker's daemon is running but this user cannot reach its socket (`docker info`: permission denied): add the user to the docker group, then log in again (a new group takes effect at the next login) and run doctor again")
		l.RootSteps = []string{`usermod -aG docker "${SUDO_USER:-$USER}"`}
		return l
	}
	if daemonDown {
		// Nothing about runtimes can be known: no runsc or kata steps.
		l.Problems = append(l.Problems, "docker is installed but its daemon is down (`docker info` did not answer): start it, then run doctor again")
		return l
	}
	_, haveRunsc := f.LookPath("runsc")
	_, haveKata := f.LookPath("kata-runtime")
	if l.CLI != "docker" { // podman and nerdctl take the runtime binary by name
		if haveRunsc {
			registered["runsc"] = true
		}
		if haveKata {
			registered["kata"] = true
		}
	}
	for _, n := range runtimePreference {
		if registered[n] {
			l.Runtimes = append(l.Runtimes, n)
		}
	}
	var kata, gvisor string
	for _, n := range l.Runtimes {
		if strings.Contains(n, "kata") && kata == "" {
			kata = n
		}
		if strings.Contains(n, "runsc") && gvisor == "" {
			gvisor = n
		}
	}
	switch {
	case kata != "" && r.KVM && (gvisor == "" || runtimeRank(kata) < runtimeRank(gvisor)):
		l.Runtime, l.Isolation = kata, "vm"
	case gvisor != "":
		l.Runtime, l.Isolation = gvisor, "sandboxed-kernel"
	case kata != "" && r.KVM:
		l.Runtime, l.Isolation = kata, "vm"
	case kata != "":
		l.Problems = append(l.Problems, r.KVMProblem+" (Kata needs it; or install gVisor, runsc)")
		l.RootSteps = append(l.RootSteps, r.KVMRootSteps...)
	case (haveRunsc || haveKata) && l.CLI != "docker", haveKata && !haveRunsc:
		l.Problems = append(l.Problems, "Kata or runsc is installed but the container CLI cannot use it")
	default:
		// Kata, when present, is used above; zm installs gVisor (gvisor.go).
		// A runsc on PATH that docker does not list is not registered as
		// it is: it may be the user's own (writable without root) or lack
		// its sidecars, which its `runsc install` would download unpinned.
		// The pinned release goes in /usr/local/bin and docker gets that.
		steps, problem := gvisorSteps(f, pm, r.Arch, l.CLI)
		p := "no isolating runtime: only runc, crun or youki (they share the host's kernel and are not isolation)"
		if haveRunsc {
			p = "runsc is installed but not registered as a docker runtime"
		}
		if problem != "" {
			p += "; " + problem
		} else {
			p += "; the root line installs gVisor (runsc) release " + gvisorRelease + ", checked against its pinned SHA-512"
		}
		l.Problems = append(l.Problems, p)
		l.RootSteps = append(l.RootSteps, steps...)
	}
	if l.Runtime != "" {
		ociEngine(f, &l, pm, r.Arch)
	}
	l.Available = len(l.Problems) == 0 && l.Runtime != ""
	detectBoxNet(ctx, run, f, &l)
	if slices.ContainsFunc(l.Runtimes, func(r string) bool { return strings.Contains(r, "kata") }) { // Kata, the default or asked for (share_runtime.go)
		detectBoxGuard(ctx, run, f, &l)
	}
	detectSudo(&l, runtimesJSON, registered, slices.ContainsFunc(l.RootSteps, func(s string) bool { return strings.Contains(s, "runsc install") }))
	l.RootSteps = dedupe(l.RootSteps)
	return l
}

// ociEngine asks the driver's engine check what lending would: a check that
// sudo -n could not run is the root step that installs its helper; Kata's
// root calls that sudo -n -k refused are a problem that names its two fixes
// (no root step: Potluck asks for no root beyond its read-only helper); any
// other refusal is a problem no root step fixes. One check can fail more than
// one way (vmhost.VerifyOCIEngine joins them): each is said.
func ociEngine(f Facts, l *Layer, pm, arch string) {
	err := f.OCIEngine(l.CLI, l.Runtime)
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	sudoSaid := false
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, vmhost.ErrOCIOwnerNeedsRoot):
			l.Problems = append(l.Problems, "oci needs a root step: lending reads which process owns the Docker socket as root on every connection, and sudo -n could not ("+err.Error()+"); the root line installs that read-only check at "+vmhost.DockerOwnerHelper+" and a sudoers rule allowing you only it")
			l.RootSteps = append(l.RootSteps, vmhost.OCIOwnerRootSteps()...)
		case errors.Is(err, vmhost.ErrOCIHostNeedsSudo):
			if !sudoSaid {
				sudoSaid = true
				l.Problems = append(l.Problems, kataSudoProblem(f, err, pm, arch, l.CLI))
			}
		default:
			l.Problems = append(l.Problems, "the local Docker engine fails the driver's check: "+err.Error())
		}
	}
}

// kataSudoProblem says why Kata boxes cannot start here (err wraps
// vmhost.ErrOCIHostNeedsSudo) and the two ways on: passwordless sudo, which
// the owner gives, or gVisor, which needs none and which the layer prefers
// over Kata once docker lists it (runtimePreference).
func kataSudoProblem(f Facts, err error, pm, arch, cli string) string {
	s := err.Error() + ". The driver runs them as root with sudo -n -k for every Kata box, with no terminal to type a password at, so a password typed here does not count. " +
		"Potluck's root line does not grant that (sh as root is all of root): give this user passwordless sudo yourself, or lend with gVisor instead, which needs none"
	steps, problem := gvisorSteps(f, pm, arch, cli)
	if problem != "" {
		return s + " (" + problem + ")"
	}
	return s + ": run as root once, then this again:\n  " + RootCommand(steps)
}

func runtimeNames(jsonOut string) []string {
	// {"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"}}: keys only,
	// without a JSON decode, so a driver's odd extras never break detection.
	var names []string
	rest := strings.TrimSpace(jsonOut)
	for {
		i := strings.Index(rest, `"`)
		if i < 0 {
			break
		}
		rest = rest[i+1:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			break
		}
		key := rest[:j]
		rest = rest[j+1:]
		if strings.HasPrefix(strings.TrimSpace(rest), ":") {
			names = append(names, key)
			// skip the value object
			depth := 0
			for k, c := range rest {
				if c == '{' {
					depth++
				} else if c == '}' {
					depth--
					if depth == 0 {
						rest = rest[k+1:]
						break
					}
				}
			}
		}
	}
	return names
}

func firstVersion(out string) string {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	for _, w := range strings.Fields(line) {
		w = strings.TrimRight(w, ",")
		if len(w) > 0 && w[0] >= '0' && w[0] <= '9' {
			return w
		}
	}
	return line
}

func detectTart(ctx context.Context, run Runner, f Facts, r *Report) Layer {
	l := Layer{Name: "tart", Isolation: "vm"}
	if r.OS != "darwin" {
		l.Problems = []string{"tart runs on macOS"}
		return l
	}
	if res, err := runBounded(ctx, run, "sysctl", "-n", "kern.hv_support"); err != nil || res.Exit != 0 || strings.TrimSpace(res.Stdout) != "1" {
		l.Problems = append(l.Problems, "Virtualization.framework is not available on this Mac")
	}
	detectMacOS(ctx, run, r, &l)
	if path, ok := f.LookPath("tart"); !ok {
		l.Problems = append(l.Problems, "tart is not installed: download tart.tar.gz from https://github.com/openai/tart/releases/latest, unpack it and link tart.app/Contents/MacOS/tart onto your PATH (no root needed; guide: https://tart.run/quick-start/)")
	} else {
		path = absoluteExecutable(path, &l)
		l.CLI = path
		if path != "" {
			l.Version = executableVersion(ctx, run, path, "Tart", &l)
		}
		if l.Version != "" {
			detectTartArguments(ctx, run, path, &l)
		}
	}
	detectSoftnet(ctx, run, f, &l)
	if p, err := HostDefaultImage(*r, "tart"); err != nil {
		l.Warnings = append(l.Warnings, err.Error())
	} else if p.Name == SmallLinux {
		l.Warnings = append(l.Warnings, "the default macOS guest is not supported on this host; the host default is the small Linux image (no GitHub Actions runner)")
	}
	detectTartDisk(f, r, &l)
	detectSleep(ctx, run, &l)
	l.Available = len(l.Problems) == 0
	return l
}

// packageManager names the first known package manager on PATH.
func packageManager(f Facts) string {
	for _, pm := range []string{"apt-get", "dnf", "zypper", "pacman"} {
		if _, ok := f.LookPath(pm); ok {
			return pm
		}
	}
	return ""
}

// installStep is the root command that installs a package, or "" when no
// package manager is known (the problem text still says what to install).
func installStep(pm, pkg string) string {
	switch pm {
	case "apt-get":
		return "apt-get install -y " + pkg
	case "dnf":
		return "dnf install -y " + pkg
	case "zypper":
		return "zypper --non-interactive install " + pkg
	case "pacman":
		return "pacman -S --noconfirm " + pkg
	}
	return ""
}

// RootCommand folds every root step into one command line for the owner, or
// "" when there are none: sudo sh -c 'step1 && step2', RootScript quoted.
func RootCommand(steps []string) string {
	if s := RootScript(steps); s != "" {
		return "sudo sh -c " + shQuote(s)
	}
	return ""
}

// RootScript is the script RootCommand runs as root: the steps, each once,
// joined with &&. zm host share and zm pool host run exactly it with sudo.
func RootScript(steps []string) string { return strings.Join(dedupe(steps), " && ") }

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Size is one rung of the ladder.
type Size struct {
	Name     string `json:"name"` // c<vCPU>m<GiB>
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mb"`
}

// Ladder is vCPU/GiB, smallest first.
var Ladder = []Size{
	{"c1m2", 1, 2 << 10}, {"c2m4", 2, 4 << 10}, {"c2m8", 2, 8 << 10}, {"c4m16", 4, 16 << 10}, {"c8m32", 8, 32 << 10},
}

// Limits is what the owner lends: Share of the machine's cores and RAM, and
// optional explicit caps (0 = none). Share 0 is zm's default, DefaultShare,
// with the owner having chosen none: Lend then lends what fits once when
// nothing fits twice (an owner's --share, even 0.5, keeps its meaning).
type Limits struct {
	Share    float64
	MaxCores int
	MaxRAMMB int64
	// Boxes is how many boxes of the largest size lent must fit inside at
	// once: the linux_slots a share binds without an admission controller
	// (slots.go). 0, or anything up to Slots, is Slots.
	Boxes int
}

// boxes is how many boxes of a size must fit for it to be lent.
func (l Limits) boxes() int { return max(l.Boxes, Slots) }

// DefaultShare is the owner-adjustable default.
const DefaultShare = 0.5

// Slots is how many boxes zm-run-tart runs at once per guest OS on a
// machine (its linux_slots and macos_slots, default 2; Apple's licence caps
// macOS guests at 2 per Mac): why a size must fit twice inside the share,
// since two boxes of the largest size lent may run at once. An owner's
// --linux-slots raises it for Linux boxes (Limits.Boxes, slots.go).
const Slots = 2

// Fraction is the share in effect: Share, or DefaultShare when unset.
func (l Limits) Fraction() float64 {
	if l.Share <= 0 || l.Share > 1 {
		return DefaultShare
	}
	return l.Share
}

// Within names what is lent in plain words: "50% of this machine", and the
// owner's caps.
func (l Limits) Within() string {
	s := fmt.Sprintf("%.0f%% of this machine", l.Fraction()*100)
	if l.MaxCores > 0 || l.MaxRAMMB > 0 {
		s += " (capped by --max-cores or --max-ram-mb)"
	}
	return s
}

// Sizes is the ladder's sizes where two VMs fit within share of the RAM and
// the cores.
func Sizes(r Report, share float64) []Size { return SizesWithin(r, Limits{Share: share}) }

// SizesWithin is Sizes with the owner's explicit caps (and Boxes: that many
// VMs, when more than two).
func SizesWithin(r Report, lim Limits) []Size { return fitting(r, lim, lim.boxes()) }

// Lending is what a share lends when the owner names no sizes.
type Lending struct {
	Sizes []Size `json:"sizes"`
	// OneAtATime: no size fits twice inside zm's default share, so these fit
	// once and each binding runs one box at a time (OneAtATimeConfig).
	OneAtATime bool `json:"one_at_a_time,omitempty"`
}

// OneAtATimeConfig is what a binding carries to run one box at a time:
// zm-run-tart counts every live box of its state directory, whichever size's
// binding made it, against the acquiring binding's slots.
var OneAtATimeConfig = map[string]string{"linux_slots": "1", "macos_slots": "1"}

// Lend is the sizes `zm host share` lends and `zm host doctor` reports:
// those where lim.boxes() boxes (Slots, or the owner's more) fit within the
// limits, none below the images' floor (ImageMinimums). When no size fits
// twice at all, the share is zm's default (lim.Share 0) and the owner asked
// for no more than two at once, it is those where one box fits, OneAtATime:
// the friend's machine still gives no more than the default share, and
// lends something without a flag. (Not when sizes fit twice but the floor
// rules them out: conformance's peers check runs two boxes at once, and two
// of a size that fits once inside half then could take the whole machine.)
func Lend(r Report, lim Limits, images []string) Lending {
	minCPU, minMem := ImageMinimums(images)
	small := func(s Size) bool { return s.CPU < minCPU || s.MemoryMB < minMem }
	twice := fitting(r, lim, lim.boxes())
	if len(twice) > 0 || lim.Share > 0 || lim.Boxes > Slots {
		return Lending{Sizes: slices.DeleteFunc(twice, small)}
	}
	once := slices.DeleteFunc(fitting(r, lim, 1), small)
	return Lending{Sizes: once, OneAtATime: len(once) > 0}
}

// fitting is the ladder's sizes where n boxes fit within the limits.
func fitting(r Report, lim Limits, n int) []Size {
	share := lim.Fraction()
	cores := float64(r.Cores) * share
	ram := float64(r.RAMMB) * share
	if lim.MaxCores > 0 && float64(lim.MaxCores) < cores {
		cores = float64(lim.MaxCores)
	}
	if lim.MaxRAMMB > 0 && float64(lim.MaxRAMMB) < ram {
		ram = float64(lim.MaxRAMMB)
	}
	var out []Size
	for _, s := range Ladder {
		if float64(n*s.CPU) <= cores+1e-9 && float64(n*s.MemoryMB) <= ram+1e-9 {
			out = append(out, s)
		}
	}
	return out
}

// SizeByName finds a ladder size.
func SizeByName(name string) (Size, error) {
	for _, s := range Ladder {
		if s.Name == name {
			return s, nil
		}
	}
	var names []string
	for _, s := range Ladder {
		names = append(names, s.Name)
	}
	return Size{}, fmt.Errorf("unknown size %q (the ladder: %s)", name, strings.Join(names, ", "))
}
