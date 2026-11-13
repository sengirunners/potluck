package conformance

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/agentrunners"
)

// Group agent-runner: what the generic agent runner job kind
// (agentrunners) needs from a box, seen by starting jobs exactly as potluck
// agent-runner does, with stand-in adapters in place of a vendor's runner.
// A stand-in reads its key's file at the start and again seconds later
// (a runner reads it when it starts and may again when it reconnects), echoes
// the arguments its files carry, and exits 7; another drains on TERM.

// agentRunnerSecret is the stand-in's secret name.
const agentRunnerSecret = "ZMCONF_AGENT_KEY"

// standInRun is the stand-in's entry point: $1 the key's file, $2 its
// directory, where "expect" holds the key in hex and "args" its arguments.
const standInRun = `#!/bin/sh
secret=$1
dir=$2
set --
` + agentrunners.ReadArgs + `"$dir/args"
printf 'args:%s\n' "$#"
for a; do printf 'arg:%s\n' "$a"; done
expect=$(cat "$dir/expect")
got=$(od -An -tx1 "$secret" | tr -d ' \n')
if [ "$got" = "$expect" ]; then echo "start: key ok"; else echo "start: key differs"; fi
printf 'mode:%s\n' "$(ls -ln "$secret" | cut -c1-10)"
sleep 3
got=$(od -An -tx1 "$secret" 2>/dev/null | tr -d ' \n')
if [ "$got" = "$expect" ]; then echo "late: key ok"; else echo "late: key gone"; fi
exit 7
`

// standInDrain waits for work until TERM, then takes 2 s to hand it back.
const standInDrain = `#!/bin/sh
trap 'echo draining; sleep 2; echo drained; exit 0' TERM
echo ready
while :; do sleep 1; done
`

// agentRunnerArgs are the stand-in's arguments: shell in them is never run.
var agentRunnerArgs = []string{"Bash(git push:*)", "it's $(echo ran) `echo ran` \"q\" *", "--x"}

func standIn(name, run string, extra map[string]agentrunners.File) agentrunners.Adapter {
	return agentrunners.Adapter{Name: name, About: "the conformance suite's stand-in runner", Secret: agentRunnerSecret,
		Needs: []string{"od", "tr", "ls", "cut", "sleep"},
		Files: func(o agentrunners.Options) (map[string]agentrunners.File, error) {
			f := map[string]agentrunners.File{agentrunners.Entry: {Data: run, Mode: 0o700}, "args": {Data: agentrunners.ArgsFile(o.Allow), Mode: 0o600}}
			for k, v := range extra {
				f[k] = v
			}
			return f, nil
		}}
}

// driverBox is agentrunners.Box straight against the driver.
type driverBox struct {
	r *run
	h string
}

func (b driverBox) Exec(_ context.Context, argv []string, stdin string) (int, string, string, error) {
	res, err := b.r.exec2(b.h, args("argv", argv, "stdin", stdin), 60)
	if err != nil {
		return 0, "", "", err
	}
	return *res.Exit, *res.Stdout, *res.Stderr, nil
}

func (b driverBox) Spawn(_ context.Context, argv []string, secrets map[string]string, timeoutS int, key string) (string, error) {
	kv := []any{"argv", argv, "secrets", secrets, "request_id", key}
	if timeoutS > 0 {
		kv = append(kv, "timeout_s", timeoutS)
	}
	return b.r.spawn(b.h, kv...)
}

func (r *run) agentRunner() {
	if r.kind != "box" {
		return
	}
	const (
		started = "agent runner job: its files arrive, its entry point gets the key's file and its arguments, its exit passes through"
		late    = "agent runner job: the key's file is still there seconds into the job"
		drain   = "agent runner job: TERM lets the runner hand back its work before it ends"
	)
	caps := []string{}
	if r.desc.Capabilities != nil {
		caps = *r.desc.Capabilities
	}
	if !r.has(verbSpawn) || !slices.Contains(caps, secretsCap) {
		for _, n := range []string{started, late, drain} {
			r.skip(n, "describe lists no spawn or no secrets capability: such a box serves no agent runner")
		}
		return
	}
	h, err := r.acquireAny(r.longTTL())
	if !r.add("acquire a box for agent runner jobs", err) {
		return
	}
	defer r.endHeld(h)
	box := driverBox{r, h.Handle}
	val := "zmconf-secret-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, val)
	r.sawSecret = true

	ad := standIn("conformance", standInRun, map[string]agentrunners.File{"expect": {Data: hex.EncodeToString([]byte(val)), Mode: 0o600}})
	var o outputResult
	pid, err := agentrunners.Start(r.ctx, box, agentrunners.Job{Adapter: ad, Options: agentrunners.Options{Allow: agentRunnerArgs}, Secret: val, Key: r.key(), TimeoutS: 60})
	if err == nil {
		if _, err = r.waitEnd(h.Handle, pid, 30*time.Second); err == nil {
			o, err = r.output(h.Handle, pid)
		}
	}
	if err != nil {
		r.add(started, err)
		r.add(late, fmt.Errorf("no job ran: %v", err))
	} else {
		out := *o.Stdout
		want := fmt.Sprintf("args:%d\n", len(agentRunnerArgs))
		for _, a := range agentRunnerArgs {
			want += "arg:" + a + "\n"
		}
		switch {
		case *o.Exit != 7:
			err = fmt.Errorf("exit %d, want the entry point's 7 (stdout %q, stderr %q)", *o.Exit, clip(out), clip(*o.Stderr))
		case !strings.HasPrefix(out, want):
			err = fmt.Errorf("the entry point's arguments: %q, want %q", clip(out), want)
		case !strings.Contains(out, "start: key ok\n"):
			err = fmt.Errorf("the key's file did not hold the key at the start (stdout %q)", clip(out))
		case !strings.Contains(out, "mode:-rw-------\n"):
			err = fmt.Errorf("the key's file is not mode 0600 (stdout %q)", clip(out))
		}
		r.add(started, err)
		if !strings.Contains(out, "late: key ok\n") {
			r.add(late, fmt.Errorf("the key's file was gone or changed 3 s into the job (stdout %q)", clip(out)))
		} else {
			r.add(late, nil)
		}
	}
	r.add(drain, r.agentRunnerDrain(box, h.Handle, val))
}

// agentRunnerDrain starts the draining stand-in, sends TERM once it is
// ready, and wants its own exit 0 after "drained".
func (r *run) agentRunnerDrain(box driverBox, h, val string) error {
	pid, err := agentrunners.Start(r.ctx, box, agentrunners.Job{Adapter: standIn("conformance-drain", standInDrain, nil), Secret: val, Key: r.key(), TimeoutS: 60})
	if err != nil {
		return err
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		o, err := r.output(h, pid, "wait_s", 2)
		if err != nil {
			return err
		}
		if strings.Contains(*o.Stdout, "ready") {
			break
		}
		if !*o.Running || !time.Now().Before(deadline) {
			return fmt.Errorf("the runner never said ready (running %v, stdout %q, stderr %q)", *o.Running, clip(*o.Stdout), clip(*o.Stderr))
		}
	}
	var sent struct {
		Sent bool `json:"sent"`
	}
	if err := must(r.call(verbSignal, h, args("pid", pid, "signal", "TERM"), &sent)); err != nil {
		return err
	}
	if _, err := r.waitEnd(h, pid, 15*time.Second); err != nil {
		return err
	}
	o, err := r.output(h, pid)
	if err != nil {
		return err
	}
	if *o.Exit != 0 || !strings.Contains(*o.Stdout, "drained") {
		return fmt.Errorf("exit %d, stdout %q: want the runner's own exit 0 after it drained", *o.Exit, clip(*o.Stdout))
	}
	return nil
}
