package vmhost

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

const (
	name          = "tart"
	version       = "0.1.0"
	costUnit      = "slot_min"
	defaultTTL    = 600
	flagSupervise = "--supervise"
	// flagMeasure re-executes the driver, detached, to measure the volumes of
	// a box whose supervisor died (volume.go), outside the state lock.
	flagMeasure = "--measure"
)

// Main is potluck-driver-tart (and zm-run-tart, its old name): one SPEC 11 envelope on stdin, one answer on stdout,
// exit 0; or, re-executed by acquire, a run's supervisor; or the owner's `host set` (hostset.go).
func Main(argv []string) int {
	if len(argv) == 3 && argv[0] == flagSupervise {
		return supervise(argv[1], argv[2])
	}
	if len(argv) == 3 && argv[0] == flagMeasure {
		return measureLeft(argv[1], argv[2])
	}
	if len(argv) == 2 && argv[0] == flagShelfFill {
		return shelfFiller(argv[1], os.Stdin, os.Stderr)
	}
	if len(argv) == 3 && argv[0] == flagHold {
		return hold(argv[1], argv[2])
	}
	if len(argv) > 0 && argv[0] == "host" { // hostset.go: the owner's override
		return hostCmd(argv[1:], os.Stdin, os.Stdout, os.Stderr)
	}
	if code, done := usageOnly(argv, os.Stdin, os.Stdout, os.Stderr); done {
		return code
	}
	in, _ := io.ReadAll(os.Stdin)
	var req struct {
		Verb   string            `json:"verb"`
		ID     string            `json:"id"`
		Args   json.RawMessage   `json:"args"`
		Config map[string]string `json:"config"`
	}
	var res any
	err := json.Unmarshal(in, &req)
	if err != nil {
		err = refuse("bad_request", "invalid request: %v", err)
	} else {
		res, err = dispatch(req.Verb, req.ID, req.Args, req.Config)
	}
	out := map[string]any{"ok": true, "result": res}
	if err != nil {
		var e refusal
		if !errors.As(err, &e) {
			e = refusal{code: "internal", msg: err.Error()}
		}
		ee := map[string]any{"code": e.code, "message": e.msg}
		if e.data != nil {
			ee["data"] = e.data
		}
		out = map[string]any{"ok": false, "error": ee}
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
	return 0
}

type drv struct {
	cfg           config
	st            store
	vm            vmLayer
	budget        hostBudget
	budgetLoaded  bool
	nativeRuntime bool
	engine        *dockerIdentity
	budgetErr     error
}

// free counts the open slots for guests of os. Live records of this state
// directory hold slots from clone to delete; for macOS, any other running
// macOS VM on the host counts too (Apple's cap is per host). Lock held.
func (d *drv) free(os string) (int, error) {
	budget, err := d.requestedHostBudget()
	if err != nil {
		return 0, err
	}
	usedCPU, usedMemory := 0, 0
	used, ours := 0, map[string]bool{}
	for _, r := range d.st.all() {
		counts := r.live()
		if !counts && r.HostBudget != nil {
			name := hostParentName(d.st.dir, r.VM, r.Starts)
			counts = !(&ownedHostParent{path: filepath.Join(hostCgroupRoot, "potluck.slice", name)}).empty()
		}
		if counts && (d.cfg.HostCPULimit > 0 || d.cfg.HostMemoryLimit > 0) {
			b := r.HostBudget
			if b == nil {
				if ociKataRuntime(r.Config["oci_runtime"]) || r.Config["oci_host_profile"] != "" {
					return 0, fmt.Errorf("a live bounded OCI run has no immutable host allocation")
				}
				cpu, _ := strconv.Atoi(r.Config["cpu"])
				mem, _ := strconv.Atoi(r.Config["memory"])
				b = &hostBudget{cpu, mem}
			}
			if b.CPU <= 0 || b.MemoryMiB <= 0 {
				return 0, fmt.Errorf("a live run has no verified host allocation")
			}
			usedCPU += b.CPU
			usedMemory += b.MemoryMiB
		}
		if counts && r.OS == os {
			used++
			ours[r.VM] = true
		}
	}
	if os == "darwin" {
		vs, err := d.vm.List()
		if err != nil {
			return 0, err
		}
		for _, v := range vs {
			if v.Running && v.Local && !ours[v.Name] {
				if o, err := d.vm.OSOf(v.Name); err == nil && o == "darwin" {
					used++
				}
			}
		}
	}
	free := max(0, d.cfg.slots(os)-used)
	if d.cfg.HostCPULimit > 0 {
		if budget.CPU <= 0 {
			return 0, fmt.Errorf("missing host CPU budget")
		}
		free = min(free, max(0, (d.cfg.HostCPULimit-usedCPU)/budget.CPU))
	}
	if d.cfg.HostMemoryLimit > 0 {
		if budget.MemoryMiB <= 0 {
			return 0, fmt.Errorf("missing host memory budget")
		}
		free = min(free, max(0, (d.cfg.HostMemoryLimit-usedMemory)/budget.MemoryMiB))
	}
	return free, nil
}

func (d *drv) describe() map[string]any {
	os, err := d.vm.OSOf(d.cfg.Image)
	verbs := []string{"describe", "acquire", "status", "release", "list", "renew", protocol.VerbProgress, protocol.VerbCancel} // progress.go
	if d.cfg.Kind == "box" {
		verbs = append(verbs, "exec", "spawn", "output", "signal", "port") // procs.go
		if d.ptyVerbs() {
			verbs = append(verbs, "input") // pty.go
		}
		if d.persists() {
			verbs = append(verbs, "stop", "start") // persist.go
		}
		if boxSecretsServed(os) {
			verbs = append(verbs, "secrets") // secrets.go
		}
	}
	out := map[string]any{"potluck": "potluck/1", "zm": "zm/v0", "name": name, "kind": d.cfg.Kind, "version": version,
		"verbs":     verbs,
		"available": true, "isolation": d.cfg.isolation(), "credentials": false,
		"cost": map[string]any{"unit": costUnit, "amount": 0}, "reports_spend": true,
		"ttl_max_s": d.cfg.TTLMax, "network": d.cfg.netName()}
	why := ""
	if err != nil {
		why = err.Error()
	} else {
		var free int
		_ = d.st.locked(func() error {
			free, err = d.free(os)
			n, _ := d.warmLive() // an acquire takes a warm box, or its slot (warm.go)
			free += n
			return err
		})
		out["slots"] = map[string]any{"os": os, "total": d.cfg.slots(os), "free": free}
		if d.cfg.Kind == "box" {
			out["size"] = map[string]any{"name": d.cfg.Size, "os": os, "cpu": d.cfg.CPU, "memory_mb": d.cfg.Memory}
		}
		switch {
		case err != nil:
			why = err.Error()
		case free == 0:
			why = fmt.Sprintf("no free %s slot (%d in use)", os, d.cfg.slots(os))
		}
	}
	h := d.cfg.host() // hostmode.go
	out["host"] = d.hostDescribe(h)
	if why == "" {
		why = h.refusal()
	}
	caps := []string{"clean-checkout", "net:" + strings.Fields(d.cfg.netName())[0], protocol.CapAcquireProgress} // progress.go
	if d.cfg.Kind == "box" && d.cfg.JITRunnerDir != "" {
		// jit_guard: the runner stops a job the broker would not serve
		caps = append(caps, "github-jit", "github-jit-guard", protocol.CapJITGuardPrivate)
		if boxSecretsServed(os) {
			// jit_guard.run: the hook holds the job for the door's ready
			// marker, a box secret, so only where box secrets are served.
			caps = append(caps, protocol.CapJITGuardRun)
		}
	}
	if d.cfg.Kind == "box" {
		caps = append(caps, "base64") // exec's encoding (exec.go): every box serves it
		if boxSecretsServed(os) {
			// exec's secrets and the secrets verb (exec.go, secrets.go): only
			// where the guest has a memory filesystem to put them in.
			caps = append(caps, "secrets", "box-secrets")
		}
		if d.ptyVerbs() {
			caps = append(caps, "pty") // spawn's pty (pty.go)
		}
	}
	if d.cfg.Kind == "box" && d.cfg.VM == "oci" {
		caps = append(caps, "net-off") // acquire's net (jobNet): off makes the box with network none
		if d.cfg.Root {
			caps = append(caps, "root") // sudo works in the box (oci_root.go)
		}
		if d.cfg.Docker {
			caps = append(caps, "docker") // rootless docker run works in the box
		}
	}
	if d.cfg.Kind == "box" && d.cfg.VM != "oci" && d.cfg.Root {
		// Tart root=on (oci_root.go parseTartRoot): the image's sudo, kept by
		// the owner's word, on a macOS guest only.
		switch {
		case err != nil: // why says the image is missing
		case os == tartRootOS:
			caps = append(caps, "root")
		case why == "":
			why = "root=on is for macOS images on tart: this image is " + os + ", whose root would hold NET_ADMIN and NET_RAW in the VM"
		}
	}
	if d.persists() {
		caps = append(caps, "persist") // acquire's persist, verbs stop and start (persist.go)
		out["disks"] = d.disks()
	}
	if d.volumes() {
		caps = append(caps, "volumes", protocol.CapVolumesSeal, protocol.CapVolumesKeep) // acquire's volumes (volume.go), mode build and release's seal (volume_seal.go) and keep (volume_keep.go)
		out["volumes"] = d.volDescribe()
	}
	if s, ok := d.shelfDescribe(); ok {
		out["shelf"] = s             // shelf.go
		caps = append(caps, "shelf") // every box handed out from now on has it read-only (cacheArgs; warm.go replaces warm boxes without it)
	}
	if d.cfg.Kind == "box" && d.cfg.Warm > 0 {
		out["warm"] = d.warmDescribe() // warm.go
	}
	if d.cfg.JITRunnerDir != "" {
		if rs := d.st.runnerStatuses(d.cfg.runnerImages()); len(rs) > 0 {
			out["runners"] = rs // runner_status.go: one per image
		}
	}
	if os != "" {
		caps = append(caps, "os:"+os)
	}
	out["capabilities"] = caps
	if why != "" {
		out["available"], out["why"] = false, why
	}
	return out
}

var (
	scriptRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	handleRe = regexp.MustCompile(`^(db|run|box):([^/]*)/(.*)$`)
	idRe     = regexp.MustCompile(`^r[0-9a-f]{12}$`)
)

func (d *drv) acquire(raw json.RawMessage) (any, error) {
	var a struct {
		RequestID string   `json:"request_id"`
		TTL       *int     `json:"ttl_s"`
		Ref       string   `json:"ref"`
		Commit    string   `json:"commit"`
		Script    string   `json:"script"`
		Args      []string `json:"args"`
		Image     *string  `json:"image"`
		JITConfig *string  `json:"jit_config"`
		JITGuard  *struct {
			Repo          string           `json:"repo"`
			Events        []string         `json:"events"`
			OpenIfPrivate bool             `json:"open_if_private"`
			Run           *protocol.JITRun `json:"run"`
		} `json:"jit_guard"`
		Net     string `json:"net"`     // a box: "on", "off" or unsaid (jobNet)
		Persist bool   `json:"persist"` // a box: keep its disk across stop and start (persist.go)
		// Volumes (a box's: volume.go) are its owner's: volume_owner, which
		// the daemon sets to the holder's identity, else actor.
		Volumes     []volReq `json:"volumes"`
		VolumeOwner string   `json:"volume_owner"`
		// VolumeProject is the owner's project, which the daemon (or the
		// host relaying for another's authority) sets: its volumes share a cap.
		VolumeProject string `json:"volume_project"`
		Actor         string `json:"actor"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, refuse("bad_request", "bad args: %v", err)
	}
	image := d.cfg.Image
	if d.cfg.Kind == "box" {
		if a.Image != nil {
			var ok bool
			if image, ok = d.cfg.pinned(*a.Image); !ok {
				return nil, refuse("bad_request", "image %q is not one of this binding's images", *a.Image)
			}
		}
		switch {
		case a.Commit != "" && !commitRe.MatchString(a.Commit):
			return nil, refuse("bad_request", "commit %q is not a full commit id", a.Commit)
		case a.Commit != "" && d.cfg.Remote == "":
			return nil, refuse("bad_request", "this box binding has no remote to check commit out from")
		case a.Script != "" || len(a.Args) > 0:
			return nil, refuse("bad_request", "a box takes no script: send commands with exec")
		case a.JITConfig != nil && d.cfg.JITRunnerDir == "":
			return nil, refuse("bad_request", "this box binding serves no GitHub runner (no runner_dir)")
		case a.JITConfig != nil && (len(*a.JITConfig) > jitMax || !jitRe.MatchString(*a.JITConfig)):
			return nil, refuse("bad_request", "jit_config is not an encoded runner config")
		case a.JITGuard != nil && (a.JITConfig == nil || !ghRepoRe.MatchString(a.JITGuard.Repo) || !jitEvents(a.JITGuard.Events)):
			return nil, refuse("bad_request", "jit_guard names a repo (owner/name) and event names, for a jit_config")
		case a.JITGuard != nil && a.JITGuard.OpenIfPrivate && len(a.JITGuard.Events) == 0:
			return nil, refuse("bad_request", "jit_guard.open_if_private widens its events: it needs events")
		case a.JITGuard != nil && a.JITGuard.Run != nil && a.JITGuard.Run.Check(a.JITGuard.Repo) != nil:
			return nil, refuse("bad_request", "%v", protocol.ErrJITRun)
		case a.Net != "" && a.Net != "on" && a.Net != "off":
			return nil, refuse("bad_request", "net is on or off")
		case a.Net == "off" && d.cfg.VM != "oci":
			return nil, refuse("bad_request", "this binding's VM layer (%s) cannot make a box without a network (no net-off capability)", d.cfg.VM)
		case a.Persist && !d.persists():
			return nil, refuse("bad_request", "this binding keeps no disks (no persist capability: persist_max 0, or the %s layer cannot)", d.cfg.VM)
		case a.Persist && a.JITConfig != nil:
			return nil, refuse("bad_request", "a GitHub runner box is never persistent")
		case len(a.Volumes) > 0 && !d.volumes():
			return nil, refuse("bad_request", "this binding mounts no volumes (no volumes capability: volume_max_gb 0, or the %s layer cannot)", d.cfg.VM)
		case len(a.Volumes) > 0 && (a.Persist || a.JITConfig != nil):
			return nil, refuse("bad_request", "a box with volumes is neither persistent nor a GitHub runner's")
		case len(a.Volumes) > 0 && cmp.Or(a.VolumeOwner, a.Actor) == "":
			return nil, refuse("bad_request", "volumes need their owner: volume_owner (or actor)")
		}
		if err := checkVolumes(a.Volumes); err != nil {
			return nil, err
		}
		if len(a.Volumes) > 0 {
			d.remeasure(cmp.Or(a.VolumeOwner, a.Actor), a.Volumes) // volume.go
		}
	} else if len(a.Volumes) > 0 {
		return nil, refuse("bad_request", "volumes are a box's")
	} else if a.Net != "" || a.Persist {
		return nil, refuse("bad_request", "net and persist are a box's")
	} else if a.JITConfig != nil || a.JITGuard != nil {
		return nil, refuse("bad_request", "jit_config is a box's")
	} else if a.Image != nil {
		return nil, refuse("bad_request", "image is a box's choice")
	}
	ttl := min(defaultTTL, d.cfg.TTLMax)
	if a.TTL != nil {
		ttl = *a.TTL
	}
	switch {
	case a.RequestID == "":
		return nil, refuse("bad_request", "no request_id")
	case ttl <= 0 || ttl > d.cfg.TTLMax:
		return nil, refuse("bad_request", "ttl_s %d is outside 1..%d", ttl, d.cfg.TTLMax)
	case d.cfg.Kind == "run" && !commitRe.MatchString(a.Commit):
		return nil, refuse("bad_request", "commit %q is not a full commit id", a.Commit)
	case d.cfg.Kind == "run" && (!scriptRe.MatchString(a.Script) || strings.Contains(a.Script, "..")):
		return nil, refuse("bad_request", "script %q is a name, never a shell string or a path", a.Script)
	}

	var guardRepo string
	var guardEvents []string
	var guardRun *protocol.JITRun
	var guardOpen bool
	if a.JITGuard != nil {
		guardRepo, guardEvents, guardRun = strings.ToLower(a.JITGuard.Repo), a.JITGuard.Events, a.JITGuard.Run
		guardOpen = a.JITGuard.OpenIfPrivate
	}
	var r record
	var existing bool
	err := d.st.locked(func() error {
		for _, o := range d.st.all() {
			if o.Target == d.cfg.Target && o.Key == a.RequestID {
				if o.State == stFailed {
					d.st.remove(o)
					continue
				}
				if o.JIT != (a.JITConfig != nil) || o.JITRepo != guardRepo || !slices.Equal(o.JITEvents, guardEvents) || o.JITOpenIfPrivate != guardOpen || !sameJITRun(o.JITRun, guardRun) {
					return refuse("bad_request", "request_id %q was used with a different jit_config", a.RequestID)
				}
				if o.Net != a.Net || o.Persist != a.Persist {
					return refuse("bad_request", "request_id %q was used with a different net or persist", a.RequestID)
				}
				if !sameVolumes(o.Volumes, a.Volumes) || len(a.Volumes) > 0 && o.VolOwner != cmp.Or(a.VolumeOwner, a.Actor) {
					return refuse("bad_request", "request_id %q was used with different volumes", a.RequestID)
				}
				d.st.stamp(&o)
				r, existing = o, true
				return nil
			}
		}
		if ok, why := d.cfg.admitsNow(); !ok { // hostmode.go
			return refuse("unavailable", "%s", why)
		}
		os, err := d.vm.OSOf(image)
		if err != nil {
			return refuse("unavailable", "%v", err)
		}
		if d.cfg.Root && d.cfg.VM != "oci" && os != tartRootOS { // oci_root.go parseTartRoot
			return refuse("bad_request", "root=on is for macOS images on tart: %s is %s, whose root would hold NET_ADMIN and NET_RAW in the VM", image, os)
		}
		if guardRun != nil && !boxSecretsServed(os) { // describe lists no github-jit-guard-run
			return refuse("bad_request", "a %s box cannot hold a job for its secrets: it serves no box secrets (no %s capability)", os, protocol.CapJITGuardRun)
		}
		wa := warmAcquire{image: image, jit: a.JITConfig != nil, persist: a.Persist, volumes: len(a.Volumes) > 0, net: a.Net, ref: a.Ref, cmt: a.Commit}
		if d.warmServes(wa) { // warm.go
			if w, ok := d.takeWarm(a.RequestID, ttl, wa); ok {
				r = w
				d.fillWarm()
				return nil
			}
		}
		free, err := d.free(os)
		if err != nil {
			return refuse("unavailable", "%v", err)
		}
		if free == 0 && d.preemptWarm(os) { // warm boxes are the first reclaimed
			free, err = d.free(os)
		}
		if err != nil {
			return refuse("unavailable", "%v", err)
		}
		if free == 0 {
			return refuse("unavailable", "no free %s slot (%d in use)", os, d.cfg.slots(os))
		}
		if n := d.kept(); a.Persist && n >= d.cfg.PersistMax {
			return refuse("unavailable", "this host keeps %d disks already (persist_max %d): rm a stopped sandbox, or create one without persist", n, d.cfg.PersistMax)
		}
		if a.Persist {
			if err := d.diskRoom(); err != nil { // persist.go
				return err
			}
		}
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		id := "r" + hex.EncodeToString(b)
		var vols []volUse
		if len(a.Volumes) > 0 {
			if vols, err = d.takeVolumes(id, cmp.Or(a.VolumeOwner, a.Actor), a.VolumeProject, a.Volumes); err != nil { // volume.go
				return err
			}
		}
		r = record{Handle: d.cfg.Kind + ":" + d.cfg.Target + "/" + id, Kind: d.cfg.Kind, Target: d.cfg.Target, Key: a.RequestID, ID: id,
			State: stStarting, OS: os, VM: d.st.vmPrefixNew() + id, Image: image, Remote: d.cfg.Remote,
			Ref: a.Ref, Commit: a.Commit, Script: a.Script, Args: a.Args, TTL: ttl, Start: time.Now().UTC(),
			Log: filepath.Join(d.st.dir, "logs", id+".log"), Config: d.rawConfig(), Net: a.Net, Volumes: vols}
		if d.engine != nil {
			snapshot := d.engine.allocation()
			r.Engine = &snapshot
		}
		if d.cfg.OCIHostProfile != "" {
			b, err := d.requestedHostBudget()
			if err != nil {
				return err
			}
			r.HostBudget = &b
		}
		if len(vols) > 0 {
			r.VolOwner, r.VolProject = cmp.Or(a.VolumeOwner, a.Actor), a.VolumeProject
		}
		if a.Persist {
			r.Persist, r.KeepS, r.Seg = true, d.cfg.PersistKeepS, r.Start
		}
		if d.cfg.Kind == "box" {
			r.Digest = digestOf(image)
		}
		// Before the first save: the supervisor reads the record, and a JIT
		// box read as JIT=false would start without its runner or network.
		jit := ""
		if a.JITConfig != nil {
			r.JIT, jit = true, *a.JITConfig
			r.JITRepo, r.JITEvents, r.JITRun, r.JITOpenIfPrivate = guardRepo, guardEvents, guardRun, guardOpen
		}
		if err := d.st.save(r); err != nil {
			return err
		}
		pid, err := d.spawnSupervisor(id, jit)
		if err != nil {
			d.st.remove(r)
			return err
		}
		r.Supervisor = pid
		return d.st.save(r)
	})
	if err != nil {
		return nil, err
	}
	if !existing || r.State == stStarting {
		if r, err = d.waitStarted(r.ID); err != nil {
			return nil, err
		}
	}
	return d.answer(r), nil
}

// rawConfig is what a supervisor needs to rebuild the config.
func (d *drv) rawConfig() map[string]string {
	c := d.cfg
	m := map[string]string{"target": c.Target, "kind": c.Kind, "vm": c.VM, "image": c.Image, "tart": c.Tart, "state_dir": c.StateDir,
		"remote": c.Remote, "scripts_dir": c.ScriptsDir, "size": c.Size, "images": strings.Join(c.Images, ","), "runner_dir": c.JITRunnerDir, "fetch": "guest", "network": "softnet",
		"cpu": fmt.Sprint(c.CPU), "memory": fmt.Sprint(c.Memory), "boot_timeout_s": fmt.Sprint(c.BootTimeout),
		"ttl_max_s": fmt.Sprint(c.TTLMax), "reclaim_idle_s": fmt.Sprint(c.Reclaim.IdleS), "window": c.Reclaim.Window, "windows": c.Reclaim.Windows, "drain_deadline_s": fmt.Sprint(c.DrainDeadline), "host_owner_uid": fmt.Sprint(c.HostOwner),
		"persist_max": fmt.Sprint(c.PersistMax), "persist_keep_s": fmt.Sprint(c.PersistKeepS), "persist_min_free_gb": fmt.Sprint(c.PersistMinFreeGB),
		"volume_max_gb": fmt.Sprint(c.VolumeMaxGB), "volume_keep_s": fmt.Sprint(c.VolumeKeepS), "volume_max_mb": fmt.Sprint(c.VolumeMaxMB), "volume_check_s": fmt.Sprint(c.VolumeCheckS),
		"volume_project_max_gb": fmt.Sprint(c.VolumeProjectMaxGB), "volume_binding_max_gb": fmt.Sprint(c.VolumeBindingMaxGB), "warm": fmt.Sprint(c.Warm)}
	c.Shelf.raw(m)
	if c.HostFetch {
		m["fetch"] = "host"
	}
	if !c.Softnet {
		m["network"] = "nat"
	}
	if c.AdmissionURL != "" {
		m["admission_url"], m["admission_token_file"], m["admission_ttl_s"] = c.AdmissionURL, c.AdmissionTokenFile, fmt.Sprint(c.AdmissionTTL)
	}
	if c.VM == "incus" {
		m["incus"], m["incus_remote"], m["incus_project"], m["incus_profile"], m["incus_conf"] = c.IncusBin, c.IncusRemote, c.IncusProject, c.IncusProfile, c.IncusConf
	}
	if c.VM == "oci" {
		m["oci_cli"], m["oci_runtime"], m["oci_network"] = c.OCICLI, c.OCIRuntime, c.OCINetwork
		if c.OCIHostProfile != "" {
			m["oci_host_profile"] = c.OCIHostProfile
		}
		if c.HostCPULimit > 0 {
			m["host_cpu_limit"] = fmt.Sprint(c.HostCPULimit)
		}
		if c.HostMemoryLimit > 0 {
			m["host_memory_limit_mb"] = fmt.Sprint(c.HostMemoryLimit)
		}
		if len(c.OCIDNS) > 0 {
			m["oci_dns"] = strings.Join(c.OCIDNS, ",")
		}
		c.rootArgs(m)
	}
	if c.VM != "oci" {
		c.rootArgs(m) // tart's root=on (oci_root.go parseTartRoot)
	}
	return m
}

// spawnSupervisor starts a run's supervisor; jit, if any, goes to it on
// stdin and nowhere else.
func (d *drv) spawnSupervisor(id, jit string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(filepath.Join(d.st.dir, "logs", id+".supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(self, flagSupervise, d.st.dir, id)
	cmd.Stdout, cmd.Stderr = logf, logf // never the driver's stdout: the caller waits for it to close
	cmd.Stdin = strings.NewReader(jit)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// waitStarted waits for the supervisor to start the run (or fail to), or
// for a cancel of its acquire (progress.go, which then ends it).
func (d *drv) waitStarted(id string) (record, error) {
	deadline := time.Now().Add(time.Duration(d.cfg.BootTimeout+15) * time.Second)
	for {
		r, err := d.st.read(id)
		if err != nil {
			return r, err
		}
		switch {
		case r.State == stStarting && d.st.cancelled(r.Target, r.Key):
			return r, errCancelled
		case r.State == stFailed:
			_ = d.st.locked(func() error { d.st.remove(r); return nil })
			return r, refuse("unavailable", "the run did not start: %s", r.Why)
		case r.State != stStarting:
			return r, nil
		case !time.Now().Before(deadline):
			return r, refuse("internal", "the run did not start in time; its supervisor may still make it, and ends it at its lease") // not a refusal: the run may still come up
		}
		time.Sleep(warmPoll)
	}
}

// spend is r's time so far: to its End, else to the instant it was read
// under the lock (store.stamp), never to a later time.Now(): an end stamped
// after the read is then never earlier than what the read answered.
func (d *drv) spend(r record) map[string]any {
	end := r.End
	if end.IsZero() {
		end = r.asOf
	}
	ms := end.Sub(r.Start).Milliseconds()
	if r.Persist { // only its time up: a stopped box is not billed (persist.go)
		ms = r.RanMs
		if !r.Seg.IsZero() && end.After(r.Seg) { // as stoppedAt counts it
			ms += end.Sub(r.Seg).Milliseconds()
		}
	}
	return map[string]any{"unit": costUnit, "amount": float64(ms) / 60000}
}

func (d *drv) answer(r record) map[string]any {
	ttl := int(r.Deadline.Sub(r.RunStart).Seconds() + 0.5)
	out := map[string]any{"handle": r.Handle, "expires_at": r.Deadline.Format(time.RFC3339Nano), "ttl_s": ttl,
		"commit": r.Commit, "log": r.Log, "spend": d.spend(r)}
	if r.Kind == "box" {
		delete(out, "log")
		if r.Commit == "" {
			delete(out, "commit")
		}
		out["size"], out["image_digest"] = d.cfg.Size, r.Digest
		out["net"] = d.cfg.boxNet(r)
		if len(r.Volumes) > 0 {
			out["volumes"] = volAnswer(r) // volume.go
		}
	}
	return out
}

// find loads a handle of this binding: SPEC 20's bad_request and not_found.
func (d *drv) find(handle string) (record, error) {
	m := handleRe.FindStringSubmatch(handle)
	if m == nil || m[1] != d.cfg.Kind || m[2] != d.cfg.Target || m[3] == "" {
		return record{}, refuse("bad_request", "%q is not a handle of this binding", handle)
	}
	if !idRe.MatchString(m[3]) {
		return record{}, refuse("not_found", "no %s", handle)
	}
	r, err := d.st.read(m[3])
	if err != nil || r.Target != d.cfg.Target || r.State == stFailed || r.Warm {
		return record{}, refuse("not_found", "no %s", handle)
	}
	d.vm = r.bindLayer(d.vm)
	return r, nil
}

func (d *drv) status(handle string) (any, error) {
	r, err := d.find(handle)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"handle": r.Handle, "state": r.wireState(), "commit": r.Commit,
		"expires_at": r.Deadline.Format(time.RFC3339Nano), "spend": d.spend(r)}
	if r.Commit == "" {
		delete(out, "commit")
	}
	if r.Exit != nil {
		out["exit"] = *r.Exit
	}
	if r.Why != "" {
		out["why"] = r.Why
	}
	return out, nil
}

func (d *drv) list() any {
	es := []map[string]any{}
	for _, r := range d.st.all() {
		if r.Target == d.cfg.Target && r.State != stFailed && !r.Warm { // a warm box is nobody's handle (warm.go)
			es = append(es, map[string]any{"handle": r.Handle, "request_id": r.Key, "state": r.wireState(),
				"expires_at": r.Deadline.Format(time.RFC3339Nano)})
		}
	}
	return map[string]any{"handles": es}
}

// release ends a box (or run). With seal (volumes-seal), a box that is
// ready and not past its deadline has its build volumes sealed (pruned to
// keep first, when it names any: volume_keep.go), any other release deletes
// them; either way before it answers (volume_seal.go).
func (d *drv) release(handle string, raw json.RawMessage) (any, error) {
	r, err := d.find(handle)
	if err != nil {
		return nil, err
	}
	var a struct {
		Seal bool     `json:"seal"`
		Keep []string `json:"keep"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, refuse("bad_request", "bad args: %v", err)
		}
	}
	if a.Keep != nil {
		if !a.Seal {
			return nil, refuse("bad_request", "keep is for a release with seal")
		}
		if err := protocol.KeepPathsOK(a.Keep); err != nil {
			return nil, refuse("bad_request", "%v", err)
		}
	}
	out := map[string]any{"released": false}
	var end *record
	starting := false
	r, err = d.st.update(r.ID, func(r *record) error {
		switch {
		case r.live(), r.State == stStopped: // a stopped box's kept disk goes too
			out["released"] = true
			starting = r.State == stStarting
			now := time.Now().UTC()
			// Past its deadline the box has expired, even before its supervisor saw it: never sealed.
			r.Seal = a.Seal && r.State == stReady && !now.After(r.Deadline) && r.builds()
			if r.Seal {
				r.Keep = a.Keep
			}
			r.State, r.End = stReleased, now
			end = r
		case r.State == stEnded:
			out["exit"] = *r.Exit
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if end != nil { // the supervisor sees the state too; ending it here makes release synchronous
		collectShelfWants(d.cfg, d.vm, *end, io.Discard, shelfWantsEndFor) // while it still runs (shelf_queue.go)
		d.vm.Destroy(end.VM, end.RunPID)
		if !starting { // a starting box's supervisor may still be making its volumes: they are its to delete (dropBuilds)
			d.endBuilds(*end) // volume_seal.go: sealed or deleted before the answer
		}
	}
	out["spend"] = d.spend(r)
	return out, nil
}

func (d *drv) renew(handle string, raw json.RawMessage) (any, error) {
	r, err := d.find(handle)
	if err != nil {
		return nil, err
	}
	var a struct {
		TTL int `json:"ttl_s"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.TTL <= 0 {
		return nil, refuse("bad_request", "renew needs ttl_s")
	}
	var at time.Time
	_, err = d.st.update(r.ID, func(r *record) error {
		if !r.live() {
			return refuse("bad_request", "%s is %s", r.Handle, r.wireState())
		}
		at = time.Now().UTC().Add(time.Duration(a.TTL) * time.Second)
		if at.After(r.since().Add(time.Duration(d.cfg.TTLMax) * time.Second)) {
			return refuse("bad_request", "past the lifetime cap of %d s since acquire (or start)", d.cfg.TTLMax)
		}
		r.Deadline = at
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"expires_at": at.Format(time.RFC3339Nano)}, nil
}

// sweep ends what a dead supervisor left, deletes this state directory's VMs
// no live record holds, and forgets old records. Lock held.
func (d *drv) sweep() error {
	// Fresh metadata requests also sweep before their first acquire. Establish
	// the same verified local producer first; never mutate a selected context
	// merely because no allocation has initialized this process yet.
	if o, ok := d.vm.(ociCLI); ok && o.engineHost == "" {
		id, err := o.verifyEngine()
		if err != nil {
			return nil
		}
		snapshot := id.allocation()
		o.engineHost, o.engine = id.endpoint, &snapshot
		d.vm = o
	}
	held := map[string]bool{}
	busy := map[string]bool{} // a VM whose supervisor runs: it may be making the box's cow copies still
	for _, r := range d.st.all() {
		if alive(r.Supervisor) {
			busy[r.VM] = true
		}
		switch {
		case r.State == stStopped && time.Now().After(r.Deadline): // the kept disk's time is up
			r.bindLayer(d.vm).Destroy(r.VM, 0)
			r.State, r.End, r.Why = stExpired, time.Now().UTC(), "its kept disk expired"
			_ = d.st.save(r)
		case r.State == stStopped:
			held[r.VM] = true // kept: never an orphan
		case r.live() && r.Persist && r.Supervisor > 0 && !alive(r.Supervisor):
			d.powerOff(r.VM, r.RunPID) // persist.go: the disk is kept, as on any stop
			stoppedAt(&r, "its supervisor died", d.st.lastBeat(r.ID))
			_ = d.st.save(r)
			held[r.VM] = true
		case r.live() && r.Supervisor > 0 && !alive(r.Supervisor):
			r.bindLayer(d.vm).Destroy(r.VM, r.RunPID)
			if len(r.Volumes) > 0 { // what it wrote, measured detached: never under this lock (volume.go)
				d.spawnDetached(flagMeasure, r.ID)
			}
			r.State, r.End, r.Why = stLost, time.Now().UTC(), "its supervisor died"
			if !r.RunStart.IsZero() && time.Now().After(r.Deadline) {
				r.State, r.Exit, r.Why = stExpired, nil, ""
				if r.Kind != "box" {
					code := 124
					r.Exit = &code
				}
			}
			_ = d.st.save(r)
		case r.Warm && !r.live() && (r.State != stFailed || time.Since(r.End) > warmBackoff) && !alive(r.Supervisor):
			d.st.remove(r) // warm.go: an ended warm box was nobody's (a failed one is kept a while: fillWarm's backoff)
		case r.live() || r.State == stFailed && alive(r.Supervisor):
			held[r.VM] = true
		case !r.End.IsZero() && time.Since(r.End) > keepFor || r.State == stFailed && time.Since(r.Start) > time.Hour:
			d.st.remove(r)
		}
		if !r.live() {
			d.st.endProcs(r.ID) // processes end with the box
		}
	}
	d.sweepVolumes()     // volume.go
	d.st.sweepProgress() // progress.go
	vs, err := d.vm.List()
	if err != nil {
		return nil // the layer is missing: describe says so; nothing of ours can be running
	}
	listed := map[string]bool{}
	for _, v := range vs {
		listed[v.Name] = true
		if v.Local && d.st.ownVM(v.Name) && !held[v.Name] {
			d.vm.Destroy(v.Name, 0)
		}
	}
	if v, ok := d.vm.(volumer); ok { // cow copies whose box is gone with no Destroy to delete them (volume.go)
		v.SweepLeaks(func(vm string) bool { return !d.st.ownVM(vm) || held[vm] || busy[vm] || listed[vm] })
	}
	d.sweepShelfQueue() // shelf_queue.go
	d.fillWarm()        // warm.go
	return nil
}

// sameJITRun says whether two acquires guarded their runner to the same run
// (or both to none).
func sameJITRun(a, b *protocol.JITRun) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
