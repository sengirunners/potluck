package api

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
)

// AddressKind classifies a mail destination or caller (SPEC.md "Addresses").
type AddressKind string

const (
	KindSession   AddressKind = "session" // <machine>/<session8>
	KindMachine   AddressKind = "machine" // machine:<host>, or a bare <host>: the machine's group mailbox
	KindHuman     AddressKind = "human"   // human:<name>
	KindWork      AddressKind = "work"    // <source>:<ref>, e.g. local:12, bd:abc-1, gh:owner/repo#7
	KindBroadcast AddressKind = "all"     // all: an announcement to every session (in one project)
	KindOwners    AddressKind = "owners"  // owners: the project's owner group mailbox (needs=human escalations)
)

// Address is a parsed address. String() gives the canonical form.
type Address struct {
	Kind    AddressKind
	Machine string // session, machine
	Session string // session: the 8-char session id
	Human   string // human
	Source  string // work: "local", "bd", "gh", ...
	Ref     string // work: what follows "<source>:"
}

// The grammar's character classes, checked by hand: these run on every
// request (Zm-As, recipients) and regexps cost start-up and allocation.
func lowerDigit(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

// matches reports whether s is first(s[0]) then 0..maxRest bytes of rest.
func matches(s string, first, rest func(byte) bool, maxRest int) bool {
	if len(s) == 0 || len(s) > maxRest+1 || !first(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !rest(s[i]) {
			return false
		}
	}
	return true
}

// ValidName reports whether s is a valid machine or human name:
// [a-z0-9][a-z0-9._-]{0,62}.
func ValidName(s string) bool {
	return matches(s, lowerDigit, func(c byte) bool { return lowerDigit(c) || c == '.' || c == '_' || c == '-' }, 62)
}

// validSource is [a-z][a-z0-9-]{0,31}.
func validSource(s string) bool {
	return matches(s, func(c byte) bool { return c >= 'a' && c <= 'z' }, func(c byte) bool { return lowerDigit(c) || c == '-' }, 31)
}

// ValidSlug reports whether s is a valid project slug or area name (same
// grammar as a name, but "none", "all" and "owners" are reserved).
func ValidSlug(s string) bool {
	return ValidName(s) && s != "none" && s != "all" && s != "owners"
}

// ValidSession8 reports whether s is a valid session id.
func ValidSession8(s string) bool { return len(s) == 8 && matches(s, lowerDigit, lowerDigit, 7) }

// ValidSessionName reports whether s may name a session (SPEC 8 "Names"):
// a slug that is not a session8 (8 letters and digits: <machine>/<x> is
// then always an address, never a name, so no stored address can ever
// resolve to a session that took the string as a name later), not a route
// under /v0/sessions/, and holds no blocked word.
func ValidSessionName(s string) bool {
	return ValidSlug(s) && !ValidSession8(s) && !HasBlockedWord(s) && !slices.Contains(sessionRoutes, s)
}

// sessionRoutes are the literal /v0/sessions/<x> routes a name would hide
// (a session test walks the routes).
var sessionRoutes = []string{"gc", "admission", "register", "hook"}

// SessionNameRef splits s into a session name and its machine ("" when s
// is a bare name): ok when s is <machine>/<name> or <name> with a valid
// session name. A session address (<machine>/<session8>) is never one.
func SessionNameRef(s string) (machine, name string, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	machine, name, slash := strings.Cut(s, "/")
	if !slash {
		machine, name = "", s
	} else if !ValidName(machine) {
		return "", "", false
	}
	return machine, name, ValidSessionName(name)
}

// BlockedWords are words no minted handle contains and no chosen name is
// made of (identifiers-2026-11-01; English only for now).
var BlockedWords = []string{"fuk", "fuc", "shit", "cunt", "dick", "cock", "piss", "twat", "nazi", "kkk", "fag", "nig",
	"rape", "porn", "anal", "tit", "ass", "sex", "cum", "jew", "kill"}

// HasBlockedWord reports whether one of a chosen name's words (split at
// '.', '_' and '-') is a blocked word. Whole words, not substrings (a
// minted handle's rule), so "class", "analysis" or "skill-review" stay
// usable; moderation of open projects is stage 4's.
func HasBlockedWord(s string) bool {
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '_' || r == '-' }) {
		if slices.Contains(BlockedWords, w) {
			return true
		}
	}
	return false
}

// ParseAddress parses any address in the grammar. Input is trimmed and lowercased
// except for a work ref, which keeps its case (GitHub owners, bead ids).
func ParseAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Address{}, Errorf(CodeBadRequest, "empty address")
	}
	low := strings.ToLower(s)
	switch {
	case low == "all":
		return Address{Kind: KindBroadcast}, nil
	case low == "owners":
		return Address{Kind: KindOwners}, nil
	case strings.HasPrefix(low, "human:"):
		n := low[len("human:"):]
		if !ValidName(n) {
			return Address{}, Errorf(CodeBadRequest, "bad human name in %q", s)
		}
		return Address{Kind: KindHuman, Human: n}, nil
	case strings.HasPrefix(low, "machine:"):
		n := low[len("machine:"):]
		if !ValidName(n) {
			return Address{}, Errorf(CodeBadRequest, "bad machine name in %q", s)
		}
		return Address{Kind: KindMachine, Machine: n}, nil
	}
	if i := strings.IndexByte(s, ':'); i > 0 {
		src, ref := strings.ToLower(s[:i]), s[i+1:]
		if !validSource(src) || ref == "" || strings.ContainsAny(ref, " \t\n") {
			return Address{}, Errorf(CodeBadRequest, "bad work item id %q (want <source>:<ref>)", s)
		}
		return Address{Kind: KindWork, Source: src, Ref: ref}, nil
	}
	if m, sess, ok := strings.Cut(low, "/"); ok {
		if !ValidName(m) || !ValidSession8(sess) {
			return Address{}, Errorf(CodeBadRequest, "bad session address %q (want <machine>/<8 chars>)", s)
		}
		return Address{Kind: KindSession, Machine: m, Session: sess}, nil
	}
	if !ValidName(low) {
		return Address{}, Errorf(CodeBadRequest, "bad address %q", s)
	}
	return Address{Kind: KindMachine, Machine: low}, nil
}

// String is the canonical form.
func (a Address) String() string {
	switch a.Kind {
	case KindSession:
		return a.Machine + "/" + a.Session
	case KindMachine:
		return "machine:" + a.Machine
	case KindHuman:
		return "human:" + a.Human
	case KindWork:
		return a.Source + ":" + a.Ref
	case KindBroadcast:
		return "all"
	case KindOwners:
		return "owners"
	}
	return ""
}

// SessionAddress builds "<machine>/<session8>".
func SessionAddress(machine, session8 string) string { return machine + "/" + session8 }

// Session8 derives a session id from an agent runtime's own session id (a
// Claude Code or Codex UUID): its first 8 alphanumeric characters, lowercased.
// An empty or too-short id gets a random one.
func Session8(runtimeID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(runtimeID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() == 8 {
				return b.String()
			}
		}
	}
	return NewSession8()
}

// NewSession8 returns a random 8-hex-char session id.
func NewSession8() string {
	var x [4]byte
	_, _ = rand.Read(x[:])
	return hex.EncodeToString(x[:])
}

// Claim labels (SPEC "Native issues", bd door: claim label). A claim's
// assignee is a routing label; from an agent session it names the session,
// "<machine>/<session8>", or a lane (a sub-agent) working under it,
// "<lane>@<machine>/<session8>". Labels are advisory: never authz.

// SessionLabel is the claim label for session address addr and lane ("" =
// the session itself).
func SessionLabel(addr, lane string) string {
	if lane == "" {
		return addr
	}
	return lane + "@" + addr
}

// LabelSession parses a claim label naming a session: its session address
// and lane (ok false: the label names no session, e.g. a person's name).
func LabelSession(label string) (addr, lane string, ok bool) {
	addr = label
	if i := strings.LastIndexByte(label, '@'); i >= 0 {
		lane, addr = label[:i], label[i+1:]
		if !ValidName(lane) {
			return "", "", false
		}
	}
	a, err := ParseAddress(addr)
	if err != nil || a.Kind != KindSession || a.String() != addr {
		return "", "", false
	}
	return addr, lane, true
}

// LabelAdopts reports whether a claim by session address addr under label
// next may take over that address's own claim made under label cur: cur
// names no session (a person's name: bd-zm's git user.name fallback before
// labels named sessions) and next names addr or a lane under it.
func LabelAdopts(cur, next, addr string) bool {
	if _, _, named := LabelSession(cur); named || cur == "" {
		return false
	}
	a, _, ok := LabelSession(next)
	return ok && a == addr
}
