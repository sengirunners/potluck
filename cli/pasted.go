package cli

// What a person pasted as host's or join's arguments: the checks those
// commands run on them before anything else (before the daemon, the host
// lock or a pull), and the words of their refusals. An invite is a secret
// until it is spent, so no refusal here repeats an argument, and none
// corrects one: a word a copy changed is refused, never cleaned and used.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// singleQuoted is v as one shell word in single quotes, whatever it holds:
// how every command printed for a person carries an invite.
func singleQuoted(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// shellQuote is v as one shell word: as it is when the shell takes it so,
// else singleQuoted.
func shellQuote(v string) string {
	if v != "" && strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") == "" {
		return v
	}
	return singleQuoted(v)
}

// pastedOdd is the first character of s (past the spaces around it) that no
// invite, pool name or flag holds, and what a copy put it where: a chat,
// document or web page swaps a terminal's plain spaces, quotes and dashes
// for these. ok is false when s holds none.
func pastedOdd(s string) (what string, ok bool) {
	s = strings.Trim(s, " \t\r\n") // a no-break space around a word is what this looks for
	i := strings.IndexFunc(s, notPlain)
	if i < 0 {
		return "", false
	}
	r := []rune(s[i:])[0]
	switch {
	case r == ' ':
		return "a no-break space (U+00A0) where a plain space goes", true
	case unicode.IsSpace(r) && r > 0x7f:
		return fmt.Sprintf("a typeset space (U+%04X) where a plain space goes", r), true
	case strings.ContainsRune("‘’‚‛′", r):
		return fmt.Sprintf("a curly quote (U+%04X) where a straight ' goes", r), true
	case strings.ContainsRune("“”„‟″", r):
		return fmt.Sprintf("a curly quote (U+%04X) where a straight \" goes", r), true
	case r >= '‐' && r <= '―' || r == '−':
		return fmt.Sprintf("a long dash (U+%04X) where - or -- goes", r), true
	case r == '\n' || r == '\r':
		return "a line break inside one word", true
	case unicode.Is(unicode.Cf, r) || unicode.IsControl(r):
		return fmt.Sprintf("an invisible character (U+%04X)", r), true
	}
	return fmt.Sprintf("the character U+%04X, which no invite, pool name or flag holds", r), true
}

// notPlain is a character outside printable ASCII.
func notPlain(r rune) bool { return r < 0x20 || r > 0x7e }

// copyAgain is what to do about a command a copy changed.
const copyAgain = "copy the command again as the invite printed it, one line with its quotes, " +
	"or type its spaces, quotes and dashes again by hand"

// notShown says why a refusal repeats none of what was typed.
const notShown = "(nothing typed is shown here: an invite is a secret until it is used)"

// pastedArgs is the positional rule of host and join: min to max words,
// each plain text. A word a copy changed is refused by its place, naming
// the flag it looks like (only a flag of cmd: never a word that may be a
// secret); more words than max are refused saying how an invite goes.
// Both are usage errors (exit 2).
func (a *app) pastedArgs(min, max int, takes string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		for i, w := range args {
			what, odd := pastedOdd(w)
			if !odd {
				continue
			}
			flag := ""
			if f := pastedFlag(cmd, w); f != "" {
				flag = " and looks like the flag " + f + ", which the shell passed as a word, not a flag"
			}
			return exitWith{2, fmt.Errorf("argument %d of %d after %s holds %s%s: %s %s",
				i+1, len(args), cmd.Name(), what, flag, copyAgain, notShown)}
		}
		if len(args) > max {
			return exitWith{2, fmt.Errorf("%s takes %s, but got %d arguments: an invite goes whole, as one word in single quotes ('<invite>'); "+
				"one split by a paste over several lines, or without its quotes, arrives as several. %s %s",
				cmd.Name(), takes, len(args), strings.ToUpper(copyAgain[:1])+copyAgain[1:], notShown)}
		}
		return cobra.MinimumNArgs(min)(cmd, args)
	}
}

// flagWord is the flag a word starts with once a copy's characters and
// dashes before it are gone.
var flagWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*`)

// pastedFlag is the flag of cmd that w is a pasted spelling of ("--image"
// for a no-break space and "--image"), or "".
func pastedFlag(cmd *cobra.Command, w string) string {
	rest := strings.TrimLeftFunc(w, func(r rune) bool { return r == '-' || r == ' ' || notPlain(r) })
	name := flagWord.FindString(rest)
	if name == "" || cmd.Flags().Lookup(name) == nil {
		return ""
	}
	return "--" + name
}

// flagEnd is a flag's name at the end of pflag's message, before the
// character a copy put after it.
var flagEnd = regexp.MustCompile(`--([a-z0-9][a-z0-9-]*)$`)

// pastedFlags sets owner's flag error func (its subcommands' too, which
// inherit it): pflag's message for a flag a copy changed ("--image" and a
// no-break space before its value) repeats the value, so a refusal that
// holds such a character says what it is instead, by the flag's name when
// it is one of the command's. Any other error goes on as owner's parent
// would send it.
func pastedFlags(owner *cobra.Command) {
	owner.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		msg := err.Error()
		what, odd := pastedOdd(msg)
		if !odd {
			if p := owner.Parent(); p != nil {
				return p.FlagErrorFunc()(cmd, err)
			}
			return err
		}
		flag := "a flag or its value holds"
		if m := flagEnd.FindStringSubmatch(msg[:strings.IndexFunc(msg, notPlain)]); m != nil && cmd.Flags().Lookup(m[1]) != nil {
			flag = "the flag --" + m[1] + " is followed by"
		}
		return exitWith{2, fmt.Errorf("%s %s: %s %s", flag, what, copyAgain, notShown)}
	})
}

// checkInvite refuses arg when it is shaped like an invite (not a pool's
// name) but is not a whole one, before anything runs, saying what a whole
// one looks like and never repeating it: only its secret (the last part,
// what a copy of a line's end keeps), a part missing, or damage. invite is
// the pool owner's command that prints a new one.
func (a *app) checkInvite(arg, newCode string) error {
	s := strings.TrimSpace(arg)
	if s == "" || !project.LooksLikeJoinCode(s) { // none: the only pool
		return nil
	}
	if _, err := project.ParseJoinCode(s); err == nil {
		return nil
	}
	const whole = "a whole invite looks like potluck-<machine>:<fingerprint>:XXXX-XXXX (zm-<machine>:... from an older pool), " +
		"or https://<host>#<fingerprint>:<secret> through a tunnel"
	var why string
	switch {
	case !strings.Contains(s, ":") && project.NormalizeJoinSecret(s) != "":
		why = "this is only the last part of an invite, its secret; " + whole
	case strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://"):
		why = "this invite link is incomplete or damaged; " + whole + ", and a link https://potluck.sh/join#..."
	case !strings.Contains(s, ":"):
		why = "this is not a whole invite: it has none of the ':' that join a whole one's 3 parts (nor is it a pool's name, " +
			"lowercase letters, digits and dashes); " + whole
	case strings.Count(s, ":") == 1:
		why = "this invite is incomplete: it has 2 of the 3 parts a whole one has, joined by ':'; " + whole
	case strings.Count(s, ":") == 2:
		why = "this invite is damaged: it has its 3 parts, but one is cut short or holds a character no invite does; " + whole
	default:
		why = "this invite is damaged: it has more than the 3 parts a whole one has, joined by ':'; " + whole
	}
	return exitWith{2, api.Errorf(api.CodeBadRequest, "%s. Nothing was downloaded or changed, and the invite is not spent %s", why, notShown).
		WithHint("copy the whole invite again, quotes included, as one line; or ask the owner for a new one: " + newCode)}
}
