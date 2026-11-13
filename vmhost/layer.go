package vmhost

import (
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
	"tart": func(c config) vmLayer { return tartCLI{c.Tart} },
	"oci": func(c config) vmLayer {
		return ociCLI{bin: c.OCICLI, runtime: c.OCIRuntime, network: c.OCINetwork, dns: c.OCIDNS, stateDir: c.StateDir, root: c.Root, docker: c.Docker, nativeBound: c.OCIHostProfile != ""}
	},
	"incus": func(c config) vmLayer {
		return incusCLI{c.IncusBin, c.IncusRemote, c.IncusProject, c.IncusProfile, c.IncusConf}
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

// waitReady waits until the guest runs a command, by deadline.
func waitReady(l vmLayer, vm string, deadline time.Time, exited <-chan struct{}) error {
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("the VM exited while booting")
		default:
		}
		if l.Exec(vm, nil, io.Discard, io.Discard, "true").Run() == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the guest agent did not answer within the boot timeout")
}
