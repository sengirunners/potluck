package config

import "testing"

// A command is the running program's name and Potluck's words; one the
// program lacks is named as missing, never as a command it does not have.
func TestCommand(t *testing.T) {
	oldP, oldX, oldL := Program, Prefix, Lacks
	t.Cleanup(func() { Program, Prefix, Lacks = oldP, oldX, oldL })

	Program, Lacks = "supper", nil
	if got := Command("join", "<code>"); got != "supper join <code>" {
		t.Errorf("Command(join, <code>) = %q", got)
	}
	if got := Command("daemon", "--detach"); got != "supper daemon --detach" {
		t.Errorf("Command(daemon, --detach) = %q", got)
	}
	if Missing("tunnel setup") {
		t.Error("with no Lacks a program has every command")
	}

	Lacks = func(w string) bool { return w == "tunnel setup" }
	if got := Command("tunnel setup", "--quick"); got != "the tunnel setup command (supper has none)" {
		t.Errorf("a missing command: %q", got)
	}
	if !Missing("tunnel setup") || Missing("join") {
		t.Error("Missing follows Lacks")
	}
	if got := Command("join"); got != "supper join" {
		t.Errorf("Command(join) = %q", got)
	}
}

// Potluck's commands take the prefix a host mounts them under, else the
// program's name; a driver reads the prefix from its environment.
func TestPotluckCommand(t *testing.T) {
	oldP, oldX := Program, Prefix
	t.Cleanup(func() { Program, Prefix = oldP, oldX })
	Program, Prefix = "supper", ""
	if got := PotluckCommand("doctor"); got != "supper doctor" {
		t.Errorf("no prefix: %q", got)
	}
	Prefix = "supper pool"
	if got := PotluckCommand("sandbox rm", "<id>"); got != "supper pool sandbox rm <id>" {
		t.Errorf("prefix: %q", got)
	}
	Prefix = ""
	t.Setenv(EnvPrefix, "dish potluck")
	PrefixFromEnv()
	if Prefix != "dish potluck" {
		t.Errorf("from env: %q", Prefix)
	}
	t.Setenv(EnvPrefix, "dish; rm -rf /")
	PrefixFromEnv()
	if Prefix != "dish potluck" {
		t.Errorf("a prefix a shell would split or run is ignored: %q", Prefix)
	}
}
