package vmhost

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sengirunners/potluck/softnet"
)

// errBootExited is a VM's process exiting before its guest answered
// (waitReady). The supervisor turns it into a bootExit, which wraps it.
var errBootExited = errors.New("the VM exited while booting")

// bootCause is a boot failure known to come from this machine's setup, not
// chance: the same boot fails the same way until the setup changes, so its
// words never say to run it again unchanged. Each cause is one row of
// bootCauses; StartAdvice finds a cause again by its what.
type bootCause struct {
	what  string                // its name in the error, before the layer's own words
	hint  string                // what to do instead of running it again (StartAdvice)
	match func(low string) bool // low: the boot output, lowercased
	line  func(low string) int  // how well an output line shows it: the last of the highest rank is quoted, 0 never
}

// bootCauses are the setup faults a failed boot is checked against, in order.
var bootCauses = []bootCause{{
	what: "Softnet refused a network rule",
	hint: "this Softnet does not accept the protocol and port rules Potluck isolates a box with (such as \"out proto tcp port 25\"), " +
		"so every boot here fails the same way until Softnet changes: " + softnet.BootHint(softnet.Command) +
		"; then check this machine again. If none covers this macOS, this Mac cannot lend isolated boxes yet: send the report to whoever invited you",
	match: softnetRefused,
	line:  softnetLine,
}}

// softnetRules are the protocol and port rules Potluck passes Softnet,
// lowercased: a boot output that quotes one is Softnet refusing it. A bare
// "@host" or address is left out: it is also part of other words (such as
// user@hostname).
var softnetRules = func() []string {
	var rs []string
	for _, r := range strings.Split(softnetMailBlock+","+SoftnetAllowRules(), ",") {
		if strings.Contains(r, " port ") {
			rs = append(rs, strings.ToLower(r))
		}
	}
	return rs
}()

func softnetRefused(low string) bool {
	for _, r := range softnetRules {
		if strings.Contains(low, r) {
			return true
		}
	}
	if !strings.Contains(low, "softnet") {
		return false
	}
	for _, w := range []string{"invalid value", "invalid rule", "unexpected argument", "failed to parse", "unrecognized"} {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// softnetLine ranks a line that quotes a rule over one that names Softnet.
func softnetLine(low string) int {
	for _, r := range softnetRules {
		if strings.Contains(low, r) {
			return 2
		}
	}
	if strings.Contains(low, "softnet") {
		return 1
	}
	return 0
}

// componentNamer is a layer that names the host programs its boot runs, for
// a failed boot's words (Tart: tart, Softnet when isolated, and macOS).
type componentNamer interface{ Components(isolated bool) string }

// setupMark follows a setup fault's components in a bootExit's words.
const setupMark = "setup fault"

// bootWordsMax bounds the layer's own words a bootExit quotes; the report
// keeps more (reportTailMax).
const bootWordsMax = 200

// bootExit is a VM that exited while booting, in the words of what ran it.
type bootExit struct {
	exit       int
	words      string     // the line of the layer's output that says why, redacted and bounded
	cause      *bootCause // nil: not a known setup fault
	components string     // "" when the layer names none
}

func (b *bootExit) Error() string {
	var s strings.Builder
	fmt.Fprintf(&s, "%s (exit %d)", errBootExited, b.exit)
	switch {
	case b.cause != nil && b.words != "":
		fmt.Fprintf(&s, ": %s: %q", b.cause.what, b.words)
	case b.cause != nil:
		fmt.Fprintf(&s, ": %s", b.cause.what)
	case b.words != "":
		fmt.Fprintf(&s, ": %q", b.words)
	default:
		s.WriteString(", printing nothing")
	}
	var tags []string
	if b.cause != nil {
		tags = append(tags, setupMark)
	}
	if b.components != "" {
		tags = append(tags, b.components)
	}
	if len(tags) > 0 {
		fmt.Fprintf(&s, " [%s]", strings.Join(tags, "; "))
	}
	return s.String()
}

func (b *bootExit) Unwrap() error { return errBootExited }

// diagnoseBoot reads why a VM exited while booting from the end of its run
// log, where the layer's boot process wrote (Boot's log).
func diagnoseBoot(l vmLayer, logPath string, exit int, isolated bool) *bootExit {
	b := &bootExit{exit: exit}
	out := redact(tailOf(logPath, reportTailMax))
	low := strings.ToLower(out)
	for i := range bootCauses {
		if bootCauses[i].match(low) {
			b.cause = &bootCauses[i]
			break
		}
	}
	b.words = bootWords(out, b.cause)
	if c, ok := l.(componentNamer); ok {
		b.components = c.Components(isolated)
	}
	return b
}

// bootWords is the line of out that says why: of the lines that show the
// cause, the last of the best rank (bootCause.line); with no cause, or no line
// showing it, the last line that is not empty. Bounded to bootWordsMax.
func bootWords(out string, c *bootCause) string {
	pick, best := "", 0
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l == "" || l == tailCut {
			continue
		}
		rank := 1
		if c != nil {
			rank = c.line(strings.ToLower(l))
		}
		if rank > 0 && rank >= best {
			pick, best = l, rank
		}
	}
	if pick == "" && c != nil {
		return bootWords(out, nil)
	}
	if len(pick) > bootWordsMax {
		pick = strings.ToValidUTF8(pick[:bootWordsMax], "") + "..."
	}
	return pick
}

// reportRe finds a kept report's path in a failed start's words.
var reportRe = regexp.MustCompile(`report: (\S+/` + failedDir + `/r[0-9a-f]{12}\.txt)`)

// StartAdvice reads what a failed start's words (an acquire's refusal, or a
// conformance FAIL line quoting one) say to do: setup is the hint of a known
// setup fault, which fails the same way every time it is run unchanged (""
// for any other failure), and report the path of the start's kept report (""
// when the words name none).
func StartAdvice(text string) (setup, report string) {
	if strings.Contains(text, errBootExited.Error()) && strings.Contains(text, "["+setupMark) {
		for _, c := range bootCauses {
			if strings.Contains(text, ": "+c.what) {
				setup = c.hint
				break
			}
		}
	}
	if m := reportRe.FindStringSubmatch(text); m != nil {
		report = m[1]
	}
	return setup, report
}
