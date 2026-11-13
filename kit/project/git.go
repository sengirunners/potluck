package project

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GitRoot walks up from dir to the nearest directory holding .git (a
// directory, or a file for worktrees). "" when dir is not in a repository.
func GitRoot(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// OriginURL reads remote "origin"'s url from the repository at root without
// running git (worktrees: .git file -> gitdir -> commondir). "" if none.
func OriginURL(root string) string {
	gitDir := filepath.Join(root, ".git")
	if fi, err := os.Stat(gitDir); err == nil && !fi.IsDir() {
		b, err := os.ReadFile(gitDir)
		if err != nil {
			return ""
		}
		d, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return ""
		}
		d = strings.TrimSpace(d)
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		gitDir = d
		if c, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			cd := strings.TrimSpace(string(c))
			if !filepath.IsAbs(cd) {
				cd = filepath.Join(gitDir, cd)
			}
			gitDir = cd
		}
	}
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = strings.EqualFold(strings.Join(strings.Fields(line), " "), `[remote "origin"]`)
			continue
		}
		if in {
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormalizeRemote turns any git remote form into "host/owner/repo":
// https://github.com/o/r.git, ssh://git@github.com/o/r, git@github.com:o/r.git
// -> github.com/o/r. The host is lowercased; the path keeps its case.
func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if at := strings.IndexByte(u, '@'); at >= 0 && at < strings.IndexByte(u+"/", '/') {
			u = u[at+1:]
		}
		host, path, _ := strings.Cut(u, "/")
		if h, _, ok := strings.Cut(host, ":"); ok { // drop a port
			host = h
		}
		u = strings.ToLower(host) + "/" + path
	} else if m := scpLike.FindStringSubmatch(u); m != nil {
		u = strings.ToLower(m[1]) + "/" + m[2]
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return u
}

// ExpandPath expands a leading ~ and resolves symlinks where the path exists
// (so /tmp and /private/tmp on macOS compare equal).
func ExpandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// under reports whether dir is root or inside it; both already expanded.
func under(dir, root string) bool {
	return root != "" && (dir == root || strings.HasPrefix(dir, strings.TrimSuffix(root, "/")+"/"))
}

// IsPathGlob reports whether a path rule is a glob ("~/code/acme*").
func IsPathGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

// expandRule expands a path rule like ExpandPath; a glob keeps its pattern
// part and only its fixed leading directories are resolved.
func expandRule(p string) string {
	if !IsPathGlob(p) {
		return ExpandPath(p)
	}
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	parts := strings.Split(p, "/")
	i := 0
	for i < len(parts) && !IsPathGlob(parts[i]) {
		i++
	}
	fixed := strings.Join(parts[:i], "/")
	if fixed == "" {
		fixed = "/"
	}
	if r, err := filepath.EvalSymlinks(fixed); err == nil {
		fixed = r
	}
	return filepath.Join(append([]string{fixed}, parts[i:]...)...)
}

// PathMatch reports whether dir (already expanded) is inside path rule p,
// returning the length of the matched root (for longest-match). A rule may
// be a glob whose * matches within one path element: "~/code/acme*"
// matches ~/code/acme-clean/src but not ~/code/other.
func PathMatch(dir, p string) (int, bool) {
	root := expandRule(p)
	if root == "" || dir == "" {
		return 0, false
	}
	if !IsPathGlob(root) {
		return len(root), under(dir, root)
	}
	n := len(strings.Split(root, "/"))
	parts := strings.Split(dir, "/")
	if len(parts) < n {
		return 0, false
	}
	prefix := strings.Join(parts[:n], "/")
	if ok, err := filepath.Match(root, prefix); err != nil || !ok {
		return 0, false
	}
	return len(prefix), true
}

// ExpandRoots turns path rules into existing absolute directories: plain
// rules expanded, globs replaced by the directories they match now.
func ExpandRoots(rules []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, r := range rules {
		root := expandRule(r)
		if !IsPathGlob(root) {
			add(root)
			continue
		}
		ms, _ := filepath.Glob(root)
		for _, m := range ms {
			if fi, err := os.Stat(m); err == nil && fi.IsDir() {
				add(m)
			}
		}
	}
	return out
}

// Glob matches s against a pattern where * matches any run of characters
// (including / and :) and everything else is literal.
func Glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		j := strings.Index(s, parts[i])
		if j < 0 {
			return false
		}
		s = s[j+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
