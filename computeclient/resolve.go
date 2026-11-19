package computeclient

// The one rule for turning what a person typed after --on into a run target
// (zm-eakh.12.24.19). A target's full name is <binding>@<machine> for a
// binding another machine serves, and `target ls` is the only place that
// prints it, so a person types what they can see: the binding's own name, a
// machine, or a machine and a size.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/placement"
)

// maxListed caps the target names a refusal lists.
const maxListed = 6

// ResolveRunTarget finds the run target of kind (a target's Kind; "" any)
// that name means, for --on. name is
//
//   - the exact target name (example-host-c2m4@example-host, or a local binding's);
//   - a binding's own name (example-host-c2m4), on whichever machine serves it;
//   - a machine (example-host): alone when it serves one target of the kind,
//     else with size;
//   - or <machine>-<size> (example-host-c2m4), by the size a target states.
//
// A target's name or binding already says its size, so size goes with a
// machine only. Matches of the last three are taken together: more than one
// target is refused with the names, never guessed, and a target on a public
// host counts only when nothing else matches (a stranger names its own
// bindings, so its name must not capture a trusted machine's). With no match
// the error names the closest targets and says where all are listed.
// Placement does not apply: the target is taken as named, as an exact name
// always was.
func ResolveRunTarget(ctx context.Context, c *client.Client, kind, name, size string) (computeapi.RunTargetView, error) {
	return resolveRunTarget(ctx, c, kind, name, size, false)
}

// FindRunTarget is the target of kind named exactly name (a handle's target:
// it never means a machine or a size), with the same refusals.
func FindRunTarget(ctx context.Context, c *client.Client, kind, name string) (computeapi.RunTargetView, error) {
	return resolveRunTarget(ctx, c, kind, name, "", true)
}

func resolveRunTarget(ctx context.Context, c *client.Client, kind, name, size string, exact bool) (computeapi.RunTargetView, error) {
	l, err := RunTargets(ctx, c, false)
	if err != nil {
		return computeapi.RunTargetView{}, err
	}
	// Sizes come from describe, which the plain list leaves out and which
	// asks every local driver: ask only when a size can decide the match.
	if !exact && (size != "" || slices.ContainsFunc(l.Targets, func(t computeapi.RunTargetView) bool {
		return t.Host != "" && strings.HasPrefix(name, t.Host+"-")
	})) {
		if l, err = RunTargets(ctx, c, true); err != nil {
			return computeapi.RunTargetView{}, err
		}
	}
	return matchRunTarget(l.Targets, kind, name, size, exact)
}

// matchRunTarget is the resolution over a list.
func matchRunTarget(ts []computeapi.RunTargetView, kind, name, size string, exact bool) (computeapi.RunTargetView, error) {
	ofKind := func(t computeapi.RunTargetView) bool { return kind == "" || t.Kind == kind }
	stated := func(t computeapi.RunTargetView) string {
		if t.Describe != nil && t.Describe.Size != nil {
			return t.Describe.Size.Name
		}
		return ""
	}
	var wrongKind error // a target of this name that is another kind: not it, unless nothing else is
	if i := slices.IndexFunc(ts, func(t computeapi.RunTargetView) bool { return t.Name == name }); i >= 0 {
		if ofKind(ts[i]) {
			return named(ts[i], size)
		}
		wrongKind = api.Errorf(api.CodeBadRequest, "run target %s is a %s, not a %s", name, ts[i].Kind, kind)
	}
	if exact {
		if wrongKind != nil {
			return computeapi.RunTargetView{}, wrongKind
		}
		return computeapi.RunTargetView{}, notFound(ts, kind, name, size, stated)
	}
	if bs := filterTargets(ts, func(t computeapi.RunTargetView) bool { return ofKind(t) && t.HostBinding == name }); len(bs) > 0 && size != "" {
		return named(bs[0], size)
	}
	onMachine := filterTargets(ts, func(t computeapi.RunTargetView) bool { return ofKind(t) && t.Host == name })
	var cands []computeapi.RunTargetView
	for _, t := range ts {
		s := stated(t)
		switch {
		case !ofKind(t):
		case t.HostBinding == name,
			t.Host == name && (size == "" || s == size),
			t.Host != "" && s != "" && t.Host+"-"+s == name && (size == "" || s == size):
			cands = append(cands, t)
		}
	}
	if slices.ContainsFunc(cands, func(t computeapi.RunTargetView) bool { return placement.TierOf(t) != placement.Public }) {
		cands = filterTargets(cands, func(t computeapi.RunTargetView) bool { return placement.TierOf(t) != placement.Public })
	}
	switch {
	case len(cands) == 1:
		return cands[0], nil
	case len(cands) > 1:
		hint := "name one in full"
		if size == "" && len(onMachine) > 1 {
			hint += ", or add --size"
		}
		return computeapi.RunTargetView{}, api.Errorf(api.CodeBadRequest, "%s matches %d run targets: %s", name, len(cands), runTargetNames(cands)).WithHint(hint)
	case wrongKind != nil && len(onMachine) == 0:
		return computeapi.RunTargetView{}, wrongKind
	}
	return computeapi.RunTargetView{}, notFound(ts, kind, name, size, stated)
}

// named is the one target a name meant; a size with it is refused.
func named(t computeapi.RunTargetView, size string) (computeapi.RunTargetView, error) {
	if size != "" {
		return computeapi.RunTargetView{}, api.Errorf(api.CodeBadRequest, "%s names one run target: --size goes with a machine name, not with a target", t.Name)
	}
	return t, nil
}

func filterTargets(ts []computeapi.RunTargetView, keep func(computeapi.RunTargetView) bool) []computeapi.RunTargetView {
	var out []computeapi.RunTargetView
	for _, t := range ts {
		if keep(t) {
			out = append(out, t)
		}
	}
	return out
}

func runTargetNames(ts []computeapi.RunTargetView) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// notFound names the closest targets: those on the machine name starts with
// or is, those of its size, those of the size asked for. Without any it
// names the first few of all.
func notFound(ts []computeapi.RunTargetView, kind, name, size string, stated func(computeapi.RunTargetView) string) error {
	var pool, near []computeapi.RunTargetView
	for _, t := range ts {
		if kind != "" && t.Kind != kind {
			continue
		}
		pool = append(pool, t)
		s := stated(t)
		machine := t.Host != "" && (name == t.Host || strings.HasPrefix(name, t.Host+"-"))
		if machine || (s != "" && (s == size || s == name || strings.HasSuffix(name, "-"+s))) {
			near = append(near, t)
		}
	}
	list := "the closest"
	if len(near) == 0 {
		near, list = pool, "those listed"
	}
	err := api.Errorf(api.CodeNotFound, "no run target %s", name).WithData(computeapi.TargetNotListed{Name: name})
	// A daemon that has just restarted lists its remote targets back over a
	// minute (zm-c5y6c.6.3.14): a miss then reads as a miss, so say so.
	wait := "; a machine that has just restarted takes a minute to list all of its own"
	ls := config.Command("target ls") // names the missing command in words where the program has none
	if len(pool) == 0 {
		return err.WithHint("no run targets are listed now; " + ls + " lists them" + wait)
	}
	return err.WithHint(list + ": " + describeTargets(near, stated) + "; " + ls + " lists them all" + wait)
}

// describeTargets lists up to maxListed target names, each with the size it
// states, and how many more there are.
func describeTargets(ts []computeapi.RunTargetView, stated func(computeapi.RunTargetView) string) string {
	slices.SortFunc(ts, func(a, b computeapi.RunTargetView) int { return strings.Compare(a.Name, b.Name) })
	var out []string
	for i, t := range ts {
		if i == maxListed {
			out = append(out, fmt.Sprintf("and %d more", len(ts)-maxListed))
			break
		}
		if s := stated(t); s != "" {
			out = append(out, t.Name+" (size "+placement.Quote(s)+")")
		} else {
			out = append(out, t.Name)
		}
	}
	return strings.Join(out, ", ")
}
