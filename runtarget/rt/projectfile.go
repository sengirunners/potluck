package rt

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// SPEC 20 bindings: [[run_targets]] in project.toml, a section this module
// owns (project.RegisterSection). Keys zm knows are fields of
// computeapi.RunTarget; any other key is driver config (a string, or an
// integer or boolean written as one).
//
//	[[run_targets]]
//	name = "mac-vm"
//	driver = "tart"
//	kind = "run"
//	isolation = "ephemeral"
//	credentials = false
//	repo = "https://github.com/acme/app"
//	branch = "main"
//	scripts = ["test:e2e"]
//	budget = { unit = "vm_s", limit = 36000, period = "day" }
//	ttl_max_s = 3600
//	handles_max = 2
//	image = "macos-sequoia-xcode"   # driver config

// runTargetName is a binding's name: the <target> in handles, so no / or :.
var runTargetName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidName reports whether s is a binding name.
func ValidName(s string) bool { return runTargetName.MatchString(s) }

// driverName is potluck-driver-<driver>'s (or zm-run-<driver>'s) suffix: never a path.
var driverName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// secretKey names a config key that would hold a credential's value
// (SPEC 20: a credential reaches a target only through a *_file key).
var secretKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|api_?key|private_?key|credential|auth)`)

// pinnedImage is a box image pinned by digest.
var pinnedImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,200}@sha256:[0-9a-f]{64}$`)

// ImagePinned says whether s is a box image pinned by digest
// (name@sha256:<64 hex>), as a binding's images must be.
func ImagePinned(s string) bool { return pinnedImage.MatchString(s) }

// ImageName is a pinned image's name (before the digest).
func ImageName(pinned string) string {
	if i := strings.LastIndex(pinned, "@"); i > 0 {
		return pinned[:i]
	}
	return pinned
}

// secretInURL is a URL value carrying userinfo with a password.
var secretInURL = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/@\s]*:[^/@\s]*@`)

func parseRunTarget(where string, t map[string]any) (computeapi.RunTarget, error) {
	r := project.NewTOMLReader(where, t)
	rt := computeapi.RunTarget{Name: r.Str("name"), Driver: r.Str("driver"), Kind: r.Str("kind"),
		Isolation: r.Str("isolation"), Repo: r.Str("repo"), Branch: r.Str("branch"),
		Scripts: r.Strs("scripts"), CreateIn: r.Strs("create_in"), Images: r.Strs("images")}
	rt.Credentials = r.Bool("credentials")
	rt.AllowAnyRef = r.Bool("allow_any_ref")
	rt.TTLMaxS = int(r.Int("ttl_max_s"))
	rt.HandlesMax = int(r.Int("handles_max"))
	r.Mark("budget")
	if v, ok := t["budget"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return rt, fmt.Errorf("%s: budget must be an inline table { unit, limit, period }", where)
		}
		br := project.NewTOMLReader(where+" budget", m)
		rt.Budget = &computeapi.RunTargetBudget{Unit: br.Str("unit"), Limit: br.Int("limit"), Period: br.Str("period")}
		if err := br.Done(); err != nil {
			return rt, err
		}
	}
	for k, v := range t {
		if r.Seen(k) {
			continue
		}
		r.Mark(k)
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case int64:
			s = strconv.FormatInt(x, 10)
		case bool:
			s = strconv.FormatBool(x)
		default:
			return rt, fmt.Errorf("%s: driver config %s must be a string, integer or boolean", where, k)
		}
		if rt.Config == nil {
			rt.Config = map[string]string{}
		}
		rt.Config[k] = s
	}
	return rt, r.Done()
}

// ValidateRunTarget checks one binding against SPEC 20's binding rules that
// need no driver: names, kind, class, repo (one of remotes), scripts for a
// credentialed or unisolated class, budget shape and no secret values.
func ValidateRunTarget(remotes []string, t computeapi.RunTarget) error {
	bad := func(format string, a ...any) error {
		return api.Errorf(api.CodeBadRequest, "run target %q: "+format, append([]any{t.Name}, a...)...)
	}
	if !runTargetName.MatchString(t.Name) {
		return api.Errorf(api.CodeBadRequest, "run target name %q: lowercase letters, digits, . _ - (at most 63)", t.Name)
	}
	if !driverName.MatchString(t.Driver) {
		return bad("driver must be a driver's name (potluck-driver-<driver>), not a path")
	}
	if t.Kind != computeapi.RunTargetKindDB && t.Kind != computeapi.RunTargetKindRun && t.Kind != computeapi.RunTargetKindBox {
		return bad("kind must be db, run or box")
	}
	box := t.Kind == computeapi.RunTargetKindBox
	if t.Isolation != computeapi.RunTargetIsolationNone && t.Isolation != computeapi.RunTargetIsolationEphemeral {
		return bad("isolation must be none or ephemeral")
	}
	if box {
		// A box runs whatever its holder sends: untrusted by construction.
		if c := (computeapi.RunTargetClass{Isolation: t.Isolation, Credentials: t.Credentials}); !c.UntrustedCapable() {
			return bad("%s, and a box runs whatever its holder sends: want isolation ephemeral and credentials false", c.CannotIsolate("a box target"))
		}
		if len(t.Scripts) > 0 || t.AllowAnyRef {
			return bad("scripts and allow_any_ref do not apply to a box")
		}
		if (t.Repo == "") != (t.Branch == "") {
			return bad("a box serves ref only with both repo and branch")
		}
		if len(t.Images) == 0 {
			return bad("a box target needs images, each pinned by digest (name@sha256:<hex>)")
		}
	} else if len(t.Images) > 0 {
		return bad("images are for a box target")
	}
	for _, im := range t.Images {
		if !pinnedImage.MatchString(im) {
			return bad("image %q is not pinned by digest (name@sha256:<64 hex>)", im)
		}
	}
	if !box && (t.Repo == "" || t.Branch == "") {
		return bad("repo and branch are required")
	}
	if t.Repo != "" {
		found := false
		for _, r := range remotes {
			if project.NormalizeRemote(r) == project.NormalizeRemote(t.Repo) {
				found = true
			}
		}
		if !found {
			return bad("repo %s is not one of the project's remotes", t.Repo)
		}
		if strings.HasPrefix(t.Branch, "-") || strings.ContainsAny(t.Branch, " \t\n~^:?*[\\") {
			return bad("branch %q is not a branch name", t.Branch)
		}
	}
	for _, s := range t.Scripts {
		if err := CheckRunScript(s); err != nil {
			return bad("scripts: %v", err)
		}
	}
	if c := (computeapi.RunTargetClass{Isolation: t.Isolation, Credentials: t.Credentials}); t.Kind == computeapi.RunTargetKindRun && c.Guarded() && len(t.Scripts) == 0 {
		return bad("%s, so it needs scripts (the only commands it may run)", c.CannotIsolate("run target "+t.Name))
	}
	if t.TTLMaxS < 0 || t.HandlesMax < 0 {
		return bad("ttl_max_s and handles_max must not be negative")
	}
	if b := t.Budget; b != nil {
		if b.Unit == "" || b.Limit <= 0 || !ValidPeriod(b.Period) {
			return bad("budget needs unit, a positive limit and period day, week or month")
		}
	}
	if t.Kind != computeapi.RunTargetKindDB && len(t.CreateIn) > 0 {
		return bad("create_in is for a db target")
	}
	if t.Kind == computeapi.RunTargetKindDB && len(t.CreateIn) == 0 {
		return bad("a db target needs create_in (host:port/maintenance-db entries)")
	}
	for _, k := range sortedKeys(t.Config) {
		if k == "target" || k == "kind" {
			return bad("config key %s is zm's own", k)
		}
		if secretKey.MatchString(k) && !strings.HasSuffix(k, "_file") {
			return bad("config %s looks like a secret value: name a 0600 file with %s_file instead", k, k)
		}
		if secretInURL.MatchString(t.Config[k]) {
			return bad("config %s holds a password in a URL: name a 0600 file with a *_file key instead", k)
		}
	}
	return nil
}

// CheckRunScript checks a script name: a name the target resolves (an npm
// script, a make target, a repository path), never a shell string.
func CheckRunScript(s string) error {
	switch {
	case s == "" || len(s) > 200:
		return fmt.Errorf("script must be 1 to 200 characters")
	case strings.HasPrefix(s, "-"):
		return fmt.Errorf("script %q starts with -", s)
	case strings.ContainsAny(s, " \t\r\n;&|$`<>()'\"\\*?!{}"):
		return fmt.Errorf("script %q is a shell string, not a name", s)
	case strings.HasPrefix(s, "/") || path.Clean(s) != s || strings.HasPrefix(s, "../"):
		return fmt.Errorf("script %q is not a clean relative name", s)
	}
	return nil
}

func validateRunTargets(remotes []string, ts []computeapi.RunTarget) error {
	seen := map[string]bool{}
	for _, t := range ts {
		if err := ValidateRunTarget(remotes, t); err != nil {
			return err
		}
		if seen[t.Name] {
			return api.Errorf(api.CodeConflict, "run target %q defined twice", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

func encodeRunTarget(b *strings.Builder, t computeapi.RunTarget) {
	fmt.Fprintf(b, "\n[[run_targets]]\nname = %s\ndriver = %s\nkind = %s\nisolation = %s\ncredentials = %t\nrepo = %s\nbranch = %s\n",
		project.TOMLString(t.Name), project.TOMLString(t.Driver), project.TOMLString(t.Kind), project.TOMLString(t.Isolation), t.Credentials,
		project.TOMLString(t.Repo), project.TOMLString(t.Branch))
	if t.AllowAnyRef {
		b.WriteString("allow_any_ref = true\n")
	}
	if len(t.Scripts) > 0 {
		fmt.Fprintf(b, "scripts = %s\n", project.TOMLStrings(t.Scripts))
	}
	if t.Budget != nil {
		fmt.Fprintf(b, "budget = { unit = %s, limit = %d, period = %s }\n", project.TOMLString(t.Budget.Unit), t.Budget.Limit, project.TOMLString(t.Budget.Period))
	}
	if t.TTLMaxS > 0 {
		fmt.Fprintf(b, "ttl_max_s = %d\n", t.TTLMaxS)
	}
	if t.HandlesMax > 0 {
		fmt.Fprintf(b, "handles_max = %d\n", t.HandlesMax)
	}
	if len(t.CreateIn) > 0 {
		fmt.Fprintf(b, "create_in = %s\n", project.TOMLStrings(t.CreateIn))
	}
	if len(t.Images) > 0 {
		fmt.Fprintf(b, "images = %s\n", project.TOMLStrings(t.Images))
	}
	for _, k := range sortedKeys(t.Config) {
		fmt.Fprintf(b, "%s = %s\n", project.TOMLKey(k), project.TOMLString(t.Config[k]))
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// section is [[run_targets]] in project.toml: a []computeapi.RunTarget.
type section struct{}

func init() { project.RegisterSection("run_targets", section{}) }

func (section) Parse(where string, raw any) (any, error) {
	ts, ok := raw.([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: run_targets must be [[run_targets]] tables", where)
	}
	var out []computeapi.RunTarget
	for i, t := range ts {
		rt, err := parseRunTarget(fmt.Sprintf("%s [[run_targets]] #%d", where, i+1), t)
		if err != nil {
			return nil, err
		}
		out = append(out, rt)
	}
	return out, nil
}

func (section) Validate(v any, f project.File) error {
	ts, _ := v.([]computeapi.RunTarget)
	return validateRunTargets(f.Repos.Remotes, ts)
}

func (section) Encode(v any) string {
	ts, _ := v.([]computeapi.RunTarget)
	var b strings.Builder
	for _, t := range ts {
		encodeRunTarget(&b, t)
	}
	return b.String()
}

// ProjectTargets answers project id's bindings (a copy).
func ProjectTargets(r *project.Registry, id string) []computeapi.RunTarget {
	ts, _ := r.Section(id, "run_targets").([]computeapi.RunTarget)
	return append([]computeapi.RunTarget(nil), ts...)
}

// SetProjectTargets replaces project id's bindings with what fn returns and
// rewrites project.toml. Daemon-internal: the caller has checked that the
// machine's owner asked on the socket without a token.
func SetProjectTargets(ctx context.Context, r *project.Registry, id string, fn func(old []computeapi.RunTarget) ([]computeapi.RunTarget, error)) ([]computeapi.RunTarget, error) {
	v, err := r.SetSection(ctx, id, "run_targets", func(old any) (any, error) {
		ts, _ := old.([]computeapi.RunTarget)
		ts, err := fn(append([]computeapi.RunTarget(nil), ts...))
		if err != nil || len(ts) == 0 {
			return nil, err
		}
		return ts, nil
	})
	if err != nil {
		return nil, err
	}
	ts, _ := v.([]computeapi.RunTarget)
	return append([]computeapi.RunTarget(nil), ts...), nil
}
