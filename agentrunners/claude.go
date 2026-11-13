package agentrunners

import (
	"fmt"
	"strconv"
	"time"
)

// claude is Claude Code's self-hosted runner (code.claude.com/docs,
// "Self-hosted environments"): `claude self-hosted-runner` polls Anthropic
// for sessions of one self-hosted environment, whose key (the environment
// secret, made in claude.ai's admin settings) it reads from a file. With
// --capacity 1 it serves one session and exits when that session ends.
//
// Every session runs through the job's own wrapper (--exec-path), which
// appends the job's flags after the runner's own arguments: Claude Code
// takes the last --permission-mode on its command line, so a session starts
// in the job's mode (a mode its person then picks in claude.ai takes over);
// --allowed-tools and --disallowed-tools add up.
//
// --base-dir is the same in every job ($HOME/claude-work): a session that
// moves to another runner (one retired, one gone) finds its checkout by that
// runner's base directory, so it must not change from runner to runner.
var claude = Adapter{
	Name:   "claude",
	About:  "Claude Code's self-hosted runner: claude self-hosted-runner",
	Secret: "CLAUDE_ENVIRONMENT_SECRET",
	Needs:  []string{"claude"},
	Files:  claudeFiles,
}

// claudeRun is the entry point: the runner, one session, this box.
const claudeRun = `#!/bin/sh
# Potluck agent runner (claude): $1 is the environment secret's file, $2 this job's directory.
secret=$1
dir=$2
set -- self-hosted-runner --environment-secret-file "$secret" --base-dir "$HOME/claude-work" --capacity 1 --exec-path "$dir/session"
` + ReadArgs + `"$dir/runner-args"
exec claude "$@"
`

// claudeSession is the wrapper the runner starts for each session: the
// runner's pinned Claude Code binary with the job's flags after its own.
const claudeSession = `#!/bin/sh
# Potluck agent runner (claude): one session, with this job's pinned flags last.
dir=$(dirname "$0")
bin=${CLAUDE_RUNNER_CLAUDE_BIN:?potluck: the runner did not name its claude binary (CLAUDE_RUNNER_CLAUDE_BIN)}
` + ReadArgs + `"$dir/session-args"
exec "$bin" "$@"
`

func claudeFiles(o Options) (map[string]File, error) {
	if err := o.Check(); err != nil {
		return nil, err
	}
	var runner []string
	if o.Idle > 0 {
		runner = append(runner, "--exit-if-unused-min", strconv.Itoa(int((o.Idle+time.Minute-1)/time.Minute)))
	}
	if !o.RetireAt.IsZero() {
		runner = append(runner, "--retire-at", strconv.FormatInt(o.RetireAt.Unix(), 10))
	}
	var session []string
	switch o.Mode {
	case "":
	case "auto", "acceptEdits", "plan", "default":
		session = append(session, "--permission-mode", o.Mode)
	default:
		return nil, fmt.Errorf("permission mode %q: auto, acceptEdits, plan, default, or none", o.Mode)
	}
	for _, t := range o.Allow {
		session = append(session, "--allowed-tools", t)
	}
	for _, t := range o.Deny {
		session = append(session, "--disallowed-tools", t)
	}
	return map[string]File{
		Entry:          {claudeRun, 0o700},
		"session":      {claudeSession, 0o700},
		"runner-args":  {ArgsFile(runner), 0o600},
		"session-args": {ArgsFile(session), 0o600},
	}, nil
}
