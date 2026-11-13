package runners

// Placement (SPEC 20 "GitHub runner broker", Placement): a job goes to any
// box binding of the pool project that serves GitHub runners, fits the size
// and OS its labels ask for and is on a trusted host, in the pool's order
// (package placement); the next is tried when one refuses.

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/placement"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// placeMax is how many boxes one try asks: each refusal costs a runner
// minted and deleted (two GitHub calls).
const placeMax = 3

var (
	osLabels   = []string{"linux", "macos", "windows"}
	archLabels = []string{"x64", "arm64", "arm"}
)

// want is what a job's labels ask of a box: size, OS and named image.
type want struct{ size, os, image string }

// labels reads a job's labels. ours: it names the pool's label (plain or
// <label>:<size>); then w is the box it wants, or why says why the pool
// cannot serve it. self-hosted and GitHub's OS and arch labels may come with
// it, along with a published named-image label such as xcode. A job naming
// no size gets default_size, else any size
// (the smallest box that fits); one naming no OS gets linux.
func (s *Service) labels(labels []string) (w want, ours bool, why string) {
	c := s.conf()
	return parseLabels(labels, c.Label, c.Pool.DefaultSize)
}

func parseLabels(labels []string, poolLabel, defaultSize string) (w want, ours bool, why string) {
	label := strings.ToLower(poolLabel)
	if label == "" {
		return want{}, false, ""
	}
	var other []string
	for _, l := range labels {
		ll := strings.ToLower(l)
		switch {
		case ll == label:
			ours = true
		case strings.HasPrefix(ll, label+":"):
			ours = true
			if sz := ll[len(label)+1:]; w.size != "" && w.size != sz {
				other = append(other, l)
			} else {
				w.size = sz
			}
		case imagepins.NamedImage(ll):
			if w.image != "" && w.image != ll {
				other = append(other, l)
			} else {
				w.image = ll
			}
		case ll == "self-hosted", slices.Contains(archLabels, ll): // arch: no box states one; the runner's own labels decide
		case slices.Contains(osLabels, ll):
			if w.os != "" && w.os != ll {
				other = append(other, l)
			} else {
				w.os = ll
			}
		default:
			other = append(other, l)
		}
	}
	switch {
	case !ours:
		return want{}, false, ""
	case len(other) > 0:
		return w, true, "it asks for labels the pool does not serve: " + strings.Join(other, ", ")
	case w.size != "" && !sizeRe.MatchString(w.size):
		return w, true, fmt.Sprintf("no size %q in the pool", w.size)
	}
	if w.size == "" {
		w.size = defaultSize
	}
	if w.os == "" {
		w.os = "linux" // as GitHub's own runs-on: a job names macos or windows to get one
	}
	return w, true, ""
}

// placed is a box binding place chose, and what its guard can check.
type placed struct {
	name string
	// guardsPrivate: its driver lists protocol.CapJITGuardPrivate, so an
	// open guard can check at the job's start that the repository is still
	// private (guard); a runner on any other gets no open guard.
	guardsPrivate bool
}

// place answers the box bindings to try for w, best first (at most
// placeMax), or why none fits. A size named in sizes goes to that binding
// alone (the owner's word). needPrivate keeps only boxes whose guard can be
// open (placed.guardsPrivate), before the cut to placeMax, so boxes that
// cannot take the job never crowd out one that can; their reason is kept
// for the job's wait.
func (s *Service) place(ctx context.Context, sub *authz.Subject, svc rt.Service, l link, w want, ttl int, needPrivate bool) ([]placed, string, error) {
	list, err := svc.List(ctx, sub, l.project, true)
	if err != nil {
		return nil, "", err
	}
	names, why := bindingFits(list.Targets, s.conf().Pool.Sizes, w, s.load(ctx), s.cooling(), ttl)
	var out []placed
	var cannot []string
	for _, n := range names {
		p := placed{name: n}
		for _, v := range list.Targets {
			if v.Name == n && v.Describe != nil {
				p.guardsPrivate = slices.Contains(v.Describe.Capabilities, protocol.CapJITGuardPrivate)
			}
		}
		if needPrivate && !p.guardsPrivate {
			cannot = append(cannot, p.name+": "+noPrivateGuard)
			continue
		}
		out = append(out, p)
	}
	if len(cannot) > 0 {
		if why != "" {
			cannot = append(cannot, why)
		}
		why = strings.Join(cannot, "; ")
	}
	if len(out) == 0 && len(cannot) > 0 {
		return nil, why, nil
	}
	if len(out) > placeMax {
		out = out[:placeMax]
	}
	return out, why, nil
}

// bindingFits applies the owner's alias selection without bypassing the
// binding's image, OS, trust, availability or required lifetime. Keep the
// input slice intact: RunsOn tries several candidates against one snapshot.
func bindingFits(views []computeapi.RunTargetView, sizes map[string]string, w want, load map[string]int, cool map[string]string, ttl int) ([]string, string) {
	if binding := sizes[w.size]; w.size != "" && binding != "" {
		selected := []computeapi.RunTargetView{}
		for _, v := range views {
			if v.Name == binding {
				selected = append(selected, v)
			}
		}
		if len(selected) == 0 {
			return nil, binding + " (mapped binding) is not listed in the pool; check that host's connection and runner advertisement"
		}
		views = selected
		w.size = ""
	}
	return fits(views, w, load, cool, ttl)
}

// refusal keeps the driver's reason for as long as placement skips the binding.
type refusal struct {
	at  time.Time
	why string
}

// refusedBy notes that target refused a box: placement skips it for RefuseCool.
func (s *Service) refusedBy(target, why string) {
	s.flight.Lock()
	s.refused[target] = refusal{at: s.now(), why: oneLine(why)}
	s.flight.Unlock()
}

// cooling is the bindings that refused a box within RefuseCool: whatever
// the number of jobs waiting, a binding that says available but refuses
// costs at most one runner minted and deleted per RefuseCool.
func (s *Service) cooling() map[string]string {
	s.flight.Lock()
	defer s.flight.Unlock()
	out := map[string]string{}
	for t, r := range s.refused {
		until := r.at.Add(s.RefuseCool)
		if s.now().Before(until) {
			out[t] = "no box: " + strconv.Quote(r.why) + " (refused a job moments ago; retry after " + until.Format(time.RFC3339) + ")"
		} else {
			delete(s.refused, t)
		}
	}
	return out
}

// load is how many jobs each binding serves now.
func (s *Service) load(ctx context.Context) map[string]int {
	out := map[string]int{}
	rows, err := s.db().QueryContext(ctx, `SELECT target, count(*) FROM runners_jobs WHERE state = ? GROUP BY target`, jobServing)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var n int
		if rows.Scan(&t, &n) == nil {
			out[t] = n
		}
	}
	return out
}

// fits ranks the box bindings that may take w by the pool's one rule
// (package placement): on a trusted host (a jit_config never goes to any
// other; the acquire refuses it too), serving GitHub runners with the guard
// (github-jit, github-jit-guard), of w's named default image, available, of
// w's size and OS, whose lifetime cap holds a job's lease, under its budget,
// and that did not refuse a job in the last RefuseCool. The order is
// placement's (team, trusted, then paid; smallest first; least busy), with
// jobs naming no image preferring plain bindings within a tier. With none,
// why names each binding and why not, quoting drivers (untrusted text); it
// starts by saying so when no runner box of the pool has the size asked for,
// with the sizes there are (the job still waits: a host may come to share it).
func fits(views []computeapi.RunTargetView, w want, load map[string]int, cool map[string]string, ttl int) ([]string, string) {
	pw := placement.Want{Size: w.size, OS: w.os, TTLS: ttl, Lease: "job runtime plus 300s startup", Public: "not a trusted host (a runner goes only to one)"}
	can := func(d *computeapi.RunTargetDescribe) string {
		switch {
		case !slices.Contains(d.Capabilities, protocol.CapGitHubJIT) || !slices.Contains(d.Capabilities, protocol.CapGitHubJITGuard):
			return "serves no GitHub runner"
		case w.image != "" && d.ImageName != w.image:
			return "default image is not " + w.image
		case d.Size != nil && !sizeRe.MatchString(d.Size.Name):
			return "invalid size " + placement.Quote(d.Size.Name)
		}
		return ""
	}
	var ok []placement.Fit
	var why, sizes []string
	for _, v := range views {
		if v.Kind != computeapi.RunTargetKindBox {
			continue
		}
		d := v.Describe
		if placement.TierOf(v) != placement.Public && d != nil && d.Size != nil && sizeRe.MatchString(d.Size.Name) &&
			slices.Contains(d.Capabilities, protocol.CapGitHubJIT) && slices.Contains(d.Capabilities, protocol.CapGitHubJITGuard) {
			sizes = append(sizes, d.Size.Name)
		}
		r := placement.Refusal(v, pw, can)
		if r == "" && cool[v.Name] != "" {
			r = cool[v.Name]
		}
		if r != "" {
			why = append(why, v.Name+": "+r)
			continue
		}
		f := placement.FitOf(v)
		if w.image == "" && d.ImageName != "" {
			f.Prefer = 1
		}
		f.Load = load[v.Name]
		ok = append(ok, f)
	}
	if len(ok) == 0 {
		if len(why) == 0 {
			return nil, "no box in the pool serves jobs yet (a member lends a machine to the pool from that machine)"
		}
		sort.Strings(why)
		head := ""
		if w.size != "" && !slices.Contains(sizes, w.size) {
			slices.Sort(sizes)
			head = fmt.Sprintf("no host in the pool shares size %s (shared now: %s): ", w.size, cmp.Or(strings.Join(slices.Compact(sizes), ", "), "none"))
		}
		return nil, clip(head + "no box fits now (quoted text is from drivers): " + strings.Join(why, "; "))
	}
	return placement.Rank(ok), ""
}

// clip keeps a job's why short enough to show.
func clip(s string) string {
	if r := []rune(s); len(r) > 600 {
		return string(r[:600]) + "..."
	}
	return s
}
