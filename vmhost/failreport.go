package vmhost

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// A start that fails leaves a report in <state_dir>/failed/<id>.txt: why, and
// the redacted end of its run log (the VM layer's own boot output) and its
// supervisor log. The record and its logs go when acquire answers the
// failure; the report stays, so the cause can be read after the VM is gone.
// The failure's words name the report's path (docs/hosts.md, "When a box
// does not start").
const (
	failedDir     = "failed"
	failKeep      = 20       // reports kept, the newest
	reportTailMax = 16 << 10 // bytes of each log a report keeps, its end
)

// tailCut stands where tailOf cut the start of a log.
const tailCut = "[... earlier output cut]"

// tailOf is the last max bytes of the file at path, from a line's start,
// valid UTF-8; "" when it cannot be read.
func tailOf(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	cut := st.Size() > max
	if cut {
		if _, err := f.Seek(-max, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return ""
	}
	s := string(b)
	if cut {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = tailCut + "\n" + s
	}
	return strings.ToValidUTF8(s, "?")
}

// redactions take secrets out of a log before it is kept or quoted: invite
// links and tunnel codes (their secret is the URL's fragment), credentials in
// a URL, bearer tokens, Potluck and GitHub tokens, JWTs, a value after a
// secret-named key, and long opaque strings (redactOpaque).
var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(https?://[^\s#"'<>]*)#[^\s"'<>]+`), "$1#[redacted]"},
	{regexp.MustCompile(`(https?://)[^\s/@]+@`), "$1[redacted]@"},
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 [redacted]"},
	{regexp.MustCompile(`\b(?:potluck|zm)_[a-z0-9_]{20,}`), "[redacted]"},
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), "[redacted]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[redacted]"},
	{regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:token|secret|password|passwd|api_?key|credential|invite|jit_config|private_key)[a-z0-9_-]*["']?\s*[:=]\s*["']?)[^\s"',;]+`), "${1}[redacted]"},
}

// opaqueRe is a run that may be an encoded secret (a jit_config, a key):
// redactOpaque takes it when it mixes cases and digits, not a hex digest.
var opaqueRe = regexp.MustCompile(`[A-Za-z0-9+/=_-]{48,}`)

func redactOpaque(s string) string {
	var up, lo, dig bool
	for _, r := range s {
		up, lo, dig = up || unicode.IsUpper(r), lo || unicode.IsLower(r), dig || unicode.IsDigit(r)
	}
	if up && lo && dig {
		return "[redacted]"
	}
	return s
}

// redact is s with its secrets taken out, and the home directory written ~
// (a report or an error may be read on another machine).
func redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.with)
	}
	s = opaqueRe.ReplaceAllStringFunc(s, redactOpaque)
	return tilde(s)
}

// tilde writes the home directory as ~.
func tilde(s string) string {
	if h, err := os.UserHomeDir(); err == nil && len(h) > 1 {
		s = strings.ReplaceAll(s, strings.TrimSuffix(h, "/")+"/", "~/")
	}
	return s
}

// keepFailure writes r's report of a failed start, why it failed, and prunes
// the reports past failKeep or keepFor. It answers the report's path as the
// failure's words show it, "" when none was written.
func (s store) keepFailure(r record, why string) string {
	dir := filepath.Join(s.dir, failedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "a start that failed, reported by potluck-driver-tart\nhandle: %s\nat: %s\nlayer: %s\nimage: %s\nwhy: %s\n",
		r.Handle, time.Now().UTC().Format(time.RFC3339), r.Config["vm"], r.Image, redact(why))
	for _, l := range []struct{ what, path string }{
		{"the run log (the VM layer's own output)", r.Log},
		{"the supervisor log", filepath.Join(s.dir, "logs", r.ID+".supervisor.log")},
	} {
		fmt.Fprintf(&b, "\n--- %s, its last %d KiB at most, secrets redacted ---\n%s\n", l.what, reportTailMax>>10, redact(tailOf(l.path, reportTailMax)))
	}
	p := filepath.Join(dir, r.ID+".txt")
	if err := os.WriteFile(p+".tmp", []byte(b.String()), 0o600); err != nil {
		return ""
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return ""
	}
	s.pruneFailures()
	return tilde(p)
}

// reportOn names the machine a kept report is on, after its path: a
// refusal is read on the caller's machine too, where that path is not.
// "" when the machine has no name to give.
func reportOn() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if h, _, _ = strings.Cut(h, "."); h == "" {
		return ""
	}
	return " (on " + h + ")"
}

// pruneFailures keeps the failKeep newest reports, none older than keepFor.
func (s store) pruneFailures() {
	ps, _ := filepath.Glob(filepath.Join(s.dir, failedDir, "*.txt"))
	type rep struct {
		path string
		at   time.Time
	}
	var rs []rep
	for _, p := range ps {
		if st, err := os.Stat(p); err == nil {
			rs = append(rs, rep{p, st.ModTime()})
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].at.After(rs[j].at) })
	for i, x := range rs {
		if i >= failKeep || time.Since(x.at) > keepFor {
			_ = os.Remove(x.path)
		}
	}
}
