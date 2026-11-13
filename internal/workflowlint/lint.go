// Package workflowlint holds the fork-safe CI lint (zm-eakh.12.23.8). In a public repository a fork's pull request
// must never reach a self-hosted or Sengi runner and never run with a write token. The lint fails when, in
// .github/workflows/*.yml:
//   - a workflow a fork can trigger (any trigger but push, schedule, workflow_dispatch) has a job whose runs-on is
//     not a GitHub-hosted label or a fork-guarded value as internal/forkguard reads it (the one rule potluck ci
//     doctor and potluck ci pr share), or a job that calls a reusable workflow (its runners cannot be checked
//     here). The guarded forms are ${{ GUARD && '<hosted>' || '<label>' }}, the routed one with
//     needs.<job>.outputs.<name> before the literal label, and potluck ci pr's
//     ${{ fromJSON(GUARD && '["<hosted>"]' || '["<label>", ...]') }}: when the guard holds, the hosted label is
//     taken and the rest is never read;
//   - pull_request_target appears anywhere outside a comment, in any letter case;
//   - a workflow has no top-level permissions, or grants a write scope there (a job asks for more on its own);
//     the fork guard counts only on pull_request, pull_request_review and pull_request_review_comment triggers,
//     the ones whose payload has github.event.pull_request;
//   - a permissions block (top level, or a job of a fork-reachable workflow) has a flow map or a scope but read/none,
//     or is empty;
//   - the file holds a bare CR, U+0085, U+2028 or U+2029, which hide lines from this reader.
//
// It reads these workflows' simple layout (block style, jobs at two spaces, one-line runs-on) and fails closed on any
// other shape. Blind spots: it reads this tree's files, and a fork's PR can rewrite them, so the repository's
// fork-approval setting and runner-group access are the real fence; it does not evaluate expressions beyond the
// guard pattern, and does not know larger (org-defined) hosted runner labels.
package workflowlint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sengirunners/potluck/internal/forkguard"
	"gopkg.in/yaml.v3"
)

var keyRe = regexp.MustCompile(`^(\s*)(["']?[A-Za-z_][\w-]*["']?):(?:\s+(.*))?$`)

func uncomment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return ""
	}
	return strings.TrimRight(regexp.MustCompile(`(^|\s)#.*$`).ReplaceAllString(line, ""), " \t\r")
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == v[len(v)-1] && (v[0] == '\'' || v[0] == '"') {
		return v[1 : len(v)-1]
	}
	return v
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

type block struct {
	inline string
	body   []string
}

// keys splits lines into the keys at exactly this indent and the lines under each. The map is keyed by
// forkguard.Key (look a key up by its keyword: on, jobs, runs-on); the order holds the keys as written. Any line that is neither such a
// key nor under one is a shape this lint does not read: an error.
func keys(lines []string, indent int) (map[string]*block, []string, error) {
	out := map[string]*block{}
	var order []string
	var cur *block
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			if cur != nil {
				cur.body = append(cur.body, l)
			}
			continue
		}
		if strings.Contains(l, "\t") && strings.TrimLeft(l, " ") != strings.TrimLeft(l, " \t") {
			return nil, nil, fmt.Errorf("tab indentation: %q", l)
		}
		ind := indentOf(l)
		if ind == indent {
			m := keyRe.FindStringSubmatch(l)
			if m == nil {
				return nil, nil, fmt.Errorf("unreadable line at indent %d: %q", indent, l)
			}
			k := unquote(m[2])
			if out[forkguard.Key(k)] != nil {
				return nil, nil, fmt.Errorf("duplicate key %q", k)
			}
			cur = &block{inline: strings.TrimSpace(m[3])}
			out[forkguard.Key(k)] = cur
			order = append(order, k)
		} else if ind > indent && cur != nil {
			cur.body = append(cur.body, l)
		} else {
			return nil, nil, fmt.Errorf("line at indent %d, want %d: %q", ind, indent, l)
		}
	}
	return out, order, nil
}

func triggers(on *block) ([]string, error) {
	in := on.inline
	switch {
	case strings.HasPrefix(in, "{"):
		return nil, fmt.Errorf("`on` in flow-map style: not read here")
	case strings.HasPrefix(in, "["):
		if !strings.HasSuffix(in, "]") {
			return nil, fmt.Errorf("`on` list not on one line")
		}
		var out []string
		for _, p := range strings.Split(strings.Trim(in, "[]"), ",") {
			if p = unquote(p); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	case in != "":
		return []string{unquote(in)}, nil
	}
	var body []string
	for _, l := range on.body {
		if strings.TrimSpace(l) != "" {
			body = append(body, l)
		}
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("`on` is empty")
	}
	base := indentOf(body[0])
	if strings.HasPrefix(strings.TrimSpace(body[0]), "- ") {
		var out []string
		for _, l := range body {
			t := strings.TrimSpace(l)
			if indentOf(l) != base || !strings.HasPrefix(t, "- ") {
				return nil, fmt.Errorf("`on` list line not read: %q", l)
			}
			out = append(out, unquote(strings.TrimPrefix(t, "- ")))
		}
		return out, nil
	}
	_, order, err := keys(body, base)
	return order, err
}

// checkPerms reports problems in one permissions value. Scopes must be read or none; a job whose workflow no fork can
// trigger may also say write (allowWrite).
func checkPerms(where string, b *block, allowWrite bool, fail func(string, ...any)) {
	if b.inline != "" {
		switch unquote(b.inline) {
		case "{}", "read-all":
		case "write-all":
			if !allowWrite {
				fail("%s permissions write-all", where)
			}
		default:
			fail("%s permissions %q not read (flow map or unknown): use a block of read/none scopes", where, b.inline)
		}
		return
	}
	if firstIndent(b.body) == 0 {
		fail("%s permissions is empty", where)
		return
	}
	scopes, _, err := keys(b.body, firstIndent(b.body))
	if err != nil {
		fail("%s permissions not read: %v", where, err)
		return
	}
	for k, v := range scopes {
		switch val := unquote(v.inline); {
		case val == "read" || val == "none":
		case val == "write" && allowWrite:
		default:
			fail("%s permission %s: %q, want read or none", where, k, v.inline)
		}
	}
}

// Lint returns the problems in one workflow file's text; none means it passes.
func Lint(src string) (problems []string) {
	fail := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	src = strings.ReplaceAll(src, "\r\n", "\n")
	for _, r := range []string{"\r", "\u0085", "\u2028", "\u2029"} {
		if strings.Contains(src, r) {
			fail("contains a line separator (%U) the line reader does not split on", []rune(r)[0])
			return
		}
	}
	var lines []string
	for _, l := range strings.Split(src, "\n") {
		lines = append(lines, uncomment(l))
	}
	for i, l := range lines { // a leading document marker
		if strings.TrimSpace(l) == "" {
			continue
		}
		if l == "---" {
			lines[i] = ""
		}
		break
	}
	if strings.Contains(strings.ToLower(strings.Join(lines, "\n")), "pull_request_target") {
		fail("uses pull_request_target")
	}
	top, _, err := keys(lines, 0)
	if err != nil {
		fail("layout not read: %v", err)
		return
	}
	onb := top["on"]
	if onb == nil {
		fail("no `on` key")
		return
	}
	trig, err := triggers(onb)
	if err != nil {
		fail("%v", err)
		return
	}
	forkReachable, guardOK := forkguard.Reachable(trig), forkguard.Holds(trig)

	if p := top["permissions"]; p == nil {
		fail("no top-level permissions")
	} else {
		checkPerms("top-level", p, false, fail)
	}

	jobs := top["jobs"]
	if jobs == nil || jobs.inline != "" || firstIndent(jobs.body) != 2 {
		fail("jobs not a block at two-space indent")
		return
	}
	jm, order, err := keys(jobs.body, 2)
	if err != nil {
		fail("jobs not read: %v", err)
		return
	}
	for _, name := range order {
		fields, _, err := keys(jm[forkguard.Key(name)].body, 4)
		if err != nil {
			fail("job %s not read: %v", name, err)
			continue
		}
		if p := fields["permissions"]; p != nil {
			checkPerms("job "+name, p, !forkReachable, fail)
		}
		if !forkReachable {
			continue
		}
		if fields["uses"] != nil {
			fail("job %s calls a reusable workflow: its runners cannot be checked", name)
			continue
		}
		ro := fields["runs-on"]
		if ro == nil {
			fail("job %s has no runs-on", name)
			continue
		}
		v, ok := scalar(ro.inline)
		if !(ok && forkguard.Hosted(v)) && !(ok && guardOK && guarded(v)) {
			fail("job %s: runs-on %q is not a GitHub-hosted label (or the fork guard, on pull_request triggers only)", name, ro.inline)
		}
	}
	return
}

func firstIndent(lines []string) int {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return indentOf(l)
		}
	}
	return 0
}

// scalar is a one-line YAML value as YAML decodes it (quotes and escapes resolved), when it is a scalar.
func scalar(inline string) (string, bool) {
	var n yaml.Node
	if yaml.Unmarshal([]byte(inline), &n) != nil || len(n.Content) != 1 || n.Content[0].Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Content[0].Value, true
}

func guarded(v string) bool {
	_, ok := forkguard.Parse(v)
	return ok
}
