package conformance

import (
	"fmt"
	"net"
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
// A macOS guest (uname -s says Darwin: Tart's root=on, zm-eakh.12.54.14) has
// no bounding set to read: root in a full VM may do anything inside it, so
// the three bit checks are skipped for it, and instead the net probe, run
// under sudo, must not open any --net-forbid or --net-mail address that
// proves something (rootFence): what fences the box must sit outside it.

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
	root, docker := slices.Contains(caps, protocol.CapRoot), slices.Contains(caps, protocol.CapDocker)
	if !root && !docker {
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
	const (
		acq    = "acquire a box for root and docker"
		sudo   = "sudo works in the box: sudo -n id -u answers 0, no password"
		admin  = "root in the box holds no NET_ADMIN (CapBnd bit 12)"
		raw    = "root in the box holds no NET_RAW (CapBnd bit 13)"
		mknod  = "root in the box holds no MKNOD (CapBnd bit 27)"
		dockrn = "docker run works in the box (an image imported from its docker-init, no network)"
		fence  = "root in a macOS box cannot reach what the net checks forbid (--net-forbid, --net-mail), run under sudo"
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
	boxes, linux, macs := 0, 0, 0
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
		if r.macGuest(h.Handle) {
			macs++
			note(fence, im, r.rootFence(h.Handle))
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
			r.skip(c, "every image is a macOS guest, which has no Linux bounding set: the VM is the boundary, and root's network is checked instead")
			continue
		}
		r.add(c, joined(errs[c]))
	}
	if macs > 0 {
		r.add(fence, joined(errs[fence]))
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

// rootFence is what a macOS box's root is held to in place of the bounding
// set (which macOS has not): root in a full VM may do anything inside it, so
// what keeps it from the lender's machine and network must sit outside the
// guest (Tart's Softnet, on the host). It runs the net probe under sudo: its
// control (it sees a port open in the box), then every --net-forbid and
// --net-mail address that proves something (as the net group: one that opens
// from this machine, and is not its loopback, unspecified, link-local or
// multicast address) must not open. With none that proves something, it
// fails: a root box's fence must be shown, not assumed.
func (r *run) rootFence(h string) error {
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
	if len(targets) == 0 {
		return fmt.Errorf("no --net-forbid or --net-mail address opens from this machine (loopback and link-local ones mean the guest itself), so nothing showed root in the box is still fenced: give one, such as this machine's LAN address on a port it listens on")
	}
	asRoot := func(a ...string) (int, error) {
		// sudo found on the box's PATH, by a shell, as every other check here
		argv := append([]string{"sh", "-c", `exec sudo -n sh -c "$0" zm-probe "$@"`, ProbeScript, "net"}, a...)
		res, err := r.exec(h, argv, "", execTimeoutMax)
		switch {
		case err != nil:
			return 0, err
		case *res.Exit == exitExpired:
			return 0, fmt.Errorf("the probe %v under sudo did not end within %d s", a, execTimeoutMax)
		}
		return *res.Exit, nil
	}
	code, err := asRoot("self", strconv.Itoa(r.peerPort()+3))
	switch {
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("the probe under sudo exited %d: it cannot see a port open inside the box, so its other answers prove nothing", code)
	}
	var bad []string
	for _, to := range targets {
		code, err := asRoot("forbid", to)
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
