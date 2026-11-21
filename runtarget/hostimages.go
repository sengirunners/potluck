package runtarget

// A host's box bindings follow its program's image pins (zm-499v, zm-grpc):
// share chooses the pins when a machine lends (hostprobe), and an update
// brings other ones in the embedded images.pin. At start and every
// hostImagesEvery, the daemon moves each box binding of this machine whose
// images Potluck chose (imagepins.Current) to this build's pins, whether it
// is machine-level (run_targets.toml) or a project's own (project.toml, what
// a host with no pool binds): it pulls the new images first, then rebinds
// once the binding has no live handles and no box is starting on it, so no
// box is ever dropped. Until then, and
// when a step fails, the binding lends the pins it has and its view says why
// (ImagesBehind: potluck host status). Only the oci layer is moved here; a
// Mac's Tart images are tens of GB and are pulled when its owner runs
// potluck host. An owner's own --image is never touched, and a move that
// would need any key besides the images changed is left to the owner.

import (
	"cmp"
	"context"
	"errors"
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
	why map[string]string // imageKey -> why it still lends the pins it has

	// gate guards starting and entered. An acquire or start counts itself in
	// (enter) before it reads its binding; a rebind writes the file only
	// while none is in flight and none entered since its look at the live
	// handles began, so no box boots the old pin unseen. The gate is never
	// held across a driver call.
	gate     sync.Mutex
	starting map[string]int    // imageKey -> acquires and starts in flight
	entered  map[string]uint64 // imageKey -> acquires and starts ever counted in

	pins   func() []imagepins.DefaultPin                      // this build's (tests replace it)
	pull   func(ctx context.Context, cli, image string) error // the layer CLI's pull (tests replace it)
	listed func()                                             // run after the last list of live handles (tests only)
}

// imageKey names a binding in the refresh's state: a machine-level binding
// by its name, a project's (its id) by project/name (a binding name holds no
// "/"), so two projects' bindings and a machine-level one of one name stay apart.
func imageKey(project, name string) string {
	if project == "" {
		return name
	}
	return project + "/" + name
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

// forget drops the counts of keys not in bound (an acquire counts itself in
// before its name is checked) and none is in flight on.
func (h *hostImages) forget(bound map[string]bool) {
	h.gate.Lock()
	defer h.gate.Unlock()
	for key := range h.entered {
		if h.starting[key] == 0 && !bound[key] {
			delete(h.entered, key)
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
// project is the id of the project whose file holds b, "" for a machine-level one.
func (s *Service) imagesBehind(project string, b computeapi.RunTarget) *computeapi.ImagesBehind {
	m, ok := s.currentImages(b)
	if !ok || !m.Behind {
		return nil
	}
	why := s.images.said(imageKey(project, b.Name))
	if why == "" {
		why = cmp.Or(ownerWhy(b, m), "the daemon pulls them and rebinds it shortly")
	}
	return &computeapi.ImagesBehind{Lends: b.Images, Current: m.Now, Why: why}
}

// currentImages is how a box binding of this machine's, machine-level or a project's, stands under this
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

// imageSite is a box binding the refresh looks at, and where it is bound:
// project is the id of the project whose project.toml holds it, "" for the
// machine-level file.
type imageSite struct {
	project string
	b       computeapi.RunTarget
}

// imageSites are this machine's box bindings, machine-level first, then each
// project's; ok is false when any list could not be read (the counts of a
// binding not listed are then kept).
func (s *Service) imageSites() (sites []imageSite, ok bool) {
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	s.host.mu.Unlock()
	ok = err == nil
	for _, t := range ts {
		sites = append(sites, imageSite{"", t.RunTarget})
	}
	for _, p := range s.d.Projects.List(s.d.System().Scope) {
		for _, b := range rt.ProjectTargets(s.d.Projects, p.ID) {
			sites = append(sites, imageSite{p.ID, b})
		}
	}
	return sites, ok
}

// RefreshHostImages moves each box binding of this machine still behind this
// build's pins to them, where it can now, and records why for the rest.
func (s *Service) RefreshHostImages(ctx context.Context) {
	// Only once the host file's migration has finished (Start and every host
	// call run it); the refresh never runs it itself, so it touches no
	// database, which a daemon shutting down may have closed.
	if ctx.Err() != nil || !s.host.done.Load() {
		return
	}
	sites, ok := s.imageSites()
	if ok {
		bound := map[string]bool{}
		for _, st := range sites {
			bound[imageKey(st.project, st.b.Name)] = true
		}
		s.images.forget(bound)
	}
	for _, st := range sites {
		if ctx.Err() != nil {
			return
		}
		key := imageKey(st.project, st.b.Name)
		m, ok := s.currentImages(st.b)
		if !ok || !m.Behind {
			s.images.say(key, "")
			continue
		}
		why := s.refreshImages(ctx, st, m)
		s.images.say(key, why)
		if why == "" {
			s.logInfo("run targets: rebound a binding to this build's image pins", "binding", st.b.Name, "project", st.project, "images", strings.Join(m.Now, ","))
		}
	}
}

// refreshImages rebinds st's binding to m.Now when it can: "" when it did,
// else why it still lends the pins it has.
func (s *Service) refreshImages(ctx context.Context, st imageSite, m imagepins.Move) string {
	b := st.b
	key := imageKey(st.project, b.Name)
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
		s.images.say(key, "pulling "+im)
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
	seen, idle := s.images.idle(key)
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
	if n, idle := s.images.idle(key); !idle || n != seen { // one came in while the handles were listed
		return starting
	}
	if st.project != "" {
		return s.rebindProjectImages(ctx, st.project, b, nb)
	}
	return s.rebindHostImages(b, nb, d)
}

// changedMeanwhile is why a rebind leaves a binding whose owner changed it
// since the refresh read it: that choice stands.
var changedMeanwhile = "its binding changed meanwhile; looked at again in " + hostImagesEvery.String()

// rebindHostImages writes nb in place of the machine-level binding b, and
// makes the next heartbeat advertise it. The gate is held.
func (s *Service) rebindHostImages(b, nb computeapi.RunTarget, d *computeapi.RunTargetDescribe) string {
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	ts, err := s.hostBindings()
	if err != nil {
		return "its binding could not be read: " + oneLine(err.Error())
	}
	i := slices.IndexFunc(ts, func(o computeapi.HostRunTarget) bool { return o.Name == b.Name })
	if i < 0 || !reflect.DeepEqual(ts[i].RunTarget, b) { // its owner changed it since: that choice stands
		return changedMeanwhile
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

// rebindProjectImages writes nb in place of b in the project's project.toml,
// as an owner's bind with replace would, once the binding passes the checks
// of one. The gate is held.
func (s *Service) rebindProjectImages(ctx context.Context, project string, b, nb computeapi.RunTarget) string {
	f, ok := s.d.Projects.File(project)
	if !ok {
		return "its project is gone"
	}
	if err := rt.ValidateRunTarget(f.Repos.Remotes, nb); err != nil {
		return "the new binding is not valid, so it lends the images it has: " + oneLine(err.Error())
	}
	errStale := errors.New("binding changed")
	_, err := rt.SetProjectTargets(ctx, s.d.Projects, project, func(old []computeapi.RunTarget) ([]computeapi.RunTarget, error) {
		i := slices.IndexFunc(old, func(o computeapi.RunTarget) bool { return o.Name == b.Name })
		if i < 0 || !reflect.DeepEqual(old[i], b) { // its owner changed it since: that choice stands
			return nil, errStale
		}
		old[i] = nb
		return old, nil
	})
	switch {
	case errors.Is(err, errStale):
		return changedMeanwhile
	case err != nil:
		return "its binding could not be written: " + oneLine(errText(err))
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
