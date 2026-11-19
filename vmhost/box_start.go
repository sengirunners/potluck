package vmhost

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// startsRunner says whether this run starts a GitHub runner in its guest, so
// its runner may need the refresh (runner_refresh.go).
func (s *sup) startsRunner(r record) bool {
	if skipRunnerRefresh(s.cfg) {
		return false
	}
	return r.Kind == "box" && r.JIT && s.cfg.JITRunnerDir != ""
}

// refreshBy is when a runner refresh must be done: just before acquire
// stops waiting for the run to start (waitStarted), so a box acquire gave up
// on is not made ready later.
func (s *sup) refreshBy(r record) time.Time {
	return r.Start.Add(time.Duration(s.cfg.BootTimeout+12) * time.Second)
}

// box makes a booted VM a box: the checkout of r.Commit if one was asked
// for, then ready until its lease ends. Nothing runs in it but what exec
// sends (from the driver, not this process).
func (s *sup) box(r record, bootBy time.Time, exited <-chan struct{}) int {
	if err := s.startDatabase(r, bootBy); err != nil {
		s.failStarted(r, err)
		return 1
	}
	if r.Commit != "" && r.Starts == 0 { // a start finds its checkout on the kept disk
		ctx, cancel := context.WithDeadline(context.Background(), bootBy.Add(10*time.Second))
		err := s.checkout(ctx, r)
		cancel()
		if err != nil {
			s.failStarted(r, err)
			return 1
		}
	}
	if r.JIT {
		if err := s.startJIT(r); err != nil {
			s.failStarted(r, err)
			return 1
		}
	}
	cancelled := false
	_, err := s.st.update(s.id, func(r *record) error {
		if r.State != stStarting || r.Starts != s.starts {
			return errors.New("released while starting")
		}
		if cancelled = s.cancelledAtReady(r); cancelled {
			return nil
		}
		now := time.Now().UTC()
		r.State, r.RunStart = stReady, now
		r.Deadline = now.Add(time.Duration(r.TTL) * time.Second)
		if lim := r.since().Add(time.Duration(s.cfg.TTLMax) * time.Second); r.Deadline.After(lim) {
			r.Deadline = lim
		}
		return nil
	})
	if err != nil || cancelled {
		return 0
	}
	if r.Warm { // warm.go: idle until an acquire takes it
		if !s.waitTaken(exited) {
			return 0
		}
		if err := s.taken(exited); err != nil {
			if !errors.Is(err, errAcquireCancelled) {
				s.st.warmRefused() // warm.go: no refill for a while
			}
			if t, lerr := s.st.load(s.id); lerr == nil {
				s.failStarted(t, err)
			}
			return 1
		}
	}
	return s.watch(nil, exited)
}

// startJIT starts the GitHub runner the authority minted a config for: the
// box's first command. Registration and deregistration are the authority's;
// the box stays ready after the runner exits, until it is released.
func (s *sup) startJIT(r record) error {
	if s.jit == "" {
		return errors.New("no whole jit_config reached the supervisor")
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.refreshBy(r))
	err := s.refreshRunner(ctx, r.Image, s.cfg.JITRunnerDir, s.runnerFetch)
	cancel()
	if err != nil {
		s.jit = ""
		return err
	}
	// No guard repo: a renter's own config, no hook. A run goes into the hook
	// as it is, so it is checked again here, where the record is read back.
	var runID, runWorkflow, runRef, runHold, secrets string
	if r.JITRun != nil {
		if err := r.JITRun.Check(r.JITRepo); r.JITRepo == "" || err != nil {
			s.jit = ""
			return errors.New("the record's jit_guard.run is not a run of its guard's repository")
		}
		runID, runWorkflow, runRef = strconv.FormatInt(r.JITRun.ID, 10), r.JITRun.WorkflowRef, r.JITRun.Ref
		runHold, secrets = strconv.Itoa(r.JITRun.Hold()), protocol.BoxSecretsDir
	}
	open := ""
	if r.JITOpenIfPrivate {
		if len(r.JITEvents) == 0 {
			s.jit = ""
			return errors.New("the record's jit_guard.open_if_private has no events to widen")
		}
		open = "private"
	}
	jobs := newJobWatch(s.log)
	s.jobs = jobs
	job := s.vm.Exec(r.VM, strings.NewReader(s.jit+"\n"), jobs, s.log, "sh", "-c", jitLaunch, "sh", s.cfg.JITRunnerDir, r.JITRepo,
		strings.Join(r.JITEvents, " "), jitStop(s.cfg), jitCache(s.cfg), runID, runWorkflow, runRef, runHold, secrets, open)
	s.jit = ""
	if err := job.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		_ = job.Wait()
		fmt.Fprintf(s.log, "potluck-driver-tart: the GitHub runner exited %d\n", job.ProcessState.ExitCode())
		close(done)
		close(jobs.done)
	}()
	// A runner that cannot start (no runner in the image, a config GitHub
	// refuses) exits within seconds: then acquire fails rather than answering
	// a box with no runner. A healthy one stays up, listening for its job.
	select {
	case <-done:
		return fmt.Errorf("the GitHub runner exited %d at start (see the run log)", job.ProcessState.ExitCode())
	case <-time.After(jitStartWindow):
		return nil
	}
}
