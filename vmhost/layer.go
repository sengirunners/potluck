package vmhost

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// vmLayer is every VM operation the driver and its supervisors make, so the
// tool under them can be swapped (Tart is FSL-licensed; see zm-eakh.12.4):
// Tart today; Virtualization.framework directly, Lume or another
// permissively licensed layer later. The binding's config picks one (vm=,
// default tart); a supervisor rebuilds it from the run's saved config.
//
// The contract a layer keeps:
//   - Clone makes a VM that shares nothing with any other run (a fresh copy of
//     the image, its own MAC), sized cpu/memory when they are > 0.
//   - Boot starts it headless with nothing of the host's attached (no shared
//     directory, clipboard, audio or display) as a host process in its own
//     process group: killing that group powers the VM off. The VM must not
//     outlive the returned pipe: when its write end closes (the supervisor
//     exited or was killed), the layer powers the VM off and deletes it.
//     isolated is SPEC 20's credential-free network: the guest reaches the
//     internet and DNS, never the host's LAN, the host itself, a tailnet or
//     link-local metadata. A layer that cannot isolate refuses isolated.
//   - Exec runs argv in the guest without SSH or any credential, its output
//     to stdout and stderr apart; its exit is the guest command's, and it
//     fails when the VM goes.
//   - List shows the VMs this layer made on this host (Local: made here, not a
//     pulled image), so sweeps find what a dead supervisor left.
//   - Destroy powers a VM off (runPID is Boot's process, 0 when no supervisor
//     of ours holds it) and deletes it. It is safe to call twice.
//   - OSOf answers "linux" or "darwin" for an image or VM.
type vmLayer interface {
	OSOf(name string) (string, error)
	Clone(image, vm string, cpu, memory int) error
	Boot(vm string, isolated bool, log io.Writer) (cmd *exec.Cmd, held io.WriteCloser, err error)
	Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd
	List() ([]vmInfo, error)
	Destroy(vm string, runPID int)
}

// loginCloser is a layer whose images may keep a network login open (Tart:
// tartCLI.CloseLogin). Its argv runs in each guest once the agent answers,
// before a checkout, a job or the box's ready; when it fails the VM ends.
type loginCloser interface{ CloseLogin() []string }

// layerConfig is a layer that reads and checks its own binding keys (Incus:
// the incus_* keys). parseConfig builds the layer from the config read so
// far, then calls ParseConfig with the raw keys and the config to fill: it
// sets the layer's fields and returns one plain-words message per bad or
// missing key, which join the other config errors. Only the keys of this
// layer (a layer is built only for vm=<its name>). A layer's keys must also
// go back out in (*drv).rawConfig (driver.go): the supervisor rebuilds the
// config from that.
type layerConfig interface {
	ParseConfig(m map[string]string, c *config) (bad []string)
}

type vmInfo struct {
	Name           string
	Running, Local bool
}

// layers are the VM layers a binding can name with vm=. Tests add theirs.
var layers = map[string]func(c config) vmLayer{
	"tart": func(c config) vmLayer { return tartCLI{bin: c.Tart, softnetPath: c.SoftnetPath} },
	"oci": func(c config) vmLayer {
		return ociCLI{bin: c.OCICLI, runtime: c.OCIRuntime, network: c.OCINetwork, dns: c.OCIDNS, stateDir: c.StateDir, root: c.Root, docker: c.Docker, nativeBound: c.OCIHostProfile != ""}
	},
	"incus": func(c config) vmLayer {
		return incusCLI{c.IncusBin, c.IncusRemote, c.IncusProject, c.IncusProfile, c.IncusConf}
	},
	"cloudhv": func(c config) vmLayer {
		return cloudhvCLI{c: c.CloudHV, state: c.StateDir, root: c.Root, docker: c.Docker}
	},
}

func layerNames() string {
	ns := make([]string, 0, len(layers))
	for n := range layers {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return strings.Join(ns, ", ")
}

func newLayer(c config) vmLayer { return layers[c.VM](c) }

// readySaid is how much of a boot try's words waitReady keeps.
const readySaid = 400

// waitReady waits until the guest runs a command, by deadline. Its error
// carries what the last try that said anything said (a guest that answers
// and cannot give what its binding says, such as root, says so each time),
// and says so when a later try was still running at the deadline, where it
// is ended. What a try says is kept only up to readySaid bytes.
func waitReady(l vmLayer, vm string, deadline time.Time, exited <-chan struct{}) error {
	said, running := "", false
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errBootExited // the supervisor says why (diagnoseBoot)
		default:
		}
		errb := &capped{max: readySaid}
		cmd := l.Exec(vm, nil, io.Discard, errb, "true")
		cmd.WaitDelay = time.Second // a try that ended is not waited on for what still holds its output
		running = false
		if cmd.Start() == nil {
			late := time.AfterFunc(time.Until(deadline), func() { _ = cmd.Process.Kill() })
			err := cmd.Wait()
			running = !late.Stop()
			if err == nil || errors.Is(err, exec.ErrWaitDelay) {
				return nil
			}
		}
		if s := strings.TrimSpace(errb.buf.String()); s != "" {
			said, running = s, false
		}
		time.Sleep(250 * time.Millisecond)
	}
	switch {
	case said != "" && running:
		return fmt.Errorf("the guest ran no command within the boot timeout: %s (a later try was still running when the time passed)", said)
	case said != "":
		return fmt.Errorf("the guest ran no command within the boot timeout: %s", said)
	case running:
		return fmt.Errorf("the guest ran no command within the boot timeout: its last try was still running when the time passed")
	}
	return fmt.Errorf("the guest agent did not answer within the boot timeout")
}
