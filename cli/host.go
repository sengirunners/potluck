package cli

// potluck doctor and potluck host's checks and flags (copied from zm's
// cmd/zm host.go and host_admission.go): what this machine can share, then
// digest-pinned images pulled, conformance run, and only the sizes that
// passed shared. The logic is in hostprobe.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/roomwait"
)

// Test hooks: share's pulls and conformance, what this machine is, and the
// driver host shares with ("" is hostprobe's default, the VM driver).
var (
	shareRunner hostprobe.Runner = hostprobe.ExecRunner{}
	detectHost                   = func(ctx context.Context, dir string) hostprobe.Report {
		return hostprobe.Detect(ctx, hostprobe.ExecRunner{}, hostprobe.SystemFacts{}, dir)
	}
	shareDriver = ""
)

type hostFlags struct {
	share    float64
	maxCores int
	maxRAMMB int64
	stateDir string
	flags    *pflag.FlagSet
}

func (f *hostFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	f.flags = fl
	fl.Float64Var(&f.share, "share", hostprobe.DefaultShare, "the fraction of this machine's cores and RAM to share (0 to 1]")
	fl.IntVar(&f.maxCores, "max-cores", 0, "cap the shared cores (0: no cap beyond --share)")
	fl.Int64Var(&f.maxRAMMB, "max-ram-mb", 0, "cap the shared RAM in MB (0: no cap beyond --share)")
	fl.StringVar(&f.stateDir, "state-dir", "", "where the driver keeps images and records (default: the driver's own state directory)")
}

func (f *hostFlags) limits() (hostprobe.Limits, error) {
	if f.share <= 0 || f.share > 1 {
		return hostprobe.Limits{}, errors.New("--share: want a fraction above 0 and at most 1")
	}
	lim := hostprobe.Limits{Share: f.share, MaxCores: f.maxCores, MaxRAMMB: f.maxRAMMB}
	if !f.flags.Changed("share") {
		lim.Share = 0 // the default, not the owner's: hostprobe.Lend may share what fits once
	}
	return lim, nil
}

// dir is the state directory host and doctor probe: --state-dir, else the
// home's. Only the potluck program's own run falls back to ~/.potluck; a
// mounted run with no home from its host is refused.
func (f *hostFlags) dir(a *app) (string, error) {
	if f.stateDir != "" {
		return f.stateDir, nil
	}
	cfg, err := a.loadClient()
	if err == nil && cfg.Home != "" {
		return cfg.Home, nil
	}
	if !a.own {
		return "", err
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, HomeDir), nil
}

type doctorReport struct {
	hostprobe.Report
	Share float64          `json:"share"`
	Sizes []hostprobe.Size `json:"sizes"`
	// OneAtATime: the sizes fit once inside the default share, not twice.
	OneAtATime bool              `json:"one_at_a_time,omitempty"`
	Admission  []doctorAdmission `json:"admission,omitempty"`
	// Isolation is each binding on this machine: may it run anyone's code,
	// and a box binding's latest block test.
	Isolation []bindingIsolation `json:"isolation,omitempty"`
	// IsolationPartial says which list of bindings could not be read, when
	// the other could ("" when both were read, or the daemon is not running).
	IsolationPartial string `json:"isolation_unread,omitempty"`
	Best             string `json:"best_layer,omitempty"`
	// Unowned are, by project, the project.toml settings nothing in this
	// build reads: kept and written back, never checked.
	Unowned     map[string][]string `json:"unowned_project_settings,omitempty"`
	RootCommand string              `json:"root_command,omitempty"`
}

func (a *app) doctorCmd() *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{Use: "doctor", Short: "What this machine can share, and the one root command that would fix the rest", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			lim, err := f.limits()
			if err != nil {
				return err
			}
			dir, err := f.dir(a)
			if err != nil {
				return err
			}
			r := detectHost(cmd.Context(), dir)
			out := doctorReport{Report: r, Share: lim.Fraction()}
			var images []string // what host shares without --image: the best layer's default
			if b, ok := r.Best(); ok {
				out.Best = b.Name
				if p, err := hostprobe.HostDefaultImage(r, b.Name); err == nil {
					images = []string{p.Image}
					for _, tp := range hostprobe.LayerTemplates(b.Name) {
						if tp.OS == p.OS && tp.Image != "" {
							images = append(images, tp.Image)
						}
					}
				}
			}
			l := hostprobe.Lend(r, lim, images)
			out.Sizes, out.OneAtATime = l.Sizes, l.OneAtATime
			out.RootCommand = hostprobe.RootCommand(r.RootSteps(out.Best))
			out.Admission = a.bindingAdmission(cmd.Context())
			var ierr error
			if out.Isolation, out.IsolationPartial, ierr = a.localIsolation(cmd.Context()); ierr != nil {
				out.IsolationPartial = "what this machine lends could not be read (" + oneLineErr(ierr) + "; is " + a.cmd("daemon") + " running?)"
			}
			if cfg, err := a.loadClient(); err == nil {
				if u := project.UnownedIn(cfg.Home); len(u) > 0 {
					out.Unowned = u
				}
			}
			if a.asJSON() {
				if err := a.printJSON(out); err != nil {
					return err
				}
				if out.Best == "" || !hostswap.Safe(r.Swap) {
					return exitSilently(1)
				}
				return nil
			}
			w := a.host.Stdout
			fmt.Fprintf(w, "%s/%s, %d cores, %d MB RAM, %.0f GB free in %s, kvm: %t\n", r.OS, r.Arch, r.Cores, r.RAMMB, r.FreeDiskGB, r.StateDir, r.KVM)
			if r.MacOS != "" {
				fmt.Fprintf(w, "macOS %s\n", r.MacOS)
			}
			for _, l := range r.Layers {
				state := "not ready"
				switch {
				case l.Available && !l.Supported:
					state = "ready, but Potluck cannot use it yet"
				case l.Available:
					state = "ready"
				}
				detail := strings.TrimSpace(strings.Join([]string{l.Version, l.CLI, l.Runtime}, " "))
				fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("  %-6s %s %s", l.Name, state, detail), " "))
				if l.SoftnetPath != "" {
					fmt.Fprintf(w, "         Softnet %s at %s\n", l.SoftnetVersion, l.SoftnetPath)
				}
				for _, p := range l.Problems {
					fmt.Fprintf(w, "         - %s\n", p)
				}
				for _, p := range l.Warnings {
					fmt.Fprintf(w, "         ! %s\n", p)
				}
				if l.Network != "" {
					capped := ""
					if l.NetworkMbit > 0 {
						capped = fmt.Sprintf("; downloads capped at %d Mbit/s for all of them together", l.NetworkMbit)
					}
					fmt.Fprintf(w, "         GitHub runners and sandboxes: filtered network %s (internet, never this machine or its LAN%s)\n", l.Network, capped)
				} else if l.NetworkProblem != "" && l.Available {
					fmt.Fprintf(w, "         - %s\n", l.NetworkProblem)
				}
			}
			var n []string
			for _, s := range out.Sizes {
				n = append(n, s.Name)
			}
			switch {
			case len(n) == 0 && len(hostprobe.Lend(r, lim, nil).Sizes) > 0:
				how := "twice inside " + lim.Within()
				if hostprobe.Lend(r, lim, nil).OneAtATime {
					how = "inside " + lim.Within() + ", even one at a time"
				}
				fmt.Fprintf(w, "sizes: none big enough for the default image fit %s\n", how)
			case len(n) == 0 && lim.Share == 0:
				fmt.Fprintf(w, "sizes: none fit inside %s, even one at a time\n", lim.Within())
			case len(n) == 0:
				fmt.Fprintf(w, "sizes: none fit twice inside %s\n", lim.Within())
			case out.OneAtATime:
				fmt.Fprintf(w, "sizes (one at a time: none fits twice inside %.0f%%): %s\n", out.Share*100, strings.Join(n, ", "))
			default:
				fmt.Fprintf(w, "sizes (two fit inside %.0f%%): %s\n", out.Share*100, strings.Join(n, ", "))
			}
			if r.SwapNote != "" {
				fmt.Fprintf(w, "! %s\n", r.SwapNote)
			}
			printDoctorAdmission(w, out.Admission)
			printIsolation(w, out.Isolation, out.IsolationPartial, prefix(a.host.Prefix))
			for _, slug := range slices.Sorted(maps.Keys(out.Unowned)) {
				fmt.Fprintf(w, "project %s: project.toml has settings nothing in this build reads (kept, not checked; a misspelt name looks the same): %s\n",
					slug, strings.Join(out.Unowned[slug], ", "))
			}
			if out.RootCommand != "" {
				fmt.Fprintf(w, "as root, once:\n  %s\n", out.RootCommand)
			}
			if out.Best == "" {
				return exitWith{1, errors.New("no layer here is ready, so this machine cannot share sandboxes yet (the problems are listed above)")}
			}
			if !hostswap.Safe(r.Swap) {
				return exitWith{1, errors.New(swapProblem(r))}
			}
			fmt.Fprintf(w, "next: %s   (layer %s; the default image unless --image name@sha256:<digest>)\n", a.cmd("host"), out.Best)
			return nil
		}}
	f.add(cmd)
	return cmd
}

// swapProblem is doctor's failure for swap that is not safe for box secrets.
// A note that already names the fix (Linux) is not followed by it again.
func swapProblem(r hostprobe.Report) string {
	state := r.Swap
	if state == "" {
		state = "not reported"
	}
	msg := "swap is " + state + ": a pool gives a machine like this no box secrets, only secret-free work, until that is fixed"
	if r.OS == "linux" && !strings.Contains(r.SwapNote, hostswap.Fix) {
		msg += "; " + hostswap.Fix
	}
	return msg
}

// rootStep is how potluck host runs doctor's root step itself: sudo asks
// the person at the terminal for the password; potluck never reads, sees,
// stores or passes it. Tests replace it.
type rootStep struct {
	tty  func() bool
	sudo func() bool
	run  func(ctx context.Context, script string) error
}

var hostRoot = rootStep{
	tty: onATerminal,
	sudo: func() bool {
		_, err := exec.LookPath("sudo")
		return err == nil
	},
	// Not CommandContext: Ctrl-C reaches sudo from the terminal, and sudo
	// then puts the terminal's echo back.
	run: func(_ context.Context, script string) error {
		c := exec.Command("sudo", "sh", "-c", script)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr // stdout stays host's (--json)
		return c.Run()
	},
}

// onATerminal: a person is at the keyboard.
func onATerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// first runs the root step host needs with sudo when a person at a
// terminal can answer sudo, then reads the machine again. Else, or when
// sudo does not finish, it says why and host goes on without it. Only a
// cancelled command is an error.
func (rs rootStep) first(ctx context.Context, o *hostprobe.ShareOptions, noSudo bool, again func() hostprobe.Report, w io.Writer, p prefix) error {
	steps := o.RootSteps()
	if len(steps) == 0 {
		return nil
	}
	need := p.name() + " needs admin rights once to " + rootWhy(o.Report, steps)
	switch {
	case o.DryRun:
		fmt.Fprintf(w, "%s; a dry run does not ask: the root line below does it\n", need)
	case noSudo:
		fmt.Fprintf(w, "%s; --no-sudo: run the root line below yourself, then this again\n", need)
	case !rs.tty():
		fmt.Fprintf(w, "%s; not at a terminal, so %s did not ask: run the root line below yourself, then this again\n", need, p.name())
	case !rs.sudo():
		fmt.Fprintf(w, "%s; sudo is not installed: run the root line below as root, then this again\n", need)
	default:
		fmt.Fprintf(w, "%s; sudo may ask for your password (%s prints the exact command).\n", need, p.cmd("doctor"))
		err := rs.run(ctx, hostprobe.RootScript(steps))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.Report = again()
		switch {
		case err != nil:
			fmt.Fprintf(w, "the admin step did not finish (%v): %s goes on without it; the root line below does it\n", err, p.name())
		case len(o.RootSteps()) > 0:
			fmt.Fprintln(w, "the admin step ran, but this machine is not ready yet (if it added you to a group, such as kvm, that counts only from your next login): what is left is below")
		}
	}
	return nil
}

// rootWhy says in plain words what the root steps are for.
func rootWhy(r hostprobe.Report, steps []string) string {
	var network []string
	for _, l := range r.Layers {
		network = append(network, l.NetworkSteps...)
	}
	net, other := false, false
	for _, s := range steps {
		if slices.Contains(network, s) {
			net = true
		} else {
			other = true
		}
	}
	switch {
	case net && other:
		return "set up what its VMs need and the firewall they run behind"
	case net:
		return "set up the firewall its VMs run behind"
	}
	return "set up what its VMs need on this machine"
}

// shareFlags are potluck host's flags.
type shareFlags struct {
	f           hostFlags
	o           hostprobe.ShareOptions
	sizes       []string
	dryRun      bool
	noSudo      bool
	pullTimeout time.Duration
	shelfGB     int
	volumeMaxGB int
	// when is --when (any or away), detach --detach, and skipAfter the
	// host program's flag that skips its AfterLend.
	when      string
	detach    bool
	skipAfter bool
	// inviteStdin is --invite-stdin: host's argument (the invite code, or
	// a pool) is read from stdin, so it is in no process's argv.
	inviteStdin bool
	instead     string // --instead-of-invite (menubar.InsteadFlag)
	linuxSlots  int
	ttlMaxS     int
	wait        time.Duration // --wait: a size's check waits this long for a free slot (roomwait)
}

func (s *shareFlags) add(cmd *cobra.Command, a *app) {
	s.f.add(cmd)
	fl, o := cmd.Flags(), &s.o
	fl.StringArrayVar(&o.Images, "image", nil, "an image pinned by digest, name@sha256:<64 hex> (repeat; default: Potluck's pinned image for the layer, macOS on tart)")
	fl.StringVar(&o.With, "with", "", "lend one named image instead of the default (xcode on tart); cannot combine with --image")
	fl.StringVar(&o.Layer, "layer", "", "oci or tart (default: the best available)")
	fl.StringSliceVar(&s.sizes, "sizes", nil, "sizes to share, e.g. c2m4,c4m16 (default: every size that fits)")
	fl.StringVar(&o.NamePrefix, "name-prefix", "", "shared sizes are named <prefix>-<size> (default: this machine's name)")
	fl.StringVar(&o.OCIRuntime, "oci-runtime", "", "oci: the isolating runtime (Kata or runsc; default: detected)")
	fl.StringVar(&o.OCIHostProfile, "oci-host-profile", "", "oci: absolute owner resource-profile JSON; required for Kata's admitted, bounded host resources")
	fl.StringVar(&o.OCICLI, "oci-cli", "", "oci: verified local Docker (default: detected; Podman and nerdctl are unavailable)")
	fl.StringVar(&o.OCINetwork, "oci-network", "", "oci: a filtered network you made for Potluck (default: its own for sandboxes that serve GitHub runners when doctor finds it, else none; a named one needs --net-mail)")
	fl.BoolVar(&o.Docker, "docker", false, "oci: sandboxes with root (sudo) and rootless Docker inside (default: what an earlier host run chose; --docker=false turns it off)")
	fl.BoolVar(&o.Root, "root", false, "tart: sandboxes keep the image's passwordless sudo, root inside each throwaway VM as on GitHub's macOS runners; conformance checks root is still fenced (default: earlier root=on, else the image pin; --root=false turns it off)")
	fl.StringVar(&o.RunnerDir, "runner-dir", "", "where the image keeps GitHub's runner, so each sandbox serves Actions jobs (default: from the pin for Potluck's own images; none: no runner)")
	fl.StringArrayVar(&o.NetAllow, "net-allow", nil, "host:port each sandbox must reach (the internet), passed to conformance (repeat)")
	fl.StringArrayVar(&o.NetForbid, "net-forbid", nil, "host:port no sandbox may reach (a LAN host, the router, metadata), passed to conformance with this machine's own addresses (repeat)")
	fl.StringArrayVar(&o.NetMail, "net-mail", nil, "an outside mail server (host, checked on 25, 465 and 587, or host:port) no sandbox may open, passed to conformance (repeat)")
	fl.IntVar(&o.NetMbit, "net-mbit", 0, "oci: cap what all sandboxes on Potluck's network download together, in Mbit/s, for a machine whose uplink you guard; a root step (default: what an earlier host run set; 0 removes the cap)")
	fl.IntVar(&s.shelfGB, "shelf-gb", 0, "oci: turn the package shelf on: make the potluck-cache volume (if missing) and cap what it holds at this many GiB, on every size, so sandboxes find public npm packages on a read-only shelf (docs/shelf.md); 0 fills nothing (default: what an earlier host run set)")
	fl.StringVar(&o.ShelfFill, "shelf-fill", "", "oci: what fills the shelf: seed (only "+a.cmd("host shelf fill")+"; the default), \"boxes\" (also the public packages that lockfiles in sandboxes name, so a sandbox can tell someone on this host recently used a given public package) or off; needs --shelf-gb above 0 (default: what an earlier host run set)")
	fl.IntVar(&s.volumeMaxGB, "volume-max-gb", 0, "oci: the most each renter's named volumes may hold on each size, in GiB, on every size this run shares (default 20; 0 turns volumes off; default: what an earlier host run set)")
	fl.IntVar(&s.linuxSlots, "linux-slots", 0, fmt.Sprintf("how many Linux sandboxes run here at once, across every size, 1..%d: without an admission controller every size shared must fit that many times inside --share; with one it decides room and this is a ceiling. macOS stays at 2 (Apple's licence) (default: what an earlier host run set, else 2)", hostprobe.MaxLinuxSlots))
	fl.IntVar(&s.ttlMaxS, "ttl-max-s", 0, fmt.Sprintf("the longest a sandbox may live, in seconds, on every size shared, %d..%d: raises or lowers the driver's own cap (4 h on tart); a runner job needs its job time plus 300 s (default: what an earlier host run set, else the driver's)", hostprobe.MinTTLMaxS, hostprobe.MaxTTLMaxS))
	fl.BoolVar(&s.dryRun, "dry-run", false, "print the plan; pull, test and share nothing")
	fl.BoolVar(&s.o.ShowCommands, "show-commands", false, "diagnostics: also print each image pull and conformance command line (the stages say what runs)")
	fl.DurationVar(&s.pullTimeout, "pull-timeout", 0, "give up on an image pull after this long (default: image pin budget, else 60m; a stopped pull resumes)")
	roomFlag(fl, &s.wait, roomwait.Default, "every slot of this machine is in use by live sandboxes, so a size cannot be checked")
	fl.BoolVar(&s.noSudo, "no-sudo", false, "never run the root step "+a.cmd("doctor")+" prints with sudo: print it, and share without it")
	fl.BoolVar(&o.Full, "full", false, "run the whole conformance suite on every size (minutes per size); default: the security checks once, a boot check per size")
	fl.IntVar(&o.ReclaimIdleS, "reclaim-idle-s", 0, fmt.Sprintf("macOS: a sandbox ends when you touch the keyboard or pointer and starts only after this many idle seconds; 0 shares all the time (default: what an earlier host run chose, else %d on macOS, 0 elsewhere)", hostprobe.DefaultReclaimIdleS))
	fl.StringVar(&s.when, "when", "", fmt.Sprintf("when sandboxes may run: any (any time; --reclaim-idle-s 0) or away (macOS: only while you are away from the keyboard and pointer; --reclaim-idle-s %d) (default: what an earlier host run chose, else away on macOS, any elsewhere)", hostprobe.DefaultReclaimIdleS))
	fl.BoolVar(&s.detach, "detach", false, "after the checks and the admin step, go on in the background, its words in the home's host.log ("+a.cmd("host status")+" says how it goes)")
	fl.BoolVar(&s.inviteStdin, flagInviteStdin, false, "read the invite code (or pool) as one line from stdin instead of an argument, so no other user sees it in the process list")
	fl.StringVar(&s.instead, menubar.InsteadFlag, "", "with no invite code or pool given, the pool to share with (empty: refuse)")
	_ = fl.MarkHidden(menubar.InsteadFlag) // the setup window's
	if al := a.host.AfterLend; al != nil && al.Skip != "" {
		fl.BoolVar(&s.skipAfter, al.Skip, false, al.SkipUsage)
	}
	fl.StringVar(&o.AdmissionURL, "admission-url", "", "a machine that runs other work too: every sandbox first takes a lease from this admission controller (cmd/potluck-driver-tart/README.md; default: what an earlier host run set; none removes it)")
	fl.StringVar(&o.AdmissionTokenFile, "admission-token-file", "", "a 0600 file whose token goes with every admission call (default: an earlier host run's)")
	fl.IntVar(&o.AdmissionTTLS, "admission-ttl-s", 0, "how long a lease lasts unrenewed, 120..3600 s (default: an earlier host run's, else the driver's 120)")
}

// rootStep is the root step for host to run once its own checks pass.
func (a *app) rootStep(ctx context.Context, s *shareFlags) func(*hostprobe.ShareOptions) error {
	return func(o *hostprobe.ShareOptions) error {
		if len(o.RootSteps()) == 0 {
			return nil
		}
		if err := hostprobe.Precheck(ctx, shareRunner, *o); err != nil {
			return err
		}
		dir, err := s.f.dir(a)
		if err != nil {
			return err
		}
		return hostRoot.first(ctx, o, s.noSudo, func() hostprobe.Report { return detectHost(ctx, dir) }, a.host.Stderr, prefix(a.host.Prefix))
	}
}

// shareOptions are the flags as ShareOptions, with this machine's facts.
func (a *app) shareOptions(cmd *cobra.Command, s *shareFlags) (hostprobe.ShareOptions, error) {
	o := s.o
	lim, err := s.f.limits()
	if err != nil {
		return o, err
	}
	o.Limits, o.Sizes, o.DryRun, o.Driver = lim, s.sizes, s.dryRun, shareDriver
	o.PullTimeout = s.pullTimeout
	o.Wait = roomWait(s.wait, nil, "") // its lines go with the other steps (Progress)
	o.StateDir = s.f.stateDir
	if cfg, err := a.loadClient(); err == nil { // conformance tests the file the daemon will pin
		o.DriverDirs = a.driverDirs(cfg)
		o.Home = cfg.Home // the daemon the sizes bind in: Potluck's record of Tart's images names it
	}
	if err := s.reclaim(cmd, &o); err != nil {
		return o, err
	}
	o.DockerSet = cmd.Flags().Changed("docker")
	o.RootSet = cmd.Flags().Changed("root")
	o.NetMbitSet = cmd.Flags().Changed("net-mbit")
	if cmd.Flags().Changed("shelf-gb") {
		o.ShelfGB = &s.shelfGB
	}
	if cmd.Flags().Changed("volume-max-gb") {
		o.VolumeMaxGB = &s.volumeMaxGB
	}
	if cmd.Flags().Changed("linux-slots") {
		o.LinuxSlots = &s.linuxSlots
	}
	if cmd.Flags().Changed("ttl-max-s") {
		o.TTLMaxS = &s.ttlMaxS
	}
	if o.NamePrefix == "" { // the machine name, read without touching the daemon
		cfg, err := a.loadClient()
		if err != nil {
			return o, err
		}
		o.NamePrefix = strings.ToLower(cfg.Machine)
	}
	if o.DryRun { // a dry run never starts the daemon: a re-run's keys are only noted
		if ts, err := a.bindingsIn(cmd.Context(), false, true); err == nil {
			o.Earlier = ts
		} else if o.With == "" && len(o.Images) == 0 {
			// An implicit image plan could silently replace a named image.
			o.EarlierUnread = err.Error()
		} else if !a.asJSON() {
			fmt.Fprintln(a.host.Stderr, "note: earlier bindings could not be read; the explicit image plan does not show earlier admission keys (host keeps them)")
		}
	}
	dir, err := s.f.dir(a)
	if err != nil {
		return o, err
	}
	o.Report = detectHost(cmd.Context(), dir)
	o.HostAddrs = hostprobe.HostAddrs()
	if !a.asJSON() {
		o.Progress = func(line string) { fmt.Fprintln(a.host.Stderr, line) }
	}
	return o, nil
}

// reclaim sets o's reclaim policy from --when or --reclaim-idle-s, marking
// it chosen for this run; with neither, an earlier host run's stands, else
// the default (hostprobe keeps the earlier one).
func (s *shareFlags) reclaim(cmd *cobra.Command, o *hostprobe.ShareOptions) error {
	fl := cmd.Flags()
	switch {
	case fl.Changed("when") && fl.Changed("reclaim-idle-s"):
		return exitWith{2, errors.New("--when or --reclaim-idle-s, not both: --when away is --reclaim-idle-s " + strconv.Itoa(hostprobe.DefaultReclaimIdleS) + ", --when any is --reclaim-idle-s 0")}
	case fl.Changed("when"):
		switch s.when {
		case "any":
			o.ReclaimIdleS = 0
		case "away":
			o.ReclaimIdleS = hostprobe.DefaultReclaimIdleS
		default:
			return exitWith{2, fmt.Errorf("--when %q: want any or away", s.when)}
		}
		o.ReclaimSet = true
	case fl.Changed("reclaim-idle-s"):
		o.ReclaimSet = true
	default:
		o.ReclaimIdleS = defaultReclaimIdleS()
	}
	return nil
}

// defaultReclaimIdleS: a Mac is shared only while its owner is away from it.
func defaultReclaimIdleS() int {
	if runtime.GOOS == "darwin" {
		return hostprobe.DefaultReclaimIdleS
	}
	return 0
}

// reportShare prints a share's result and fails when a size stayed unshared.
func (a *app) reportShare(res hostprobe.ShareResult, w io.Writer) error {
	if a.asJSON() {
		if err := a.printJSON(res); err != nil {
			return err
		}
	} else {
		printShare(w, res, prefix(a.host.Prefix))
	}
	if res.Failed() {
		return errors.New("some sizes were not shared")
	}
	return nil
}

func printShare(w io.Writer, res hostprobe.ShareResult, p prefix) {
	verb := "ran"
	if res.DryRun {
		verb = "would run"
	}
	fmt.Fprintf(w, "layer %s\n", res.Layer)
	switch {
	case res.NamedImage != "":
		fmt.Fprintf(w, "image %s (selected with --with %s)\n", strings.Join(res.Images, ", "), res.NamedImage)
	case res.DefaultImage && len(res.Images) > 1:
		fmt.Fprintf(w, "images %s (the default, then its templates for %s; --image to choose another)\n", strings.Join(res.Images, ", "), p.cmd("sandbox create --template"))
	case res.DefaultImage:
		fmt.Fprintf(w, "image %s (the default; --image to choose another)\n", strings.Join(res.Images, ", "))
	}
	if !res.Full {
		fmt.Fprintln(w, "quick check: the security checks once, a boot check per size (--full for the whole suite)")
	}
	if res.ReclaimIdleS > 0 {
		fmt.Fprintf(w, "sandboxes end when you touch the keyboard or pointer, and start after %ds idle (--when any: always shared)%s\n", res.ReclaimIdleS, keptFrom(res.ReclaimFrom))
	} else if res.ReclaimFrom != "" {
		fmt.Fprintf(w, "sandboxes may run any time%s\n", keptFrom(res.ReclaimFrom))
	}
	if ad := res.Admission; ad != nil {
		fmt.Fprintf(w, "admission: every sandbox lent first takes a lease from %s%s; the checks this command runs take none", ad["admission_url"], admissionDetail(ad))
		if res.AdmissionFrom != "" {
			fmt.Fprintf(w, "; kept from %s (--admission-url none removes it)", res.AdmissionFrom)
		}
		fmt.Fprintln(w)
	}
	if res.RunnerDir != "" {
		net := ""
		if res.Network != "" && res.Network != "none" {
			net = ", network " + res.Network
		}
		fmt.Fprintf(w, "GitHub Actions: each sandbox serves a runner (runner_dir %s%s)\n", res.RunnerDir, net)
	}
	printStorage(w, res)
	for _, n := range res.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	for _, p := range res.Pulls {
		switch {
		case res.ShowCommands:
			fmt.Fprintf(w, "%s: %s\n", verb, shellLine(p.Command))
		case res.DryRun:
			fmt.Fprintf(w, "would %s\n", p.What)
		}
	}
	for _, s := range res.Sizes {
		if res.ShowCommands {
			fmt.Fprintf(w, "%s: %s\n", verb, strings.Join(s.Conformance, " "))
		}
		switch {
		case res.DryRun:
			fmt.Fprintf(w, "  would share %s (%d vCPU, %d MB) if it passes\n", s.Binding, s.Size.CPU, s.Size.MemoryMB)
		case s.Bound:
			fmt.Fprintf(w, "  shared %s\n", s.Binding)
		default:
			fmt.Fprintf(w, "  %s\n", s.Error)
		}
	}
}

// keptFrom says which earlier shared size a choice was kept from ("" for
// none).
func keptFrom(name string) string {
	if name == "" {
		return ""
	}
	return "; kept from " + name + " (--when changes it)"
}

// printStorage says the shelf and volume keys every size is bound with.
func printStorage(w io.Writer, res hostprobe.ShareResult) {
	st := res.Storage
	if st == nil {
		return
	}
	var kv []string
	for _, k := range slices.Sorted(maps.Keys(st.Keys)) {
		kv = append(kv, k+"="+st.Keys[k])
	}
	kept := ""
	if st.From != "" {
		kept = "; the ones you did not pass kept from " + st.From
	}
	fmt.Fprintf(w, "shelf and volumes, on every size that passes: %s%s\n", strings.Join(kv, ", "), kept)
	if len(st.Volume) > 0 {
		verb := "makes"
		if res.DryRun {
			verb = "would make"
		}
		fmt.Fprintf(w, "  %s the shelf volume if missing: %s\n", verb, strings.Join(st.Volume, " "))
	}
	if gb, ok := st.Keys["shelf_max_gb"]; st.Keys["shelf_fill"] == "boxes" && ok && gb != "0" {
		fmt.Fprintln(w, "  shelf_fill=boxes: the host reads the public packages each box's lockfile names and fetches them for the shelf; a box can tell that someone on this host recently used a given public package")
	}
}

// bindingsIn is this machine's shared sizes in one scope: machine-level
// (host), else the resolved pool's. start lets it start the daemon; else a
// daemon that is not running has nothing to say.
func (a *app) bindingsIn(ctx context.Context, start, host bool) ([]computeapi.RunTarget, error) {
	l, err := a.bindingViewsIn(ctx, start, host, false)
	return targetsOf(l), err
}

// bindingViewsIn is bindingsIn's list as the daemon answers it, each binding
// with its effective class (describe: the project's drivers asked too; a
// machine-level binding's driver always is).
func (a *app) bindingViewsIn(ctx context.Context, start, host, describe bool) (computeapi.RunTargetList, error) {
	var c *client.Client
	if start {
		var err error
		if c, _, err = a.client(); err != nil {
			return computeapi.RunTargetList{}, err
		}
	} else {
		var err error
		if c, err = a.readClient(); err != nil {
			return computeapi.RunTargetList{}, err
		}
		if a.pool() != "" {
			c = inPool(c, a.pool())
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var l computeapi.RunTargetList
	var err error
	if host {
		l, err = computeclient.HostRunTargets(ctx, c)
	} else {
		l, err = computeclient.RunTargets(ctx, c, describe)
	}
	return l, err
}

func admissionDetail(ad map[string]string) string {
	var d []string
	if t := ad["admission_ttl_s"]; t != "" {
		d = append(d, "lease "+t+" s")
	}
	if f := ad["admission_token_file"]; f != "" {
		d = append(d, "token from "+f)
	}
	if len(d) == 0 {
		return ""
	}
	return " (" + strings.Join(d, ", ") + ")"
}

// doctorAdmission is a shared size's admission controller, for doctor.
type doctorAdmission struct {
	Binding   string `json:"binding"`
	URL       string `json:"admission_url"`
	TTLS      string `json:"admission_ttl_s,omitempty"`
	TokenFile string `json:"admission_token_file,omitempty"`
}

// bindingAdmission is each of this machine's shared sizes that asks an
// admission controller first (read without starting the daemon).
func (a *app) bindingAdmission(ctx context.Context) []doctorAdmission {
	ts, _ := a.bindingsIn(ctx, false, true)
	if p, err := a.bindingsIn(ctx, false, false); err == nil {
		ts = append(ts, p...)
	}
	var out []doctorAdmission
	for _, t := range ts {
		if t.Kind == "box" && t.Host == "" && t.Config["admission_url"] != "" {
			out = append(out, doctorAdmission{Binding: t.Name, URL: t.Config["admission_url"], TTLS: t.Config["admission_ttl_s"], TokenFile: t.Config["admission_token_file"]})
		}
	}
	slices.SortFunc(out, func(x, y doctorAdmission) int { return strings.Compare(x.Binding, y.Binding) })
	return slices.CompactFunc(out, func(x, y doctorAdmission) bool { return x.Binding == y.Binding })
}

// printDoctorAdmission is doctor's admission line, one per controller.
func printDoctorAdmission(w io.Writer, as []doctorAdmission) {
	var order []string
	by := map[string][]string{}
	for _, ad := range as {
		k := ad.URL + admissionDetail(map[string]string{"admission_ttl_s": ad.TTLS, "admission_token_file": ad.TokenFile})
		if by[k] == nil {
			order = append(order, k)
		}
		by[k] = append(by[k], ad.Binding)
	}
	for _, k := range order {
		fmt.Fprintf(w, "admission: %s take a lease from %s before each sandbox\n", strings.Join(by[k], ", "), k)
	}
}
