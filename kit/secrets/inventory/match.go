package inventory

import (
	"path"
	"strings"
)

// matchSegments reports whether name matches pattern, both split at sep. A
// segment is a path.Match pattern; a "**" segment matches any number of
// segments (also none). A malformed pattern is an error.
func matchSegments(pattern, name, sep string) (bool, error) {
	return match(strings.Split(pattern, sep), strings.Split(name, sep))
}

func match(pat, name []string) (bool, error) {
	if len(pat) == 0 {
		return len(name) == 0, nil
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if ok, err := match(pat[1:], name[i:]); ok || err != nil {
				return ok, err
			}
		}
		return false, nil
	}
	if len(name) == 0 {
		// Still check the pattern for syntax, so Validate sees a bad one.
		_, err := path.Match(pat[0], "")
		return false, err
	}
	ok, err := path.Match(pat[0], name[0])
	if err != nil || !ok {
		return false, err
	}
	return match(pat[1:], name[1:])
}

// literalPrefix is the leading segments of a pattern that have no wildcard.
func literalPrefix(pattern, sep string) []string {
	var out []string
	for _, s := range strings.Split(pattern, sep) {
		if strings.ContainsAny(s, `*?[\`) {
			break
		}
		out = append(out, s)
	}
	return out
}

// checkSegments is the first syntax error in any segment of pattern.
func checkSegments(pattern, sep string) error {
	for _, seg := range strings.Split(pattern, sep) {
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}
