package refdriver

// Root and Docker in a box (SPEC 20 "Boxes", Root and Docker;
// zm-eakh.12.5.26): this driver's boxes are host directories, so it lists
// root and docker and puts a stand-in sudo and docker first on each exec's
// PATH, with a Bug switch for each way a driver can get them wrong. They
// stand in for what conformance asks; they grant nothing.

import (
	"os"
	"path/filepath"
	"strconv"
)

// rootServed: this driver's boxes list root (bug no-root: they have none;
// root-unlisted: sudo works, unlisted).
func (d *drv) rootServed() bool { return d.cfg["kind"] == "box" && !d.is("no-root", "root-unlisted") }

// rootDescribe adds capabilities root and docker, as the bug says.
func (d *drv) rootDescribe(caps []string) []string {
	switch {
	case !d.rootServed():
		return caps
	case d.is("docker-without-root"):
		return append(caps, "docker")
	case d.is("no-docker"):
		return append(caps, "root")
	}
	return append(caps, "root", "docker")
}

// rootPath is the stand-ins' directory, written once per driver state.
func (d *drv) rootPath() (string, error) {
	dir := filepath.Join(d.state, "rootbin")
	bnd := map[string]string{"root-net-admin": "00000000a00415fb", "root-net-raw": "00000000a00425fb", "root-mknod": "00000000a80405fb"}[d.bug]
	if bnd == "" {
		bnd = "00000000a00405fb" // Docker's default set less NET_RAW and MKNOD, as zm-run-tart's root box
	}
	refuse := d.is("root-no-sudo", "no-root")
	// root-past-fence: root's commands run without the guest firewall
	// (Firewall, on the test's PATH), as root in a VM whose only fence is
	// inside the guest could take it down.
	past := map[bool]string{true: "PATH=" + dir + ":/usr/bin:/bin:/usr/sbin:/sbin; export PATH\n"}[d.is("root-past-fence")]
	sudo := `#!/bin/sh
[ "$1" = -n ] && shift
` + past + map[bool]string{true: `echo "sudo: a password is required" >&2; exit 1
`}[refuse] + `case "$1" in
id) echo 0 ;;
grep) printf 'CapBnd:\t%s\n' ` + bnd + ` ;;
chown) touch "$3.root-owned" ;; # root's now (secrets.go): this machine's user cannot chown
rm) for a; do last=$a; done; rm -f "$last.root-owned"; exec "$@" ;;
*) exec "$@" ;;
esac
`
	docker := `#!/bin/sh
case "$1" in
info) ` + map[bool]string{true: `echo "Cannot connect to the Docker daemon" >&2; exit 1`, false: "exit 0"}[d.is("docker-down")] + ` ;;
import) cat >/dev/null; echo sha256:0 ;;
run) ` + map[bool]string{true: `echo "docker: Error response from daemon" >&2; exit 125`, false: `echo "tini version 0.19.0"`}[d.is("docker-run-fails")] + ` ;;
rmi) exit 0 ;;
*) exit 2 ;;
esac
`
	init := "#!/bin/sh\necho tini version 0.19.0\n"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stand := map[string]string{"sudo": sudo, "docker": docker, "docker-init": init, "stat": d.statStandIn(), "cat": d.catStandIn()}
	// The box's kernel (uname -s): Linux, whatever this machine runs, as the
	// guest the stand-in bounding set describes; config guest_os=darwin, a
	// macOS guest, as a Tart box with root=on (zm-eakh.12.54.14).
	kern := map[bool]string{true: "Darwin", false: "Linux"}[d.cfg["guest_os"] == "darwin"]
	stand["uname"] = "#!/bin/sh\nif [ \"$1\" = -s ]; then echo " + kern + "; exit 0; fi\nPATH=/usr/bin:/bin exec uname \"$@\"\n"
	if d.is("box-secrets-wrong-owner") { // commands run as a user other than the one owning the box's secrets
		stand["id"] = "#!/bin/sh\nif [ \"$1\" = -u ]; then echo $(( $(/usr/bin/id -u) + 1 )); exit 0; fi\nexec /usr/bin/id \"$@\"\n"
	} else {
		_ = os.Remove(filepath.Join(dir, "id"))
	}
	for n, s := range stand {
		p := filepath.Join(dir, n) // a temp file renamed in: another exec may be running it
		tmp := p + ".tmp-" + strconv.Itoa(os.Getpid())
		if err := os.WriteFile(tmp, []byte(s), 0o700); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, p); err != nil {
			return "", err
		}
	}
	return dir, nil
}
