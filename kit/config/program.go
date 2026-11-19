package config

import (
	"os"
	"regexp"
)

// The program a binary is, for the words people read: the first word of
// every command a message names, and the program a daemon's home claim and
// machine record name (kit/server). A program built on Potluck's kit sets
// Program (with Prefix when it mounts Potluck's commands under a word, and
// Lacks when it carries only some of kit's commands) first in main, before
// it prints anything, builds its commands or starts a daemon (cli.UseHome:
// "potluck"). Messages read them when they are made, never at package init.
var (
	// Program is the running program's name.
	Program = "potluck"
	// Prefix is how a person types Potluck's commands (cli.Commands: host,
	// doctor, sandbox, ...) in the running program: "zm potluck" for a
	// program that mounts them under its word "potluck". "" is Program
	// alone (they are at its top level).
	Prefix string
	// Lacks, when set, reports whether the running program has no command
	// of these words (subcommand names, no flags or arguments): a program
	// that carries only some of the commands kit's messages name answers
	// from its own command tree. Nil: it has every one.
	Lacks func(words string) bool
)

// EnvPrefix carries the daemon's Potluck prefix (PotluckPrefix) to the
// drivers it starts, whose messages name Potluck's commands too.
const EnvPrefix = "POTLUCK_PREFIX"

// PrefixFromEnv sets Prefix from EnvPrefix, when that holds words a
// shell takes as they are (a driver's main calls it first; anything else
// is ignored and the prefix stays Program).
func PrefixFromEnv() {
	if v := os.Getenv(EnvPrefix); ValidPrefix(v) {
		Prefix = v
	}
}

// ValidPrefix reports whether s is what a prefix may hold: words a shell
// takes as they are (letters, digits, '.', '_' and '-', one space
// between), since hints and generated scripts carry it unquoted.
func ValidPrefix(s string) bool { return prefixWords.MatchString(s) }

var prefixWords = regexp.MustCompile(`^[A-Za-z0-9._-]+( [A-Za-z0-9._-]+)*$`)

// Missing reports whether the running program has no command words
// (Lacks).
func Missing(words string) bool { return Lacks != nil && Lacks(words) }

// Command is one of the running program's own commands as a person types
// it, in Potluck's words: Command("join", "<code>") is "potluck join
// <code>" in the potluck program and "zm join <code>" in zm. For a command
// the program has none of (Missing) it says so instead, without the
// arguments, so a hint never names a command that does not exist.
func Command(words string, args ...string) string {
	if Missing(words) {
		return "the " + words + " command (" + Program + " has none)"
	}
	return join(Program, words, args)
}

// PotluckCommand is one of Potluck's commands (host, doctor, sandbox rm,
// driver pin, ...) as a person types it in the running program:
// PotluckPrefix and the command's words, "potluck doctor" in the potluck
// program and "zm potluck doctor" where the prefix is "zm potluck".
func PotluckCommand(words string, args ...string) string {
	return join(PotluckPrefix(), words, args)
}

// PotluckPrefix is Prefix, or Program when Prefix is empty.
func PotluckPrefix() string {
	if Prefix != "" {
		return Prefix
	}
	return Program
}

func join(first, words string, args []string) string {
	s := first + " " + words
	for _, a := range args {
		s += " " + a
	}
	return s
}
