package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/kit/secrets"
)

// Home is the home a check reads.
type Home struct {
	// Dir is the home directory ($ZM_HOME).
	Dir string
	// RunsAgentSessions is whether agent sessions run as the OS user that
	// owns this home. The caller knows; the package never guesses it.
	RunsAgentSessions bool
}

// Held is one key a home holds.
type Held struct {
	// Row is the ID of the row it belongs to; "" is an unlisted key.
	Row string
	// Where is its place: a path relative to the home, or "config.json
	// <dotted.path>" for a config entry.
	Where string
	// Kind is how it was found.
	Kind LocationKind
	// Loose is set when a file that holds the key is readable by group or
	// others, which is below class O.
	Loose bool
	// Target is the file a config reference names outside the home, when one
	// does.
	Target string
	// Literal is set when config.json holds the key itself where a secret
	// reference belongs.
	Literal bool
}

// Scan finds the keys a home holds: the files and config entries that a row's
// location matches, the files that a row's config reference names inside the
// home, and every file under a root that no row matches. It reads names, modes
// and config.json's structure, never a key. A home that is not an existing
// absolute directory is an error, never an empty result.
func Scan(h Home, t Table) ([]Held, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(h.Dir) {
		return nil, fmt.Errorf("inventory: home %q is not an absolute path", h.Dir)
	}
	if fi, err := os.Stat(h.Dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("inventory: home %q is not a directory: %v", h.Dir, err)
	}
	s := scanner{home: h.Dir, t: t, refTargets: map[string][]string{}}
	if err := s.config(); err != nil {
		return nil, err
	}
	if err := s.files(); err != nil {
		return nil, err
	}
	slices.SortFunc(s.held, func(a, b Held) int {
		return strings.Compare(a.Where+"\x00"+a.Row, b.Where+"\x00"+b.Row)
	})
	return s.held, nil
}

type scanner struct {
	home string
	t    Table
	held []Held
	// refTargets are the files inside the home that a row's config reference
	// names, by relative path: the row they belong to wherever they sit.
	refTargets map[string][]string
	// configLoose is whether config.json is readable by group or others,
	// which makes every key written into it loose.
	configLoose bool
}

func (s *scanner) files() error {
	inRoot := map[string]bool{}
	var rels []string
	walk := func(dir string, root bool) error {
		return s.walk(dir, root, inRoot, &rels)
	}
	for _, r := range s.t.Roots {
		if err := walk(r, true); err != nil {
			return err
		}
	}
	for _, row := range s.t.Rows {
		for _, l := range row.Locations {
			if l.Kind != KindFile {
				continue
			}
			segs := strings.Split(l.Pattern, "/")
			pre := literalPrefix(l.Pattern, "/")
			// A literal path is read alone; a pattern walks only the
			// directory its literal start names.
			if len(pre) == len(segs) {
				pre = segs
			}
			if err := walk(strings.Join(pre, "/"), false); err != nil {
				return err
			}
		}
	}
	for rel := range s.refTargets {
		if err := walk(rel, false); err != nil {
			return err
		}
	}
	slices.Sort(rels)
	rels = slices.Compact(rels)
	for _, rel := range rels {
		loose, ok, err := s.regularFile(rel)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		rowIDs := slices.Clone(s.refTargets[rel])
		for _, row := range s.t.Rows {
			for _, l := range row.Locations {
				if l.Kind != KindFile {
					continue
				}
				if m, _ := matchSegments(l.Pattern, rel, "/"); m {
					rowIDs = append(rowIDs, row.ID)
				}
			}
		}
		slices.Sort(rowIDs)
		rowIDs = slices.Compact(rowIDs)
		for _, id := range rowIDs {
			s.held = append(s.held, Held{Row: id, Where: rel, Kind: KindFile, Loose: loose})
		}
		// Ignore excuses only a file no row lists, never a listed key.
		if ignored, _ := s.matchAny(s.t.Ignore, rel); len(rowIDs) == 0 && inRoot[rel] && !ignored {
			s.held = append(s.held, Held{Where: rel, Kind: KindFile, Loose: loose})
		}
	}
	return nil
}

// walk adds to rels every regular file under dir (a slash path under the
// home), following a symlink to a directory so a key kept behind one is still
// seen; a directory already visited by its real path is not entered twice.
func (s *scanner) walk(dir string, root bool, inRoot map[string]bool, rels *[]string) error {
	visited := map[string]bool{}
	var rec func(rel, real string) error
	rec = func(rel, real string) error {
		fi, err := os.Stat(real)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) { // absent, or a dangling link
				return nil
			}
			return err
		}
		if !fi.IsDir() {
			if fi.Mode().IsRegular() {
				if root {
					inRoot[rel] = true
				}
				*rels = append(*rels, rel)
			}
			return nil
		}
		resolved, err := filepath.EvalSymlinks(real)
		if err != nil {
			return err
		}
		// A link to a directory under a root is an alias: the root walk
		// reaches that directory by its own path, and listing it twice would
		// name a listed key by a path no row matches. A link to anywhere else
		// is followed, its files counted as content of the directory it sits in.
		if rel != dir && s.isLink(real) && s.underARoot(resolved) {
			return nil
		}
		if visited[resolved] {
			return nil
		}
		visited[resolved] = true
		entries, err := os.ReadDir(real)
		if err != nil {
			return err
		}
		for _, e := range entries {
			child := e.Name()
			if rel != "" {
				child = rel + "/" + e.Name()
			}
			if err := rec(child, filepath.Join(real, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return rec(dir, filepath.Join(s.home, filepath.FromSlash(dir)))
}

func (s *scanner) matchAny(patterns []string, rel string) (bool, error) {
	for _, p := range patterns {
		if m, err := matchSegments(p, rel, "/"); m || err != nil {
			return m, err
		}
	}
	return false, nil
}

// regularFile stats rel (following a symlink) and reports whether it is a
// regular file and whether group or others can read it.
func (s *scanner) regularFile(rel string) (loose, ok bool, err error) {
	fi, err := os.Stat(filepath.Join(s.home, filepath.FromSlash(rel)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) { // a dangling link
			return false, false, nil
		}
		return false, false, err
	}
	if !fi.Mode().IsRegular() {
		return false, false, nil
	}
	return looseMode(fi.Mode()), true, nil
}

func looseMode(m fs.FileMode) bool {
	return runtime.GOOS != "windows" && m.Perm()&0o077 != 0
}

// config reads config.json as plain JSON, so a field this package has never
// heard of that holds a secret reference is still found. A path is a list of
// keys, so a key that contains a dot is one segment.
func (s *scanner) config() error {
	b, err := os.ReadFile(filepath.Join(s.home, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("inventory: config.json: %w", err)
	}
	if fi, err := os.Stat(filepath.Join(s.home, "config.json")); err == nil {
		s.configLoose = looseMode(fi.Mode())
	}
	s.node(nil, doc)
	return nil
}

// node judges one value of config.json at path and descends into it.
func (s *scanner) node(path []string, v any) {
	if len(path) > 0 && v != nil {
		rowID, kind := s.configRow(path)
		where := "config.json " + strings.Join(path, ".")
		str, isStr := v.(string)
		switch {
		case isStr && str == "":
			return
		case kind == KindConfigValue:
			s.held = append(s.held, Held{Row: rowID, Where: where, Kind: kind, Loose: s.configLoose})
			return
		case kind == KindConfigRef && !isStr:
			// An object, list, number or bool where a reference belongs.
			s.held = append(s.held, Held{Row: rowID, Where: where, Kind: kind, Literal: true, Loose: s.configLoose})
			return
		case isStr:
			ref, err := secrets.Parse(str)
			switch {
			case err == nil:
				h := Held{Row: rowID, Where: where, Kind: KindConfigRef}
				if ref.Scheme == "file" {
					if rel, in := s.inHome(ref.Name); in {
						if rowID != "" {
							s.refTargets[rel] = append(s.refTargets[rel], rowID)
						}
					} else {
						h.Loose = looseFile(ref.Name)
						h.Target = ref.Name
					}
				}
				s.held = append(s.held, h)
			case kind == KindConfigRef:
				s.held = append(s.held, Held{Row: rowID, Where: where, Kind: kind, Literal: true, Loose: s.configLoose})
			}
			return
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			s.node(append(path[:len(path):len(path)], k), c)
		}
	case []any:
		for i, c := range x {
			s.node(append(path[:len(path):len(path)], strconv.Itoa(i)), c)
		}
	}
}

// inHome is p as a path relative to the home, when p is inside it. A path is
// compared as written first, then with symlinks resolved, so a home reached
// through a link and the same home spelled by its real path agree.
func (s *scanner) inHome(p string) (string, bool) {
	under := func(home, p string) (string, bool) {
		rel, err := filepath.Rel(home, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", false
		}
		return filepath.ToSlash(rel), true
	}
	if rel, ok := under(s.home, p); ok {
		return rel, true
	}
	realHome, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		return "", false
	}
	realP, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return under(realHome, realP)
}

// looseFile is whether the file at p is readable by group or others.
func looseFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && looseMode(fi.Mode())
}

// configRow is the row a config path belongs to ("" and "" for none).
func (s *scanner) configRow(path []string) (string, LocationKind) {
	for _, row := range s.t.Rows {
		for _, l := range row.Locations {
			if l.Kind == KindFile {
				continue
			}
			if m, _ := match(strings.Split(l.Pattern, "."), path); m {
				return row.ID, l.Kind
			}
		}
	}
	return "", ""
}

// isLink is whether p is itself a symbolic link.
func (s *scanner) isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

// underARoot is whether the resolved path p is a table root's real directory
// or inside one.
func (s *scanner) underARoot(p string) bool {
	for _, r := range s.t.Roots {
		real, err := filepath.EvalSymlinks(filepath.Join(s.home, filepath.FromSlash(r)))
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(real, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}
