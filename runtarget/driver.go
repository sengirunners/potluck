package runtarget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// SPEC 20's verbs and time limits.
const (
	verbDescribe = "describe"
	verbAcquire  = "acquire"
	verbStatus   = "status"
	verbRelease  = "release"
	verbList     = "list"
	verbRenew    = "renew"
	verbExec     = "exec"
	verbSetup    = "setup"

	verbLimit    = rt.VerbLimit
	acquireLimit = rt.AcquireLimit
	waitDelay    = 2 * time.Second
)

// envAllowlist is all of the daemon's environment a driver gets (SPEC 20:
// env = "inherit" does not apply to run targets). driverEnv adds only that
// and how a person types Potluck's commands here (config.EnvPrefix), which
// the driver's messages name.
var envAllowlist = []string{"PATH", "HOME", "LANG", "TERM", "TZ"}

func driverEnv() []string {
	env := []string{config.EnvPrefix + "=" + config.PotluckPrefix()}
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// errTimeout marks a driver that did not answer in its time limit.
var errTimeout = errors.New("timed out")

// call runs one verb of binding t's driver and decodes its result into out.
// The driver's stderr is human text and is dropped: it never reaches logs,
// events or answers (a db url could be in it). A driver's refusal comes back
// as an *api.Error with the driver's code.
func (s *Service) call(ctx context.Context, t computeapi.RunTarget, verb, id string, args any, out any) error {
	limit := verbLimit
	if verb == verbAcquire {
		limit = acquireLimit
		if s.acquireWithin > 0 {
			limit = s.acquireWithin
		}
	}
	return s.callWithin(ctx, t, verb, id, args, out, limit)
}

// driverConfig is the config a driver is sent for binding t: its own keys,
// its name and kind, and the owner's image pins.
func driverConfig(t computeapi.RunTarget) map[string]string {
	cfg := map[string]string{}
	for k, v := range t.Config {
		cfg[k] = v
	}
	cfg["target"], cfg["kind"] = t.Name, t.Kind
	if len(t.Images) > 0 {
		cfg["images"] = strings.Join(t.Images, ",") // the owner's pins: the only images a box may boot
	}
	return cfg
}

// callWithin is call with an explicit time limit (exec: its timeout_s + 30 s).
func (s *Service) callWithin(ctx context.Context, t computeapi.RunTarget, verb, id string, args any, out any, limit time.Duration) error {
	if t.Host != "" { // a binding another machine serves (remote.go)
		return s.callRemote(ctx, t, verb, id, args, out, limit)
	}
	exe, findErr := s.lookPath(t.Driver)
	v, err := s.pins.open(t.Name, t.Driver, exe, findErr) // pin.go: refused unless it is the pinned file
	if err != nil {
		return err
	}
	defer v.Close()
	env := computeapi.RunTargetEnvelope{Potluck: protocol.Protocol, Zm: protocol.ProtocolOld, Verb: verb, ID: id, Config: driverConfig(t)}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		env.Args = b
	}
	in, err := json.Marshal(env)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(cctx, v.path)
	cmd.Env = driverEnv()
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(in)
	var stdout protocol.AnswerBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	if err := execVerified(cmd, v); err != nil {
		return api.Errorf(api.CodeUnavailable, "run target %q: driver %s refused: %v", t.Name, t.Driver, err)
	}
	runErr := cmd.Run()
	if runErr != nil { // a failed call leaves nothing of its group running, as on a timeout
		killGroup(cmd)
	}
	if cctx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("potluck-driver-%s %s: %w after %s", t.Driver, verb, errTimeout, limit)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ans, err := protocol.ReadAnswer(stdout.Bytes(), runErr) // the suite's rule too
	if err != nil {
		killGroup(cmd) // a refused answer keeps nothing of the driver, even a child that exited 0 left behind
		return api.Errorf(api.CodeInternal, "potluck-driver-%s %s: %v", t.Driver, verb, err)
	}
	if !ans.OK {
		e := api.Errorf(ans.Error.Code, "potluck-driver-%s %s: %s", t.Driver, verb, oneLine(ans.Error.Message))
		e.Data = volumeErrorData(verb, ans.Error.Code, ans.Error.Data)
		if verb == verbStatus && ans.Error.Code == api.CodeNotFound {
			return rt.NoHandle(e) // the target has no such handle: the one not_found that is about the handle
		}
		return &driverWords{e}
	}
	if out == nil {
		return nil
	}
	if err := protocol.DecodeResult(ans.Result, out); err != nil {
		killGroup(cmd) // a result that does not decode is a refused answer too
		return api.Errorf(api.CodeInternal, "potluck-driver-%s %s: bad result: %v", t.Driver, verb, err)
	}
	return nil
}

// driverWords is a driver's own refusal: its message is the driver's
// words, which may quote what it was sent (withheld, acquires.go). It still
// is the *api.Error to errors.As and api.IsCode.
type driverWords struct{ err *api.Error }

func (e *driverWords) Error() string { return e.err.Error() }
func (e *driverWords) Unwrap() error { return e.err }

// oneLine bounds a driver's message: it is data, shown to the caller.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "…" // never half a character
	}
	return s
}

// killGroup ends whatever is left of the driver's process group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
