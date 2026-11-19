package vmhost

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	kitconfig "github.com/sengirunners/potluck/kit/config"
)

// Kept disks on the OCI layer (persist.go): a kept box is the same container,
// stopped and started again, so its writable layer survives where the
// runtime writes there. Kata does. gVisor's runsc by default keeps the root's
// changes in an overlay of its own (overlay2 root:self, or root:memory),
// dropped at every stop: measured on runsc release-20261102.0 under docker
// 29.8 (zm-eakh.12.5.23), files in /etc, /usr/local, /var/tmp and HOME were
// all gone after docker stop and start. With overlay2 none the root's writes
// go through the gofer to the container's own writable layer, and all of
// them survived; runsc takes that flag per container from the OCI
// annotation dev.gvisor.flag.overlay2, which docker and podman set with
// --annotation, so no host config (daemon.json, a runtime entry) changes.
// The box's HOME is also a named volume of its own, kept under any runtime
// (an older runsc, nerdctl); Destroy removes it. zm mounts nothing else and
// nothing of the host's.

// ociHomeVolume is a kept box's HOME volume.
func ociHomeVolume(vm string) string { return vm + "-home" }

// ociKeptDeadman is ociDeadman whose guard stops the container (kept)
// rather than removing it.
const ociKeptDeadman = `vm=$1; bin=$2; shift 2; exec 3<&0
"$bin" start -a "$vm" 3<&- & p=$!
( cat <&3; "$bin" stop -t 0 "$vm"; kill -KILL $p ) >/dev/null 2>&1 & g=$!; exec 3<&-
wait $p; s=$?; kill $g 2>/dev/null; exit $s`

// CloneKept creates the container with HOME on its own volume, the image's
// HOME (its HOME variable, else /root for root, else /home/<user>).
func (o ociCLI) CloneKept(image, vm string, cpu, memory int) error {
	var err error
	o, err = o.outerVolumeEngine()
	if err != nil {
		return err
	}
	home, err := o.imageHome(image)
	if err != nil {
		return err
	}
	extra := []string{"--mount", "type=volume,source=" + ociHomeVolume(vm) + ",target=" + home}
	if o.rootFlag() {
		extra = append(extra, "--annotation", ociRunscKeepRoot)
	}
	return o.clone(image, vm, cpu, memory, extra)
}

func (o ociCLI) BootKept(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	var err error
	o, err = o.outerVolumeEngine()
	if err != nil {
		return nil, nil, err
	}
	return o.boot(vm, isolated, log, ociKeptDeadman)
}

func (o ociCLI) PowerOff(vm string, runPID int) {
	if runPID > 0 {
		_ = syscall.Kill(-runPID, syscall.SIGKILL)
	}
	if _, err := o.run("stop", "-t", "0", vm); err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such container") {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
	}
}

// ociRunscKeepRoot is the annotation that turns runsc's root overlay off
// for one container, so its root's changes reach its writable layer.
const ociRunscKeepRoot = "dev.gvisor.flag.overlay2=none"

// gvisor says whether the binding's runtime is gVisor's runsc (its "runsc"
// entry or another, such as root's runsc-root): the sentry, not the host's
// kernel, runs the box's programs and mounts its volumes.
func (o ociCLI) gvisor() bool { return strings.Contains(o.runtime, "runsc") }

// rootFlag says whether a kept box gets ociRunscKeepRoot: runsc under a CLI
// with --annotation (docker, podman).
func (o ociCLI) rootFlag() bool {
	cli := filepath.Base(o.bin)
	return o.gvisor() && (strings.HasPrefix(cli, "docker") || strings.HasPrefix(cli, "podman"))
}

// KeepsRoot: Kata writes the root to the container's layer, and so does
// runsc with ociRunscKeepRoot; otherwise only HOME is kept. (Conformance's
// persist group checks the claim: a file outside HOME must survive.)
func (o ociCLI) KeepsRoot() bool {
	return strings.HasPrefix(o.runtime, "kata") || strings.Contains(o.runtime, ".kata") || o.rootFlag()
}

// imageConfig reads the image's user and environment (KEY=value lines)
// from its config: no box runs to find them.
func (o ociCLI) imageConfig(image string) (user string, env []string, err error) {
	out, err := o.run("image", "inspect", "--format", "{{.Config.User}}\n{{range .Config.Env}}{{println .}}{{end}}", image)
	if err != nil {
		return "", nil, err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, l := range lines[1:] {
		env = append(env, strings.TrimSpace(l))
	}
	return strings.TrimSpace(lines[0]), env, nil
}

// imageEnv is the value the image's environment gives key, and whether it
// sets it.
func imageEnv(env []string, key string) (v string, ok bool) {
	for _, l := range env {
		if s, set := strings.CutPrefix(l, key+"="); set {
			v, ok = s, true
		}
	}
	return v, ok
}

// imageHome reads the image's HOME from its config.
func (o ociCLI) imageHome(image string) (string, error) {
	u, env, err := o.imageConfig(image)
	if err != nil {
		return "", err
	}
	user, _, _ := strings.Cut(u, ":")
	home, _ := imageEnv(env, "HOME")
	switch {
	case home != "":
	case user == "" || user == "root" || user == "0":
		home = "/root"
	case strings.Trim(user, "0123456789") == "":
		return "", fmt.Errorf("image %s runs as uid %s with no HOME: %s cannot tell where to keep its home", image, user, kitconfig.Program)
	default:
		home = "/home/" + user
	}
	if !path.IsAbs(home) || path.Clean(home) != home || home == "/" || strings.ContainsAny(home, ",=\"\n") {
		return "", fmt.Errorf("image %s's HOME %q is not a plain absolute path", image, home)
	}
	return home, nil
}

var (
	_ keeper = ociCLI{}
	_ keeper = tartCLI{}
)

// DiskDir is where the CLI keeps containers and volumes (docker's root dir,
// podman's graph root), for the free-disk floor; "" when it does not say.
func (o ociCLI) DiskDir() string {
	for _, f := range []string{"{{.DockerRootDir}}", "{{.Store.GraphRoot}}"} {
		if out, err := o.run("info", "--format", f); err == nil {
			if d := strings.TrimSpace(out); path.IsAbs(d) {
				return d
			}
		}
	}
	return ""
}

// keptBootHinter turns a kept box's failed boot into what to do about it.
type keptBootHinter interface{ KeptBootErr(error) error }

// KeptBootErr names the runsc a kept box failed to boot under: runsc refuses
// a container whose annotations override a flag it does not allow (its
// overrideAllowlist), so an older runsc fails the start rather than keeping
// only HOME.
func (o ociCLI) KeptBootErr(err error) error {
	if !o.rootFlag() {
		return err
	}
	return fmt.Errorf("%w; a kept box on runsc boots with the annotation %s (so its root survives a stop), which %s refuses unless it allows overlay2 per container: see the run log, and update runsc (%s installs a pinned one)", err, ociRunscKeepRoot, o.runscVersion(), kitconfig.PotluckCommand("doctor"))
}

// runscVersion is the runtime's own version line, as the CLI runs it.
func (o ociCLI) runscVersion() string {
	bin := "runsc"
	if out, err := o.run("info", "--format", `{{(index .Runtimes "`+o.runtime+`").Path}}`); err == nil && path.IsAbs(strings.TrimSpace(out)) {
		bin = strings.TrimSpace(out)
	}
	out, err := boundedHostCommand(2*time.Second, bin, "--version")
	if v, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); err == nil && v != "" {
		return "this host's runsc (" + v + ")"
	}
	return "this host's runsc (version unknown)"
}
