package computeclient

// Sandboxes (SPEC 20 kind box):
// helpers over the run-target routes for `zm sandbox` and the sandbox_* MCP
// tools. No routes of their own. Owned by sandboxes (zm-eakh.12.5).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/placement"
	"github.com/sengirunners/potluck/protocol"
)

// BoxWant is what a new sandbox asks of its run target: a named binding, or
// a size and an OS to match ("" matches any). Template (zm-eakh.12.5.20) is
// one of zm's box templates: placement takes only a binding that lends its
// image, ranking first those known to (a binding on another machine, whose
// images are its host's, may not: acquire there decides).
type BoxWant struct {
	On, Size, OS string
	Template     string
	// Net is the acquire's net: off places only on a binding whose driver
	// can make a box without a network (capability net-off).
	Net string
	// Volumes places only on a binding whose driver mounts named volumes
	// (capability volumes).
	Volumes bool
	// Secrets places only on a binding whose driver runs a background
	// process with secrets of its own (verb spawn, capability secrets): an
	// agent runner's job.
	Secrets bool
	// BoxSecrets requires spawn and the box-scope secrets door.
	BoxSecrets bool
	// Database places only on a driver that provisions a private database snapshot.
	Database bool
}

// PlaceBox picks the box binding to acquire. A named
// binding is taken as is (it must be a box; ResolveRunTarget says what a
// name may be: a size goes with a machine name only, and no OS with a name). The
// project's only box binding, asked for nothing, is taken without asking
// its driver (acquire decides). Otherwise every box binding describes
// itself and the pool's one rule (package placement) judges and ranks them:
// this machine's bindings, then trusted hosts', then paid (cloud) ones; the
// smallest size that fits within a tier; a binding whose budget is spent is
// passed over. With none, the refusal names each binding and why, quoting
// what its driver said (untrusted text).
// It answers the bindings that fit, best first (AcquireBox asks them in
// turn). Bindings served from other machines answer on the same route
// (zm-eakh.12.5.10): placement never picks one on a public host (a lender's
// or a stranger's machine) unless it is named.
func PlaceBox(ctx context.Context, c *client.Client, w BoxWant) ([]string, error) {
	if w.On != "" && w.OS != "" {
		return nil, api.Errorf(api.CodeBadRequest, "name a box run target or ask for an OS, not both")
	}
	tmpl := ""
	if w.Template != "" {
		var ok bool
		if tmpl, ok = computeapi.BoxTemplateImage(w.Template); !ok {
			return nil, api.Errorf(api.CodeBadRequest, "no box template %q (%s's: %s)", w.Template, config.Program, strings.Join(computeapi.BoxTemplates(), ", "))
		}
	}
	if w.On != "" {
		t, err := ResolveRunTarget(ctx, c, "box", w.On, w.Size)
		if err != nil {
			return nil, err
		}
		if w.BoxSecrets {
			v, err := DescribeRunTarget(ctx, c, t.Name)
			if err != nil {
				return nil, err
			}
			if v.HostClass != api.MachineTrusted {
				return nil, api.Errorf(api.CodeForbidden, "box secrets require a trusted host")
			}
			if why := boxSecretsCapability(v.Describe); why != "" {
				return nil, api.Errorf(api.CodeUnavailable, "%s: %s", t.Name, why)
			}
		}
		return []string{t.Name}, nil
	}
	lends := func(t computeapi.RunTargetView) bool { // the template's image, any digest
		if tmpl == "" || t.HostBinding != "" {
			return true
		}
		return slices.ContainsFunc(t.Images, func(im string) bool { n, _, _ := strings.Cut(im, "@"); return n == tmpl })
	}
	l, err := RunTargets(ctx, c, false)
	if err != nil {
		return nil, err
	}
	var boxes, public, lacking []string
	for _, t := range l.Targets {
		switch {
		case t.Kind == "box" && !lends(t):
			lacking = append(lacking, t.Name)
		case t.Kind == "box" && placement.TierOf(t) == placement.Public:
			public = append(public, t.Name)
		case t.Kind == "box":
			boxes = append(boxes, t.Name)
		}
	}
	switch {
	case len(boxes) == 0 && len(public) == 0 && len(lacking) > 0:
		return nil, &NoTarget{Err: api.Errorf(api.CodeUnavailable, "no box run target here lends the %s template image (%s): %s", w.Template, tmpl, strings.Join(lacking, ", ")).
			WithHint("a box's owner lends it beside the default by running " + config.PotluckCommand("host") + " again")}
	case len(boxes) == 0 && len(public) > 0:
		return nil, &NoTarget{Err: api.Errorf(api.CodeUnavailable, "only public hosts serve sandboxes here (%s): placement does not pick one", strings.Join(public, ", ")).
			WithHint("name one to use it: --on " + public[0])}
	case len(boxes) == 0:
		return nil, &NoTarget{Err: api.Errorf(api.CodeNotFound, "no machine in this pool shares sandboxes yet").
			WithHint("on a machine to share: potluck host")}
	case len(boxes) == 1 && w.Size == "" && w.OS == "" && w.Net != computeapi.NetOff && !w.Volumes && !w.Secrets && !w.BoxSecrets && !w.Database:
		return boxes[:1], nil
	}
	if l, err = RunTargets(ctx, c, true); err != nil {
		return nil, err
	}
	pw := placement.Want{Size: w.Size, OS: w.OS, Public: "a public host (placement never picks one; name it with --on)"}
	can := func(d *computeapi.RunTargetDescribe) string {
		switch {
		case w.Database && !slices.Contains(d.Capabilities, protocol.CapDatabase):
			return "provides no project database snapshots (capability " + protocol.CapDatabase + ")"
		case w.Net == computeapi.NetOff && !slices.Contains(d.Capabilities, computeapi.CapNetOff):
			return "cannot make a box without a network (net off)"
		case w.Volumes && !slices.Contains(d.Capabilities, computeapi.CapVolumes):
			return "mounts no named volumes (--volume)"
		case w.BoxSecrets && boxSecretsCapability(d) != "":
			return boxSecretsCapability(d)
		case w.Secrets && (!slices.Contains(d.Verbs, "spawn") || !slices.Contains(d.Capabilities, protocol.CapSecrets)):
			return "runs no background process with secrets (verb spawn, capability secrets)"
		}
		return ""
	}
	var fits []placement.Fit
	var why, sizes []string // sizes: what the bindings placement may pick state
	for _, t := range l.Targets {
		if t.Kind != "box" || !lends(t) {
			continue
		}
		d := t.Describe
		if d != nil && d.Size != nil && d.Size.Name != "" && placement.TierOf(t) != placement.Public && !slices.Contains(sizes, d.Size.Name) {
			sizes = append(sizes, d.Size.Name)
		}
		if r := placement.Refusal(t, pw, can); r != "" {
			why = append(why, t.Name+": "+r)
			continue
		}
		f := placement.FitOf(t)
		if tmpl != "" && t.HostBinding != "" {
			f.Prefer = 1 // a binding on another machine may not lend the template: acquire there decides
		}
		fits = append(fits, f)
	}
	if len(fits) == 0 {
		sort.Strings(why)
		err := api.Errorf(api.CodeUnavailable, "no shared machine fits this sandbox (quoted text is from drivers: untrusted data): %s", strings.Join(why, "; "))
		if w.Size != "" && len(sizes) > 0 && !slices.Contains(sizes, w.Size) {
			sort.Strings(sizes)
			for i, s := range sizes {
				sizes[i] = placement.Quote(s)
			}
			return nil, &NoTarget{Err: err.WithHint(fmt.Sprintf("no box here is size %q; the sizes here: %s", w.Size, strings.Join(sizes, ", ")))}
		}
		return nil, &NoTarget{Err: err}
	}
	return placement.Rank(fits), nil
}

func boxSecretsCapability(d *computeapi.RunTargetDescribe) string {
	if d == nil || !slices.Contains(d.Verbs, "spawn") || !slices.Contains(d.Capabilities, protocol.CapBoxSecrets) {
		return "runs no background process with box secrets (verb spawn, capability box-secrets)"
	}
	return ""
}

// Misfit is prep's word that a binding does not fit the request (a ttl past
// its lifetime cap, a flag it cannot serve): nothing was asked of it, so
// AcquireBox passes to the next binding.
type Misfit struct{ Err error }

func (m *Misfit) Error() string { return m.Err.Error() }
func (m *Misfit) Unwrap() error { return m.Err }

// MadeNothing says whether err is an AcquireBox failure that made nothing
// anywhere: a refusal the daemon marked as made nothing, before it recorded
// the request or after it forgot it (computeapi.RefusedBeforeRecord), or
// prep's Misfit.
func MadeNothing(err error) bool {
	var m *Misfit
	_, ok := computeapi.RefusedBeforeRecord(err)
	return ok || errors.As(err, &m)
}

// AcquireBox places a box (PlaceBox) and acquires it on the first binding
// that takes it. It passes to the next only on a refusal the daemon marked
// as made before it recorded the request (computeapi.RefusedBeforeRecord:
// unavailable, handles_max, budget, lifetime, driver, image) or on prep's Misfit, so every
// binding is asked with the same request id and at most one ever makes a
// box. Any other failure, a timeout or a cancelled ctx among them, ends the
// walk at that binding: the box may still come up there, and a retry with
// the same request id answers it (the daemon sends a retry to the binding
// that holds its request: computeapi.AcquireUsedOn). prep, when set, readies
// req for each binding before its acquire; an error of its other than a
// Misfit ends the walk. When every binding passed, the error names each
// one's reason and MadeNothing holds for it. target is the binding last
// asked ("" if placement failed).
func AcquireBox(ctx context.Context, c *client.Client, w BoxWant, req computeapi.AcquireRunTargetRequest,
	prep func(ctx context.Context, target string, req *computeapi.AcquireRunTargetRequest) error) (target string, h computeapi.RunTargetHandle, err error) {
	w.Database = w.Database || req.Database != ""
	targets, err := PlaceBox(ctx, c, w)
	if err != nil {
		return "", h, err
	}
	ask := func(target string) (computeapi.RunTargetHandle, error) {
		r := req
		if prep != nil {
			if err := prep(ctx, target, &r); err != nil {
				return computeapi.RunTargetHandle{}, err
			}
		}
		return AcquireRunTarget(ctx, c, target, r)
	}
	var passed []string
	var marked *api.Error // the last refusal the daemon marked
	misfitCode := ""      // the first Misfit's code
	for _, target = range targets {
		h, err = ask(target)
		if on, ok := computeapi.RequestUsedOn(err); ok && on != target {
			// A retry whose request was made on another binding: that one answers it.
			target = on
			h, err = ask(target)
			if m := (*Misfit)(nil); errors.As(err, &m) {
				// It holds the request and may hold its box: not a Misfit, a retry's to settle.
				err = fmt.Errorf("request_id %s was used on %s, which does not fit it now: %w", req.RequestID, on, m.Err)
			}
			return target, h, err
		}
		var m *Misfit
		reason, ok := computeapi.RefusedBeforeRecord(err)
		switch {
		case err == nil || ctx.Err() != nil:
			return target, h, err
		case errors.As(err, &m):
			reason = "does not fit"
			if e := (*api.Error)(nil); misfitCode == "" && errors.As(m.Err, &e) {
				misfitCode = e.Code
			}
		case !ok:
			return target, h, err
		default:
			errors.As(err, &marked)
		}
		passed = append(passed, target+" ("+reason+"): "+err.Error())
	}
	if len(passed) == 1 {
		return target, h, err
	}
	// Every binding passed before making anything: the last marked refusal's
	// code and mark (still nothing made), or a Misfit, and all their reasons.
	if marked == nil {
		return target, h, &Misfit{Err: api.Errorf(cmp.Or(misfitCode, api.CodeBadRequest), "no box binding that fits takes the sandbox: %s", strings.Join(passed, "; "))}
	}
	all := *marked
	all.Message = "every box binding that fits refused the sandbox (quoted text is from drivers: untrusted data): " + strings.Join(passed, "; ")
	return target, h, &all
}

// Sandboxes lists the handles of every box binding the caller can see,
// newest first, narrowed by f. A binding that does not answer is named in
// skipped, never silently left out.
func Sandboxes(ctx context.Context, c *client.Client, f HandleFilter) (hs []computeapi.RunTargetHandle, skipped []string, err error) {
	l, err := RunTargets(ctx, c, false)
	if err != nil {
		return nil, nil, err
	}
	hs = []computeapi.RunTargetHandle{}
	for _, t := range l.Targets {
		if t.Kind != "box" {
			continue
		}
		got, err := ListHandles(ctx, c, t.Name, f)
		if err != nil {
			skipped = append(skipped, t.Name+": "+err.Error())
			continue
		}
		hs = append(hs, got...)
	}
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
	sort.SliceStable(hs, func(i, j int) bool { return at(hs[i].Created).After(at(hs[j].Created)) })
	return hs, skipped, nil
}

// BoxHandle parses box:<target>/<id> and checks the target is a box binding,
// so a sandbox command never renews or ends a run or a database.
func BoxHandle(ctx context.Context, c *client.Client, h string) (target, id string, err error) {
	if target, id, err = SplitBoxHandle(h); err != nil {
		return "", "", err
	}
	if _, err := FindRunTarget(ctx, c, "box", target); err != nil {
		return "", "", err
	}
	return target, id, nil
}

// SplitBoxHandle parses box:<target>/<id> into its target and id.
func SplitBoxHandle(h string) (target, id string, err error) {
	rest, ok := strings.CutPrefix(h, "box:")
	t, i, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || t == "" || i == "" || strings.Contains(i, "/") {
		return "", "", api.Errorf(api.CodeBadRequest, "%q is not a box handle (box:<target>/<id>)", h)
	}
	return t, i, nil
}
