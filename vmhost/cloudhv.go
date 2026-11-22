package vmhost

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cloudhvCLI is the cloud-hypervisor VM layer (vm=cloudhv, docs/cloudhv.md):
// each box is a KVM virtual machine run by a cloud-hypervisor process of the
// driver's own user, with no daemon and no root. What it adds to the other
// layers is the nap: an idle VM is saved to disk and its process ended, so it
// holds no memory, and the next command restores it with every program still
// running (cloudhv_mon.go).
//
// Against the layer contract:
//   - Clone makes a directory per VM under ch_dir: a qcow2 overlay over the
//     owner's raw image (nothing is copied; the image is only read), and the
//     VM's own address, tap name, MAC and vsock context id.
//   - Boot asks the root helper (cloudhv_net.go) for the VM's tap and the
//     host's fence, and refuses to start when it cannot have them: the VM's
//     only path out is that tap. It then starts the VM's monitor, the host
//     process that lives as long as the VM does, napping or awake.
//   - Exec runs a command through potluck-guest on the VM's vsock: no SSH,
//     no credential. A napping VM is woken first.
//   - The guest gets nothing of the host's: one disk, one NIC, one vsock.
type cloudhvCLI struct {
	c     cloudhvConfig
	state string // the binding's state directory
	// root and docker are the binding's root=on and docker=on
	// (parseCloudhvRoot): what each VM made here is made with.
	root, docker bool
}

// parseCloudhvRoot reads root and docker on a cloud-hypervisor binding
// (config.parseRoot calls it when one of them is on). A box here is a whole
// VM of its own, and its fence is on the host (cloudhv_net.go), so root=on
// gives the box's user the guest's own root, with nothing held back: describe
// lists root and root-vm, and conformance's root group shows from root's own
// shell that the fence still holds. docker=on, which needs root, also starts
// the image's Docker when the box boots. The image gives its user no sudo
// of its own: the guest agent writes the rule in a VM made with root=on, and
// removes one it finds at boot (cloudhvguest.Run).
func (c *config) parseCloudhvRoot(m map[string]string, docker bool, bad *[]string) {
	switch {
	case m["root"] == "off" && docker:
		*bad = append(*bad, "docker=on needs root (whoever reaches Docker is root in the box): leave root unset or on")
	case c.Kind != "box":
		*bad = append(*bad, "root and docker on cloudhv are a box binding's (capabilities root, root-vm and docker)")
	default:
		c.Root, c.Docker = true, docker
	}
}

// cloudhvConfig is the layer's binding keys.
type cloudhvConfig struct {
	// Bin is cloud-hypervisor, Firmware the UEFI firmware it boots
	// (CLOUDHV.fd), QemuImg the qemu-img that makes each VM's overlay.
	Bin, Firmware, QemuImg string
	// Images holds the owner's images, <name>.raw. Dir holds one directory
	// per VM.
	Images, Dir string
	// Net is the private range the VMs' links are cut from, a /30 each.
	Net string
	// NapIdleS naps a VM that has run no command for this long (0: never).
	NapIdleS int
	// Console keeps the guest's serial console in the VM's directory. Off by
	// default: a guest can fill the host's disk through it.
	Console bool
}

const (
	cloudhvNetDefault = "10.213.0.0/16"
	// flagCloudHVVM re-executes the driver as one VM's monitor,
	// flagCloudHVExec as one command's client, flagCloudHVNet (as root) as
	// the network helper.
	flagCloudHVVM   = "--cloudhv-vm"
	flagCloudHVExec = "--cloudhv-exec"
	flagCloudHVNet  = "--cloudhv-net"
	// A VM whose binding names no cpu or memory (MiB) gets these.
	cloudhvCPU, cloudhvMemory = 2, 4096
)

var cloudhvImageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// ParseConfig is cloudhvCLI's layerConfig: the ch_* keys.
func (cloudhvCLI) ParseConfig(m map[string]string, c *config) (bad []string) {
	h := cloudhvConfig{Bin: m["ch"], Firmware: m["ch_firmware"], QemuImg: m["ch_qemu_img"], Images: m["ch_images"], Dir: m["ch_dir"],
		Net: m["ch_net"], Console: m["ch_console"] == "on"}
	if h.Bin == "" {
		h.Bin = "cloud-hypervisor"
	}
	if h.QemuImg == "" {
		h.QemuImg = "qemu-img"
	}
	if h.Dir == "" {
		h.Dir = filepath.Join(c.StateDir, "cloudhv")
	}
	if h.Net == "" {
		h.Net = cloudhvNetDefault
	}
	// Clean paths: a VM's directory is told to be under ch_dir by its prefix.
	h.Dir, h.Images = filepath.Clean(h.Dir), filepath.Clean(h.Images)
	for _, k := range []string{"ch_firmware", "ch_images"} {
		if v := m[k]; v == "" {
			bad = append(bad, k+" is required")
		} else if !filepath.IsAbs(v) {
			bad = append(bad, k+" must be an absolute path")
		}
	}
	if !filepath.IsAbs(h.Dir) {
		bad = append(bad, "ch_dir must be an absolute path")
	}
	if _, _, err := cloudhvRange(h.Net); err != nil {
		bad = append(bad, "ch_net "+strconv.Quote(h.Net)+": "+err.Error())
	}
	if m["network"] == "nat" {
		bad = append(bad, "network=nat is not for vm=cloudhv: its VMs have only the fenced link")
	}
	if v := m["ch_console"]; v != "" && v != "on" && v != "off" {
		bad = append(bad, "ch_console is on or off")
	}
	num := func(k string, def, lo, hi int) int {
		v := m[k]
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			bad = append(bad, fmt.Sprintf("%s must be a number from %d to %d", k, lo, hi))
			return def
		}
		return n
	}
	h.NapIdleS = num("ch_nap_idle_s", 0, 0, 30*24*3600)
	if h.NapIdleS > 0 && m["runner_dir"] != "" {
		bad = append(bad, "ch_nap_idle_s with runner_dir: a nap saves the box's memory to the host's disk, and a runner's registration is a credential held in it")
	}
	c.CloudHV = h
	return bad
}

// raw writes the layer's keys back out for the supervisor (rawConfig).
func (h cloudhvConfig) raw(m map[string]string) {
	m["ch"], m["ch_firmware"], m["ch_qemu_img"], m["ch_images"], m["ch_dir"], m["ch_net"] = h.Bin, h.Firmware, h.QemuImg, h.Images, h.Dir, h.Net
	m["ch_nap_idle_s"] = strconv.Itoa(h.NapIdleS)
	if h.Console {
		m["ch_console"] = "on"
	}
}

// cloudhvRange reads ch_net: a private IPv4 range under 10/8 that holds at
// least one /30. It answers the range's first address and how many links fit.
func cloudhvRange(s string) (base uint32, links int, err error) {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return 0, 0, errors.New("not a network (for example " + cloudhvNetDefault + ")")
	}
	ones, bits := n.Mask.Size()
	ip := n.IP.To4()
	if ip == nil || bits != 32 || ip[0] != 10 || ones < 8 || ones > 30 {
		return 0, 0, errors.New("must be an IPv4 range inside 10.0.0.0/8, /30 or wider")
	}
	return binary.BigEndian.Uint32(ip), 1 << (30 - ones), nil
}

// cloudhvVM is a VM's own record (vm.json in its directory): what Clone
// chose, read by Boot, by the monitor and by the network helper's caller.
type cloudhvVM struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// ImageID is which file the image was when the VM's disk was made over
	// it (cloudhvImageID). The disk is only the blocks the VM changed, so
	// over any other file it is garbage.
	ImageID  string `json:"image_id,omitempty"`
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mib"`
	// The VM's link: the guest's address (its vsock context id is the same
	// 32 bits, which is how the guest learns it), the host's end, the tap.
	GuestIP string `json:"guest_ip"`
	HostIP  string `json:"host_ip"`
	Tap     string `json:"tap"`
	MAC     string `json:"mac"`
	Net     string `json:"net"`
	Kept    bool   `json:"kept,omitempty"`

	Bin      string `json:"bin"`
	Firmware string `json:"firmware"`
	NapIdleS int    `json:"nap_idle_s,omitempty"`
	Console  bool   `json:"console,omitempty"`
	// Root: the guest agent gives the box's user sudo; Docker: it starts the
	// image's Docker. Both are the binding's when the VM was made, and a
	// kept VM keeps them (keptRootDiffers).
	Root   bool `json:"root,omitempty"`
	Docker bool `json:"docker,omitempty"`
}

// cloudhvStore writes a VM's record, whole or not at all.
func cloudhvStore(dir string, v cloudhvVM) error {
	b, _ := json.MarshalIndent(v, "", " ")
	tmp := filepath.Join(dir, "vm.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "vm.json"))
}

// cloudhvImageID names an image file as it is now: its size and modification
// time. Not a digest: it tells a rebuilt or rewritten file from the one a
// VM's disk was made over, without reading 20 GB. A copy that keeps the time
// (a restored backup) is the same image.
func cloudhvImageID(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%d", st.Size(), st.ModTime().UnixNano()), nil
}

// imageSame refuses a VM whose image is no longer the file its disk was made
// over: replaced by a rebuild, or written to.
func (v cloudhvVM) imageSame() error {
	if v.ImageID == "" {
		return nil
	}
	if id, err := cloudhvImageID(v.Image); err != nil || id != v.ImageID {
		return fmt.Errorf("image %s is not the file VM %s's disk was made over (it was replaced or changed), so the disk cannot be used: the VM must be made again", v.Image, v.Name)
	}
	return nil
}

// CID is the VM's vsock context id: its guest address as a number.
func (v cloudhvVM) CID() uint32 { return binary.BigEndian.Uint32(net.ParseIP(v.GuestIP).To4()) }

func (l cloudhvCLI) dir(vm string) string { return filepath.Join(l.c.Dir, vm) }

func cloudhvLoad(dir string) (cloudhvVM, error) {
	var v cloudhvVM
	b, err := os.ReadFile(filepath.Join(dir, "vm.json"))
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(b, &v)
}

// imagePath finds an image's file. An image is <name>, the file
// <ch_images>/<name>.raw. A box's image is pinned, <name>@sha256:<hex>
// (box.go): the digest is the file's own, which whoever built the image wrote
// beside it in <name>.raw.sha256 (scripts/cloudhv-image.sh does). Hashing
// tens of gigabytes at every acquire is too slow, so the pin is checked
// against that file, and an image changed after its digest was written is
// refused.
func (l cloudhvCLI) imagePath(image string) (string, error) {
	name, pin, pinned := strings.Cut(image, "@sha256:")
	if !cloudhvImageName.MatchString(name) {
		return "", fmt.Errorf("image %q is not a plain name", name)
	}
	p := filepath.Join(l.c.Images, name+".raw")
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("no image %s", p)
	}
	if !pinned {
		return p, nil
	}
	sum, err := os.ReadFile(p + ".sha256")
	sst, serr := os.Stat(p + ".sha256")
	if err != nil || serr != nil {
		return "", fmt.Errorf("image %s has no digest file %s.sha256 (write it with: sha256sum %s | cut -d' ' -f1 > %s.sha256)", name, p, p, p)
	}
	if sst.ModTime().Before(st.ModTime()) {
		return "", fmt.Errorf("image %s changed after its digest was written: write %s.sha256 again and pin the new digest", name, p)
	}
	if f := strings.Fields(string(sum)); len(f) == 0 || f[0] != pin {
		return "", fmt.Errorf("image %s is pinned to a digest that is not the one in %s.sha256", name, p)
	}
	return p, nil
}

func (l cloudhvCLI) OSOf(name string) (string, error) {
	if _, err := cloudhvLoad(l.dir(name)); err == nil {
		return "linux", nil
	}
	if _, err := l.imagePath(name); err != nil {
		return "", err
	}
	if _, err := exec.LookPath(l.c.Bin); err != nil {
		return "", fmt.Errorf("cloud-hypervisor (%s) is not installed", l.c.Bin)
	}
	if _, err := exec.LookPath(l.c.QemuImg); err != nil {
		return "", fmt.Errorf("qemu-img (%s) is not installed: it makes each VM's disk", l.c.QemuImg)
	}
	if _, err := os.Stat(l.c.Firmware); err != nil {
		return "", fmt.Errorf("no firmware at ch_firmware %s", l.c.Firmware)
	}
	return "linux", nil
}

func (l cloudhvCLI) Clone(image, vm string, cpu, memory int) error {
	return l.clone(image, vm, cpu, memory, false)
}

// CloneKept is keeper's (persist.go).
func (l cloudhvCLI) CloneKept(image, vm string, cpu, memory int) error {
	return l.clone(image, vm, cpu, memory, true)
}

func (l cloudhvCLI) clone(image, vm string, cpu, memory int, kept bool) error {
	img, err := l.imagePath(image)
	if err != nil {
		return err
	}
	if cpu <= 0 {
		cpu = cloudhvCPU
	}
	if memory <= 0 {
		memory = cloudhvMemory
	}
	if err := os.MkdirAll(l.c.Dir, 0o700); err != nil {
		return err
	}
	// One Clone at a time picks a link, so two VMs never take the same one.
	lock, err := os.OpenFile(filepath.Join(l.c.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	base, links, err := cloudhvRange(l.c.Net)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	ents, _ := os.ReadDir(l.c.Dir)
	for _, e := range ents {
		if v, err := cloudhvLoad(filepath.Join(l.c.Dir, e.Name())); err == nil {
			taken[v.GuestIP] = true
		}
	}
	imgID, err := cloudhvImageID(img)
	if err != nil {
		return err
	}
	v := cloudhvVM{Name: vm, Image: img, ImageID: imgID, CPU: cpu, MemoryMB: memory, Net: l.c.Net, Kept: kept,
		Bin: l.c.Bin, Firmware: l.c.Firmware, NapIdleS: l.c.NapIdleS, Console: l.c.Console, Root: l.root, Docker: l.docker}
	// Links are handed out in turn, not lowest first, so an address a VM
	// just gave up is the last to be used again.
	nextFile := filepath.Join(l.c.Dir, ".next")
	first := 0
	if b, err := os.ReadFile(nextFile); err == nil {
		first, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	for i := 0; i < links; i++ {
		n := (max(first, 0) + i) % links
		host, guest := base+uint32(4*n)+1, base+uint32(4*n)+2
		g := cloudhvIP(guest)
		tap := cloudhvTapOf(guest)
		if taken[g] || cloudhvTapExists(tap) { // a tap with no VM here is another binding's, or not yet cleaned up
			continue
		}
		v.GuestIP, v.HostIP, v.Tap = g, cloudhvIP(host), tap
		v.MAC = fmt.Sprintf("52:54:00:%02x:%02x:%02x", byte(guest>>16), byte(guest>>8), byte(guest))
		_ = os.WriteFile(nextFile, []byte(strconv.Itoa((n+1)%links)), 0o600)
		break
	}
	if v.GuestIP == "" {
		return fmt.Errorf("no free link in ch_net %s", l.c.Net)
	}
	dir := l.dir(vm)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	out, err := exec.Command(l.c.QemuImg, "create", "-q", "-f", "qcow2", "-b", img, "-F", "raw", filepath.Join(dir, "disk.qcow2")).CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("%s create: %v: %s", l.c.QemuImg, err, strings.TrimSpace(string(out)))
	}
	if err := cloudhvStore(dir, v); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return nil
}

func cloudhvIP(n uint32) string {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, n)
	return b.String()
}

// cloudhvTapExists says whether the host already has a device of that name.
var cloudhvTapExists = func(tap string) bool {
	_, err := os.Stat("/sys/class/net/" + tap)
	return err == nil
}

func (l cloudhvCLI) Boot(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	return l.boot(vm, "delete", log)
}

// BootKept is keeper's: when the pipe closes the VM is powered off and its
// disk stays.
func (l cloudhvCLI) BootKept(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	return l.boot(vm, "keep", log)
}

// boot starts the VM's monitor. The network is the same fenced link whether
// or not the run asked for isolation: this layer has no unfenced one.
func (l cloudhvCLI) boot(vm, mode string, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	dir := l.dir(vm)
	v, err := cloudhvLoad(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("no VM %s: %v", vm, err)
	}
	if cloudhvMonAlive(dir) {
		return nil, nil, fmt.Errorf("VM %s is already running", vm)
	}
	held, locked := cloudhvNetLock(dir, cloudhvNetWait)
	if !locked {
		if held != nil {
			held.Close()
		}
		return nil, nil, fmt.Errorf("VM %s: an earlier start is still setting up its network", vm)
	}
	if v, err = cloudhvLoad(dir); err != nil { // destroyed while this boot waited for the lock
		held.Close()
		return nil, nil, fmt.Errorf("no VM %s: %v", vm, err)
	}
	if err := v.imageSame(); err != nil {
		held.Close()
		return nil, nil, err
	}
	// A VM naps as its binding says now, not as it said when the VM was
	// made: secrets are served by the binding's setting (secrets.go).
	if v.NapIdleS != l.c.NapIdleS {
		v.NapIdleS = l.c.NapIdleS
		if err := cloudhvStore(dir, v); err != nil {
			held.Close()
			return nil, nil, err
		}
	}
	err = cloudhvNetUp(v, held)
	held.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("the VM's fenced network could not be set up, so it is not started: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(self, flagCloudHVVM, dir, mode)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, pipe, cmd.Start()
}

func (l cloudhvCLI) Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		self = "potluck-driver-tart" // Start then fails with the reason
	}
	cmd := exec.Command(self, append([]string{flagCloudHVExec, l.dir(vm)}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd
}

func (l cloudhvCLI) List() ([]vmInfo, error) {
	ents, err := os.ReadDir(l.c.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var vs []vmInfo
	for _, e := range ents {
		dir := filepath.Join(l.c.Dir, e.Name())
		if v, err := cloudhvLoad(dir); err == nil && v.Name == e.Name() {
			vs = append(vs, vmInfo{Name: v.Name, Running: cloudhvMonAlive(dir), Local: true})
		}
	}
	return vs, nil
}

// cloudhvPids are the monitor and hypervisor a VM's directory names.
func cloudhvPids(dir string) (mon, ch int) {
	var st cloudhvState
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st.PID, st.HypervisorPID
}

// cloudhvMonAlive says whether the VM's monitor runs. The pid alone could be
// a later process's, so the monitor holds a lock on its directory for as long
// as it lives; asking takes it shared, so two askers never mistake each other
// for a monitor.
func cloudhvMonAlive(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "mon.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	return true
}

// stop ends the VM's processes: asked first (the monitor powers off and
// cleans up as its mode says) when grace is above 0, then killed. The pids
// are read from a file, so each is signalled only while it is still a
// process of this VM (cloudhvPidOf): never a later process that got the
// number. That also ends a hypervisor whose monitor died without it.
func (l cloudhvCLI) stop(dir string, runPID int, grace time.Duration) {
	mon, ch := cloudhvPids(dir)
	if grace > 0 && cloudhvMonAlive(dir) && cloudhvPidOf(mon, dir) {
		_ = syscall.Kill(mon, syscall.SIGTERM)
		for end := time.Now().Add(grace); time.Now().Before(end) && cloudhvMonAlive(dir); {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if runPID > 0 {
		_ = syscall.Kill(-runPID, syscall.SIGKILL)
	}
	for pid, ours := range map[int]bool{mon: cloudhvPidOf(mon, dir), ch: cloudhvPidOf(ch, dir)} {
		if ours {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && (cloudhvMonAlive(dir) || cloudhvPidOf(ch, dir)); {
		time.Sleep(20 * time.Millisecond)
	}
}

func (l cloudhvCLI) Destroy(vm string, runPID int) {
	dir := l.dir(vm)
	v, err := cloudhvLoad(dir)
	l.stop(dir, runPID, 0)
	// The network lock is held until the directory is gone, so a boot that
	// was waiting for it finds no VM and makes no tap.
	held, _ := cloudhvNetLock(dir, cloudhvNetWait)
	ours := strings.HasPrefix(dir, l.c.Dir+string(filepath.Separator)) && vm != ""
	if ours {
		// The record first: a boot that opens the lock file from here on
		// finds no VM, whatever else of the directory is still there.
		_ = os.Remove(filepath.Join(dir, "vm.json"))
	}
	if err == nil {
		cloudhvNetDown(v)
	}
	if ours {
		_ = os.RemoveAll(dir)
	}
	if held != nil {
		held.Close()
	}
}

// netDown removes a stopped VM's tap, once no helper of an earlier boot is
// still making it (cloudhvNetLock).
func (cloudhvCLI) netDown(dir string, v cloudhvVM) {
	held, _ := cloudhvNetLock(dir, cloudhvNetWait)
	cloudhvNetDown(v)
	if held != nil {
		held.Close()
	}
}

// PowerOff is keeper's: the VM stops and its disk stays. A napping VM's saved
// memory goes with the stop: a stopped box starts from its disk.
func (l cloudhvCLI) PowerOff(vm string, runPID int) {
	dir := l.dir(vm)
	v, err := cloudhvLoad(dir)
	if err != nil {
		return
	}
	l.stop(dir, runPID, cloudhvStopFor)
	l.netDown(dir, v)
	cloudhvClearRuntime(dir)
}

// KeepsRoot is keeper's: the whole disk survives a stop.
func (cloudhvCLI) KeepsRoot() bool { return true }

// cloudhvClearRuntime removes what belongs to one boot of a VM and leaves its
// disk and record.
func cloudhvClearRuntime(dir string) {
	for _, n := range []string{"snap", "snap.tmp", "api.sock", "vsock.sock", "ctl.sock", "state.json", "console.log"} {
		_ = os.RemoveAll(filepath.Join(dir, n))
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents { // the hypervisor's per-port vsock sockets
			if strings.HasPrefix(e.Name(), "vsock.sock_") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}
