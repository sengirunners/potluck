package config

import (
	"regexp"
	"slices"
	"strings"
)

// The program a binary is, for the words people read: the name in command
// suggestions ("run `zm host doctor`") and the verbs that differ between
// programs. The defaults are zm's, and with them Cmd and Text return what
// kit always printed. A program of its own sets both before it prints
// anything (cmd/potluck: "potluck" and its verbs, cli.UseHome).
var (
	Program = "zm"
	// Verbs maps zm's command words to this program's ("host doctor" ->
	// "doctor"). Cmd and Text use the longest key that starts the words. A
	// key with an empty value is a zm command this program has none of:
	// Text names it as a command this program does not have yet.
	Verbs map[string]string
)

// IsZm reports whether the program is zm (the default).
func IsZm() bool { return Program == "zm" }

// Cmd is a command in this program's words, given zm's: Cmd("daemon
// --detach") is "zm daemon --detach" for zm and "potluck daemon --detach"
// for potluck. A zm command with no counterpart keeps zm's words under this
// program's name; callers that can meet one word the sentence with Pick.
func Cmd(zmWords string) string {
	if IsZm() {
		return "zm " + zmWords
	}
	if v, rest, ok := verb(zmWords); ok && v != "" {
		return Program + " " + v + rest
	}
	return Program + " " + zmWords
}

// Has reports whether this program has the command zm calls zmWords: always for
// zm, and for another program unless its Verbs say it has none (an empty
// value). A hint that sends a person to a command asks first.
func Has(zmWords string) bool {
	if IsZm() {
		return true
	}
	v, _, ok := verb(zmWords)
	return !ok || v != ""
}

// Pick is zm's text for zm and other's for any other program: for a
// sentence that has to be worded differently, not just renamed.
func Pick(zm, other string) string {
	if IsZm() {
		return zm
	}
	return other
}

// verb finds the longest Verbs key that starts words (whole words only).
func verb(words string) (to, rest string, ok bool) {
	keys := make([]string, 0, len(Verbs))
	for k := range Verbs {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
	for _, k := range keys {
		if r, found := strings.CutPrefix(words, k); found && (r == "" || r[0] == ' ' || r[0] == ';' || r[0] == ',' || r[0] == ')' || r[0] == '`' || r[0] == '.') {
			return Verbs[k], r, true
		}
	}
	return "", "", false
}

// zmCommand is "zm" as a word followed by a command's words.
var zmCommand = regexp.MustCompile(`(^|[^-./_A-Za-z0-9$~])zm ([a-z][a-z-]*(?: [a-z][a-z-]*)*)`)

// zmWord is "zm" as a word of its own: never part of zm-run-tart, ~/.zm,
// zm_, ZM_HOME or Zm-As.
var zmWord = regexp.MustCompile(`(^|[^-./_A-Za-z0-9$~])zm($|[^-_/.A-Za-z0-9]|\.$|\.[^A-Za-z0-9])`)

// Text rewrites a message kit wrote in zm's words into this program's: each
// "zm <verbs>" becomes this program's command (Verbs), and "zm" alone its
// name. For zm it returns s unchanged. Names that are on the wire or on disk
// (zm-run-tart, ~/.zm, Zm-As, ZM_SECRETS) are left as they are. A program's
// own output calls it on what it prints (cmd/potluck: errors, hints, logs).
func Text(s string) string {
	if IsZm() || !strings.Contains(s, "zm") {
		return s
	}
	s = zmCommand.ReplaceAllStringFunc(s, func(m string) string {
		sub := zmCommand.FindStringSubmatch(m)
		pre, words := sub[1], sub[2]
		v, rest, ok := verb(words)
		switch {
		case !ok:
			return m // prose ("zm has no ..."): renamed below
		case v == "":
			k := strings.TrimSuffix(words, rest)
			return pre + "the " + k + " command (" + Program + " has none yet)" + rest
		}
		return pre + Program + " " + v + rest
	})
	// Twice: two words that share the character between them ("zm zm").
	for range 2 {
		s = zmWord.ReplaceAllString(s, "${1}"+Program+"${2}")
	}
	return s
}
