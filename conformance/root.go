package conformance

import (
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Root and Docker in a box (SPEC 20 "Boxes", Root and Docker;
// zm-eakh.12.5.26). A driver listing capability root gives the box's user
// working sudo; root then holds the box's whole bounding set, so that set
// must lack NET_ADMIN (root that could re-address the box's interface could
// answer for a neighbour on a shared bridge), NET_RAW (forged packets) and
// MKNOD. One listing docker (and root with it) runs a
// container, from an image imported from the box's own docker-init (static,
// shipped with dockerd), so the check needs no network and no registry. Its
// boxes ask for the network (net on, where the driver takes net), and root
// must lack NET_RAW and MKNOD too, whatever the box's network.
// Every image the binding lends is checked (config images), not the default
// alone. A driver listing neither must not give sudo either.
//
// A whole VM is the other case: a macOS guest (uname -s says Darwin: Tart's
// root=on, zm-eakh.12.54.14), or a box whose driver lists root-vm (a VM of
// the box's own, as the cloud-hypervisor layer's root=on). Root in a whole VM
// owns the guest's kernel, so a bounding set there holds nothing back and the
// three bit checks are skipped. What fences the box must sit outside it, and
// that is shown from root's own shell: the net probe, run under sudo, must
// not open any --net-forbid or --net-mail address that proves something
// (rootFence). In a Linux guest root first empties whatever packet filter the
// guest runs itself, which is root's to empty and so fences nothing. Root
// there can also change the box's addresses, routes and neighbours, which is
// what the NET_ADMIN check keeps from a box on a shared bridge, so root tries
// it: a forbidden address must stay closed from an address beside it and
// named a neighbour on the box's own link (rootReaddress); another box named
// a neighbour by its hardware address must stay closed (rootNeighbour); and
// an address that opens from the box's own must not open from another box's
// address or any other, nor may taking that box's address cut it off
// (rootTake). root-vm is the driver's word, so it is checked too: the box's
// kernel is no other box's and not this machine's (ownKernel), since on a
// kernel it shares a box's root is held by the bits this group then skips.

// dockerUp is how long the docker check waits for the box's dockerd, which
// its PID 1 starts at boot.
var dockerUp = 90 * time.Second

// Linux capability numbers (capability.h).
const (
	capNetAdmin = 12
	capNetRaw   = 13
	capMknod    = 27
)

func (r *run) root() {
	if r.kind != "box" {
		return
	}
	caps := []string{}
	if r.desc.Capabilities != nil {
		caps = *r.desc.Capabilities
	}
	root, docker, vm := slices.Contains(caps, protocol.CapRoot), slices.Contains(caps, protocol.CapDocker), slices.Contains(caps, protocol.CapRootVM)
	if !root && !docker && !vm {
		r.skip("root and docker in the box", "describe lists neither root nor docker")
		r.add("without capability root, sudo does not work in the box", r.noSudo())
		return
	}
	if !r.add("a driver listing docker lists root", func() error {
		if docker && !root {
			return fmt.Errorf("capabilities %v: docker without root", caps)
		}
		return nil
	}()) {
		return
	}
	if !r.add("a driver listing root-vm lists root", func() error {
		if vm && !root {
			return fmt.Errorf("capabilities %v: root-vm without root", caps)
		}
		return nil
	}()) {
		return
	}
	const (
		acq    = "acquire a box for root and docker"
		sudo   = "sudo works in the box: sudo -n id -u answers 0, no password"
		admin  = "root in the box holds no NET_ADMIN (CapBnd bit 12)"
		raw    = "root in the box holds no NET_RAW (CapBnd bit 13)"
		mknod  = "root in the box holds no MKNOD (CapBnd bit 27)"
		dockrn = "docker run works in the box (an image imported from its docker-init, no network)"
		fence  = "root in a whole-VM box cannot reach what the net checks forbid (--net-forbid, --net-mail), run under sudo"
		readdr = "root in a whole-VM Linux box cannot reach them from another address, or named neighbours on its link"
		neigh  = "root in a whole-VM Linux box cannot reach another box named a neighbour on its link"
		take   = "root in a whole-VM Linux box cannot use an address that is not its own, or cut another box off by taking its address"
		kern   = "a root-vm box has a kernel of its own: its boot id is not this machine's, and no other box sees a kernel setting its root changes"
	)
	errs := map[string][]string{}
	note := func(check, image string, err error) {
		if err != nil {
			if image != "" {
				err = fmt.Errorf("image %s: %v", image, err)
			}
			errs[check] = append(errs[check], err.Error())
		}
	}
	// wholes are whole-VM boxes, vms the Linux ones among them, linux the
	// boxes that are not whole VMs.
	boxes, linux, wholes, vms := 0, 0, 0, 0
	for _, im := range r.boxImages() {
		h, err := r.acquireImage(im)
		if note(acq, im, err); err != nil {
			r.endHeld(h)
			continue
		}
		boxes++
		res, err := r.sh(h.Handle, `sudo -n true && sudo -n id -u`, "")
		if err == nil && (*res.Exit != 0 || strings.TrimSpace(*res.Stdout) != "0") {
			err = fmt.Errorf("sudo -n id -u exited %d with %q (stderr %q), want 0 and 0", *res.Exit, clip(*res.Stdout), clip(*res.Stderr))
		}
		note(sudo, im, err)
		if mac := r.macGuest(h.Handle); mac || vm {
			wholes++
			note(fence, im, r.rootFence(h.Handle, !mac))
			if !mac {
				vms++
				note(readdr, im, r.rootReaddress(h.Handle))
				b := r.rootBeside(h.Handle)
				note(neigh, im, b.neigh)
				note(take, im, b.take)
				note(kern, im, b.kernel)
			}
			if docker {
				note(dockrn, im, r.dockerRuns(h.Handle))
			}
			r.endHeld(h)
			continue
		}
		linux++
		// Each bit under its own name; NET_RAW is never excused: this group
		// asks for the network (net on, where listed) and never for none, so
		// a box that has one cannot claim it has not.
		bnd, err := r.rootCapBnd(h.Handle)
		for _, b := range []struct {
			check, name, why string
			bit              uint
		}{{admin, "NET_ADMIN", "it could take a neighbour's address", capNetAdmin}, {raw, "NET_RAW", "it could forge packets", capNetRaw},
			{mknod, "MKNOD", "a box needs no device nodes", capMknod}} {
			e := err
			if e == nil && bnd&(1<<b.bit) != 0 {
				e = fmt.Errorf("root's CapBnd %016x has %s: %s", bnd, b.name, b.why)
			}
			note(b.check, im, e)
		}
		if docker {
			note(dockrn, im, r.dockerRuns(h.Handle))
		}
		r.endHeld(h)
	}
	if boxes == 0 { // nothing below was checked
		r.add(acq, joined(errs[acq]))
		return
	}
	for _, c := range []string{acq, sudo} {
		r.add(c, joined(errs[c]))
	}
	for _, c := range []string{admin, raw, mknod} {
		if linux == 0 {
			r.skip(c, "every image is a whole VM (a macOS guest, or root-vm), where root owns the guest's kernel: the VM is the boundary, and root's network is checked instead")
			continue
		}
		r.add(c, joined(errs[c]))
	}
	if wholes > 0 {
		r.add(fence, joined(errs[fence]))
	}
	if vms > 0 {
		for _, c := range []string{readdr, neigh, take, kern} {
			r.add(c, joined(errs[c]))
		}
	}
	if !docker {
		r.skip("docker in the box", "describe lists no docker")
		return
	}
	r.add(dockrn, joined(errs[dockrn]))
}

func joined(es []string) error {
	if len(es) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(es, "; "))
}

// boxImages are the images the binding lends (config images): "" for the
// default (the first, acquired as any box is), then each other by name.
func (r *run) boxImages() []string {
	ims := []string{""}
	for i, s := range strings.Split(r.cfg["images"], ",") {
		if s = strings.TrimSpace(s); s != "" && i > 0 {
			ims = append(ims, s)
		}
	}
	return ims
}

// acquireImage acquires a box of image ("" the default), with the network
// where the driver takes net (net.go netOn), and checks the answer.
func (r *run) acquireImage(image string) (held, error) {
	net := r.netOn()
	if image == "" {
		return r.acquireBoxNet(net)
	}
	h := held{key: r.key()}
	m := []any{"request_id", h.key, "actor", actor, "ttl_s", r.longTTL(), "image", image}
	if net != "" {
		m = append(m, "net", net)
	}
	h.sent = args(m...)
	before := time.Now()
	a, e, err := r.acquire(h.sent, r.cfg)
	h.acquired = a
	if err = must(e, err); err != nil {
		return h, err
	}
	return h, r.acquireShape(a, r.longTTL(), before)
}

// rootCapBnd is root's capability bounding set in box h.
func (r *run) rootCapBnd(h string) (uint64, error) {
	res, err := r.sh(h, `sudo -n grep CapBnd /proc/self/status`, "")
	if err != nil {
		return 0, err
	}
	_, v, ok := strings.Cut(*res.Stdout, "CapBnd:")
	n, perr := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
	if *res.Exit != 0 || !ok || perr != nil {
		return 0, fmt.Errorf("sudo -n grep CapBnd /proc/self/status exited %d with %q (stderr %q), want root's CapBnd", *res.Exit, clip(*res.Stdout), clip(*res.Stderr))
	}
	return n, nil
}

// noSudo wants sudo, where the box has it, to refuse its user: a box whose
// driver lists no root must not have it.
func (r *run) noSudo() error {
	h, err := r.acquireAny(r.longTTL())
	defer r.endHeld(h)
	if err != nil {
		return err
	}
	res, err := r.sh(h.Handle, `command -v sudo >/dev/null || { echo no-sudo; exit 0; }; sudo -n true </dev/null >/dev/null 2>&1 && echo root || echo refused`, "")
	if err == nil && strings.TrimSpace(*res.Stdout) == "root" {
		err = fmt.Errorf("sudo -n true succeeded in a box whose driver lists no root capability")
	}
	return err
}

// dockerRuns waits for the box's docker, then runs a container from an image
// it imports there and removes it.
func (r *run) dockerRuns(h string) error {
	var last execResult
	for deadline := time.Now().Add(dockerUp); ; time.Sleep(time.Second) {
		res, err := r.sh(h, `docker info >/dev/null`, "")
		if err != nil {
			return err
		}
		if *res.Exit == 0 {
			break
		}
		last = res
		if time.Now().After(deadline) {
			return fmt.Errorf("docker info did not answer within %s: %s", dockerUp, clip(*last.Stderr))
		}
	}
	img := "zmconf-docker-" + strings.ToLower(r.tag)
	res, err := r.sh(h, `set -e; d=$(dirname "$(command -v docker-init)"); tar -C "$d" -c docker-init | docker import - "$1" >/dev/null
docker run --rm --network none "$1" /docker-init --version; docker rmi -f "$1" >/dev/null`, img)
	switch {
	case err != nil:
		return err
	case *res.Exit != 0 || !strings.Contains(*res.Stdout, "tini"):
		return fmt.Errorf("docker run exited %d with %q (stderr %q), want docker-init's version", *res.Exit, clip(*res.Stdout), clip(*res.Stderr))
	}
	return nil
}

// macGuest reports a box whose kernel says Darwin (uname -s): a macOS guest
// in a full VM (Tart's root=on, zm-eakh.12.54.14). Anything else, an error
// included, is taken for Linux, whose bounding-set checks then run and fail
// closed where /proc is not Linux's.
func (r *run) macGuest(h string) bool {
	res, err := r.sh(h, `uname -s`, "")
	return err == nil && *res.Exit == 0 && strings.TrimSpace(*res.Stdout) == "Darwin"
}

// fenceTargets are the --net-forbid and --net-mail addresses (ip:port) that
// prove something about a box's fence, as the net group takes them: each
// opens from this machine, and is not its loopback, unspecified, link-local
// or multicast address (which mean the guest itself inside a VM).
func (r *run) fenceTargets() []string {
	var targets []string
	for _, a := range append(slices.Clone(r.opt.NetForbid), r.opt.NetMail...) {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		ips := []string{host}
		if !isIPLiteral(host) {
			if ips, err = net.DefaultResolver.LookupHost(r.ctx, host); err != nil {
				continue
			}
		}
		for _, ip := range ips {
			to := net.JoinHostPort(ip, port)
			if localOnly(ip) == "" && opensHere(r.ctx, to) == nil && !slices.Contains(targets, to) {
				targets = append(targets, to)
			}
		}
	}
	return targets
}

// allowTargets are the IPv4 --net-allow addresses (ip:port) that open from
// this machine, each name resolved here: what root's change of address is
// tried against (rootTake).
func (r *run) allowTargets() []string {
	var targets []string
	for _, a := range r.opt.NetAllow {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		ips := []string{host}
		if !isIPLiteral(host) {
			if ips, err = net.DefaultResolver.LookupHost(r.ctx, host); err != nil {
				continue
			}
		}
		for _, ip := range ips {
			to := net.JoinHostPort(ip, port)
			if net.ParseIP(ip).To4() != nil && opensHere(r.ctx, to) == nil && !slices.Contains(targets, to) {
				targets = append(targets, to)
			}
		}
	}
	return targets
}

// rootProbe runs the probe's net subcommand under sudo in box h, and answers
// its exit.
func (r *run) rootProbe(h string, a ...string) (int, error) {
	code, _, err := r.rootProbeOut(h, a...)
	return code, err
}

// rootProbeOut is rootProbe with what the probe printed.
func (r *run) rootProbeOut(h string, a ...string) (int, string, error) {
	// sudo found on the box's PATH, by a shell, as every other check here
	argv := append([]string{"sh", "-c", `exec sudo -n sh -c "$0" zm-probe "$@"`, ProbeScript, "net"}, a...)
	res, err := r.exec(h, argv, "", execTimeoutMax)
	switch {
	case err != nil:
		return 0, "", err
	case *res.Exit == exitExpired:
		return 0, "", fmt.Errorf("the probe %v under sudo did not end within %d s", a, execTimeoutMax)
	}
	return *res.Exit, strings.TrimSpace(*res.Stdout), nil
}

// unfilter has root in box h empty the packet filter the guest runs itself.
func (r *run) unfilter(h string) error {
	code, err := r.rootProbe(h, "unfilter")
	if err == nil && code != 0 {
		err = fmt.Errorf("the probe's unfilter under sudo exited %d", code)
	}
	return err
}

// rootFence is what a whole-VM box's root is held to in place of the bounding
// set: root in a full VM may do anything inside it, so what keeps it from the
// lender's machine and network must sit outside the guest (Tart's Softnet, the
// cloud-hypervisor layer's nftables table: on the host). It runs the net
// probe under sudo: its control (it sees a port open in the box), then every
// address that proves something (fenceTargets) must not open. With none that
// proves something, it fails: a root box's fence must be shown, not assumed.
// In a Linux guest root first empties the guest's own packet filter (the
// probe's unfilter), and it stays empty for the checks after this one: a
// fence inside the guest is root's to take down.
func (r *run) rootFence(h string, linux bool) error {
	targets := r.fenceTargets()
	if len(targets) == 0 {
		return fmt.Errorf("no --net-forbid or --net-mail address opens from this machine (loopback and link-local ones mean the guest itself), so nothing showed root in the box is still fenced: give one, such as this machine's LAN address and a port it listens on")
	}
	if linux {
		if err := r.unfilter(h); err != nil {
			return err
		}
	}
	code, err := r.rootProbe(h, "self", strconv.Itoa(r.peerPort()+3))
	switch {
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("the probe under sudo exited %d: it cannot see a port open inside the box, so its other answers prove nothing", code)
	}
	var bad []string
	for _, to := range targets {
		code, err := r.rootProbe(h, "forbid", to)
		switch {
		case err != nil:
			return err
		case code == exitReached:
			bad = append(bad, to+" opened from root's shell in the box")
		case code != 0:
			bad = append(bad, fmt.Sprintf("%s: the probe under sudo exited %d", to, code))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// errNoReaddress is a probe that could not make root's change (exit 67).
var errNoReaddress = fmt.Errorf("root could not change the box's addresses, routes or neighbours (the box has no default route, or ip refused): root in a whole VM can, so this box is not what root-vm says, and nothing was shown")

// rootReaddress is the second part of a whole-VM Linux box's fence. Its root
// can give the box's link any address and route, so each IPv4 address that
// proves something (fenceTargets) must also stay closed when root sends from
// an address beside it, and when root names it a neighbour on the box's own
// link (the probe's readdress). A fence that goes by the sender's address,
// or that only a router enforces, opens there. That the probe sees an open
// port as open is rootFence's control, in the same box.
func (r *run) rootReaddress(h string) error {
	var targets []string
	for _, to := range r.fenceTargets() {
		if host, _, err := net.SplitHostPort(to); err == nil && net.ParseIP(host).To4() != nil {
			targets = append(targets, to)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no IPv4 --net-forbid or --net-mail address opens from this machine, so nothing showed root's own addresses and routes do not get it past the fence: give one, such as this machine's LAN address and a port it listens on")
	}
	var bad []string
	for _, to := range targets {
		code, err := r.rootProbe(h, "readdress", to)
		switch {
		case err != nil:
			return err
		case code == exitReached:
			bad = append(bad, to+" opened once root took another address or named it a neighbour")
		case code == exitNoTools:
			return fmt.Errorf("the box has no ip (iproute2), so what root can do with its link was not tried")
		case code == exitNoReaddress:
			return errNoReaddress
		case code != 0:
			bad = append(bad, fmt.Sprintf("%s: the probe under sudo exited %d", to, code))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// besideChecks are what root in a whole-VM Linux box is held to beside
// another box of its binding.
type besideChecks struct{ neigh, take, kernel error }

// rootBeside acquires that other box, has it listen, and runs the three
// checks that need it against box h (rootNeighbour, rootTake, ownKernel). The
// other box's root empties its own packet filter first, as h's did: a box
// that only its own filter closes to its neighbours is not fenced from them.
// Without the other box none of the three showed anything, and each fails.
func (r *run) rootBeside(h string) besideChecks {
	all := func(err error) besideChecks { return besideChecks{err, err, err} }
	ps := strconv.Itoa(r.peerPort() + 4)
	l, err := r.acquireBoxNet(r.netOn())
	if err != nil {
		r.endHeld(l)
		return all(fmt.Errorf("the listening box: %v", err))
	}
	b, err := r.listenIn(l, ps)
	defer b.close()
	if err != nil {
		return all(err)
	}
	if err := r.unfilter(l.Handle); err != nil {
		return all(fmt.Errorf("the listening box: %v", err))
	}
	res, err := r.exec(l.Handle, probeArgv("net", "link"), "", execTimeoutMax)
	if err != nil {
		return all(fmt.Errorf("the listening box: %v", err))
	}
	link := strings.Fields(*res.Stdout)
	if *res.Exit != 0 || len(link) != 2 || net.ParseIP(link[0]).To4() == nil {
		return all(fmt.Errorf("the listening box's link: the probe exited %d with %q, want its address and hardware address", *res.Exit, clip(*res.Stdout)))
	}
	if _, err := net.ParseMAC(link[1]); err != nil {
		return all(fmt.Errorf("the listening box's link: %q is no hardware address", clip(link[1])))
	}
	return besideChecks{
		neigh:  r.rootNeighbour(h, b, link[0], link[1]),
		take:   r.rootTake(h, l.Handle, link[0]),
		kernel: r.ownKernel(h, l.Handle),
	}
}

// rootNeighbour is what the NET_ADMIN check keeps from a box on a shared
// bridge. Another box of the binding listens (b, at ip and hardware address
// mac); root in box h names that address a neighbour on its own link, at
// that hardware address, and must not reach it (the probe's neighbour). Two
// boxes on one link reach each other that way whatever a router between them
// drops.
func (r *run) rootNeighbour(h string, b *boxListener, ip, mac string) error {
	code, err := r.rootProbe(h, "neighbour", ip, mac, b.port)
	switch {
	case err != nil:
		return err
	case code == 0 && !b.up():
		return b.stopped()
	case code == exitPeerReached:
		return fmt.Errorf("root reached another box (%s) on port %s", ip, b.port)
	case code == exitNoTools:
		return fmt.Errorf("the box has no ip (iproute2), so what root can do with its link was not tried")
	case code == exitNoReaddress:
		return errNoReaddress
	case code != 0:
		return fmt.Errorf("the probe under sudo exited %d", code)
	}
	return nil
}

// rootTake holds a whole-VM Linux box to its own address. Root in box h
// gives its link the other box's address (peer) and announces it, then its
// subnet's first and last address, and sends from each: an address that
// opens from the box's own (an IPv4 --net-allow one, allowTargets) must not
// open from any of them (the probe's take). A fence that lets a box send
// from an address that is not its own lets it answer for whoever has that
// address, or will. Then the other box must still reach that address: on a
// link two boxes share, the one that announced the other's address draws its
// traffic. With no address to try it against, it fails: nothing was shown.
func (r *run) rootTake(h, other, peer string) error {
	targets := r.allowTargets()
	if len(targets) == 0 {
		return fmt.Errorf("no IPv4 --net-allow address opens from this machine, so nothing showed root cannot use an address that is not the box's own: give one, such as example.com:443")
	}
	for _, to := range targets {
		// The control: the other box reaches it before anything is taken.
		if code, err := r.boxNet(other, "allow", to); err != nil {
			return err
		} else if code != 0 {
			continue
		}
		code, out, err := r.rootProbeOut(h, "take", to, peer)
		switch {
		case err != nil:
			return err
		case code == exitUnreachable:
			continue // closed from the box's own address: it proves nothing
		case code == exitReached:
			return fmt.Errorf("%s opened from %s, which is not the box's own address", to, clip(out))
		case code == exitNoTools:
			return fmt.Errorf("the box has no ip (iproute2), so what root can do with its link was not tried")
		case code == exitNoReaddress:
			return errNoReaddress
		case code != 0:
			return fmt.Errorf("the probe under sudo exited %d", code)
		}
		if code, err := r.boxNet(other, "allow", to); err != nil {
			return err
		} else if code != 0 {
			return fmt.Errorf("another box (%s) no longer reaches %s once root took and announced its address (the probe exited %d)", peer, to, code)
		}
		return nil
	}
	return fmt.Errorf("no --net-allow address (%s) opens from both boxes, so nothing showed root cannot use an address that is not the box's own", strings.Join(targets, ", "))
}

// ownKernel checks the driver's word root-vm: box h's kernel is its own. A
// driver whose boxes share a kernel must not list root-vm: this group then
// skips the bits (NET_ADMIN, NET_RAW, MKNOD) that hold root on a shared
// kernel. Two things show it:
//
//   - A kernel's boot id is made once per boot, so a box that reads this
//     machine's is on this machine's kernel. Two boxes' ids are not
//     compared with each other: VMs restored from one snapshot of a booted
//     VM carry one id, and containers can be given an id each.
//   - Root in h changes a setting the kernel has one of
//     (kernel.printk_ratelimit_burst, which only limits how fast the kernel
//     logs): it must take, and the other box must not see it. Root in a
//     container either cannot change it or changes it for every box on the
//     kernel.
func (r *run) ownKernel(h, other string) error {
	here, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	for i, h := range []string{h, other} {
		res, err := r.exec(h, probeArgv("net", "kernel"), "", execTimeoutMax)
		if err == nil {
			s := strings.TrimSpace(*res.Stdout)
			switch {
			case *res.Exit != 0 || s == "":
				err = fmt.Errorf("the probe's kernel exited %d with %q: the box has no boot id (/proc/sys/kernel/random/boot_id), so nothing showed its kernel is its own", *res.Exit, clip(s))
			case s == strings.TrimSpace(string(here)):
				err = fmt.Errorf("the box's kernel is this machine's own (boot id %s): it is not a VM of its own", clip(s))
			}
		}
		if err != nil && i == 1 {
			err = fmt.Errorf("the listening box: %v", err)
		}
		if err != nil {
			return err
		}
	}
	return r.ownKernelSetting(h, other)
}

// ownKernelSetting is ownKernel's second half: root in h changes
// kernel.printk_ratelimit_burst, reads it back, and puts it back; the other
// box must read what it read before.
func (r *run) ownKernelSetting(h, other string) error {
	const name = "kernel.printk_ratelimit_burst"
	read := func(h string, set ...string) (string, error) {
		code, out, err := r.rootProbeOut(h, append([]string{"kset"}, set...)...)
		switch {
		case err != nil:
			return "", err
		case code != 0 && len(set) > 0:
			return "", fmt.Errorf("root in a root-vm box could not change %s, a setting of the whole kernel (the probe exited %d): root of a kernel of its own can", name, code)
		case code != 0 || out == "":
			return "", fmt.Errorf("the probe under sudo could not read %s (it exited %d), so nothing showed the box's kernel is its own", name, code)
		}
		return out, nil
	}
	was, err := read(h)
	if err != nil {
		return err
	}
	beside, err := read(other)
	if err != nil {
		return fmt.Errorf("the listening box: %v", err)
	}
	mark := 7919
	for strconv.Itoa(mark) == was || strconv.Itoa(mark) == beside {
		mark++
	}
	got, err := read(h, strconv.Itoa(mark))
	defer func() { _, _ = read(h, was) }()
	switch {
	case err != nil:
		return err
	case got != strconv.Itoa(mark):
		return fmt.Errorf("root in a root-vm box set %s to %d and it reads %s: root of a kernel of its own can change it", name, mark, clip(got))
	}
	switch now, err := read(other); {
	case err != nil:
		return fmt.Errorf("the listening box: %v", err)
	case now != beside:
		return fmt.Errorf("root in one box set %s to %d and another box, which read %s before, reads %s: the two are on one kernel", name, mark, clip(beside), clip(now))
	}
	return nil
}
