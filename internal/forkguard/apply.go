package forkguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNoJob is Apply's refusal when the workflow has no job of that id.
var ErrNoJob = errors.New("no such job")

// Change is what Apply did to one job's runs-on.
type Change struct {
	Text    []byte // the whole file after the edit; the input itself when Changed is false
	Changed bool   // false: the job already had exactly this guarded runs-on
	Line    int    // the 1-based line of the runs-on value
	Before  string // the runs-on value as it was written
	After   string // the value now written (Before when unchanged)
}

// Apply edits the workflow file src so job's runs-on is the guarded value (Expression) that keeps a fork's pull
// request on the GitHub-hosted label the job uses today and sends the repository's own runs to own. It changes that
// one value's text in place and nothing else: comments, quoting, order, line endings and the other jobs stay as
// they were, and the decoded workflow is checked to differ in that one field only.
//
// It refuses (an error, src unchanged) a job that does not exist (ErrNoJob), calls a reusable workflow, has no
// runs-on, or whose runs-on is not one GitHub-hosted label written on one line: a matrix or other expression, a
// guard with other labels, a runner group or labels mapping, a list of several labels, an anchor or alias. It also
// refuses a workflow with no `on` or with a trigger outsiders can start but pull_request, several YAML documents, merge keys in the
// job, or line separators other than LF and CRLF. A job that already has exactly this value is no change.
func Apply(src []byte, job string, own []string) (Change, error) {
	text := string(src)
	for _, r := range []string{"\u0085", " ", " "} {
		if strings.Contains(text, r) {
			return Change{}, fmt.Errorf("the file holds a line separator (%U) that hides lines; remove it first", []rune(r)[0])
		}
	}
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\r") {
		return Change{}, fmt.Errorf("the file holds a bare carriage return that hides lines; remove it first")
	}
	doc, err := oneDocument(src)
	if err != nil {
		return Change{}, err
	}
	if doc.Kind != yaml.MappingNode {
		return Change{}, fmt.Errorf("the workflow is not a YAML mapping")
	}
	jobs, err := Field(doc, "jobs")
	if err != nil {
		return Change{}, err
	}
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return Change{}, fmt.Errorf("the workflow has no jobs mapping")
	}
	j, err := Field(jobs, job)
	if err != nil {
		return Change{}, err
	}
	if j == nil {
		return Change{}, fmt.Errorf("%w %q; its jobs are %s", ErrNoJob, job, strings.Join(keysOf(jobs), ", "))
	}
	if j.Kind == yaml.AliasNode || j.Anchor != "" {
		return Change{}, fmt.Errorf("job %s is a YAML anchor or alias, which other places share; edit it by hand", job)
	}
	if j.Kind != yaml.MappingNode || j.Style&yaml.FlowStyle != 0 {
		return Change{}, fmt.Errorf("job %s is not a block mapping; write it as one key per line first", job)
	}
	if m, _ := Field(j, "<<"); m != nil {
		return Change{}, fmt.Errorf("job %s uses a YAML merge key (<<), which can hide its runs-on; edit it by hand", job)
	}
	if u, err := Field(j, "uses"); err != nil {
		return Change{}, err
	} else if u != nil {
		return Change{}, fmt.Errorf("job %s calls a reusable workflow: its runners are chosen in the workflow it calls", job)
	}
	ro, err := Field(j, "runs-on")
	if err != nil {
		return Change{}, err
	}
	if ro == nil {
		return Change{}, fmt.Errorf("job %s has no runs-on", job)
	}
	events, err := Triggers(doc)
	if err != nil {
		return Change{}, err
	}
	if bad := unwritable(events); len(bad) > 0 {
		return Change{}, fmt.Errorf("the workflow also runs on %s; outside people can start those, so the guard cannot keep the job off the lent Mac (only push, schedule, workflow_dispatch and pull_request are written here); move job %s to a workflow without them",
			strings.Join(bad, ", "), job)
	}
	if ro.Kind == yaml.AliasNode || ro.Anchor != "" {
		return Change{}, fmt.Errorf("job %s's runs-on is a YAML anchor or alias, which other places share; edit it by hand", job)
	}
	expr, err := Expression("ubuntu-latest", own) // validates own; the fork label is filled in below
	if err != nil {
		return Change{}, err
	}
	var hosted string
	switch ro.Kind {
	case yaml.MappingNode:
		return Change{}, fmt.Errorf("job %s's runs-on names a runner group or a labels mapping; only a single GitHub-hosted label can be guarded", job)
	case yaml.SequenceNode:
		if len(ro.Content) != 1 || ro.Content[0].Kind != yaml.ScalarNode || ro.Style&yaml.FlowStyle == 0 {
			return Change{}, fmt.Errorf("job %s's runs-on is a list of labels; only a single GitHub-hosted label, on one line, can be guarded", job)
		}
		hosted = ro.Content[0].Value
	case yaml.ScalarNode:
		if ro.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return Change{}, fmt.Errorf("job %s's runs-on is a block scalar; write it on one line first", job)
		}
		hosted = ro.Value
		if g, ok := Parse(ro.Value); ok {
			if g.JSON && slices.Equal(g.Own, own) {
				return Change{Text: src, Line: ro.Line, Before: ro.Value, After: ro.Value}, nil
			}
			have := strings.Join(g.Own, ", ")
			if g.Routed != "" {
				have = g.Routed + ", else " + have
			}
			return Change{}, fmt.Errorf("job %s's runs-on is already fork-guarded, with other labels (%s); edit it by hand to change them", job, have)
		}
		switch {
		case strings.Contains(ro.Value, "matrix."):
			return Change{}, fmt.Errorf("job %s's runs-on comes from its matrix (%s); move the Mac case into its own job first", job, ro.Value)
		case strings.Contains(ro.Value, "${{"):
			return Change{}, fmt.Errorf("job %s's runs-on is an expression (%s) that is not the fork guard; edit it by hand", job, ro.Value)
		}
	default:
		return Change{}, fmt.Errorf("job %s's runs-on is not readable", job)
	}
	if !Hosted(hosted) {
		return Change{}, fmt.Errorf("job %s runs on %q, not a GitHub-hosted label, so a fork's pull request would have no GitHub runner to stay on", job, hosted)
	}
	if expr, err = Expression(hosted, own); err != nil {
		return Change{}, err
	}

	for i := 0; i+1 < len(j.Content); i += 2 {
		if k := j.Content[i]; Key(k.Value) == "runs-on" && (k.Line != ro.Line || k.Style != 0 && k.Style != yaml.SingleQuotedStyle && k.Style != yaml.DoubleQuotedStyle) {
			return Change{}, fmt.Errorf("job %s's runs-on is not written as `runs-on: <label>` on one line; write it that way first", job)
		}
	}
	if ro.Style&yaml.TaggedStyle != 0 || ro.Kind == yaml.ScalarNode && ro.Tag != "!!str" {
		return Change{}, fmt.Errorf("job %s's runs-on carries a YAML tag; remove it first", job)
	}
	lines := strings.SplitAfter(text, "\n")
	if ro.Line < 1 || ro.Line > len(lines) {
		return Change{}, fmt.Errorf("job %s's runs-on is not readable", job)
	}
	line := lines[ro.Line-1]
	eol := ""
	for _, e := range []string{"\r\n", "\n"} {
		if strings.HasSuffix(line, e) {
			eol, line = e, strings.TrimSuffix(line, e)
			break
		}
	}
	runes := []rune(line)
	if ro.Column < 1 || ro.Column > len(runes) {
		return Change{}, fmt.Errorf("job %s's runs-on is not readable", job)
	}
	prefix, rest := string(runes[:ro.Column-1]), string(runes[ro.Column-1:])
	n := valueLength(rest)
	if n <= 0 {
		return Change{}, fmt.Errorf("job %s's runs-on is not one value on one line; write it as `runs-on: %s` first", job, hosted)
	}
	before, tail := rest[:n], rest[n:]
	if t := strings.TrimLeft(tail, " \t"); t != "" && !strings.HasPrefix(t, "#") {
		return Change{}, fmt.Errorf("job %s's runs-on is not one value on one line; write it as `runs-on: %s` first", job, hosted)
	}
	if !sameValue(before, ro) {
		return Change{}, fmt.Errorf("job %s's runs-on is not one value on one line; write it as `runs-on: %s` first", job, hosted)
	}
	lines[ro.Line-1] = prefix + expr + tail + eol
	out := []byte(strings.Join(lines, ""))
	if err := onlyRunsOnDiffers(src, out, job, expr); err != nil {
		return Change{}, fmt.Errorf("the edit would change more than job %s's runs-on (%v); edit it by hand", job, err)
	}
	return Change{Text: out, Changed: true, Line: ro.Line, Before: before, After: expr}, nil
}

// oneDocument decodes src, which must hold exactly one YAML document.
func oneDocument(src []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("the workflow is not valid YAML: %v", err)
	}
	var more yaml.Node
	if err := dec.Decode(&more); err != io.EOF {
		return nil, fmt.Errorf("the file holds more than one YAML document; GitHub reads one")
	}
	if len(root.Content) != 1 {
		return nil, fmt.Errorf("the workflow is empty")
	}
	return root.Content[0], nil
}

func keysOf(n *yaml.Node) []string {
	var out []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, n.Content[i].Value)
	}
	return out
}

// eventNames is a workflow's `on` as trigger names: one, a list, or a mapping's keys.
func eventNames(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, v := range n.Content {
			out = append(out, v.Value)
		}
		return out
	case yaml.MappingNode:
		return keysOf(n)
	}
	return []string{"an unreadable `on`"}
}

// unwritable is the triggers among events on which Apply does not write the guard: any an outsider can start but
// pull_request. pull_request_review and pull_request_review_comment carry a pull request, so Holds counts them, but
// an outsider can review a pull request from the repository's own branch and start its job on the lent Mac.
func unwritable(events []string) []string {
	var out []string
	for _, e := range events {
		if !trusted[e] && e != "pull_request" {
			out = append(out, e)
		}
	}
	return out
}

// valueLength is how many bytes at the start of s are one YAML flow value: a quoted scalar, a one-line flow
// sequence, or a plain scalar up to a comment or the end. 0 when none ends on this line.
func valueLength(s string) int {
	if s == "" {
		return 0
	}
	switch s[0] {
	case '\'':
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				return i + 1
			}
		}
		return 0
	case '"':
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				return i + 1
			}
		}
		return 0
	case '[':
		if i := strings.IndexByte(s, ']'); i > 0 {
			return i + 1
		}
		return 0
	}
	end := len(s)
	if i := strings.Index(s, " #"); i >= 0 {
		end = i
	}
	if i := strings.Index(s, "\t#"); i >= 0 && i < end {
		end = i
	}
	return len(strings.TrimRight(s[:end], " \t"))
}

// sameValue reports whether text, decoded alone, is the value n.
func sameValue(text string, n *yaml.Node) bool {
	var d yaml.Node
	if yaml.Unmarshal([]byte("v: "+text+"\n"), &d) != nil || len(d.Content) != 1 || len(d.Content[0].Content) != 2 {
		return false
	}
	v := d.Content[0].Content[1]
	if v.Kind != n.Kind || v.Value != n.Value || len(v.Content) != len(n.Content) {
		return false
	}
	for i := range v.Content {
		if v.Content[i].Kind != n.Content[i].Kind || v.Content[i].Value != n.Content[i].Value {
			return false
		}
	}
	return true
}

// onlyRunsOnDiffers checks that after, decoded, is before with jobs.<job>.runs-on set to expr and nothing else.
func onlyRunsOnDiffers(before, after []byte, job, expr string) error {
	var a, b map[string]any
	if err := yaml.Unmarshal(before, &a); err != nil {
		return err
	}
	if err := yaml.Unmarshal(after, &b); err != nil {
		return err
	}
	jobs, _ := keyed(a, "jobs").(map[string]any)
	j, _ := keyed(jobs, job).(map[string]any)
	if j == nil {
		return fmt.Errorf("job not found again")
	}
	for k := range j {
		if Key(k) == "runs-on" {
			j[k] = expr
		}
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("other values changed")
	}
	return nil
}

// keyed is m's value under key, compared through Key; nil when absent.
func keyed(m map[string]any, key string) any {
	for k, v := range m {
		if Key(k) == Key(key) {
			return v
		}
	}
	return nil
}
