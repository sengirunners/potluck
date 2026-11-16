// Package inventory is the secrets key inventory as code: every key a home
// holds has a row with a class, and Check reports a key that has no row, a key
// held below its class, and a key that sits where an agent session can read it.
//
// The classes and the rules are the key inventory decision of 2026-11-15
// (zm docs/decisions/2026-11-15-secrets-key-inventory.md). Potluck lists its
// own keys (Potluck); a program that keeps more keys in the same home (zm)
// adds its rows with Table.With and calls Check. The package reads paths,
// file modes and secret references only; it never reads a key's bytes.
package inventory

import (
	"fmt"
	"strings"
)

// Class is where a key may live. Rank orders them by how well the place
// protects the key: a key held at a lower rank than its class is a finding.
type Class string

const (
	// ClassP is person-present: hardware that needs a touch, never a file.
	ClassP Class = "P"
	// ClassS is service-isolated: an account or environment no agent session
	// runs in.
	ClassS Class = "S"
	// ClassO is owner-user at rest: the keychain, or a 0600 file in a 0700
	// directory. Every process of the OS user can read it, agents included.
	ClassO Class = "O"
	// ClassE is ephemeral in a box, through the secrets door.
	ClassE Class = "E"
)

// Rank is 3 for P, 2 for S, 1 for O and 0 for E; an unknown class is -1.
func (c Class) Rank() int {
	switch c {
	case ClassP:
		return 3
	case ClassS:
		return 2
	case ClassO:
		return 1
	case ClassE:
		return 0
	}
	return -1
}

// LocationKind is how a Location is searched for in a home.
type LocationKind string

const (
	// KindFile is a file under the home. Pattern is a slash path relative to
	// the home; a segment may use path.Match wildcards and a "**" segment
	// matches any number of segments.
	KindFile LocationKind = "file"
	// KindConfigRef is a secret reference (kit/secrets: file:, keychain:,
	// env:) at a path in config.json, dot-separated, "*" matching one segment.
	KindConfigRef LocationKind = "config-ref"
	// KindConfigValue is a secret written as a plain value at a path in
	// config.json, in the same notation.
	KindConfigValue LocationKind = "config-value"
)

// Location is one place a key can be found in a home.
type Location struct {
	Kind    LocationKind
	Pattern string
}

// Row is one key of the inventory.
type Row struct {
	// ID is the row's stable name; exceptions are keyed by it.
	ID string
	// Name says what the key is, in plain words (shown in findings).
	Name  string
	Class Class
	// Locations are where a home may hold it. A class E row has none.
	Locations []Location
	// Bead is the work item that owns the row's verdict (reduce, move).
	Bead string
	// WarnWithAgents makes a home that holds this key and also runs agent
	// sessions get a warning: the key is wide, and every agent session of the
	// owner can read it.
	WarnWithAgents bool
}

// Table is the inventory a check runs against.
type Table struct {
	Rows []Row
	// Roots are directories of the home (slash paths) where every file is a
	// key: a file there that no row's location matches is unlisted. A key
	// kept elsewhere is found only through a row.
	Roots []string
	// Ignore are file patterns (as KindFile) that are not keys.
	Ignore []string
	// Exceptions maps a row ID to the bead that moves its key to its class.
	// A row with an exception does not fail for being held below its class;
	// nothing else is excused.
	Exceptions map[string]string
}

// With returns a copy of t with rows, roots and ignore patterns added.
func (t Table) With(rows []Row, roots, ignore []string) Table {
	t.Rows = append(append([]Row(nil), t.Rows...), rows...)
	t.Roots = append(append([]string(nil), t.Roots...), roots...)
	t.Ignore = append(append([]string(nil), t.Ignore...), ignore...)
	ex := make(map[string]string, len(t.Exceptions))
	for k, v := range t.Exceptions {
		ex[k] = v
	}
	t.Exceptions = ex
	return t
}

// WithExceptions returns a copy of t with exceptions added.
func (t Table) WithExceptions(more map[string]string) Table {
	t = t.With(nil, nil, nil)
	for k, v := range more {
		t.Exceptions[k] = v
	}
	return t
}

// Validate refuses a table whose rows cannot be checked: a duplicate or empty
// ID, an unknown class, a class E row with locations, a bad pattern, or an
// exception that names no row or no bead.
func (t Table) Validate() error {
	seen := map[string]bool{}
	for _, r := range t.Rows {
		switch {
		case r.ID == "":
			return fmt.Errorf("inventory: a row has no id (%q)", r.Name)
		case seen[r.ID]:
			return fmt.Errorf("inventory: row %q is listed twice", r.ID)
		case r.Class.Rank() < 0:
			return fmt.Errorf("inventory: row %q has class %q, want P, S, O or E", r.ID, r.Class)
		case r.Class == ClassE && len(r.Locations) > 0:
			return fmt.Errorf("inventory: row %q is class E and so has no place in a home", r.ID)
		}
		seen[r.ID] = true
		for _, l := range r.Locations {
			if err := l.validate(); err != nil {
				return fmt.Errorf("inventory: row %q: %w", r.ID, err)
			}
		}
	}
	for id, bead := range t.Exceptions {
		if !seen[id] {
			return fmt.Errorf("inventory: exception for %q, which is no row", id)
		}
		if row := t.row(id); row.Class.Rank() <= ClassO.Rank() {
			return fmt.Errorf("inventory: exception for %q, which is class %s and is never held below its class", id, row.Class)
		}
		if bead == "" || strings.ContainsAny(bead, " \t\n") {
			return fmt.Errorf("inventory: exception for %q needs a bead id, got %q", id, bead)
		}
	}
	for _, p := range t.Roots {
		if !plainSegments(p) || len(literalPrefix(p, "/")) != len(strings.Split(p, "/")) {
			return fmt.Errorf("inventory: root %q must be a plain directory path under the home", p)
		}
	}
	for _, p := range t.Ignore {
		if !plainSegments(p) {
			return fmt.Errorf("inventory: ignore pattern %q must be a path under the home", p)
		}
		if err := checkSegments(p, "/"); err != nil {
			return fmt.Errorf("inventory: ignore pattern %q: %w", p, err)
		}
	}
	return nil
}

func (t Table) row(id string) Row {
	for _, r := range t.Rows {
		if r.ID == id {
			return r
		}
	}
	return Row{}
}

func (l Location) validate() error {
	switch l.Kind {
	case KindFile:
		if !plainSegments(l.Pattern) {
			return fmt.Errorf("file pattern %q must be a path under the home", l.Pattern)
		}
		// The scan walks the directory a pattern's literal start names; with
		// no literal start that is the whole home.
		if len(literalPrefix(l.Pattern, "/")) == 0 {
			return fmt.Errorf("file pattern %q must start with a directory or file name", l.Pattern)
		}
	case KindConfigRef, KindConfigValue:
	default:
		return fmt.Errorf("unknown location kind %q", l.Kind)
	}
	if l.Pattern == "" {
		return fmt.Errorf("a %s location has no pattern", l.Kind)
	}
	sep := "/"
	if l.Kind != KindFile {
		sep = "."
	}
	return checkSegments(l.Pattern, sep)
}

// plainSegments is whether p is a slash path with no empty, "." or ".."
// segment (and so no leading or trailing slash).
func plainSegments(p string) bool {
	if p == "" {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}
