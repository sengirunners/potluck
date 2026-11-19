package runtarget

// A host's box bindings follow its program's image pins (zm-499v): share
// chooses the pins when a machine lends (hostprobe), and an update brings
// other ones in the embedded images.pin. At start and every
// hostImagesEvery, the daemon moves each machine-level box binding whose
// images Potluck chose (imagepins.Current) to this build's pins: it pulls
// the new images first, then rebinds once the binding has no live handles
// and no box is starting on it, so no box is ever dropped. Until then, and
// when a step fails, the binding lends the pins it has and its view says why
// (ImagesBehind: potluck host status). Only the oci layer is moved here; a
// Mac's Tart images are tens of GB and are pulled when its owner runs
// potluck host. An owner's own --image is never touched, and a move that
// would need any key besides the images changed is left to the owner.

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runtarget/rt"
)

const (
	// hostImagesEvery is how often a binding still behind is tried again:
	// its live boxes may have ended, or a failed pull may pass now.
	hostImagesEvery = 10 * time.Minute
	// hostImagePullLimit bounds one image's pull.
	hostImagePullLimit = 60 * time.Minute
)

// hostImages is the refresh's state: why each binding still lends other
// pins, and the gate between a rebind and the boxes starting on it.
type hostImages struct {
	mu  sync.Mutex
	why map[string]string // binding name -> why it still lends the pins it has

	// gate guards starting and entered. An acquire or start counts itself in
	// (enter) before it reads its binding; a rebind writes the file only
	// while none is in flight and none entered since its look at the live
	// handles began, so no box boots the old pin unseen. The gate is never
	// held across a driver call.
	gate     sync.Mutex
	starting map[string]int    // binding name -> acquires and starts in flight
	entered  map[string]uint64 // binding name -> acquires and starts ever counted in

	pins   func() []imagepins.DefaultPin                      // this build's (tests replace it)
	pull   func(ctx context.Context, cli, image string) error // the layer CLI's pull (tests replace it)
	listed func()                                             // run after the last list of live handles (tests only)
}

func (h *hostImages) say(name, why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if why == "" {
		delete(h.why, name)
		return
	}
	if h.why == nil {
		h.why = map[string]string{}
	}
	h.why[name] = why
}

func (h *hostImages) said(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.why[name]
}

// enter counts an acquire or start on binding name in; call the func it
// returns when the driver answered.
func (h *hostImages) enter(name string) func() {
	h.gate.Lock()
	if h.starting == nil {
		h.starting, h.entered = map[string]int{}, map[string]uint64{}
	}
	h.starting[name]++
	h.entered[name]++
	h.gate.Unlock()
	return func() {
		h.gate.Lock()
		if h.starting[name]--; h.starting[name] <= 0 {
			delete(h.starting, name)
		}
		h.gate.Unlock()
	}
}

// forget drops the counts of names no binding in ts has (an acquire counts
// itself in before its name is checked) and none is in flight on.
func (h *hostImages) forget(ts []computeapi.HostRunTarget) {
	h.gate.Lock()
	defer h.gate.Unlock()
	for name := range h.entered {
		if h.starting[name] == 0 && !slices.ContainsFunc(ts, func(t computeapi.HostRunTarget) bool { return t.Name == name }) {
			delete(h.entered, name)
		}
	}
}

// idle is how many acquires and starts binding name has had counted in, and
// whether none is in flight now. Gate held.
func (h *hostImages) idle(name string) (uint64, bool) {
	return h.entered[name], h.starting[name] == 0
}

func (h *hostImages) table() []imagepins.DefaultPin {
	if h.pins != nil {
		return h.pins()
	}
	return imagepins.DefaultPins()
}

// pullImage pulls image through the binding's OCI CLI, as share does.
func pullImage(ctx context.Context, cli, image string) error {
	ctx, cancel := context.WithTimeout(ctx, hostImagePullLimit)
	defer cancel()
	out, err := exec.CommandContext(ctx, cli, "pull", "--", image).CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > 300 {
			tail = "..." + strings.ToValidUTF8(tail[len(tail)-300:], "")
		}
		return fmt.Errorf("%s pull %s: %v: %s", cli, image, err, tail)
	}
	return nil
}

// imagesBehind is b's ImagesBehind under this build's pins, nil when it lends
// them or its images are not Potluck's choice.
func (s *Service) imagesBehind(b computeapi.RunTarget) *computeapi.ImagesBehind {
	m, ok := s.currentImages(b)
	if !ok || !m.Behind {
		return nil
	}
	why := s.images.said(b.Name)
	if why == "" {
		why = cmp.Or(ownerWhy(b, m), "the daemon pulls them and rebinds it shortly")
	}
	return &computeapi.ImagesBehind{Lends: b.Images, Current: m.Now, Why: why}
}

// currentImages is how a box binding of this machine's stands under this
// build's pins; ok is false when its images are not Potluck's choice.
func (s *Service) currentImages(b computeapi.RunTarget) (imagepins.Move, bool) {
	if b.Kind != computeapi.RunTargetKindBox || b.Host != "" {
		return imagepins.Move{}, false
	}
	return imagepins.Current(s.images.table(), cmp.Or(b.Config["vm"], "tart"), b.Images, b.Config)
}

// ownerWhy is why only the owner's run of potluck host moves b, with the
// command; "" when the daemon may.
func ownerWhy(b computeapi.RunTarget, m imagepins.Move) string {
	why := m.Owner
	if why == "" && cmp.Or(b.Config["vm"], "tart") != "oci" {
		why = "a Mac's images are tens of GB, pulled when you ask"
	}
	if why == "" {
		return ""
	}
	args := []string{"--yes"}
	if n := b.Config["image_name"]; n != "" && b.Config[imagepins.ImageDefaultKey] != imagepins.ImageDefault {
		args = append(args, "--with", n)
	}
	return "run " + config.PotluckCommand("host", args...) + " on this machine to lend them (" + why + ")"
}

// RefreshHostImages moves each machine-level box binding still behind this
// build's pins to them, where it can now, and records why for the rest.
func (s *Service) RefreshHostImages(ctx context.Context) {
	// Only once the host file's migration has finished (Start and every host
	// call run it); the refresh never runs it itself, so it touches no
	// database, which a daemon shutting down may have closed.
	if ctx.Err() != nil || !s.host.done.Load() {
		return
	}
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	s.host.mu.Unlock()
	if err != nil {
		return
	}
	s.images.forget(ts)
	for _, t := range ts {
		b := t.RunTarget
		m, ok := s.currentImages(b)
		if !ok || !m.Behind {
			s.images.say(b.Name, "")
			continue
		}
		why := s.refreshImages(ctx, b, m)
		s.images.say(b.Name, why)
		if why == "" {
			s.logInfo("run targets: rebound a host binding to this build's image pins", "binding", b.Name, "images", strings.Join(m.Now, ","))
		}
	}
}

// refreshImages rebinds b to m.Now when it can: "" when it did, else why b
// still lends the pins it has.
func (s *Service) refreshImages(ctx context.Context, b computeapi.RunTarget, m imagepins.Move) string {
	if why := ownerWhy(b, m); why != "" {
		return why
	}
	if why := s.liveWhy(ctx, b); why != "" { // before a pull: a binding in use waits anyway
		return why
	}
	cli := b.Config["oci_cli"]
	if cli == "" {
		return "the binding names no oci_cli to pull with: run " + config.PotluckCommand("host", "--yes") + " on this machine"
	}
	pull := s.images.pull
	if pull == nil {
		pull = pullImage
	}
	for _, im := range m.Now {
		if slices.Contains(b.Images, im) {
			continue
		}
		s.images.say(b.Name, "pulling "+im)
		if err := pull(ctx, cli, im); err != nil {
			return "the pull failed, so it lends the images it has (tried again in " + hostImagesEvery.String() + "): " + oneLine(err.Error())
		}
	}
	nb := b
	nb.Images = slices.Clone(m.Now)
	// The same checks an owner's bind has: the driver describes the box (it
	// reads its image) and the class still isolates; either failing keeps
	// the pins it has.
	d, err := s.describeOf(ctx, nb)
	if err != nil {
		return "the driver did not take the new images, so it lends the ones it has: " + oneLine(errText(err))
	}
	if eff := stricter(nb, d); !eff.UntrustedCapable() {
		return "with the new images " + eff.CannotIsolate("potluck-driver-"+nb.Driver) + ", so it lends the ones it has"
	}
	const starting = "waiting: a box is starting on it"
	s.images.gate.Lock()
	seen, idle := s.images.idle(b.Name)
	s.images.gate.Unlock()
	if !idle {
		return starting
	}
	if why := s.liveWhy(ctx, b); why != "" {
		return why
	}
	if s.images.listed != nil {
		s.images.listed()
	}
	s.images.gate.Lock()
	defer s.images.gate.Unlock()
	if n, idle := s.images.idle(b.Name); !idle || n != seen { // one came in while the handles were listed
		return starting
	}
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	ts, err := s.hostBindings()
	if err != nil {
		return "its binding could not be read: " + oneLine(err.Error())
	}
	i := slices.IndexFunc(ts, func(o computeapi.HostRunTarget) bool { return o.Name == b.Name })
	if i < 0 || !reflect.DeepEqual(ts[i].RunTarget, b) { // its owner changed it since: that choice stands
		return "its binding changed meanwhile; looked at again in " + hostImagesEvery.String()
	}
	h := computeapi.HostRunTarget{RunTarget: nb, Share: ts[i].Share}
	if err := rt.ValidateHostRunTarget(h); err != nil {
		return "the new binding is not valid, so it lends the images it has: " + oneLine(err.Error())
	}
	ts[i] = h
	if err := s.saveHostBindings(ts); err != nil {
		return "its binding could not be written: " + oneLine(err.Error())
	}
	// The next heartbeat advertises the new pins with this describe, at
	// once: a dropped cache would leave the size out of that beat, and the
	// pool's old advert would key a box's layers by the old digest.
	s.host.cache.Store(b.Name, &hostDescribe{d: d, at: time.Now()})
	if s.link != nil {
		s.link.KickBeat()
	}
	return ""
}

// liveWhy is why b cannot be rebound now: its live handles (stopped boxes
// among them keep disks it must not drop), or that they cannot be listed.
func (s *Service) liveWhy(ctx context.Context, b computeapi.RunTarget) string {
	var v computeapi.RunTargetView
	s.addLive(ctx, &v, b)
	switch {
	case v.Error != "":
		return "its boxes could not be listed, so it is not rebound yet: " + oneLine(v.Error)
	case len(v.Stopped) > 0:
		return fmt.Sprintf("waiting for %s to end (%d stopped, keeping a disk until it ends)", boxes(len(v.Live)), len(v.Stopped))
	case len(v.Live) > 0:
		return "waiting for " + boxes(len(v.Live)) + " to end"
	}
	return ""
}

func boxes(n int) string {
	if n == 1 {
		return "its box"
	}
	return fmt.Sprintf("its %d boxes", n)
}

// refreshHostImagesLoop runs RefreshHostImages at start and every
// hostImagesEvery until ctx ends.
func (s *Service) refreshHostImagesLoop(ctx context.Context) {
	s.RefreshHostImages(ctx)
	t := time.NewTicker(hostImagesEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RefreshHostImages(ctx)
		}
	}
}

func (s *Service) logInfo(msg string, args ...any) {
	if s.d.Log != nil {
		s.d.Log.Info(msg, args...)
	}
}
