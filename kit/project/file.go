package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// FileName is project.toml, in $ZM_HOME/projects/<ulid>/ and (the
// repo-local form) in <repo>/.zm/.
const FileName = "project.toml"

// File is $ZM_HOME/projects/<ulid>/project.toml: the project's config.
// The registry row is authoritative for id and slug; the daemon rewrites this
// file on every change (hand edits need a daemon restart).
//
//	id = "01J9Z..."
//	slug = "acme"
//	name = "Acme"
//	paths = ["~/code/acme"]
//	remotes = ["github.com/acme/app"]
//
//	[[areas]]
//	name = "api"
//	paths = ["~/code/acme/services/api"]
//	work = ["bd:api-*"]
//
// Every other top-level key and table belongs to a module (section.go) or
// to nobody in this binary; both are kept and written back.
type File struct {
	ID    string
	Slug  string
	Name  string
	Repos api.RepoRules
	Areas []api.Area
	// Sections are the [[name]]/[name] tables other modules own
	// (section.go), by name: run targets' [[run_targets]] (runtarget).
	// Tables no module in this binary owns are here too, kept as decoded.
	Sections map[string]any
	// Keys are the top-level keys other modules own (RegisterKey), by
	// name, and the ones no module in this binary owns, kept as decoded.
	Keys map[string]any
}

// ParseFile decodes a project.toml.
func ParseFile(src string) (File, error) {
	doc, err := DecodeTOML(src)
	if err != nil {
		return File{}, err
	}
	r := NewTOMLReader("project.toml", doc)
	f := File{ID: r.Str("id"), Slug: r.Str("slug"), Name: r.Str("name")}
	f.Repos.Paths, f.Repos.Remotes = r.Strs("paths"), r.Strs("remotes")
	for i, t := range r.Tables("areas") {
		ar := NewTOMLReader(fmt.Sprintf("project.toml [[areas]] #%d", i+1), t)
		a := api.Area{Name: ar.Str("name"), Work: ar.Strs("work"), Labels: ar.Strs("labels")}
		a.Paths, a.Remotes = ar.Strs("paths"), ar.Strs("remotes")
		if err := ar.Done(); err != nil {
			return File{}, err
		}
		f.Areas = append(f.Areas, a)
	}
	if err := parseOthers(r, doc, &f); err != nil {
		return File{}, err
	}
	if err := r.Done(); err != nil {
		return File{}, err
	}
	return f, f.Validate()
}

// Validate checks names and uniqueness.
func (f File) Validate() error {
	if !api.ValidSlug(f.Slug) {
		return api.Errorf(api.CodeBadRequest, "bad project slug %q (lowercase letters, digits, . _ -; not none/all/owners)", f.Slug)
	}
	seen := map[string]bool{}
	for _, a := range f.Areas {
		if !api.ValidSlug(a.Name) {
			return api.Errorf(api.CodeBadRequest, "bad area name %q", a.Name)
		}
		if seen[a.Name] {
			return api.Errorf(api.CodeConflict, "area %q defined twice", a.Name)
		}
		seen[a.Name] = true
	}
	return validateSections(f)
}

// withSettings is f with req's slug and name applied (nil fields kept);
// repo rules are the registry's (machine owner).
func (f File) withSettings(req api.UpdateProjectRequest) File {
	if req.Slug != nil {
		f.Slug = strings.ToLower(strings.TrimSpace(*req.Slug))
	}
	if req.Name != nil {
		f.Name = strings.TrimSpace(*req.Name)
	}
	return f
}

// Encode renders f as TOML (deterministic).
func (f File) Encode() string {
	var b strings.Builder
	b.WriteString("# zm project config. Change it with `zm project`/`zm area`; hand edits need a daemon restart.\n")
	fmt.Fprintf(&b, "id = %s\nslug = %s\nname = %s\n", TOMLString(f.ID), TOMLString(f.Slug), TOMLString(f.Name))
	if len(f.Repos.Paths) > 0 {
		fmt.Fprintf(&b, "paths = %s\n", TOMLStrings(f.Repos.Paths))
	}
	if len(f.Repos.Remotes) > 0 {
		fmt.Fprintf(&b, "remotes = %s\n", TOMLStrings(f.Repos.Remotes))
	}
	encodeKeys(&b, f)
	for _, a := range f.Areas {
		fmt.Fprintf(&b, "\n[[areas]]\nname = %s\n", TOMLString(a.Name))
		if len(a.Paths) > 0 {
			fmt.Fprintf(&b, "paths = %s\n", TOMLStrings(a.Paths))
		}
		if len(a.Remotes) > 0 {
			fmt.Fprintf(&b, "remotes = %s\n", TOMLStrings(a.Remotes))
		}
		if len(a.Work) > 0 {
			fmt.Fprintf(&b, "work = %s\n", TOMLStrings(a.Work))
		}
		if len(a.Labels) > 0 {
			fmt.Fprintf(&b, "labels = %s\n", TOMLStrings(a.Labels))
		}
	}
	encodeSections(&b, f)
	return b.String()
}

// RepoFile is <repo>/.zm/project.toml: names the project (by slug, which
// is portable between machines; ULIDs are per machine) and optionally an area.
//
//	project = "acme"
//	area = "api"
type RepoFile struct {
	Project string
	Area    string
}

// RepoFileDir is the directory a RepoFile lives in, relative to the repo root.
const RepoFileDir = ".zm"

// ParseRepoFile decodes a .zm/project.toml.
func ParseRepoFile(src string) (RepoFile, error) {
	doc, err := DecodeTOML(src)
	if err != nil {
		return RepoFile{}, err
	}
	r := NewTOMLReader(".zm/project.toml", doc)
	f := RepoFile{Project: r.Str("project"), Area: r.Str("area")}
	if err := r.Done(); err != nil {
		return RepoFile{}, err
	}
	if f.Project == "" {
		return f, fmt.Errorf(".zm/project.toml: project is required")
	}
	return f, nil
}

// Encode renders the repo file.
func (f RepoFile) Encode() string {
	s := "# Which zm project this repo belongs to (slug). Written by `zm project use`.\nproject = " + TOMLString(f.Project) + "\n"
	if f.Area != "" {
		s += "area = " + TOMLString(f.Area) + "\n"
	}
	return s
}

// WriteRepoFile writes <root>/.zm/project.toml.
func WriteRepoFile(root string, f RepoFile) (string, error) {
	dir := filepath.Join(root, RepoFileDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, FileName)
	return p, os.WriteFile(p, []byte(f.Encode()), 0o644)
}
