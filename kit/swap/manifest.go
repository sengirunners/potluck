package swap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// Database kinds, as BuildManifest.Schema names them.
const (
	KindRegistry = "registry"
	KindProject  = "project"
	KindIssues   = "issues"
	KindReplica  = "replica"
)

// Databases maps every SQLite file zm keeps under home (relative path) to
// its kind: registry.db, projects/<id>/project.db and issues.db, and the
// replica copies under replicas/.
func Databases(home string) map[string]string {
	out := map[string]string{}
	if _, err := os.Stat(filepath.Join(home, store.FileRegistry)); err == nil {
		out[store.FileRegistry] = KindRegistry
	}
	add := func(glob, kind string) {
		ms, _ := filepath.Glob(filepath.Join(home, glob))
		for _, m := range ms {
			if rel, err := filepath.Rel(home, m); err == nil {
				out[rel] = kind
			}
		}
	}
	add(filepath.Join(store.DirProjects, "*", store.FileProject), KindProject)
	add(filepath.Join(store.DirProjects, "*", "issues.db"), KindIssues)
	add(filepath.Join("replicas", "*.db"), KindReplica)
	return out
}

// Applied lists each database's applied migrations, read only.
func Applied(ctx context.Context, home string) (map[string][]string, error) {
	out := map[string][]string{}
	for rel := range Databases(home) {
		names, err := store.AppliedIn(ctx, filepath.Join(home, rel))
		if err != nil {
			return out, fmt.Errorf("%s: %w", rel, err)
		}
		out[rel] = names
	}
	return out, nil
}

// NewBuild describes a build from its route patterns, its team proxy rule
// and the migrations it registers per kind.
func NewBuild(routes []string, proxies func(method, path string) bool, schema map[string][]string) api.BuildManifest {
	b := api.BuildManifest{Protocol: api.WireProtocol, Version: api.Version, Commit: api.Commit, Schema: schema}
	b.Routes = append([]string{}, routes...)
	sort.Strings(b.Routes)
	b.Proxied = []string{}
	for _, p := range b.Routes {
		if m, path, ok := samplePath(p); ok && proxies(m, path) {
			b.Proxied = append(b.Proxied, p)
		}
	}
	return b
}

// samplePath turns "GET /v0/work/{id}" into GET and /v0/work/x.
func samplePath(pattern string) (method, path string, ok bool) {
	method, path, ok = strings.Cut(pattern, " ")
	if !ok {
		return "", "", false
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") {
			segs[i] = "x"
		}
	}
	return method, strings.Join(segs, "/"), true
}

// LegacyProxied are the routes a joined project needs forwarded when the
// running daemon predates manifests and cannot say what it forwards: the
// reads bd and zm make.
var LegacyProxied = []string{"GET /v0/issues", "GET /v0/issues/projects", "GET /v0/issues/{id}", "GET /v0/work"}

// Running is what a preflight knows of the running daemon: nil Manifest
// when it predates manifests (Health and Joined are then all there is).
type Running struct {
	Health   api.Health
	Manifest *api.DaemonManifest
	Joined   []string
}

// neverForwarded are patterns an older build listed as forwarded only
// because its sample path ("x") read as a session address: no real call
// to them was ever forwarded, so a build that does not list them loses
// nothing (zm-eakh.12.35.9).
var neverForwarded = map[string]bool{"GET /v0/sessions/{id}/activity": true}

// Check is the preflight: can build take over from running (nil: no daemon
// answers) on a home whose databases have applied migrations?
func Check(build api.BuildManifest, running *Running, applied map[string][]string, kinds map[string]string) api.Preflight {
	pf := api.Preflight{Build: buildName(build.Version, build.Commit), Problems: []string{}, Warnings: []string{}}
	// Schema: data written by a newer build.
	pf.Problems = append(pf.Problems, Newer(build.Schema, applied, kinds)...)
	if pend := Pending(build.Schema, applied, kinds); len(pend) > 0 {
		pf.Notes = append(pf.Notes, fmt.Sprintf("this build applies %d migrations at its first start (%s): the swap is one-way; "+
			"once they are applied the previous build cannot read the data, so a new daemon that answers but fails its verify is kept (rolled_forward) instead of rolled back",
			len(pend), strings.Join(pend, ", ")))
	}
	if running == nil {
		pf.Notes = append(pf.Notes, "no daemon answers on this home: checked its databases only")
		pf.OK = len(pf.Problems) == 0
		return pf
	}
	h := running.Health
	pf.Running = buildName(h.Version, h.Commit)
	if !api.SameProtocol(h.Protocol, build.Protocol) {
		pf.Problems = append(pf.Problems, fmt.Sprintf("the running daemon speaks %s, this build %s", h.Protocol, build.Protocol))
	}
	have := set(build.Routes)
	forwards := set(build.Proxied)
	joined := running.Joined
	if m := running.Manifest; m != nil {
		joined = m.Joined
	}
	var needFwd []string
	if m := running.Manifest; m != nil {
		needFwd = m.Build.Proxied
	} else {
		needFwd = LegacyProxied
		pf.Notes = append(pf.Notes, "the running daemon predates manifests: checked its protocol, the databases and what joined projects need")
	}
	if len(joined) > 0 {
		for _, p := range needFwd {
			if !forwards[p] && !neverForwarded[p] {
				pf.Problems = append(pf.Problems, fmt.Sprintf("joined projects (%s): the running build forwards %s to their authority; this build does not",
					strings.Join(joined, ", "), p))
			}
		}
	}
	if m := running.Manifest; m != nil {
		used := set(m.Used)
		for _, p := range m.Build.Routes {
			switch {
			case have[p]:
			case used[p]:
				pf.Problems = append(pf.Problems, fmt.Sprintf("%s was answered since the daemon started; this build lacks it", p))
			default:
				pf.Warnings = append(pf.Warnings, fmt.Sprintf("%s is not in this build (unused since the daemon started)", p))
			}
		}
	}
	pf.OK = len(pf.Problems) == 0
	return pf
}

// Newer lists, for each database in applied, the migrations a build with
// schema (migration names per kind) does not know: data a newer build
// wrote. A kind schema does not list is skipped.
func Newer(schema, applied map[string][]string, kinds map[string]string) []string {
	known := map[string]map[string]bool{}
	for k, names := range schema {
		known[k] = set(names)
	}
	var out []string
	for _, f := range sortedKeys(applied) {
		k := kinds[f]
		if known[k] == nil {
			continue
		}
		for _, n := range applied[f] {
			if !known[k][n] {
				out = append(out, fmt.Sprintf("%s has migration %s, which this build does not know: the data is newer than the build", f, n))
			}
		}
	}
	return out
}

// Pending lists the migrations a build with schema registers that some
// database in applied lacks (each name once, with its kind): what that
// build applies when it starts on this home.
func Pending(schema, applied map[string][]string, kinds map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range sortedKeys(applied) {
		k := kinds[f]
		have := set(applied[f])
		for _, n := range schema[k] {
			if !have[n] && !seen[k+" "+n] {
				seen[k+" "+n] = true
				out = append(out, k+" "+n)
			}
		}
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func buildName(version, commit string) string {
	if commit == "" {
		return version
	}
	return version + " (" + commit + ")"
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
