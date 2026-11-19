package vmhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// guestDir is the driver's working directory inside the guest: the checkout
// script empties it and keeps its bundle there.
const guestDir = "/tmp/zm-run"

// srcDir is where record r's checkout lives inside the guest: guestDir/src,
// or, in a box whose cow volume says checkout (a deps layer), that volume's
// path, relative to HOME, so code and the layer's node_modules sit at the
// paths the layer was built at. Every reader and writer of the checkout path
// takes it from here, and resolves it in the guest with baseInHome.
func srcDir(r record) string {
	for _, v := range r.Volumes {
		if v.Checkout {
			return v.Path
		}
	}
	return guestDir + "/src"
}

// baseInHome is the guest shell lines that make $base (a srcDir) absolute:
// a relative one is under HOME, as a volume's mount is (mountPath). An empty
// or absolute $base is left as it is and HOME is not looked at (so a script
// under set -e in a guest with no HOME runs as it did). With no absolute
// HOME it exits 125, never a path under /.
const baseInHome = `case $base in ""|/*) ;; *) h=$HOME; [ -n "$h" ] || h=$(cd / && cd ~ 2>/dev/null && pwd) || h=
case $h in /?*) base=${h%/}/$base ;; *) echo "potluck-driver-tart: no HOME for $base" >&2; exit 125 ;; esac ;; esac
`

// supervise lives as long as one run: it clones and boots the VM, puts the
// checkout in, runs the script, and ends the VM at expires_at, on reclaim, on
// release, or when the script exits. It is detached from zm and the driver,
// so the deadline holds while zm is down. It always deletes the clone.
func supervise(dir, id string) int {
	st := store{dir}
	r, err := st.load(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "supervise:", err)
		return 1
	}
	cfg, err := parseConfig(r.Config)
	if err != nil {
		fail(st, r, err)
		return 1
	}
	cfg = cfg.jobNet(r.JIT, r.Net) // the box's network, from its acquire
	var jit string
	if r.JIT { // from the driver, on stdin: never stored
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<17))
		if jit = strings.TrimSpace(string(b)); len(jit) > jitMax || !jitRe.MatchString(jit) {
			jit = "" // cut short in the handoff: startJIT refuses it
		}
	}
	vm := r.bindLayer(newLayer(cfg))
	logf, err := os.OpenFile(r.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fail(st, r, err)
		return 1
	}
	defer logf.Close()
	s := &sup{st: st, cfg: cfg, vm: vm, id: id, name: r.VM, log: logf, jit: jit, starts: r.Starts}
	code := s.run(r)
	return code
}

type sup struct {
	st           store
	cfg          config
	vm           vmLayer
	id           string
	name         string // the VM
	log          io.Writer
	runPID       int
	held         io.WriteCloser // the deadman guard's stdin
	jit          string         // a box's JIT runner config, held in memory only
	adm          *held          // the host admission controller's lease, if it has one
	host         *ociHostLease  // one admitted, finite host budget for a standalone OCI VM
	stop         func()         // ends the lease's keepAlive
	starts       int            // the record's Starts this supervisor serves (persist.go): a later start's is another's
	lastBeat     time.Time      // when beat last marked this supervisor alive
	measureFails int            // volume measurements failed in a row (volume.go)
	keyDone      bool           // the record's acquire recorded ready, or kept no progress: no cancel ends it (progress.go)

	runnerFetch <-chan hostFetch // the host's copy of the pinned GitHub runner, fetched while the VM boots (runner_refresh.go)
	jobs        *jobWatch        // the GitHub runner's output, watched for its job (drain.go); nil without a runner
}

// stillStarting says whether the record is still ours to start (a release
// while starting ends it).
func (s *sup) stillStarting() bool {
	r, err := s.st.load(s.id)
	return err == nil && r.State == stStarting && r.Starts == s.starts
}

// finish ends the VM as the record says: a stopped persistent box is powered
// off and kept (persist.go), any other deleted. A box another start took
// over is left to that start's supervisor.
func (s *sup) finish() {
	r, err := s.st.load(s.id)
	if err == nil && r.Persist && r.Starts != s.starts {
		return
	}
	if err == nil { // while the box still runs (shelf_queue.go)
		collectShelfWants(s.cfg, s.vm, r, s.log, shelfWantsEndFor)
	}
	if k, ok := s.vm.(keeper); ok && err == nil && r.Persist && r.State == stStopped {
		k.PowerOff(s.name, s.runPID)
		return
	}
	s.vm.Destroy(s.name, s.runPID)
}

// stopKept stops a live persistent box, keeping its disk (finish powers it off).
func (s *sup) stopKept(why string) {
	_, _ = s.st.update(s.id, func(r *record) error {
		if r.live() && r.Starts == s.starts {
			stopped(r, why)
		}
		return nil
	})
}

func (s *sup) run(r record) int {
	defer func() { s.finish(); s.dropBuilds(r); s.releaseRoom(); s.finished(); s.measureVolumes(r) }()
	if r.Warm { // warm.go: a warm box asks for room when it is taken
	} else if err := s.takeRoom(r); err != nil {
		fail(s.st, r, err)
		return 1
	}
	bootBy := time.Now().Add(time.Duration(s.cfg.BootTimeout) * time.Second)
	if s.startsRunner(r) {
		s.runnerFetch = prefetchRunner(s.st.dir, r.OS)
	}
	k, keep := s.vm.(keeper)
	keep = keep && r.Persist
	if !s.phase(r, "cloning") {
		return 1
	}
	var err error
	switch {
	case r.Starts > 0: // a start: the kept VM is there
	case keep:
		err = k.CloneKept(r.Image, r.VM, s.cfg.CPU, s.cfg.Memory)
	case len(r.Volumes) > 0: // volume.go: acquire refuses volumes where the layer mounts none
		v, ok := s.vm.(volumer)
		if !ok {
			err = errors.New("this VM layer mounts no volumes")
			break
		}
		err = v.CloneVolumes(r.Image, r.VM, s.cfg.CPU, s.cfg.Memory, r.mounts())
	default:
		err = s.vm.Clone(r.Image, r.VM, s.cfg.CPU, s.cfg.Memory)
	}
	if err != nil {
		fail(s.st, r, err)
		return 1
	}
	if err := s.adm.gone(); err != nil { // a slow clone outlived the lease: no boot on room not held
		fail(s.st, r, err)
		return 1
	}
	if err := s.refusesNew(); err != nil { // the host drained or closed since acquire (drain.go)
		fail(s.st, r, err)
		return 1
	}
	if !s.phase(r, "booting") {
		return 1
	}
	boot := s.vm.Boot
	if keep {
		boot = k.BootKept
	}
	booted, held, err := boot(r.VM, s.cfg.Softnet, s.log)
	if err != nil {
		fail(s.st, r, err)
		return 1
	}
	s.runPID = booted.Process.Pid
	s.holdAwake()
	s.held = held // never closed: it closes when this process exits, after the deferred destroy
	exited := make(chan struct{})
	go func() { _ = booted.Wait(); close(exited) }()
	if _, err := s.st.update(s.id, func(r *record) error { r.RunPID = s.runPID; return nil }); err != nil {
		return 1
	}
	if !s.stillStarting() {
		return 0
	}
	if err := waitReady(s.vm, r.VM, bootBy, exited); err != nil {
		if errors.Is(err, errBootExited) { // exited is closed: Wait has set ProcessState
			err = diagnoseBoot(s.vm, r.Log, booted.ProcessState.ExitCode(), s.cfg.Softnet)
		}
		if h, ok := s.vm.(keptBootHinter); ok && keep {
			err = h.KeptBootErr(err) // oci_persist.go: a runsc that refuses the root's annotation
		}
		fail(s.st, r, err)
		return 1
	}
	if v, ok := s.vm.(volumer); ok && len(r.Volumes) > 0 { // volume.go: the guest's user writes its rw and cow mounts
		if err := v.OwnMounts(r.Image, r.VM, r.mounts()); err != nil {
			fail(s.st, r, err)
			return 1
		}
	}
	if lc, ok := s.vm.(loginCloser); ok { // before anything is answered or run
		ctx, cancel := context.WithDeadline(context.Background(), bootBy.Add(10*time.Second))
		err := s.guest(ctx, nil, "closing the guest's logins", lc.CloseLogin()...)
		cancel()
		if err != nil {
			fail(s.st, r, err)
			return 1
		}
	}
	if !s.stillStarting() {
		return 0
	}
	if err := s.adm.gone(); err != nil {
		fail(s.st, r, err)
		return 1
	}
	if err := s.refusesNew(); err != nil {
		fail(s.st, r, err)
		return 1
	}
	if (r.Kind != "box" || r.Commit != "") && !s.phase(r, "checkout") {
		return 1
	}
	if r.Kind == "box" {
		return s.box(r, bootBy, exited)
	}
	job, err := s.start(r, bootBy)
	if err != nil {
		fail(s.st, r, err)
		return 1
	}
	done := make(chan int, 1)
	go func() {
		_ = job.Wait()
		done <- job.ProcessState.ExitCode()
	}()
	cancelled := false
	r, err = s.st.update(s.id, func(r *record) error {
		if r.State != stStarting {
			return errors.New("released while starting")
		}
		if cancelled = s.cancelledAtReady(r); cancelled {
			return nil
		}
		now := time.Now().UTC()
		r.State, r.RunStart = stRunning, now
		r.Deadline = now.Add(time.Duration(r.TTL) * time.Second)
		if lim := r.Start.Add(time.Duration(s.cfg.TTLMax) * time.Second); r.Deadline.After(lim) {
			r.Deadline = lim
		}
		return nil
	})
	if err != nil || cancelled {
		return 0
	}
	return s.watch(done, exited)
}

// start begins the run in the guest: the script on a clean checkout of
// r.Commit.
func (s *sup) start(r record, bootBy time.Time) (*exec.Cmd, error) {
	ctx, cancel := context.WithDeadline(context.Background(), bootBy.Add(10*time.Second))
	defer cancel()
	if err := s.checkout(ctx, r); err != nil {
		return nil, err
	}
	if r.Starts == 0 && s.st.cancelled(r.Target, r.Key) { // progress.go: the script never starts for a cancelled acquire
		return nil, errAcquireCancelled
	}
	script := r.Script
	if s.cfg.ScriptsDir != "" {
		script = path.Join(s.cfg.ScriptsDir, r.Script)
	}
	// A fixed shell program; the script and its args are positional, never parsed.
	const launch = `base=$1; shift
` + baseInHome + `cd "$base" || exit 125; [ -f "$1" ] || { echo "potluck-driver-tart: no script $1 at this commit" >&2; exit 127; }
if [ -x "$1" ]; then s=./$1; shift; exec "$s" "$@"; fi; exec sh "$@"`
	job := s.vm.Exec(r.VM, nil, s.log, s.log, append([]string{"sh", "-c", launch, "sh", srcDir(r), script}, r.Args...)...)
	return job, job.Start()
}

// watch waits for the run's (or box's) end and records how it ended. A box
// has no job: done is nil.
func (s *sup) watch(done <-chan int, exited <-chan struct{}) int {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastPolicy, lastQuota := time.Time{}, time.Now()
	var boxGone <-chan struct{} // a run's job notices the VM going on its own
	if done == nil {
		boxGone = exited
	}
	for {
		select {
		case code := <-done:
			if code != 0 { // tart exec also fails when the VM goes: give its exit a moment to show
				select {
				case <-exited:
				case <-time.After(time.Second):
				}
			}
			if s.vmAlive(exited) {
				s.end(stEnded, &code, "")
			} else {
				s.end(stLost, nil, "the VM stopped under the run")
			}
			return 0
		case <-boxGone:
			if s.persistent() { // the guest shut itself down: its disk is kept
				s.stopKept("the box shut itself down")
				return 0
			}
			s.end(stLost, nil, "the VM stopped")
			return 0
		case <-tick.C:
		}
		s.beat() // persist.go
		r, err := s.st.load(s.id)
		if err != nil || !r.live() || r.Starts != s.starts {
			return 0 // released (or stopped): the driver ended it; the deferred finish deletes (or keeps) it
		}
		if time.Now().After(r.Deadline) && r.Persist { // a persistent box's lease ends in a stop
			guestSync(s.vm, r.VM)
			s.stopKept("")
			return 0
		}
		if time.Now().After(r.Deadline) {
			collectShelfWants(s.cfg, s.vm, r, s.log, shelfWantsEndFor) // before the box goes (shelf_queue.go)
			s.vm.Destroy(r.VM, s.runPID)
			var exit *int // a box never has one
			if done != nil {
				code := 124
				exit = &code
			}
			s.end(stExpired, exit, "")
			return 0
		}
		if err := s.adm.gone(); err != nil {
			if r.Persist {
				s.stopKept("reclaimed: " + err.Error())
				return 0
			}
			s.vm.Destroy(r.VM, s.runPID)
			s.end(stReclaimed, nil, err.Error())
			return 0
		}
		if r.writesVolumes() && time.Since(lastQuota) >= s.cfg.volCheckEvery() { // volume.go
			lastQuota = time.Now()
			if why := s.quotaCheck(); why != "" {
				s.vm.Destroy(r.VM, s.runPID)
				s.end(stReclaimed, nil, why)
				return 0
			}
		}
		if time.Since(lastPolicy) >= time.Second {
			lastPolicy = time.Now()
			if s.endedByCancel(r) { // progress.go
				s.vm.Destroy(r.VM, s.runPID)
				s.end(stFailed, nil, errAcquireCancelled.Error())
				return 0
			}
			if s.hostTick(r) { // drain.go: the override and the policy
				return 0
			}
		}
	}
}

// reclaimTiming is the run log's record of a reclaim, how zm-eakh.12.1.4's
// latency is measured on real use: on owner input, the input happened idle
// before the supervisor saw it, so input-to-gone is idle + (gone - seen).
func reclaimTiming(seen, gone time.Time, idle time.Duration, byInput bool) string {
	ms := func(d time.Duration) int64 { return d.Milliseconds() }
	if !byInput {
		return fmt.Sprintf("potluck-driver-tart: reclaimed: seen %s, VM gone %d ms later", seen.UTC().Format(time.RFC3339Nano), ms(gone.Sub(seen)))
	}
	return fmt.Sprintf("potluck-driver-tart: reclaimed on owner input: input %s, seen %d ms after it, VM gone %d ms after it",
		seen.Add(-idle).UTC().Format(time.RFC3339Nano), ms(idle), ms(idle+gone.Sub(seen)))
}

func (s *sup) vmAlive(exited <-chan struct{}) bool {
	select {
	case <-exited:
		return false
	default:
		return true
	}
}

// end records a live run's end; a release that got there first wins. Its
// build volumes go at once (volume_seal.go), not only after finish, so an
// acquire that sees the end never finds them still there; one a container
// still references is left to finish's dropBuilds.
func (s *sup) end(state string, exit *int, why string) {
	r, err := s.st.update(s.id, func(r *record) error {
		if r.live() {
			r.State, r.Exit, r.Why, r.End = state, exit, why, time.Now().UTC()
		}
		return nil
	})
	if err == nil {
		s.dropBuilds(r)
	}
}

// fail records a start's failure. Unless its acquire was cancelled, the
// failure keeps a report (failreport.go) that its words name, since the
// record and its logs go once acquire answers it.
func fail(st store, r record, err error) {
	why := err.Error()
	if !errors.Is(err, errAcquireCancelled) {
		if p := st.keepFailure(r, why); p != "" {
			why += "; report: " + p
		}
	}
	fmt.Fprintln(os.Stderr, "potluck-driver-tart:", r.Handle, why)
	_, _ = st.update(r.ID, func(o *record) error {
		switch {
		case o.State != stStarting || o.Starts != r.Starts:
		case o.Persist && o.Starts > 0: // a start that failed: the box stays stopped, its disk kept
			stopped(o, "the box did not start: "+why)
		default:
			o.State, o.Why, o.End = stFailed, why, time.Now().UTC()
		}
		return nil
	})
}

// checkout puts a clean checkout of exactly r.Commit at srcDir(r), then
// links what the shelf holds of its lockfile into the box's own npm cache
// and reads the box's wants (shelf_queue.go).
func (s *sup) checkout(ctx context.Context, r record) error {
	if err := s.fetchCheckout(ctx, r); err != nil {
		return err
	}
	shelfLink(s.vm, s.name, srcDir(r), s.log)
	collectShelfWants(s.cfg, s.vm, r, s.log, shelfWantsReadFor)
	return nil
}

// fetchCheckout puts the checkout in. With a reachable remote the guest
// fetches the commit itself; with a local remote (or fetch=host) the host
// fetches it and copies in a bundle. Either way no credential enters the
// guest. Both scripts empty guestDir ($1, which never holds a volume) and
// keep the bundle there; the checkout ($2, srcDir) is never removed or
// remade, since it may be a volume's mount point: only a .git in it goes,
// and checkout -f writes the commit's files over what is there and leaves
// every other file (a deps layer's node_modules) as it is.
func (s *sup) fetchCheckout(ctx context.Context, r record) error {
	const pre = `set -e; base=$2
` + baseInHome + `rm -rf "$1"; mkdir -p "$1" "$base"; rm -rf "$base/.git"; git init -q "$base"; cd "$base"
`
	const co = `git -c advice.detachedHead=false checkout -q -f --detach "$4"`
	if !s.cfg.HostFetch {
		const prog = pre + `git -c maintenance.auto=false fetch -q --depth=1 --no-tags "$3" "$4"; ` + co
		return s.guest(ctx, nil, "checkout in the guest", "sh", "-c", prog, "sh", guestDir, srcDir(r), r.Remote, r.Commit)
	}
	bundle, err := s.bundle(ctx, r)
	if err != nil {
		return err
	}
	defer os.Remove(bundle)
	f, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer f.Close()
	const prog = pre + `cat > "$1/in.bundle"; git -c maintenance.auto=false fetch -q "$1/in.bundle" "$3"; ` + co + `; rm -f "$1/in.bundle"`
	return s.guest(ctx, f, "checkout in the guest", "sh", "-c", prog, "sh", guestDir, srcDir(r), "refs/zm-run/"+r.ID, r.Commit)
}

func (s *sup) guest(ctx context.Context, stdin io.Reader, what string, argv ...string) error {
	var out strings.Builder
	cmd := s.vm.Exec(s.name, stdin, &out, &out, argv...)
	stop := context.AfterFunc(ctx, func() {
		if cmd.Process != nil { // ctx already done: AfterFunc can run before Run starts the process
			_ = cmd.Process.Kill()
		}
	})
	defer stop()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// bundle fetches commit from the remote into a host cache and writes a bundle
// holding it, under refs/zm-run/<id>.
func (s *sup) bundle(ctx context.Context, r record) (string, error) {
	cache := filepath.Join(s.st.dir, "cache", "repo.git")
	git := func(a ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "maintenance.auto=false", "--git-dir", cache}, a...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(a, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	var bundle string
	err := flocked(filepath.Join(s.st.dir, "cache", "lock"), func() error { // one fetch into the cache at a time
		if _, err := os.Stat(cache); err != nil {
			if err := exec.Command("git", "init", "-q", "--bare", cache).Run(); err != nil {
				return err
			}
		}
		if git("cat-file", "-e", r.Commit+"^{commit}") != nil {
			if err := git("fetch", "-q", "--no-tags", r.Remote, r.Commit); err != nil {
				if err2 := git("fetch", "-q", "--no-tags", r.Remote, "+refs/heads/*:refs/remote-heads/*"); err2 != nil {
					return err
				}
			}
		}
		ref := "refs/zm-run/" + r.ID
		if err := git("update-ref", ref, r.Commit); err != nil {
			return fmt.Errorf("commit %s is not on the remote: %v", r.Commit, err)
		}
		defer func() { _ = git("update-ref", "-d", ref) }()
		bundle = filepath.Join(s.st.dir, "cache", r.ID+".bundle")
		return git("bundle", "create", "-q", bundle, ref)
	})
	return bundle, err
}

// takeRoom asks the host's admission controller (admission_url) for room
// for this VM before it exists; a refusal is the run's failure, and acquire
// answers unavailable with it.
func (s *sup) takeRoom(r record) error {
	if s.cfg.VM == "oci" {
		o := r.bindLayer(s.vm).(ociCLI)
		id, err := o.verifyEngine()
		if err != nil {
			return err
		}
		o.engineHost = id.endpoint
		s.vm = o
	}
	var err error
	s.host, err = hostLeaseFor(s.cfg)
	if err != nil {
		return err
	}
	if s.host != nil && (r.HostBudget == nil || *r.HostBudget != s.host.budget) {
		return fmt.Errorf("host resource profile differs from the run's immutable allocation")
	}
	a, err := newAdmission(s.cfg)
	if err != nil || a == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cpu, memory := s.cfg.CPU, s.cfg.Memory
	if s.host != nil {
		cpu, memory = s.host.budget.CPU, s.host.budget.MemoryMiB
	}
	asked := time.Now()
	l, err := a.take(ctx, cpu, memory, "zm:"+r.Target+"/"+r.ID)
	if nr := (noRoom{}); errors.As(err, &nr) {
		s.st.recordRefusal(s.cfg.AdmissionURL, nr, time.Now()) // describe reports it (admission_refusal.go)
	}
	if err != nil {
		return err
	}
	s.st.clearRefusal(s.cfg.AdmissionURL, asked)
	now := time.Now()
	s.adm = &held{a: a, lease: l, renewed: now, tried: now, lost: make(chan struct{}), done: make(chan struct{})}
	if s.host != nil {
		if time.Until(l.ExpiresAt) < time.Duration(s.cfg.BootTimeout+s.host.profile.CleanupReserveS)*time.Second {
			_ = a.release(ctx, l.Lease)
			s.adm = nil
			return fmt.Errorf("admission lease leaves too little time for boot and bounded cleanup")
		}
		if err = s.host.confirm(l.ExpiresAt); err != nil {
			_ = a.release(ctx, l.Lease)
			s.adm = nil
			return err
		}
		s.host.parent, err = makeHostParent(hostParentName(s.cfg.StateDir, r.VM, r.Starts), s.host.budget)
		if err != nil {
			_ = a.release(ctx, l.Lease)
			s.adm = nil
			return err
		}
		o := s.vm.(ociCLI)
		o.host = s.host
		s.vm = o
		s.adm.confirmed = s.host.confirm
	}
	kctx, stop := context.WithCancel(context.Background())
	s.stop = stop
	go s.adm.keepAlive(kctx) // from now, through clone and boot, not only once the VM runs
	return nil
}

// releaseRoom gives the room back, whatever ended the VM.
func (s *sup) releaseRoom() {
	if s.adm == nil {
		return
	}
	s.stop()
	<-s.adm.done
	if s.host != nil && s.host.parent != nil && !s.host.parent.empty() {
		fmt.Fprintln(s.log, "not releasing admission: owned host parent is still populated or unreadable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.adm.a.release(ctx, s.adm.lease.Lease); err != nil {
		fmt.Fprintln(s.log, "potluck-driver-tart: releasing the admission lease:", err)
	}
}
