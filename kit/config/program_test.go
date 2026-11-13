package config

import "testing"

// With zm's defaults every message is what kit always printed: Text returns
// it unchanged and Cmd gives zm's words. The samples are verbatim from main.
var zmMessages = []string{
	"start it with: zm daemon --detach; most zm commands start it on their own",
	"the daemon speaks %s; this zm speaks %s",
	"use the same zm version for the daemon and the CLI (zm daemon stop; zm daemon --detach)",
	"run it again once the daemon is back (zm status; zm daemon --detach starts it)",
	"a zm daemon already runs for %s (pid %s); if `zm status` cannot reach it, its socket is gone: kill %s, then start again",
	"pass --project <slug>, set ZM_PROJECT, or run `zm project use <slug>` in the repo; `zm project why` explains",
	"a tailnet login works from zm clients at this node's own name; scripts send Zm-Protocol: zm/v0, browsers sign in with a token",
	"a join code looks like zm-<machine>:<fingerprint>:XXXX-XXXX or https://<host>#<fingerprint>:<secret>",
	"a join code names the authority's tailnet node: run zm tailnet up first (or set tunnel.url and use --via tunnel)",
	"no zm-run-%s on the daemon's PATH",
	"no layer can lend this machine: run `zm host doctor` for what is missing",
	"zm has no default %s image for %s boxes yet (zm's own is not published): pass --image name@sha256:<64 hex>, ...",
	"no box in the pool serves jobs yet (a member lends one with zm pool host)",
	"zm sandbox rm ends it; zm sandbox create --persist makes a box that stops and starts",
	"~/.zm/zm.sock, $ZM_HOME, dev.zm.run-target, zm_sealed",
}

func TestDefaultWordsAreZms(t *testing.T) {
	if Program != "zm" || Verbs != nil || !IsZm() {
		t.Fatalf("defaults: %q %v", Program, Verbs)
	}
	for _, m := range zmMessages {
		if got := Text(m); got != m {
			t.Errorf("Text changed zm's message:\n got %q\nwant %q", got, m)
		}
	}
	for in, want := range map[string]string{"daemon --detach": "zm daemon --detach", "host doctor": "zm host doctor", "tunnel setup": "zm tunnel setup"} {
		if got := Cmd(in); got != want {
			t.Errorf("Cmd(%q) = %q", in, got)
		}
	}
	if Pick("a", "b") != "a" || !Has("tunnel setup") {
		t.Fatal("Pick or Has")
	}
}

// Another program's words: its name, its verbs, a note for zm's alone, and
// names on the wire or on disk left as they are.
func TestOtherProgramWords(t *testing.T) {
	Program, Verbs = "potluck", map[string]string{"daemon": "daemon", "daemon --detach": "daemon --detach",
		"status": "daemon status", "host doctor": "doctor", "pool host": "host", "sandbox rm": "sandbox rm",
		"sandbox create": "sandbox create", "tailnet up": "", "tunnel setup": "", "project use": "", "project why": ""}
	t.Cleanup(func() { Program, Verbs = "zm", nil })
	if Has("tunnel setup") || Has("tunnel setup --quick") || !Has("daemon --detach") || !Has("some new command") {
		t.Error("Has: a verb mapped to nothing is a command this program lacks; the rest it has")
	}
	for in, want := range map[string]string{
		zmMessages[0]:  "start it with: potluck daemon --detach; most potluck commands start it on their own",
		zmMessages[3]:  "run it again once the daemon is back (potluck daemon status; potluck daemon --detach starts it)",
		zmMessages[6]:  "a tailnet login works from potluck clients at this node's own name; scripts send Zm-Protocol: zm/v0, browsers sign in with a token",
		zmMessages[8]:  "a join code names the authority's tailnet node: run the tailnet up command (potluck has none yet) first (or set tunnel.url and use --via tunnel)",
		zmMessages[9]:  "no zm-run-%s on the daemon's PATH",
		zmMessages[10]: "no layer can lend this machine: run `potluck doctor` for what is missing",
		zmMessages[11]: "potluck has no default %s image for %s boxes yet (potluck's own is not published): pass --image name@sha256:<64 hex>, ...",
		zmMessages[12]: "no box in the pool serves jobs yet (a member lends one with potluck host)",
		zmMessages[13]: "potluck sandbox rm ends it; potluck sandbox create --persist makes a box that stops and starts",
		zmMessages[14]: zmMessages[14],
		"zm zm":        "potluck potluck",
		"(zm)":         "(potluck)",
		"zm.":          "potluck.",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	if got := Cmd("status"); got != "potluck daemon status" {
		t.Errorf("Cmd(status) = %q", got)
	}
	if got := Pick("a", "b"); got != "b" {
		t.Errorf("Pick = %q", got)
	}
}

// A binary with its own human variable (cmd/potluck: POTLUCK_HUMAN) reads
// it and not ZM_HUMAN; zm's default reads ZM_HUMAN.
func TestOwnHumanVariable(t *testing.T) {
	t.Cleanup(func() { HumanEnv = EnvHuman })
	home := t.TempDir()
	t.Setenv(EnvHuman, "zmuser")
	t.Setenv("OWN_HUMAN", "ownuser")
	host := func() (string, bool) { return "studio", true }
	if c, _ := load(home, osUser, host); c.Human != "zmuser" {
		t.Fatalf("zm's default: %q", c.Human)
	}
	HumanEnv = "OWN_HUMAN"
	if c, _ := load(home, osUser, host); c.Human != "ownuser" {
		t.Fatalf("own variable: %q", c.Human)
	}
	t.Setenv("OWN_HUMAN", "")
	if c, _ := load(home, func() string { return "osuser" }, host); c.Human != "osuser" {
		t.Fatalf("own variable unset reads ZM_HUMAN: %q", c.Human)
	}
}
