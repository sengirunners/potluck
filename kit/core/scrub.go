package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Scrub is a redaction (SPEC "Native issues": comment redact) every module
// applies to its own copies of a text that must stop existing: a leaked
// secret, personal data, abuse. The issues module runs it after it has
// overwritten the comment and its history.
type Scrub struct {
	Project  string   // project ULID
	Subjects []string // what copies are filed under: the issue id, its work ids
	Texts    []string // every version of the text (the comment and its edits)
	// At is when the redaction happened, the same on every retry of it
	// (zero: unknown, treat as now). A copy made after it cannot hold text
	// already gone at the source: the session module does not hold a task
	// started later unless that task's own copies show the text
	// (zm-8tp5.50).
	At time.Time
}

// Scrubber overwrites a module's copies of sc's texts with Redacted. It must
// be idempotent and touch nothing but those texts.
type Scrubber func(ctx context.Context, sc Scrub) error

// Redacted replaces a scrubbed text.
const Redacted = "[redacted]"

// MinUnscoped is the shortest text a scrubber may look for in rows not
// filed under one of the scrub's subjects (a short text could match
// unrelated content).
const MinUnscoped = 8

// AddScrubber registers a module's scrubber (at wiring time).
func (d *Deps) AddScrubber(f Scrubber) {
	d.scrubMu.Lock()
	d.scrubbers = append(d.scrubbers, f)
	d.scrubMu.Unlock()
}

// BackupScrubber blanks a module's secrets in tx, a transaction on a copy
// of registry.db staged for an off-site set, never the live file (zm-jpv3):
// a signing key must not leave the machine with the backup. It must keep
// the schema and every other value as they are.
type BackupScrubber func(ctx context.Context, tx *sql.Tx) error

// AddBackupScrubber registers a module's backup scrubber (at wiring time).
func (d *Deps) AddBackupScrubber(f BackupScrubber) {
	d.scrubMu.Lock()
	d.backupScrubbers = append(d.backupScrubbers, f)
	d.scrubMu.Unlock()
}

// BackupScrubbers are the registered backup scrubbers.
func (d *Deps) BackupScrubbers() []BackupScrubber {
	d.scrubMu.Lock()
	defer d.scrubMu.Unlock()
	return append([]BackupScrubber(nil), d.backupScrubbers...)
}

// ScrubAll runs every module's scrubber; all run, and their errors are
// joined.
func (d *Deps) ScrubAll(ctx context.Context, sc Scrub) error {
	d.scrubMu.Lock()
	fs := append([]Scrubber(nil), d.scrubbers...)
	d.scrubMu.Unlock()
	var errs []error
	for _, f := range fs {
		errs = append(errs, f(ctx, sc))
	}
	return errors.Join(errs...)
}

// ScrubText replaces every text in s, raw and as it appears inside a JSON
// string, with Redacted. minLen skips texts shorter than it (0: none).
func ScrubText(s string, texts []string, minLen int) string {
	for _, t := range texts {
		if t == "" || len(t) < minLen {
			continue
		}
		s = strings.ReplaceAll(s, t, Redacted)
		if b, err := json.Marshal(t); err == nil {
			if enc := string(b[1 : len(b)-1]); enc != t {
				s = strings.ReplaceAll(s, enc, Redacted)
			}
		}
	}
	return s
}

// Contains reports whether s holds any of texts (raw or JSON-escaped) at
// least minLen long: a cheap filter before ScrubText.
func Contains(s string, texts []string, minLen int) bool {
	return ScrubText(s, texts, minLen) != s
}

// TextKeys are the JSON keys whose string values hold user text (a comment's
// body, an edit's old and new text). ScrubJSON touches only those, so a
// comment that says exactly "done" leaves a status field alone.
var TextKeys = map[string]bool{"body": true, "text": true}

// ScrubJSON replaces every string value under a TextKeys key (directly, or
// in an array under one) in js that equals one of texts, any length, with
// Redacted, leaving keys, other fields and the rest of the document alone:
// for copies filed under the redacted item, whose text fields hold the
// redacted text exactly. js that is not JSON is returned as is.
func ScrubJSON(js string, texts []string) (string, bool) {
	want := map[string]bool{}
	for _, t := range texts {
		if t != "" {
			want[t] = true
		}
	}
	if len(want) == 0 {
		return js, false
	}
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return js, false
	}
	changed := false
	var walk func(x any, textual bool) any
	walk = func(x any, textual bool) any {
		switch t := x.(type) {
		case string:
			if textual && want[t] {
				changed = true
				return Redacted
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i], textual) // [old, new] under "text"
			}
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k], TextKeys[k])
			}
		}
		return x
	}
	v = walk(v, false)
	if !changed {
		return js, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return js, false
	}
	return string(b), true
}
