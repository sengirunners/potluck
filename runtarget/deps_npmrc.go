package runtarget

// A project's .npmrc beside its lockfile changes how npm ci resolves the
// tree (onCall's legacy-peer-deps=true: without it npm ci stops on
// ERESOLVE). It is an npm layer's input, but a builder gets nothing of the
// caller's beyond public inputs, and an .npmrc is where npm keeps
// credentials and registries. So it is read as syntax (key=value lines,
// comments, blank lines) and only keys that change resolution and carry no
// secret pass; any other key is refused by name, its value never echoed.
// What the builder gets, and what the key holds, is the file rewritten to
// those key=value lines: no comment, spacing or other byte of the caller's.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// npmrcFile is the .npmrc an npm layer takes: the project's, beside its
// lockfile. npm reads no workspace's own.
const npmrcFile = ".npmrc"

// npmrcKeys are the .npmrc keys an npm layer takes, each with the values it
// takes. None names a registry, a credential, a script or a path, and no
// value is free text (npm expands ${VAR} in values; none of these values
// has one).
var npmrcKeys = map[string][]string{
	"legacy-peer-deps": npmrcBool,
	"strict-peer-deps": npmrcBool,
	"engine-strict":    npmrcBool,
	"install-links":    npmrcBool,
	"save-exact":       npmrcBool,
	"include":          npmrcDepTypes,
	"omit":             npmrcDepTypes,
	"install-strategy": {"hoisted", "nested", "shallow", "linked"},
}

var (
	npmrcBool     = []string{"true", "false"}
	npmrcDepTypes = []string{"dev", "optional", "peer"}
)

// npmrcCanon reads an .npmrc as syntax and answers it as the builder gets
// it: each key=value line, in order, as "key=value\n" (include and omit as
// npm's list form, key[]=value, as they may be written). A line that is not
// a comment (# or ;), blank or key=value, a key not in npmrcKeys, or a
// value the key does not take is refused, naming the line and key only.
func npmrcCanon(b []byte) ([]byte, error) {
	var out strings.Builder
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, fmt.Errorf("%s line %d is not key=value, a comment or blank", npmrcFile, i+1)
		}
		name, list := strings.CutSuffix(k, "[]")
		vals, known := npmrcKeys[name]
		switch {
		case !known:
			return nil, fmt.Errorf("%s line %d: key %s is not one an npm layer takes (it takes %s; never a registry or a credential)", npmrcFile, i+1, shownKey(k), strings.Join(npmrcKeyNames(), ", "))
		case list && !slices.Equal(vals, npmrcDepTypes):
			return nil, fmt.Errorf("%s line %d: key %s is not a list", npmrcFile, i+1, name)
		case !slices.Contains(vals, v):
			return nil, fmt.Errorf("%s line %d: key %s takes %s", npmrcFile, i+1, name, strings.Join(vals, ", "))
		}
		out.WriteString(k + "=" + v + "\n")
	}
	return []byte(out.String()), nil
}

// npmrcKeyNames are npmrcKeys' names, sorted, for a refusal.
func npmrcKeyNames() []string {
	var out []string
	for k := range npmrcKeys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// shownKey is a refused key as a message shows it, quoted: a key is a name,
// but one scoped to a registry URL (//user:pass@host/:_auth) or a line like
// "<token> x=y" would put a secret in the key, so only a plain config name
// is shown (letters, digits, - _ . and an @scope: prefix), never one with a
// / or a space.
func shownKey(k string) string {
	if len(k) > 64 || !shownKeyRe.MatchString(k) {
		return "(not shown: not a plain name, so it may hold a URL or a secret)"
	}
	return fmt.Sprintf("%q", k)
}

var shownKeyRe = regexp.MustCompile(`^(@[A-Za-z0-9._-]+:)?[A-Za-z0-9._-]+(\[\])?$`)
