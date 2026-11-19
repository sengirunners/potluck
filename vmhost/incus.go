package vmhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	kitconfig "github.com/sengirunners/potluck/kit/config"
)

// incusCLI is the Incus VM layer (vm=incus, zm-eakh.12.7.1): each run is a
// KVM virtual machine under Incus, in a project the host's owner made for zm,
// driven through the incus CLI. Nothing about a particular host is written
// here: the remote, project and profile are the owner's binding keys.
//
// Against the layer contract, and the rules a shared host adds:
//   - Never incus-admin. The remote must be one reached with a client
//     certificate (https), which the owner restricts to the zm project
//     (`incus config trust add-certificate --restricted --projects <p>`); a
//     local unix-socket remote is the admin socket and is refused, so zm can
//     never see or touch another project's instances. The project's limits
//     (limits.cpu, limits.memory, limits.instances) cap the owner's share.
//   - Clone is `incus init --vm` from the owner's image with the owner's
//     profile (root disk, one NIC on the zm bridge), labelled
//     user.zm.run-target, sized when cpu/memory > 0.
//   - Before Boot the layer reads the VM's expanded config and refuses one
//     that reaches the host: any disk but the root (a host path), any device
//     but disk and nic, any raw.* key; for an isolated run every NIC must
//     have security.port_isolation (no VM-to-VM: one renter cannot reach
//     another's VM or a neighbour's) and a security.acls the owner wrote to
//     drop the LAN, the host, tailnets, metadata and private IPv6, and whose
//     egress drops or rejects outbound mail (incus_acl.go).
//   - Boot starts it and runs a watcher whose exit means the VM is gone: a
//     state answer other than Running, or "not found". An error reaching
//     incusd (a restart, a heal) is not "gone": it keeps asking. A guard
//     deletes the VM when the supervisor's pipe closes, retrying through an
//     incusd restart until the delete lands or Incus says it is not found.
//   - Exec is `incus exec` through incus-agent (vsock): no SSH, the guest
//     command's exit code. List is the project's instances carrying the label.
type incusCLI struct {
	bin, remote, project, profile, conf string
}

// incusName is a plain Incus remote, project or profile name.
var incusName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// ParseConfig is incusCLI's layerConfig (layer.go): it reads the incus_* keys
// into c and refuses a malformed or missing one, in plain words.
func (incusCLI) ParseConfig(m map[string]string, c *config) (bad []string) {
	c.IncusBin, c.IncusRemote, c.IncusProject, c.IncusProfile, c.IncusConf = m["incus"], m["incus_remote"], m["incus_project"], m["incus_profile"], m["incus_conf"]
	if c.IncusBin == "" {
		c.IncusBin = "incus"
	}
	if c.IncusProject == "" {
		c.IncusProject = "zm"
	}
	for k, v := range map[string]string{"incus_remote": c.IncusRemote, "incus_project": c.IncusProject, "incus_profile": c.IncusProfile} {
		if v != "" && !incusName.MatchString(v) {
			bad = append(bad, k+" "+strconv.Quote(v)+" is not a plain Incus name")
		}
	}
	if c.IncusProfile == "" {
		bad = append(bad, "incus_profile is required: the profile (root disk, a NIC on the "+kitconfig.Program+" bridge with port isolation and an ACL) every run's VM uses")
	}
	if c.IncusProject == "default" {
		bad = append(bad, "incus_project must be a project made for "+kitconfig.Program+", not default")
	}
	return bad
}

// incusLabelNew is set on the VMs this driver makes; incusLabel is on those
// made before the rename's step 2. List takes both as ours.
const (
	incusLabel    = "user.zm.run-target"
	incusLabelNew = "user.potluck.run-target"
)

// ref is name on the layer's remote.
func (l incusCLI) ref(name string) string {
	if l.remote == "" {
		return name
	}
	return l.remote + ":" + name
}

// imageRef is an image on the layer's remote, unless the owner named one on
// another remote ("images:debian/12").
func (l incusCLI) imageRef(image string) string {
	if strings.Contains(image, ":") {
		return image
	}
	return l.ref(image)
}

func (l incusCLI) command(a ...string) *exec.Cmd {
	cmd := exec.Command(l.bin, a...)
	if l.conf != "" {
		cmd.Env = append(os.Environ(), "INCUS_CONF="+l.conf)
	}
	return cmd
}

func (l incusCLI) run(a ...string) (string, error) {
	out, err := l.command(a...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", l.bin, strings.Join(a, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func notFound(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "not found")
}

// restricted refuses the admin socket: the remote must be a certificate one.
func (l incusCLI) restricted() error {
	name := l.remote
	if name == "" {
		out, err := l.run("remote", "get-default")
		if err != nil {
			return err
		}
		name = strings.TrimSpace(out)
	}
	out, err := l.run("remote", "list", "--format", "json")
	if err != nil {
		return err
	}
	var rs map[string]struct{ Addr, Protocol string }
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		return fmt.Errorf("incus remote list: %v", err)
	}
	r, ok := rs[name]
	switch {
	case !ok:
		return fmt.Errorf("incus has no remote %q", name)
	case !strings.HasPrefix(r.Addr, "https://"):
		return fmt.Errorf("incus remote %q is %s, the admin socket: %s needs a remote reached with a client certificate restricted to project %s", name, r.Addr, kitconfig.Program, l.project)
	}
	// A certificate restricted to the project sees that project only; a
	// full-trust one (incus-admin in effect) sees them all.
	out, err = l.run("query", name+":/1.0/projects")
	if err != nil {
		return err
	}
	var ps []string
	if err := json.Unmarshal([]byte(out), &ps); err != nil {
		return fmt.Errorf("incus projects: %v", err)
	}
	if len(ps) != 1 || ps[0] != "/1.0/projects/"+l.project {
		return fmt.Errorf("incus remote %q sees projects %v: its certificate must be restricted to project %s alone (incus config trust add-certificate --restricted --projects %s)", name, ps, l.project, l.project)
	}
	// The owner's share is the project's limits: without them nothing caps
	// what zm's VMs take from the host.
	out, err = l.run("query", name+":/1.0/projects/"+l.project)
	if err != nil {
		return err
	}
	var p struct {
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return fmt.Errorf("incus project %s: %v", l.project, err)
	}
	var missing []string
	for _, k := range []string{"limits.instances", "limits.cpu", "limits.memory"} {
		if strings.TrimSpace(p.Config[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("incus project %s sets no %s: the owner's share of this host is not enforced (incus project set %s limits.memory=... etc.)", l.project, strings.Join(missing, ", "), l.project)
	}
	return nil
}

func (l incusCLI) OSOf(name string) (string, error) {
	if err := l.restricted(); err != nil {
		return "", err
	}
	if out, err := l.run("image", "info", l.imageRef(name), "--project", l.project); err == nil {
		if !strings.Contains(out, "virtual-machine") {
			return "", fmt.Errorf("incus image %q is not a virtual-machine image: %s runs untrusted code in VMs only", name, kitconfig.Program)
		}
		return "linux", nil
	}
	if _, err := l.run("info", l.ref(name), "--project", l.project); err == nil {
		return "linux", nil
	}
	return "", fmt.Errorf("no incus VM image %q in project %s", name, l.project)
}

func (l incusCLI) Clone(image, vm string, cpu, memory int) error {
	a := []string{"init", l.imageRef(image), l.ref(vm), "--vm", "--project", l.project, "--profile", l.profile,
		"--config", incusLabelNew + "=1"}
	if cpu > 0 {
		a = append(a, "--config", "limits.cpu="+strconv.Itoa(cpu))
	}
	if memory > 0 {
		a = append(a, "--config", "limits.memory="+strconv.Itoa(memory)+"MiB")
	}
	_, err := l.run(a...)
	return err
}

// incusInstance is what the layer reads of a VM.
type incusInstance struct {
	Name            string                       `json:"name"`
	Status          string                       `json:"status"`
	Type            string                       `json:"type"`
	Config          map[string]string            `json:"config"`
	ExpandedConfig  map[string]string            `json:"expanded_config"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
}

// fenced checks a VM's expanded config before it boots.
func fenced(in incusInstance, isolated bool) error {
	var bad []string
	if in.Type != "virtual-machine" {
		bad = append(bad, "it is a "+strconv.Quote(in.Type)+", not a virtual-machine")
	}
	keys := make([]string, 0, len(in.ExpandedConfig))
	for k := range in.ExpandedConfig {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, "raw.") {
			bad = append(bad, "config "+k+" is set")
		}
	}
	names := make([]string, 0, len(in.ExpandedDevices))
	for n := range in.ExpandedDevices {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d := in.ExpandedDevices[n]
		switch d["type"] {
		case "disk":
			if d["path"] != "/" || d["source"] != "" {
				bad = append(bad, "device "+n+" is a disk other than the root (a host path or volume)")
			}
		case "nic":
			// Only a NIC on a managed bridge or OVN network takes port
			// isolation and ACLs; macvlan, physical, routed, ipvlan or a
			// parent= host bridge reach the LAN around them.
			if isolated && (d["network"] == "" || d["parent"] != "" || d["nictype"] != "" && d["nictype"] != "bridged" && d["nictype"] != "ovn") {
				bad = append(bad, "nic "+n+" is not on a managed bridge or OVN network (network= set, no parent=, nictype bridged or ovn)")
			}
			if isolated && d["security.port_isolation"] != "true" {
				bad = append(bad, "nic "+n+" lacks security.port_isolation=true (VMs could reach each other)")
			}
			if isolated && strings.TrimSpace(d["security.acls"]) == "" {
				bad = append(bad, "nic "+n+" has no security.acls (nothing drops the LAN, the host or metadata)")
			}
		default:
			bad = append(bad, "device "+n+" is a "+d["type"]+": only the root disk and nics are allowed")
		}
	}
	if len(bad) > 0 {
		return errors.New("the VM is not fenced: " + strings.Join(bad, "; ") + " (fix the profile)")
	}
	return nil
}

func (l incusCLI) instance(vm string) (incusInstance, error) {
	var in incusInstance
	out, err := l.run("query", l.apiPath("/1.0/instances/"+vm+"?project="+l.project))
	if err != nil {
		return in, err
	}
	return in, json.Unmarshal([]byte(out), &in)
}

func (l incusCLI) apiPath(p string) string {
	if l.remote == "" {
		return p
	}
	return l.remote + ":" + p
}

// incusBoot watches a started VM; a guard deletes it when the supervisor's
// pipe (fd 3) closes. The watcher exits only when Incus answers
// that the VM is not running or not found: an error reaching incusd (a
// restart) is not the VM going. The guard retries its delete through such a
// restart, for at most 10 minutes. The guard holds none of Boot's output.
const incusBoot = `ref=$1; bin=$2; proj=$3; api=$4; exec 3<&0
( cat <&3; n=0; while [ $n -lt 300 ]; do
    o=$("$bin" delete --force "$ref" --project "$proj" 2>&1) && break
    case "$o" in *"not found"*|*"Not Found"*|*"Not found"*) break ;; esac
    n=$((n+1)); sleep 2
  done ) >/dev/null 2>&1 & g=$!
exec 3<&-
while :; do
  if o=$("$bin" query "$api" 2>&1); then
    case "$o" in *'"status": "Running"'*|*'"status":"Running"'*) ;; *) break ;; esac
  else
    case "$o" in *"not found"*|*"Not Found"*|*"Not found"*) break ;; esac
  fi
  sleep 1
done
kill $g 2>/dev/null; exit 0`

func (l incusCLI) Boot(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	in, err := l.instance(vm)
	if err != nil {
		return nil, nil, err
	}
	check := func(in incusInstance) error {
		if err := fenced(in, isolated); err != nil || !isolated {
			return err
		}
		return l.mailFenced(in)
	}
	if err := check(in); err != nil {
		return nil, nil, err
	}
	if _, err := l.run("start", l.ref(vm), "--project", l.project); err != nil {
		return nil, nil, err
	}
	// Checked again now it runs: a change between the check and the start
	// (the same certificate can edit the VM) is caught before any run.
	if in, err = l.instance(vm); err == nil {
		err = check(in)
	}
	if err != nil {
		l.Destroy(vm, 0)
		return nil, nil, err
	}
	cmd := exec.Command("sh", "-c", incusBoot, "sh", l.ref(vm), l.bin, l.project,
		l.apiPath("/1.0/instances/"+vm+"/state?project="+l.project))
	if l.conf != "" {
		cmd.Env = append(os.Environ(), "INCUS_CONF="+l.conf)
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	held, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, held, cmd.Start()
}

func (l incusCLI) Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd {
	a := []string{"exec", l.ref(vm), "--project", l.project, "--force-noninteractive"}
	if stdin == nil {
		a = append(a, "--disable-stdin")
	}
	cmd := l.command(append(append(a, "--"), argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd
}

func (l incusCLI) List() ([]vmInfo, error) {
	a := []string{"list"}
	if l.remote != "" {
		a = append(a, l.remote+":")
	}
	out, err := l.command(append(a, "--project", l.project, "--format", "json")...).Output()
	if err != nil {
		return nil, fmt.Errorf("incus list: %v", err)
	}
	var ins []incusInstance
	if err := json.Unmarshal(out, &ins); err != nil {
		return nil, fmt.Errorf("incus list: %v", err)
	}
	var vs []vmInfo
	for _, in := range ins {
		if in.Config[incusLabel] == "1" || in.Config[incusLabelNew] == "1" {
			vs = append(vs, vmInfo{Name: in.Name, Running: in.Status == "Running", Local: true})
		}
	}
	return vs, nil
}

func (l incusCLI) Destroy(vm string, runPID int) {
	if runPID > 0 {
		_ = syscall.Kill(-runPID, syscall.SIGKILL)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		out, err := l.run("delete", "--force", l.ref(vm), "--project", l.project)
		if err == nil || notFound(out) {
			return
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
			return
		}
	}
}
