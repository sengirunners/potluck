package vmhost

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

func (d *drv) acquire(raw json.RawMessage) (any, error) {
	var a struct {
		Database  *protocol.Database `json:"database"`
		RequestID string             `json:"request_id"`
		TTL       *int               `json:"ttl_s"`
		Ref       string             `json:"ref"`
		Commit    string             `json:"commit"`
		Script    string             `json:"script"`
		Args      []string           `json:"args"`
		Image     *string            `json:"image"`
		JITConfig *string            `json:"jit_config"`
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
	if err := d.checkDatabase(a.Database, image, a.Persist, a.JITConfig != nil, a.Volumes); err != nil {
		return nil, err
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
				if !sameDatabase(o.Database, a.Database) {
					return refuse("bad_request", "request_id was used with a different database")
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
		if a.Database == nil && d.warmServes(wa) { // warm.go
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
			return refuse("unavailable", "%s (%d in use)", NoFreeSlot(os), d.cfg.slots(os))
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
			Database: a.Database, State: stStarting, OS: os, VM: d.st.vmPrefixNew() + id, Image: image, Remote: d.cfg.Remote,
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
	if r.Database != nil && r.State != stReady {
		return nil, refuse("conflict", "database box ended before it became ready")
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
	if c.SoftnetPath != "" {
		m["softnet_path"] = c.SoftnetPath
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

// sameJITRun says whether two acquires guarded their runner to the same run
// (or both to none).
func sameJITRun(a, b *protocol.JITRun) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
