package teamlink

// What an HQ answer passes on of a move engine's (home_control.go): free
// text only as a fixed phrase, codes only from their vocabulary, ids, names
// and the public address only in their shape. Anything else is replaced,
// and the field's name and length logged, never its text.

import (
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

// maxPhrase bounds a free-text field of an HQ answer, in bytes.
const maxPhrase = 300

// withheld replaces a free-text field that is not a fixed phrase.
const withheld = "the engine's text was withheld: it was not a fixed phrase"

// phraseBanned are the bytes no fixed phrase holds: what a path, a URL, a
// query, a mail address or markup needs.
const phraseBanned = "/\\@=&#<>[]{}|`^~$*+"

// fixedPhrase reports whether s may stand in a free-text field of an HQ
// answer. It must be all of:
//   - at most maxPhrase bytes of printable ASCII: no control or format
//     character, nothing outside ASCII (so no lookalike slash, no
//     direction mark, no zero-width space), one line;
//   - none of phraseBanned, no '%' before a hex digit, and no '?' before
//     anything but a space or the end (a query string);
//   - no '.' directly before a letter or digit (a host name, an IP
//     address, a file name; "1.5" too), and no ':' directly before
//     anything but a space or the end, but in a clock time (9:05, 14:05:30)
//     (a port, an IPv6 address, a scheme);
//   - nothing secretLike.
//
// Plain sentences with machine names, clock times and whole numbers pass.
func fixedPhrase(s string) bool {
	if len(s) > maxPhrase {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || strings.IndexByte(phraseBanned, c) >= 0 {
			return false
		}
		next := byte(' ')
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch c {
		case '.':
			if alnum(next) {
				return false
			}
		case '?':
			if next != ' ' {
				return false
			}
		case '%':
			if strings.IndexByte("0123456789abcdefABCDEF", next) >= 0 {
				return false
			}
		case ':':
			if next != ' ' && !clockAt(s, i) {
				return false
			}
		}
	}
	return !secretLike(s)
}

// clockAt reports whether the ':' at s[i] is inside a clock time: one or
// two digits before it, exactly two after.
func clockAt(s string, i int) bool {
	before := 0
	for j := i - 1; j >= 0 && digit(s[j]); j-- {
		before++
	}
	after := 0
	for j := i + 1; j < len(s) && digit(s[j]); j++ {
		after++
	}
	return before >= 1 && before <= 2 && after == 2
}

// keyPrefixes start a word that is a credential (secretLike), lower case.
var keyPrefixes = []string{"sk-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xoxb-", "xoxp-", "xoxa-", "xoxs-", "age-secret-key-"}

// secretLike reports whether s holds what a credential looks like: a
// Potluck token's prefix anywhere, a word that starts as a known key does
// (keyPrefixes, any case), a run of 20 or more ASCII letters and digits
// with both in it, or of 32 or more of them (an access key id, a key in
// base32 or base64; a ULID too), or a run of letters, digits, '_' and '-'
// holding 20 or more letters and digits of which 6 or more are digits and
// 2 or more letters (a key in short segments: a UUID, "ab12-cd34-...").
func secretLike(s string) bool {
	low := strings.ToLower(s)
	if strings.Contains(low, authz.TokenPrefix) || strings.Contains(low, authz.TokenPrefixOld) {
		return true
	}
	for i := 0; i < len(low); i++ {
		if i > 0 && (alnum(low[i-1]) || low[i-1] == '_' || low[i-1] == '-') {
			continue // only at a word's start
		}
		for _, p := range keyPrefixes {
			if strings.HasPrefix(low[i:], p) {
				return true
			}
		}
	}
	run, letters, digits := 0, false, false
	for i := 0; i <= len(s); i++ {
		if i < len(s) && alnum(s[i]) {
			run++
			letters = letters || !digit(s[i])
			digits = digits || digit(s[i])
			continue
		}
		if run >= 32 || run >= 20 && letters && digits {
			return true
		}
		run, letters, digits = 0, false, false
	}
	return segmentedKey(s)
}

// segmentedKey reports whether s holds a run of letters, digits, '_' and
// '-' with 20 or more letters and digits, 6 or more of them digits and 2 or
// more letters (secretLike).
func segmentedKey(s string) bool {
	letters, digits := 0, 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && (alnum(s[i]) || s[i] == '_' || s[i] == '-') {
			switch {
			case digit(s[i]):
				digits++
			case alnum(s[i]):
				letters++
			}
			continue
		}
		if letters+digits >= 20 && digits >= 6 && letters >= 2 {
			return true
		}
		letters, digits = 0, 0
	}
	return false
}

func digit(c byte) bool { return c >= '0' && c <= '9' }

func alnum(c byte) bool { return digit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// validName is an identity's name an answer passes on.
func validName(s string) bool { return api.ValidName(s) && !secretLike(s) }

// validMachine is a machine's name an answer passes on: a name of letters,
// digits and '-' only, so no host name, IP address or file name leaves as
// one.
func validMachine(s string) bool {
	if !validName(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !alnum(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

// validSlug is a project slug an answer passes on.
func validSlug(s string) bool { return api.ValidSlug(s) && !secretLike(s) }

// validID is a move's or plan's id an answer passes on: a ULID (the
// engine's ids, kit/ids), or a short lower-case word
// ([a-z0-9][a-z0-9_-]{0,39}) that is nothing secretLike, so no token, key,
// UUID or hex digest leaves as an id.
func validID(s string) bool {
	if ids.Valid(s) {
		return true
	}
	if len(s) == 0 || len(s) > 40 || !alnum(s[0]) || strings.ToLower(s) != s {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !alnum(s[i]) && s[i] != '_' && s[i] != '-' {
			return false
		}
	}
	return !secretLike(s)
}

// validSecretName is a secret's plain name an answer passes on: letters,
// digits, '_' and '-', at most 64, nothing secretLike.
func validSecretName(s string) bool {
	if len(s) == 0 || len(s) > 64 || !alnum(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !alnum(s[i]) && s[i] != '_' && s[i] != '-' {
			return false
		}
	}
	return !secretLike(s)
}

// validHostPort is the public address an answer passes on: host or
// host:port, the host a lower-case DNS name or an IP address; no scheme,
// user, path, query or fragment.
func validHostPort(s string) bool {
	host := s
	if strings.Contains(s, ":") {
		h, port, err := net.SplitHostPort(s)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
		host = h
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 253 || secretLike(host) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(digit(c) || c >= 'a' && c <= 'z' || c == '-') {
				return false
			}
		}
	}
	return true
}

// cleaner replaces what an engine answered that may not leave as it is:
// free text that is not a fixed phrase (withheld), and a code outside its
// vocabulary, an id, a name or an address of the wrong shape
// (api.UnknownCode). It logs the field's name and length, never its text.
type cleaner struct{ log *slog.Logger }

func (c cleaner) replace(s *string, field, with string) {
	c.log.Warn("HQ: a field of the move engine's answer is not passed on as it is", "field", field, "len", len(*s))
	*s = with
}

// phrase is free text: "" stays.
func (c cleaner) phrase(s *string, field string) {
	if *s != "" && !fixedPhrase(*s) {
		c.replace(s, field, withheld)
	}
}

// code is one of vocab.
func (c cleaner) code(s *string, vocab []string, field string) {
	if !slices.Contains(vocab, *s) {
		c.replace(s, field, api.UnknownCode)
	}
}

// check is a value that ok accepts; "" (none) stays when empty may.
func (c cleaner) check(s *string, ok func(string) bool, empty bool, field string) {
	if (*s != "" || !empty) && !ok(*s) {
		c.replace(s, field, api.UnknownCode)
	}
}

func (c cleaner) name(s *string, field string) { c.check(s, validName, true, field) }

func (c cleaner) machine(s *string, field string) { c.check(s, validMachine, true, field) }

func (c cleaner) reason(r *api.Reason, field string) {
	c.code(&r.Code, api.Reasons, field+".code")
	c.phrase(&r.Text, field+".text")
}

func cleanStatus(st *api.AuthorityStatus, log *slog.Logger) {
	c := cleaner{log}
	c.machine(&st.Machine, "machine")
	c.code(&st.State, api.HQStates, "state")
	if st.Silent != nil {
		c.code(&st.Silent.StandbyProbe, api.Probes, "silent.standby_probe")
	}
	c.machine(&st.Standby.Machine, "standby.machine")
	c.code(&st.Standby.State, api.StandbyStates, "standby.state")
	for i := range st.Standby.Reasons {
		c.reason(&st.Standby.Reasons[i], "standby.reasons")
	}
	if s := st.Setup; s != nil {
		c.machine(&s.Machine, "setup.machine")
		c.code(&s.Step, api.SetupSteps, "setup.step")
		if s.Failure != nil {
			c.reason(s.Failure, "setup.failure")
		}
	}
	for _, m := range []*api.HomeMove{st.Move, st.LastMove} {
		if m != nil {
			cleanMove(m, log)
		}
	}
	if st.FormerHQ != nil {
		c.machine(&st.FormerHQ.Machine, "former_hq.machine")
	}
}

func cleanMove(m *api.HomeMove, log *slog.Logger) {
	c := cleaner{log}
	c.check(&m.ID, validID, false, "move.id")
	c.code(&m.Kind, api.MoveKinds, "move.kind")
	c.code(&m.State, api.MoveStates, "move.state")
	c.machine(&m.From, "move.from")
	c.machine(&m.To, "move.to")
	c.name(&m.By.Identity, "move.by.identity")
	c.machine(&m.By.Machine, "move.by.machine")
	c.phrase(&m.Failure, "move.failure")
	for i := range m.Steps {
		c.code(&m.Steps[i].ID, api.StepIDs, "move.steps.id")
		c.code(&m.Steps[i].State, api.StepStates, "move.steps.state")
		c.phrase(&m.Steps[i].Detail, "move.steps.detail")
	}
	if m.Follow != nil {
		for i := range m.Follow.Stragglers {
			c.machine(&m.Follow.Stragglers[i].Machine, "move.follow.stragglers.machine")
			c.reason(&m.Follow.Stragglers[i].Reason, "move.follow.stragglers.reason")
		}
	}
}

func cleanMoveAnswer(a *api.MoveAnswer, log *slog.Logger) {
	c := cleaner{log}
	p := &a.Preflight
	c.check(&p.ID, validID, true, "preflight.id")
	c.machine(&p.From, "preflight.from")
	c.machine(&p.To, "preflight.to")
	for i := range p.Checks {
		c.code(&p.Checks[i].Code, api.Checks, "preflight.checks.code")
		c.phrase(&p.Checks[i].Text, "preflight.checks.text")
		c.phrase(&p.Checks[i].Fix, "preflight.checks.fix")
	}
	for i := range p.Projects {
		c.check(&p.Projects[i], validSlug, false, "preflight.projects")
	}
	if p.PublicAddress != nil {
		c.check(&p.PublicAddress.Address, validHostPort, false, "preflight.public_address.address")
	}
	for i := range p.Unseen {
		c.machine(&p.Unseen[i].Machine, "preflight.unseen.machine")
	}
	if a.Move != nil {
		cleanMove(a.Move, log)
	}
}

func cleanCandidates(l *api.CandidateList, log *slog.Logger) {
	c := cleaner{log}
	for i := range l.Preview.Secrets {
		c.check(&l.Preview.Secrets[i], validSecretName, false, "preview.secrets")
	}
	for i := range l.Candidates {
		c.machine(&l.Candidates[i].Machine, "candidates.machine")
		for j := range l.Candidates[i].Reasons {
			c.reason(&l.Candidates[i].Reasons[j], "candidates.reasons")
		}
	}
}
