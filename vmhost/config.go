// Package vmhost is a SPEC 20 run target that runs each job in a throwaway
// VM on this host: a fresh clone of a base image, booted with an isolated
// network and nothing of the host's attached, holding a clean checkout of
// exactly the commit sent, or a box (a GitHub JIT runner is a box's). A detached supervisor per
// run enforces expires_at and the reclaim policy itself, and deletes the VM
// when the run ends. Every VM operation goes through vmLayer (layer.go): Tart
// on a Mac, an OCI runtime (Kata, gVisor) on Linux. cmd/potluck-driver-tart is its
// executable; nothing here is linked into the daemon.
package vmhost

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	kitconfig "github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
)

// Apple's licence and Virtualization.framework allow 2 macOS guests per host.
const macCap = 2

type config struct {
	Target, Kind string
	VM           string   // the VM layer (layers): tart unless set
	Image        string   // a local image of that layer every run clones (a box: Images[0])
	Size         string   // box: the binding's one size name (describe.size.name)
	Images       []string // box: owner-set images, each pinned by digest; the first is the default
	JITRunnerDir string   // box: where the image keeps the GitHub runner; set = the binding serves jit_config
	Tart         string   // the tart binary
	SoftnetPath  string   // optional absolute helper path, pinned by host preflight
	StateDir     string
	Remote       string // the binding repo's remote: the guest fetches commit from it
	HostFetch    bool   // the host fetches and copies a bundle in (a local remote, or fetch=host)
	Softnet      bool   // false only with network=nat, an unsafe dev setting
	LinuxSlots   int
	MacSlots     int
	TTLMax       int
	CPU, Memory  int // 0 keeps the image's
	ScriptsDir   string
	BootTimeout  int
	Reclaim      Policy
	// DrainDeadline (drain_deadline_s, default 1800): how long running work
	// has once a drain begins (hostmode.go) before it is reclaimed; 0
	// reclaims at once.
	DrainDeadline int
	// HostOwner (host_owner_uid, default the driver's uid): who must own the
	// host override file and its directory (hostmode.go), besides root.
	HostOwner int
	// vm=oci (oci.go): the container CLI, the runtime (a VM or sandboxed
	// kernel: Kata, gVisor) and the network (none unless the owner made a
	// filtered one), and on a network the box's resolvers (oci_dns, default
	// ociDefaultDNS; none with network none or another namespace's).
	OCICLI, OCIRuntime, OCINetwork string
	OCIHostProfile                 string
	HostCPULimit, HostMemoryLimit  int
	OCIDNS                         []string
	// Root (root=on) gives the box's user working sudo, Docker (docker=on,
	// implies Root) rootless Docker (oci_root.go).
	Root, Docker bool
	// vm=incus (incus.go): the incus binary, the certificate remote ("" is
	// the CLI's default), the project made for zm, the owner's profile (root
	// disk and a fenced NIC) and INCUS_CONF ("" is the CLI's own).
	IncusBin, IncusRemote, IncusProject, IncusProfile, IncusConf string
	// admission_url (admission.go): the host's admission controller, asked
	// for room before every VM; its bearer token file and the lease TTL.
	AdmissionURL, AdmissionTokenFile string
	AdmissionTTL                     int
	// persist (persist.go): at most PersistMax kept disks in the state
	// directory (0: none), each kept PersistKeepS once stopped.
	PersistMax, PersistKeepS int
	PersistMinFreeGB         int // refuse a kept disk below this much free disk under the layer's disk directory (0: no floor)
	// volumes (volume.go): an owner's named volumes on this binding, at most
	// VolumeMaxGB GiB (0: no volumes), each deleted VolumeKeepS after a box
	// last named it.
	VolumeMaxGB, VolumeKeepS int
	VolumeMaxMB              int // a finer cap than volume_max_gb, when set (a small host, tests)
	VolumeCheckS             int // how often a supervisor measures a box's writable volumes
	VolumeProjectMaxGB       int // a project's volumes on this binding (0: 4x the owner's cap)
	VolumeBindingMaxGB       int // all volumes on this binding (0: 8x the owner's cap)
	// shelf (shelf.go): the owner's package shelf on the cache volume.
	Shelf shelfCfg
	// warm (warm.go): boxes kept booted and idle, taken by an acquire at once.
	Warm int
}

var relDir = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)

func parseConfig(m map[string]string) (config, error) {
	c := config{Target: m["target"], Kind: m["kind"], VM: m["vm"], Image: m["image"], Tart: m["tart"], StateDir: m["state_dir"],
		Remote: m["remote"], ScriptsDir: strings.Trim(m["scripts_dir"], "/"), Softnet: true, SoftnetPath: m["softnet_path"]}
	if c.Kind == "" {
		c.Kind = "run"
	}
	if c.VM == "" {
		c.VM = "tart"
	}
	if c.Tart == "" {
		c.Tart = "tart"
	}
	if c.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, err
		}
		c.StateDir = protocol.DriverStateDir(home, "tart") // ~/.potluck-driver-tart, or an existing ~/.zm-run-tart
	}
	var bad []string
	if c.SoftnetPath != "" && (!filepath.IsAbs(c.SoftnetPath) || filepath.Base(c.SoftnetPath) != "softnet" || strings.ContainsRune(c.SoftnetPath, os.PathListSeparator)) {
		bad = append(bad, "softnet_path must be an absolute path to an executable named softnet")
	}
	ints := []struct {
		key  string
		into *int
		def  int
	}{{"linux_slots", &c.LinuxSlots, 2}, {"macos_slots", &c.MacSlots, macCap}, {"ttl_max_s", &c.TTLMax, 4 * 3600},
		{"cpu", &c.CPU, 0}, {"memory", &c.Memory, 0}, {"boot_timeout_s", &c.BootTimeout, 90},
		{"host_cpu_limit", &c.HostCPULimit, 0}, {"host_memory_limit_mb", &c.HostMemoryLimit, 0},
		{"reclaim_idle_s", &c.Reclaim.IdleS, 0}, {"persist_max", &c.PersistMax, persistMaxDefault}, {"persist_keep_s", &c.PersistKeepS, persistKeepDefault}, {"persist_min_free_gb", &c.PersistMinFreeGB, persistMinFreeDefault},
		{"volume_max_gb", &c.VolumeMaxGB, volumeMaxGBDefault}, {"volume_keep_s", &c.VolumeKeepS, volumeKeepDefault}, {"volume_max_mb", &c.VolumeMaxMB, 0}, {"volume_check_s", &c.VolumeCheckS, volumeCheckDefault},
		{"volume_project_max_gb", &c.VolumeProjectMaxGB, 0}, {"volume_binding_max_gb", &c.VolumeBindingMaxGB, 0}, {"warm", &c.Warm, 0},
		{"drain_deadline_s", &c.DrainDeadline, drainDeadlineDefault}}
	ints = append(ints, c.Shelf.ints()...)
	for _, f := range ints {
		*f.into = f.def
		if v := m[f.key]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				bad = append(bad, f.key+" is not a whole number")
			}
			*f.into = n
		}
	}
	c.MacSlots = min(c.MacSlots, macCap)
	c.Shelf.parse(m, c.VM, &bad)
	if c.TTLMax <= 0 {
		bad = append(bad, "ttl_max_s must be positive")
	}
	if c.PersistKeepS < 60 {
		bad = append(bad, "persist_keep_s must be at least 60")
	}
	if c.Warm > 0 && c.Kind != "box" {
		bad = append(bad, "warm is a box binding's")
	}
	if c.VolumeCheckS < 1 {
		bad = append(bad, "volume_check_s must be at least 1")
	}
	if c.VolumeKeepS < 60 {
		bad = append(bad, "volume_keep_s must be at least 60")
	}
	if c.BootTimeout <= 0 || c.BootTimeout > 100 {
		bad = append(bad, "boot_timeout_s must be 1..100 (acquire answers in 120 s)")
	}
	if c.DrainDeadline > maxDrainDeadline {
		bad = append(bad, fmt.Sprintf("drain_deadline_s must be 0..%d", maxDrainDeadline))
	}
	c.HostOwner = os.Getuid()
	if v := m["host_owner_uid"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			bad = append(bad, "host_owner_uid is not a uid")
		}
		c.HostOwner = n
	}
	if c.Reclaim.Window, c.Reclaim.Windows = m["window"], m["windows"]; c.Reclaim.windowed() {
		if _, err := c.Reclaim.spans(); err != nil {
			bad = append(bad, err.Error())
		}
	}
	if c.AdmissionURL, c.AdmissionTokenFile = m["admission_url"], m["admission_token_file"]; c.AdmissionURL != "" {
		u, err := url.Parse(c.AdmissionURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
			bad = append(bad, "admission_url must be an http(s) URL with no credentials or query (a token goes in admission_token_file)")
		}
		if c.CPU <= 0 || c.Memory <= 0 {
			bad = append(bad, "admission_url needs cpu and memory: the lease asks for that much room")
		}
		c.AdmissionTTL = max(defaultAdmissionTTL, c.BootTimeout+30)
		if v := m["admission_ttl_s"]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < c.BootTimeout+30 || n > 3600 {
				bad = append(bad, fmt.Sprintf("admission_ttl_s must be %d..3600 (outlasting boot_timeout_s by 30 s, since renewals start once the VM runs)", c.BootTimeout+30))
			}
			c.AdmissionTTL = n
		}
		if c.AdmissionTokenFile != "" {
			if _, err := readTokenFile(c.AdmissionTokenFile); err != nil {
				bad = append(bad, err.Error())
			}
		}
	} else if c.AdmissionTokenFile != "" {
		bad = append(bad, "admission_token_file without admission_url")
	}
	if f := layers[c.VM]; f != nil { // a layer that checks its own keys (layerConfig)
		if lc, ok := f(c).(layerConfig); ok {
			bad = append(bad, lc.ParseConfig(m, &c)...)
		}
	}
	if c.VM == "oci" {
		c.OCICLI, c.OCIRuntime, c.OCINetwork = m["oci_cli"], m["oci_runtime"], m["oci_network"]
		c.OCIHostProfile = m["oci_host_profile"]
		if c.Warm > 0 && (ociKataRuntime(c.OCIRuntime) || c.OCIHostProfile != "") {
			bad = append(bad, "bounded OCI host resources cannot create unleased warm boxes")
		}
		if c.OCICLI == "" {
			c.OCICLI = "docker"
		}
		if c.OCINetwork == "" {
			c.OCINetwork = "none"
		}
		if m["network"] != "nat" && !ociIsolatedNet(c.OCINetwork) { // isolated (network=nat is the unsafe dev setting)
			bad = append(bad, "oci_network "+strconv.Quote(c.OCINetwork)+" may reach the host or its LAN: none, or the plain name of a network made for "+kitconfig.Program+" with egress filtered")
		}
		switch v := m["oci_dns"]; {
		case c.OCINetwork == "none" && v != "":
			bad = append(bad, "oci_dns needs a network: oci_network is none")
		case !ociNetHasDNS(c.OCINetwork) && v != "":
			bad = append(bad, "oci_dns with oci_network "+strconv.Quote(c.OCINetwork)+": a container joining another namespace keeps its DNS (the CLIs refuse --dns)")
		case !ociNetHasDNS(c.OCINetwork):
		case v == "":
			c.OCIDNS = ociDefaultDNS
		default:
			dns, err := parseOCIDNS(v)
			if err != nil {
				bad = append(bad, err.Error())
			}
			c.OCIDNS = dns
		}
		if !ociRuntimeIsBoundary(c.OCIRuntime) {
			bad = append(bad, "oci_runtime must be a VM or sandboxed-kernel runtime by name (kata, kata-fc, io.containerd.kata.v2, runsc, ...), not "+strconv.Quote(c.OCIRuntime)+": a plain container shares the host's kernel")
		}
	}
	if c.Reclaim.IdleS > 0 && runtime.GOOS != "darwin" {
		bad = append(bad, "reclaim_idle_s reads a Mac's keyboard and pointer idle time: on "+runtime.GOOS+" use window")
	}
	if m["runner"] != "" { // a GitHub runner is a box: the broker mints its jit_config, no host holds a GitHub token
		bad = append(bad, "runner=github is gone: a GitHub runner is a box binding with runner_dir, served by the runner broker's jit_config")
	}
	if layers[c.VM] == nil {
		bad = append(bad, "vm "+strconv.Quote(c.VM)+" is not a VM layer ("+layerNames()+")")
	}
	switch c.Kind {
	case "run":
		if c.Image == "" {
			bad = append(bad, "no image (a local VM image to clone)")
		}
		if c.Remote == "" {
			bad = append(bad, "no remote (the repo the guest fetches commit from)")
		}
	case "box":
		c.parseBox(m, &bad)
	default:
		bad = append(bad, "kind "+strconv.Quote(c.Kind)+": potluck-driver-tart serves run and box")
	}
	if !relDir.MatchString(c.ScriptsDir) || strings.Contains(c.ScriptsDir, "..") {
		bad = append(bad, "scripts_dir must be a relative path inside the checkout")
	}
	switch m["network"] {
	case "", "softnet":
	case "nat":
		c.Softnet = false
	default:
		bad = append(bad, "network is softnet or nat")
	}
	c.parseRoot(m, &bad) // oci_root.go: after network (tart root needs Softnet)
	local := !strings.Contains(c.Remote, "://") && !regexp.MustCompile(`^[^/]+@[^/:]+:`).MatchString(c.Remote) ||
		strings.HasPrefix(c.Remote, "file://")
	// The guest has no network, so the host fetches: oci_network none, or a
	// runner binding, whose boxes get its network only when the daemon says
	// so (jobNet): one config serves both, so its checkouts come from the host.
	noNet := c.VM == "oci" && (c.OCINetwork == "none" || c.JITRunnerDir != "")
	switch m["fetch"] {
	case "", "auto":
		c.HostFetch = local || noNet
	case "host":
		c.HostFetch = true
	case "guest":
		if local {
			bad = append(bad, "fetch=guest needs a remote the guest can reach, not a local path")
		}
		if noNet {
			bad = append(bad, "fetch=guest needs a network: oci_network is none (a runner binding's boxes get one only when acquired with net on or a jit_config, so the host fetches)")
		}
	default:
		bad = append(bad, "fetch is auto, guest or host")
	}
	if u, err := url.Parse(c.Remote); err == nil && u.User != nil && !c.HostFetch {
		bad = append(bad, "the remote carries credentials: a guest gets none (use fetch=host)")
	}
	if len(bad) > 0 {
		return c, refuse("bad_request", "config: %s", strings.Join(bad, "; "))
	}
	return c, nil
}

func (c config) netName() string {
	if c.VM == "incus" {
		if c.Softnet {
			return "incus profile " + c.IncusProfile + " (port isolation and an ACL checked before boot)"
		}
		return "incus profile " + c.IncusProfile + " (unchecked: network=nat)"
	}
	if c.VM == "oci" && c.Softnet {
		if c.OCINetwork == "none" {
			return "none (no network)"
		}
		if c.JITRunnerDir != "" {
			return c.OCINetwork + " (filtered by its owner) for GitHub runners and boxes acquired with net on (a trusted host's); none for the rest"
		}
		return c.OCINetwork + " (filtered by its owner); none for a box acquired with net off"
	}
	return c.vmNetName()
}

// jobNet is the config a box's VM is made with, from what its acquire
// brought: a jit_config, and net ("on", "off" or unsaid). On oci a box
// acquired with net off gets network none and a host-fetched checkout;
// one that brought a jit_config, or net on
// (the daemon sends it only for a trusted host's binding: SPEC 20 "Boxes",
// Network), gets the binding's network. Unsaid (an older daemon, or a
// conformance run) keeps the rule before net existed: on a runner binding
// only a runner box gets the network (zm-eakh.12.24.6 review), elsewhere
// every box does. Any jit_config, guarded or not: jit_guard is the caller's
// own (the daemon checks only its shape), so requiring it would stop no
// renter and cut off a renter's own runner (SPEC 20 "Boxes").
func (c config) jobNet(jit bool, net string) config {
	if c.VM != "oci" {
		return c
	}
	if net == "off" || net == "" && c.JITRunnerDir != "" && !jit {
		c.OCINetwork, c.OCIDNS = "none", nil // and no resolver: oci_dns is refused with none
		c.HostFetch = true                   // the guest cannot fetch for itself
	}
	return c
}

// boxNet is acquire's answer net for box r: "off" when its VM was made with
// no network (oci network none), else "on" (Tart's Softnet and Incus's fenced
// NIC reach the internet).
func (c config) boxNet(r record) string {
	if c.VM == "oci" && c.jobNet(r.JIT, r.Net).OCINetwork == "none" {
		return "off"
	}
	return "on"
}

func (c config) vmNetName() string {
	if c.Softnet {
		return "softnet"
	}
	return "nat (unsafe: the guest reaches the LAN)"
}

// isolation is describe's class: "ephemeral" only with the layer's isolated
// network on (Softnet on Tart, the checked fenced NIC on Incus, none or an
// owner-filtered network on OCI). network=nat reaches the host and its LAN,
// so it is "none": zm then refuses it as a box and treats a run as unisolated.
func (c config) isolation() string {
	if c.Softnet {
		return "ephemeral"
	}
	return "none"
}

func (c config) slots(os string) int {
	if os == "darwin" {
		return c.MacSlots
	}
	return c.LinuxSlots
}

type refusal struct {
	code, msg string
	data      any // the answer's error data, when there is any (protocol.Busy)
}

func (e refusal) Error() string { return e.code + ": " + e.msg }

func refuse(code, format string, a ...any) error {
	return refusal{code: code, msg: fmt.Sprintf(format, a...)}
}

// fullNow is an acquire's unavailable answer when the host is only full for
// now (protocol.ReasonNoRoom): msg the answer's words, why what is full,
// retryS the admission controller's retry time (0: none given).
func fullNow(msg, why string, retryS int) error {
	return refusal{code: "unavailable", msg: msg, data: protocol.Busy{Reason: protocol.ReasonNoRoom, RetryS: max(retryS, 0), Why: protocol.RefusalReason(why)}}
}
