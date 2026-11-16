package inventory

import (
	"fmt"
	"slices"
)

// Severity of a Finding: a Fail makes the check fail, a Warn does not.
type Severity string

const (
	Fail Severity = "FAIL"
	Warn Severity = "warn"
)

// FindingKind says what a Finding is about.
type FindingKind string

// Finding kinds.
const (
	// FindUnlisted: a key in a home that no row lists.
	FindUnlisted FindingKind = "unlisted"
	// FindBelowClass: a key held where its class does not allow, with no exception.
	FindBelowClass FindingKind = "below-class"
	// FindLoose: a key file that group or others can read, below class O.
	FindLoose FindingKind = "loose-mode"
	// FindLiteral: config.json holds a key itself where a reference belongs.
	FindLiteral FindingKind = "literal"
	// FindAgents: a wide key sits in a home that runs agent sessions.
	FindAgents FindingKind = "agents-can-read"
)

// Finding is one thing the check found. Fix is a plain sentence for the
// person who reads it.
type Finding struct {
	Severity Severity
	Kind     FindingKind
	Row      string // row ID, "" for an unlisted key
	Where    string
	Detail   string
	Fix      string
	// Exception is the bead that will move the key, when the row has an
	// exception; set on the agents warning.
	Exception string
}

// Report is the result of one check.
type Report struct {
	Held     []Held
	Findings []Finding
}

// Failed is whether any finding is a Fail.
func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Severity == Fail })
}

// Of returns the findings of one severity.
func (r Report) Of(s Severity) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == s {
			out = append(out, f)
		}
	}
	return out
}

// Check scans the home and judges what it holds against the table. An error
// means the check could not run (a bad table, an unreadable home), which a
// caller must not read as a pass.
func Check(h Home, t Table) (Report, error) {
	held, err := Scan(h, t)
	if err != nil {
		return Report{}, err
	}
	rows := map[string]Row{}
	for _, r := range t.Rows {
		rows[r.ID] = r
	}
	rep := Report{Held: held}
	warned := map[string]bool{}
	for _, k := range held {
		if k.Row == "" {
			rep.Findings = append(rep.Findings, Finding{
				Severity: Fail, Kind: FindUnlisted, Where: k.Where,
				Detail: fmt.Sprintf("%s is a key with no row in the inventory", k.Where),
				Fix:    "Add a row for it (class, owner bead) to the key inventory. If it is a copy a write left behind, delete it; if a write is still running, run the check again.",
			})
			continue
		}
		row := rows[k.Row]
		bead, excused := t.Exceptions[row.ID]
		if k.Literal {
			rep.Findings = append(rep.Findings, Finding{
				Severity: Fail, Kind: FindLiteral, Row: row.ID, Where: k.Where,
				Detail: fmt.Sprintf("%s holds the %s itself, not a reference to it", k.Where, row.Name),
				Fix:    "Put the key in a 0600 file or the keychain and write file:PATH or keychain:SERVICE in its place.",
			})
		}
		if k.Loose {
			rep.Findings = appendOnce(rep.Findings, warned, "loose:"+k.where(), Finding{
				Severity: Fail, Kind: FindLoose, Row: row.ID, Where: k.Where,
				Detail: fmt.Sprintf("%s (%s) can be read by group or others", k.where(), row.Name),
				Fix:    "Run chmod 600 on " + k.where() + ".",
			})
		}
		// Anything found in a home sits at class O; a class P or S key is
		// held below its class there.
		// An excused row is held below its class on purpose until its bead
		// lands; the agents warning below still tells the owner.
		if ClassO.Rank() < row.Class.Rank() && !k.Literal && !excused {
			rep.Findings = appendOnce(rep.Findings, warned, "below:"+row.ID, Finding{
				Severity: Fail, Kind: FindBelowClass, Row: row.ID, Where: k.Where,
				Detail: fmt.Sprintf("%s is class %s but sits at class O in %s, where any process of this user can read it", row.Name, row.Class, k.Where),
				Fix:    "Move the key to its class's home, or list an exception with the bead that will.",
			})
		}
		if row.WarnWithAgents && h.RunsAgentSessions {
			rep.Findings = appendOnce(rep.Findings, warned, "agents:"+row.ID, Finding{
				Severity: Warn, Kind: FindAgents, Row: row.ID, Where: k.Where, Exception: bead,
				Detail: fmt.Sprintf("this home holds the %s and also runs agent sessions; every agent session can read %s", row.Name, k.Where),
				Fix:    "Keep this key on a home where no agent session runs, or move it to its class's home" + moveBy(bead) + ".",
			})
		}
	}
	return rep, nil
}

// appendOnce adds f unless key was added before: one finding of a kind per
// row, however many places the row's key is held.
func appendOnce(fs []Finding, seen map[string]bool, key string, f Finding) []Finding {
	if seen[key] {
		return fs
	}
	seen[key] = true
	return append(fs, f)
}

func moveBy(bead string) string {
	if bead == "" {
		return ""
	}
	return " (" + bead + " does that)"
}

// where names the file to fix: the file a reference points at, else the place.
func (k Held) where() string {
	if k.Target != "" {
		return k.Target
	}
	return k.Where
}
