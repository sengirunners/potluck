package vmhost

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ociCLI is the Linux VM layer (vm=oci, zm-eakh.12.2): each run is a
// container from an OCI image under a runtime that is a real boundary, driven
// through a container CLI (docker, podman or nerdctl). It is how a Linux box
// lends throwaway machines: Kata Containers gives every run its own microVM (Firecracker or
// Cloud Hypervisor) where /dev/kvm exists, gVisor's runsc a user-space kernel
// where it does not. A plain container runtime (runc, crun, youki) shares the
// host's kernel and is refused: a run target that runs untrusted code needs a
// boundary, not a namespace.
//
// Against the layer contract:
//   - Clone is `<cli> create` with the runtime, no mounts (but the read-only
//     resolv.conf below), no new privileges, a process limit and the network
//     below; PID 1 sleeps, and the run comes in through Exec. OS is always
//     linux.
//   - Boot is `<cli> start -a` under a deadman guard: when the supervisor's
//     pipe closes, the guard removes the container (rm -f). Killing Boot's
//     process group alone does not stop a container, so Destroy always
//     removes it too.
//   - isolated: the container's network is oci_network, "none" by default (no
//     network at all: checkouts come in as a host-side bundle, fetch=host). A
//     named network is one the owner made for zm with egress filtered to the
//     internet only (no LAN, host, tailnet or metadata addresses; README);
//     the layer cannot see firewall rules, so it takes the owner's word for a
//     named one and refuses the CLI's shared ones (bridge, host, default).
//   - DNS (zm-eakh.12.24.7): on a network, the container's resolvers are
//     oci_dns (default ociDefaultDNS), passed as --dns. On a user-defined
//     network that alone is not enough under a boundary runtime: Docker keeps
//     its embedded resolver in resolv.conf (127.0.0.11, served through NAT
//     rules in the container's network namespace, which gVisor's netstack and
//     a Kata VM never pass through) and makes --dns its upstream; Podman
//     writes only aardvark-dns, on the network's gateway (a private address
//     the owner's filter drops). So docker and podman also get a read-only
//     bind of a resolv.conf zm writes for that box naming those resolvers
//     (gVisor's FAQ workaround; both let a user mount on /etc/resolv.conf
//     replace their own; Destroy removes the file). nerdctl writes --dns into
//     resolv.conf itself and adds its own mount on that path regardless, so it
//     gets --dns and --dns-search . (no host search domains) alone. A network
//     joining another namespace (container:, ns:; only with network=nat)
//     keeps that namespace's DNS: the CLIs refuse --dns there.
//   - Exec is `<cli> exec`: no SSH, no agent in the image, the guest
//     command's exit code.
//   - List is every container carrying ociLabelNew or ociLabel.
type ociCLI struct {
	bin, runtime, network string
	dns                   []string // resolver IPs; none with network none
	stateDir              string   // each box's resolv.conf naming dns is under it
	root, docker          bool     // root=on, docker=on (oci_root.go)
	host                  *ociHostLease
	engineHost            string
	engine                *dockerAllocation
	nativeBound           bool
}

// ociLabelNew labels the containers this driver makes; ociLabel is the label
// of those made before the rename's step 2. List takes both as ours.
const (
	ociLabel    = "dev.zm.run-target=1"
	ociLabelNew = "dev.potluck.run-target=1"
)

// ociSharedNets are the CLIs' own networks, which reach the host or its LAN.
var ociSharedNets = map[string]bool{"bridge": true, "host": true, "default": true, "podman": true, "private": true,
	"slirp4netns": true, "pasta": true, "nat": true}

// ociNetName is a plain network name: no "container:<id>", "ns:<path>" or
// "slirp4netns:<options>" forms, which join another namespace or reach the
// host.
var ociNetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ociIsolatedNet reports a network an isolated run may use: none, or a plain
// name the owner made for zm (not one of the CLIs' shared ones).
func ociIsolatedNet(n string) bool {
	return n == "none" || ociNetName.MatchString(n) && !ociSharedNets[strings.ToLower(n)]
}

// ociRuntimes are the runtimes that are a VM or a sandboxed kernel, by exact
// name (podman takes a path for --runtime; a path or any other name is
// refused, so a renamed runc cannot pass).
var ociRuntimes = regexp.MustCompile(`^(runsc|runsc-root|io\.containerd\.runsc\.v1|kata|kata-runtime|kata-(qemu|clh|fc|stratovirt|dragonball)|io\.containerd\.kata(-(qemu|clh|fc|stratovirt|dragonball))?\.v2)$`)

// ociRuntimeIsBoundary reports a runtime that is a VM or a sandboxed kernel
// (Kata, gVisor), not a plain container runtime.
func ociRuntimeIsBoundary(r string) bool { return ociRuntimes.MatchString(r) }

func (o ociCLI) command(a ...string) *exec.Cmd {
	cmd := exec.Command(o.bin, a...)
	cmd.Env = dockerEngineEnv(o.engineHost)
	return cmd
}
func (o ociCLI) boundedCommand(wait time.Duration, a ...string) ([]byte, error) {
	return boundedHostCommandEnv(wait, dockerEngineEnv(o.engineHost), o.bin, a...)
}

// run answers stdout only; engine warnings belong in errors, never parsed data.
func (o ociCLI) run(a ...string) (string, error) {
	cmd := o.command(a...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", o.bin, strings.Join(a, " "), err, strings.TrimSpace(stderr.String()+" "+string(out)))
	}
	return string(out), nil
}

func (o ociCLI) OSOf(name string) (string, error) {
	if _, err := o.run("image", "inspect", name); err == nil {
		return "linux", nil
	}
	if _, err := o.run("container", "inspect", name); err == nil {
		return "linux", nil
	}
	return "", fmt.Errorf("no OCI image %q here (%s pull it first)", name, o.bin)
}

// ociBoxNets are Potluck's own filtered networks (hostprobe's BoxNet and
// LegacyBoxNet, which vmhost cannot import) with the systemd unit that keeps
// each one's filter: a box starts on one only while its unit is active, so a
// filter that failed at boot fails closed. A machine set up under zm keeps
// the old names until its owner rolls them.
var ociBoxNets = map[string]string{
	"potluck-box": "potluck-box-net.service",
	"zm-box":      "zm-box-net.service",
}

// ociNetReady says whether the named network's filter is in place, where
// Potluck can tell without root: for its own networks, the unit is active
// (reading the rules themselves needs root). Tests swap it.
var ociNetReady = func(network string) error {
	unit, own := ociBoxNets[network]
	if !own {
		return nil // an owner's network: the owner's word
	}
	out, _ := exec.Command("systemctl", "is-active", unit).Output()
	if s := strings.TrimSpace(string(out)); s != "active" {
		return fmt.Errorf("oci_network %s: its filter unit %s is %q, not active, so its egress filter may be missing: potluck host doctor says why (a box made with net off, potluck sandbox create --net off, needs no network)", network, unit, s)
	}
	return nil
}

func (o ociCLI) Clone(image, vm string, cpu, memory int) error {
	return o.clone(image, vm, cpu, memory, nil)
}

// clone is Clone with extra create flags (a kept box's HOME volume: oci_persist.go).
func (o ociCLI) clone(image, vm string, cpu, memory int, extra []string) error {
	id, err := o.verifyEngine()
	if err != nil {
		return err
	}
	o.engineHost = id.endpoint
	if id.kata && o.host == nil {
		return fmt.Errorf("Kata needs a verified oci_host_profile and an admitted finite host resource parent")
	}
	a := []string{"create", "--name", vm, "--label", ociLabelNew, "--runtime", o.runtime, "--network", o.network}
	if o.host != nil {
		if _, err := o.host.profile.verifyRuntime(); err != nil {
			return err
		}
		if err := o.host.parent.verify(); err != nil {
			return err
		}
		a = append(a, "--cgroup-parent", o.host.parent.name)
	}
	if id.kata && o.docker {
		if err := ociGuardReady(o.network); err != nil {
			return err
		}
		if err := checkKataTun(); err != nil {
			return err
		}
		a = append(a, "--device", ociGuestTun+":"+ociGuestTun+":rw")
		a = append(a, ociKataDockerProc...)
	}
	if o.root { // root's sudo is setuid (oci_root.go); root holds the bounding set, which needs no device nodes
		if o.runtime == ociSudoRuntime {
			if err := o.checkSudoRuntime(); err != nil {
				return err
			}
		}
		a = append(a, "--cap-drop", "MKNOD")
		if o.network == "none" { // on a network NET_RAW goes below anyway
			a = append(a, "--cap-drop", "NET_RAW")
		}
	} else {
		a = append(a, "--security-opt", "no-new-privileges")
	}
	a = append(a, "--pids-limit", "4096", "--entrypoint", "sh")
	pid1 := "trap 'exit 0' TERM; while :; do sleep 3600 & wait $!; done"
	a, pid1 = o.dockerBoot(a, pid1)
	if o.network != "none" {
		if err := ociNetReady(o.network); err != nil {
			return err
		}
		// On a shared bridge a raw socket could forge ARP or another box's
		// address (a Kata guest's own kernel would let it).
		a = append(a, "--cap-drop", "NET_RAW")
	}
	// DNS keys off this box's own network (a box acquired with net off, or a
	// runner binding's box without net on, is made with none: jobNet), so a
	// box without one gets no --dns or mount.
	dns, err := o.dnsArgs(vm)
	if err != nil {
		return err
	}
	cache, err := o.cacheArgs(image)
	if err != nil {
		return err
	}
	a = append(append(a, dns...), cache...)
	if cpu > 0 {
		a = append(a, "--cpus", strconv.Itoa(cpu))
	}
	if memory > 0 {
		a = append(a, "--memory", strconv.Itoa(memory)+"m")
	}
	a = append(a, extra...)
	anon, err := o.anonArgs(image, vm, a) // oci_box_volumes.go
	if err != nil {
		return err
	}
	a = append(a, anon...)
	// PID 1 waits (any sh: busybox's sleep takes no "infinity"); the run
	// comes in through Exec.
	_, err = o.run(append(a, image, "-c", pid1)...)
	return err
}

// ShelfVolume is the owner's cache for boxes (zm-eakh.12.43), which is
// also the shelf (shelf.go): a volume of the container CLI's by this name,
// which the owner makes and the publisher fills (shelf_publish.go) and no
// renter can name (a renter's volumes have names the driver derives). When it
// exists, every box gets it read-only at ociCacheDir, mounted as a named
// volume is (oci_volume.go: gVisor's gofer), with the same mount for every
// tenant (ociCacheMount), so a box downloads less: a GitHub runner box is
// started with its tools/ in use (jitLaunch), and every box reads its
// goproxy/ through GOPROXY (cacheArgs). No such volume, no mount: that is the
// default.
const ShelfVolume, ociCacheDir = "potluck-cache", "/opt/potluck-cache"

// ociCacheMount is the one mount every box with the shelf gets.
const ociCacheMount = "type=volume,source=" + ShelfVolume + ",target=" + ociCacheDir + ",readonly,volume-nocopy"

// cacheArgs is the shelf's mount for a box created now, decided as describe
// decides capability shelf (ShelfMounted), and with it GOPROXY
// (ociCacheGoproxy: the shelf's Go modules first) unless image's own
// environment sets GOPROXY. A state it cannot read fails the create: a box
// never lacks the shelf describe lists. The flags depend on the volume and
// the image alone, so every tenant's box of an image gets the same ones (a
// warm box too). A run's own GOPROXY (exec's environment) wins over it, and
// GOPRIVATE and GONOPROXY keep their meaning: Go fetches the modules they
// match directly, never through a proxy.
func (o ociCLI) cacheArgs(image string) ([]string, error) {
	on, err := o.ShelfMounted()
	if err != nil || !on {
		return nil, err
	}
	a := []string{"--mount", ociCacheMount}
	_, env, err := o.imageConfig(image)
	if err != nil {
		return nil, err
	}
	if _, own := imageEnv(env, "GOPROXY"); own {
		return a, nil
	}
	return append(a, "-e", "GOPROXY="+ociCacheGoproxy), nil
}

// ociDefaultDNS are the resolvers a box on a named network gets when the
// owner sets no oci_dns: two public anycast resolvers run by two operators
// (Cloudflare, Google), so one operator's outage leaves DNS up. The owner's
// filter drops private, CGNAT, link-local and multicast addresses and the
// host itself, so the host's own upstream (often the LAN router, or a stub
// on 127.0.0.53) is unreachable from a box by design.
var ociDefaultDNS = []string{"1.1.1.1", "8.8.8.8"}

// ociDNSMax is how many nameservers a resolver reads (glibc's and musl's MAXNS).
const ociDNSMax = 3

// parseOCIDNS checks oci_dns: comma-separated public unicast IPs. A private,
// CGNAT, loopback, link-local, multicast or unspecified address is refused:
// the filter drops it, or it is the box or the host itself.
func parseOCIDNS(v string) ([]string, error) {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil || ip.Zone() != "" {
			return nil, fmt.Errorf("oci_dns %q is not an IP address", s)
		}
		if ip = ip.Unmap(); !ociPublicIP(ip) {
			return nil, fmt.Errorf("oci_dns %s is not a public address: the box's filter drops private, CGNAT, link-local and multicast addresses, and loopback is the box itself", s)
		}
		out = append(out, ip.String())
	}
	switch {
	case len(out) == 0:
		return nil, fmt.Errorf("oci_dns names no address")
	case len(out) > ociDNSMax:
		return nil, fmt.Errorf("oci_dns names %d addresses: a resolver reads at most %d", len(out), ociDNSMax)
	}
	return out, nil
}

// ociNonPublic are special-purpose ranges netip does not class as private or
// non-global: this network, CGNAT (tailnets), IETF protocol assignments, the
// three IPv4 documentation ranges, benchmarking, reserved; and in IPv6's
// global unicast 2000::/3, Teredo and 6to4 (tunnels that can carry to any
// IPv4 address, private ones included) and documentation.
var ociNonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("3fff::/20"),
}

// ociGlobal6 is IPv6's global unicast space: anything outside it (NAT64's
// 64:ff9b::/96, which can reach a private IPv4 address, among others) is refused.
var ociGlobal6 = netip.MustParsePrefix("2000::/3")

func ociPublicIP(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !ociGlobal6.Contains(ip) {
		return false
	}
	for _, p := range ociNonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// ociNetHasDNS reports a network whose DNS create may set: not none, and not
// one joining another namespace (container:<id>, ns:<path>), where Docker and
// Podman refuse --dns.
func ociNetHasDNS(n string) bool {
	return n != "none" && !strings.HasPrefix(n, "container:") && !strings.HasPrefix(n, "ns:")
}

// dnsArgs are create's DNS flags for box vm: none unless ociNetHasDNS; else
// --dns for each resolver and, but for nerdctl, a read-only resolv.conf of
// the box's own naming them (see the type's comment).
func (o ociCLI) dnsArgs(vm string) ([]string, error) {
	if !ociNetHasDNS(o.network) || len(o.dns) == 0 {
		return nil, nil
	}
	var a []string
	for _, ip := range o.dns {
		a = append(a, "--dns", ip)
	}
	if strings.HasPrefix(filepath.Base(o.bin), "nerdctl") {
		return append(a, "--dns-search", "."), nil // "." writes no search line, so none of the host's
	}
	p, err := o.resolvConf(vm)
	if err != nil {
		return nil, err
	}
	return append(a, "--mount", "type=bind,source="+p+",target=/etc/resolv.conf,readonly"), nil
}

// resolvPath is box vm's resolv.conf.
func (o ociCLI) resolvPath(vm string) string {
	return filepath.Join(o.stateDir, "oci-resolv", vm+".conf")
}

// resolvConf writes box vm's resolv.conf naming o.dns and answers its path.
// Each box has its own, so no box's file is another's; Destroy removes it.
func (o ociCLI) resolvConf(vm string) (string, error) {
	var b strings.Builder
	b.WriteString("# potluck-driver-tart: this box's resolvers (oci_dns)\n")
	for _, ip := range o.dns {
		b.WriteString("nameserver " + ip + "\n")
	}
	p, dir := o.resolvPath(vm), filepath.Dir(o.resolvPath(vm))
	if !filepath.IsAbs(p) || strings.ContainsAny(p, ",=\"\n") || filepath.Base(p) != vm+".conf" {
		return "", fmt.Errorf("state_dir %q, vm %q: a --mount source must be an absolute path without , = or \"", o.stateDir, vm)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(b.String())
	if err == nil {
		err = tmp.Chmod(0o644) // the guest's user reads it
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return p, os.Rename(tmp.Name(), p)
}

// ociDeadman runs `<cli> start -a` under a guard: when the supervisor's end
// of stdin closes, the guard removes the container. (fd 3: sh gives a
// background job /dev/null as stdin.) The guard holds none of Boot's output:
// when the container ends on its own, start's exit is Boot's at once, and the
// guard's cat (left waiting on the pipe) keeps nothing open that Wait waits on.
const ociDeadman = `vm=$1; bin=$2; shift 2; exec 3<&0
"$bin" start -a "$vm" 3<&- & p=$!
( cat <&3; "$bin" rm -f "$vm"; kill -KILL $p ) >/dev/null 2>&1 & g=$!; exec 3<&-
wait $p; s=$?; kill $g 2>/dev/null; exit $s`

func (o ociCLI) Boot(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	id, err := o.verifyEngine()
	if err != nil {
		return nil, nil, err
	}
	o.engineHost = id.endpoint
	if id.kata && o.host == nil {
		return nil, nil, fmt.Errorf("Kata requires an admitted finite host parent")
	}
	if err := o.guardBoot(vm); err != nil { // before either path: every Kata box boots through bootHost
		return nil, nil, err
	}
	if o.host != nil {
		return o.bootHost(vm, isolated, log)
	}
	return o.boot(vm, isolated, log, ociDeadman)
}

func (o ociCLI) boot(vm string, isolated bool, log io.Writer, deadman string) (*exec.Cmd, io.WriteCloser, error) {
	if err := o.guardBoot(vm); err != nil {
		return nil, nil, err
	}
	if isolated && !ociIsolatedNet(o.network) {
		return nil, nil, fmt.Errorf("oci_network %q may reach the host or its LAN: an isolated run needs none or the plain name of a network made for zm with egress filtered", o.network)
	}
	cmd := exec.Command("sh", "-c", deadman, "sh", vm, o.bin)
	cmd.Env = dockerEngineEnv(o.engineHost)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	held, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, held, cmd.Start()
}

func (o ociCLI) Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd {
	a := []string{"exec"}
	if stdin != nil {
		a = append(a, "-i")
	}
	cmd := o.command(append(append(a, vm), argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd
}

func (o ociCLI) List() ([]vmInfo, error) {
	var vs []vmInfo
	seen := map[string]bool{}
	// Two asks: label filters of one ps are all required, not either.
	for _, label := range []string{ociLabel, ociLabelNew} {
		out, err := o.command("ps", "-a", "--filter", "label="+label, "--format", "{{.Names}}\t{{.Status}}").Output()
		if err != nil {
			return nil, fmt.Errorf("%s ps: %v", o.bin, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name, status, ok := strings.Cut(line, "\t")
			if !ok || name == "" || seen[name] {
				continue
			}
			seen[name] = true
			vs = append(vs, vmInfo{Name: name, Running: strings.HasPrefix(status, "Up"), Local: true})
		}
	}
	return vs, nil
}

func (o ociCLI) Destroy(vm string, runPID int) {
	if o.host != nil {
		o.destroyHost(vm, runPID)
		return
	}
	if o.nativeBound || ociKataRuntime(o.runtime) {
		// Sweeps can clean an exact old run even after its profile drifts.
		// Kept/warm boxes do not use this bounded path.
		name := hostParentName(o.stateDir, vm, 0)
		o.host = &ociHostLease{parent: &ownedHostParent{name: name, path: filepath.Join(hostCgroupRoot, "potluck.slice", name)}}
		o.destroyHost(vm, runPID)
		return
	}
	if runPID > 0 {
		_ = syscall.Kill(-runPID, syscall.SIGKILL)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		_, err := o.run("rm", "-f", "-v", vm) // -v: and any anonymous volume it has (a box this driver made has none: oci_box_volumes.go)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "no such container") {
			if o.stateDir != "" && filepath.Base(vm) == vm {
				_ = os.Remove(o.resolvPath(vm)) // the box is gone: so is its resolv.conf
			}
			o.dropBoxVolumes(vm) // and its HOME and VOLUME directories' volumes (oci_box_volumes.go)
			o.dropCOW(vm)        // and its cow volumes (oci_volume.go)
			return
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
			return
		}
	}
}
