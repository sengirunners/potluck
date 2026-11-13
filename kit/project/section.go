package project

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// Section is a part of project.toml another module owns (zm-eakh.12.35
// G4): a [[name]] or [name] table (RegisterSection) or a top-level key
// (RegisterKey) the project registry stores, validates and writes back
// without knowing what it holds. A table or top-level key no registered
// module owns is kept as decoded and written back unchanged (keys sorted),
// so a binary without that module loads the file and never drops it.
type Section interface {
	// Parse reads the decoded value (DecodeTOML): []map[string]any for
	// [[name]] tables, map[string]any for a [name] table; for a top-level
	// key a string, int64, bool, []any or map[string]any (an inline
	// table). where is the file's name, for errors.
	Parse(where string, raw any) (any, error)
	// Validate checks a parsed (or set) value against the whole file.
	Validate(v any, f File) error
	// Encode is the value as TOML: a section's tables with their headers,
	// a key's "name = value" line. "" writes nothing.
	Encode(v any) string
}

var (
	sectionsMu sync.RWMutex
	sections   = map[string]Section{}
	keys       = map[string]Section{}
)

// baseKey reports whether project.toml itself reads the top-level name.
func baseKey(name string) bool {
	switch name {
	case "id", "slug", "name", "paths", "remotes", "areas":
		return true
	}
	return false
}

// RegisterSection makes s own project.toml's section name (call it from an
// init). Registering a name twice (as a section or a key), or a name
// project.toml itself uses, panics.
func RegisterSection(name string, s Section) {
	register(name, s, false)
}

// RegisterKey makes s own project.toml's top-level key name (call it from
// an init): `name = value` before the first table. The same rules as
// RegisterSection. s.Encode writes the whole line, newline included.
func RegisterKey(name string, s Section) {
	register(name, s, true)
}

func register(name string, s Section, key bool) {
	sectionsMu.Lock()
	defer sectionsMu.Unlock()
	_, dupS := sections[name]
	_, dupK := keys[name]
	if dupS || dupK || baseKey(name) || !bareKey(name) || s == nil {
		panic("project: project.toml name " + name + " registered twice or reserved")
	}
	if key {
		keys[name] = s
	} else {
		sections[name] = s
	}
}

func sectionOf(name string) Section {
	sectionsMu.RLock()
	defer sectionsMu.RUnlock()
	return sections[name]
}

func keyOf(name string) Section {
	sectionsMu.RLock()
	defer sectionsMu.RUnlock()
	return keys[name]
}

// rawSection is a section or top-level key no registered module owns: the
// decoded value.
type rawSection struct{ v any }

// isSection reports whether a decoded root value is a table or tables.
func isSection(v any) bool {
	switch v.(type) {
	case []map[string]any, map[string]any:
		return true
	}
	return false
}

// parseOthers reads every top-level name of doc that r has not read yet
// (the ones project.toml itself does not use) into f.Keys and f.Sections:
// parsed by its owner, else kept as decoded. Nothing is refused for being
// unknown: another build's module may own it (a mixed fleet shares project
// files), and this build must load the file and write the name back.
func parseOthers(r *TOMLReader, doc tomlDoc, f *File) error {
	for _, k := range sortedSectionNames(doc) {
		if r.Seen(k) {
			continue
		}
		r.Mark(k)
		v := doc[k]
		if keyOf(k) == nil && sectionOf(k) == nil {
			// A name one slip away from one this binary reads is a typo,
			// not another build's setting: keeping it would silently drop
			// what the writer meant.
			if want := nearName(k); want != "" {
				return fmt.Errorf("project.toml: unknown name %q: did you mean %q?", k, want)
			}
		}
		// An owner reads its name whatever the value's shape (and refuses
		// the wrong one). An unowned table under a name that cannot be a
		// table header (a quoted key) is written back as an inline table.
		asKey := keyOf(k) != nil || (sectionOf(k) == nil && (!isSection(v) || !bareKey(k)))
		pv := any(rawSection{v})
		s := keyOf(k)
		if s == nil {
			s = sectionOf(k)
		}
		if s != nil {
			var err error
			if pv, err = s.Parse("project.toml", v); err != nil {
				return err
			}
		}
		if asKey {
			*f = f.WithKey(k, pv)
		} else {
			*f = f.WithSection(k, pv)
		}
	}
	return nil
}

// WithSection is f with section name set to v (nil removes it). f's own
// map is not changed, so a copy of a File can be edited safely.
func (f File) WithSection(name string, v any) File {
	f.Sections = withValue(f.Sections, name, v)
	return f
}

// WithKey is f with the top-level key name set to v (nil removes it); as
// WithSection.
func (f File) WithKey(name string, v any) File {
	f.Keys = withValue(f.Keys, name, v)
	return f
}

func withValue(m map[string]any, name string, v any) map[string]any {
	m = maps.Clone(m)
	if v == nil {
		delete(m, name)
		if len(m) == 0 {
			return nil
		}
		return m
	}
	if m == nil {
		m = map[string]any{}
	}
	m[name] = v
	return m
}

// nearName is the name this binary reads that k is a misspelling of: equal
// but for case, or one letter added, dropped or changed. "" for none. The
// names are project.toml's own and the ones modules have registered.
func nearName(k string) string {
	names := []string{"id", "slug", "name", "paths", "remotes", "areas"}
	sectionsMu.RLock()
	for n := range keys {
		names = append(names, n)
	}
	for n := range sections {
		names = append(names, n)
	}
	sectionsMu.RUnlock()
	sort.Strings(names)
	low := strings.ToLower(k)
	for _, n := range names {
		if n != k && oneEdit(low, n) {
			return n
		}
	}
	return ""
}

// oneEdit reports whether a and b are equal or one insertion, deletion or
// substitution apart.
func oneEdit(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 1 {
		return false
	}
	i := 0
	for i < len(a) && a[i] == b[i] {
		i++
	}
	if len(a) == len(b) {
		return i == len(a) || a[i+1:] == b[i+1:]
	}
	return a[i:] == b[i+1:]
}

// Unowned are f's top-level keys and sections that no module in this binary
// owns, by name: kept and written back, never checked.
func (f File) Unowned() []string {
	var out []string
	for _, m := range []map[string]any{f.Keys, f.Sections} {
		for name, v := range m {
			if _, raw := v.(rawSection); raw {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Unowned lists, by project slug, the project.toml names no module in this
// binary owns (File.Unowned); projects with none are left out.
func (r *Registry) Unowned() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string][]string{}
	for _, e := range r.byID {
		if names := e.file.Unowned(); len(names) > 0 {
			out[e.file.Slug] = names
		}
	}
	return out
}

// UnownedIn is Registry.Unowned read from the project.toml files under
// home, without a daemon. A file that does not load is skipped.
func UnownedIn(home string) map[string][]string {
	out := map[string][]string{}
	paths, _ := filepath.Glob(filepath.Join(home, store.DirProjects, "*", FileName))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if f, err := ParseFile(string(b)); err == nil && len(f.Unowned()) > 0 {
			out[f.Slug] = f.Unowned()
		}
	}
	return out
}

func validateSections(f File) error {
	for _, name := range sortedSectionNames(f.Keys) {
		if err := validateOwned(keyOf(name), f.Keys[name], f); err != nil {
			return err
		}
	}
	for _, name := range sortedSectionNames(f.Sections) {
		if err := validateOwned(sectionOf(name), f.Sections[name], f); err != nil {
			return err
		}
	}
	return nil
}

// validateOwned has owner s (nil: nobody) check v. A value still as decoded
// (its owner registered after the file was loaded) is parsed first, so an
// owner never sees anything but its own parsed value.
func validateOwned(s Section, v any, f File) error {
	if s == nil {
		return nil
	}
	if raw, ok := v.(rawSection); ok {
		var err error
		if v, err = s.Parse("project.toml", raw.v); err != nil {
			return err
		}
	}
	return s.Validate(v, f)
}

// encodeKeys writes the top-level keys other modules own or nobody knows,
// by name. They go before the first table: after one they would be read as
// that table's keys.
func encodeKeys(b *strings.Builder, f File) {
	for _, name := range sortedSectionNames(f.Keys) {
		v := f.Keys[name]
		if r, ok := v.(rawSection); ok {
			b.WriteString(TOMLKey(name) + " = " + tomlValue(r.v) + "\n")
			continue
		}
		if s := keyOf(name); s != nil {
			// A key's line without its newline would swallow the next one.
			if line := s.Encode(v); line != "" {
				b.WriteString(strings.TrimRight(line, "\n") + "\n")
			}
		}
	}
}

func encodeSections(b *strings.Builder, f File) {
	for _, name := range sortedSectionNames(f.Sections) {
		v := f.Sections[name]
		if r, ok := v.(rawSection); ok {
			switch x := r.v.(type) {
			case []map[string]any:
				for _, t := range x {
					fmt.Fprintf(b, "\n[[%s]]\n%s", name, tomlTable(t))
				}
			case map[string]any:
				fmt.Fprintf(b, "\n[%s]\n%s", name, tomlTable(x))
			}
			continue
		}
		if s := sectionOf(name); s != nil {
			b.WriteString(s.Encode(v))
		}
	}
}

func sortedSectionNames(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Section is f's parsed section name (nil when it has none, or no module
// owns name in this binary). Treat it as read-only.
func (f File) Section(name string) any { return owned(f.Sections[name]) }

// Key is f's parsed top-level key name; as Section.
func (f File) Key(name string) any { return owned(f.Keys[name]) }

func owned(v any) any {
	if _, raw := v.(rawSection); raw {
		return nil
	}
	return v
}

// Section is project id's parsed section name (nil when it has none, or
// no module owns name in this binary). Treat it as read-only.
func (r *Registry) Section(id, name string) any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.byID[id]
	if e == nil {
		return nil
	}
	return e.file.Section(name)
}

// Key is project id's parsed top-level key name; as Section.
func (r *Registry) Key(id, name string) any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.byID[id]
	if e == nil {
		return nil
	}
	return e.file.Key(name)
}

// SetSection replaces project id's section name with what fn returns (nil
// removes it), validates the file and rewrites project.toml. fn runs under
// the registry lock, so two concurrent changes never lose one another, and
// must not call the registry. Daemon-internal: the caller has checked who
// asked.
func (r *Registry) SetSection(ctx context.Context, id, name string, fn func(old any) (any, error)) (any, error) {
	return r.setOwned(ctx, id, name, false, fn)
}

// SetKey replaces project id's top-level key name; as SetSection.
func (r *Registry) SetKey(ctx context.Context, id, name string, fn func(old any) (any, error)) (any, error) {
	return r.setOwned(ctx, id, name, true, fn)
}

func (r *Registry) setOwned(ctx context.Context, id, name string, key bool, fn func(old any) (any, error)) (any, error) {
	if !key && sectionOf(name) == nil {
		return nil, api.Errorf(api.CodeInternal, "project.toml section %s has no owner in this binary", name)
	}
	if key && keyOf(name) == nil {
		return nil, api.Errorf(api.CodeInternal, "project.toml key %s has no owner in this binary", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.byID[id]
	if e == nil {
		return nil, api.NotFound("project", id)
	}
	old := e.file.Sections[name]
	owner := sectionOf(name)
	if key {
		old, owner = e.file.Keys[name], keyOf(name)
	}
	if raw, ok := old.(rawSection); ok {
		// Loaded before the owner registered: fn gets the owner's parsed
		// value, never the decoded one.
		var err error
		if old, err = owner.Parse("project.toml", raw.v); err != nil {
			return nil, err
		}
	}
	v, err := fn(old)
	if err != nil {
		return nil, err
	}
	f := e.file.WithSection(name, v)
	if key {
		f = e.file.WithKey(name, v)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.rewrite(id, e, f); err != nil {
		return nil, err
	}
	e.file = f
	return v, nil
}
