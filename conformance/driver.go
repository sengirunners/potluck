package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Verbs (SPEC 20).
const (
	verbDescribe = "describe"
	verbAcquire  = "acquire"
	verbStatus   = "status"
	verbRelease  = "release"
	verbList     = "list"
	verbRenew    = "renew"
	verbSetup    = "setup"
	verbExec     = "exec" // a box's only extra verb
)

var requiredVerbs = []string{verbDescribe, verbAcquire, verbStatus, verbRelease, verbList}

const (
	execPrefix   = protocol.DriverPrefix
	verbLimit    = 30 * time.Second
	acquireLimit = 120 * time.Second
	waitDelay    = 2 * time.Second // as the daemon's adapter runner
	maxOutput    = 16 << 20        // what the suite keeps of a driver's stderr and of all its answers together
	// execTimeoutMax is the longest timeout_s the suite sends an exec.
	execTimeoutMax = 30
	// execCap is SPEC 20's cap on each of a box exec's stdout and stderr.
	execCap = 1 << 20
)

// envAllowlist is all of the suite's environment a driver gets (SPEC 20).
var envAllowlist = []string{"PATH", "HOME", "LANG", "TERM", "TZ"}

func allowedEnv() []string {
	env := []string{}
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Find locates a driver: a name with a path separator is used as is, anything
// else is potluck-driver-<name> or zm-run-<name> on PATH (protocol.LookDriver).
func Find(name string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		st, err := os.Stat(name)
		if err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s is not an executable file", name)
		}
		return name, nil
	}
	exe := name
	if protocol.DriverProgram(exe) == "" {
		exe = execPrefix + exe
	}
	p, err := protocol.LookDriver(name)
	if err != nil {
		return "", fmt.Errorf("no %s on PATH", exe)
	}
	return filepath.Clean(p), nil
}

func randomTag() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// capped keeps the first maxOutput bytes written to it.
type capped struct {
	bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room < len(p) {
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

type callRecord struct {
	verb  string
	took  time.Duration
	limit time.Duration
	late  bool
}

func (r *run) limit(verb string) time.Duration {
	switch {
	case verb == verbAcquire && r.opt.AcquireLimit > 0:
		return r.opt.AcquireLimit
	case verb == verbAcquire, verb == verbStart: // a start boots the box again
		return acquireLimit
	case verb == verbExec && r.opt.VerbLimit > 0: // timeout_s (at most 30 here) + 30 s
		return r.opt.VerbLimit + execTimeoutMax*time.Second
	case verb == verbExec:
		return verbLimit + execTimeoutMax*time.Second
	case r.opt.VerbLimit > 0:
		return r.opt.VerbLimit
	}
	return verbLimit
}

// exchange runs the driver once with raw stdin, as the daemon's adapter runner
// does: its own process group, killed on a timeout, a held-open pipe a failure.
// It returns stdout and the driver's run error, which decode judges by
// protocol.ReadAnswer (a kept-open pipe is part of exit); a timeout is err.
func (r *run) exchange(verb string, in []byte, env []string) (stdout []byte, exit error, err error) {
	limit := r.limit(verb)
	if verb == verbExec { // an exec sent a longer timeout_s (peersBox's listener) gets that much longer
		var e struct {
			Args struct {
				Timeout int `json:"timeout_s"`
			} `json:"args"`
		}
		if json.Unmarshal(in, &e) == nil && e.Args.Timeout > execTimeoutMax {
			limit += time.Duration(e.Args.Timeout-execTimeoutMax) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(r.ctx, limit)
	defer cancel()
	var cmd *exec.Cmd
	var out protocol.AnswerBuffer
	var stderr capped
	start := time.Now()
	// A driver written a moment ago can still be open for writing in a child
	// that another goroutine forked before its exec (Linux ETXTBSY,
	// golang/go#22315): the exec then fails without running. Retry briefly.
	for try := 0; ; try++ {
		out, stderr = protocol.AnswerBuffer{}, capped{}
		c := exec.CommandContext(ctx, r.path)
		cmd = c
		cmd.Env = env
		cmd.Stdin = bytes.NewReader(in)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = waitDelay
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err = cmd.Run(); !errors.Is(err, syscall.ETXTBSY) || try == 20 || ctx.Err() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	took := time.Since(start)
	late := errors.Is(ctx.Err(), context.DeadlineExceeded)
	r.mu.Lock() // a box's listening exec (peersBox) answers on its own goroutine
	r.calls = append(r.calls, callRecord{verb: verb, took: took, limit: limit, late: late})
	r.stderr.Write(stderr.Bytes())
	if verb != verbAcquire {
		r.answers.Write(out.Bytes())
	}
	r.mu.Unlock()
	if err != nil && cmd.Process != nil { // a failed run leaves nothing of its group running (the daemon also ends it when it refuses a good run's answer)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if late {
		return nil, nil, fmt.Errorf("%s: no answer within %s", verb, limit)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		exit = fmt.Errorf("%w, stderr %q", err, msg) // %w: ReadAnswer tells a held-open pipe from a non-zero exit
	}
	return out.Bytes(), exit, nil
}

// decode reads one call's stdout and exit by the daemon's own rule
// (protocol.ReadAnswer). A refusal comes back as *protocol.Error.
func decode(verb string, stdout []byte, exit error, out any) (*protocol.Error, error) {
	ans, err := protocol.ReadAnswer(stdout, exit)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", verb, err)
	}
	if !ans.OK {
		return ans.Error, nil
	}
	if out != nil {
		if err := protocol.DecodeResult(ans.Result, out); err != nil {
			return nil, fmt.Errorf("%s: result does not decode: %v", verb, err)
		}
	}
	return nil, nil
}

func envelope(verb, id string, a json.RawMessage, cfg map[string]string) []byte {
	if a == nil {
		a = json.RawMessage("{}")
	}
	in, _ := json.Marshal(protocol.NewRequest(verb, id, a, cfg))
	return in
}

// callCfg sends one request. A refusal comes back as *protocol.Error; a broken
// answer (crash, bad JSON, timeout) is a plain error.
func (r *run) callCfg(verb, id string, a json.RawMessage, cfg map[string]string, out any) (*protocol.Error, error) {
	stdout, exit, err := r.exchange(verb, envelope(verb, id, a, cfg), r.env)
	if err != nil {
		return nil, err
	}
	return decode(verb, stdout, exit, out)
}

func (r *run) call(verb, id string, a json.RawMessage, out any) (*protocol.Error, error) {
	return r.callCfg(verb, id, a, r.cfg, out)
}

// wantCode checks a refusal against the codes allowed.
func wantCode(e *protocol.Error, err error, codes ...string) error {
	switch {
	case err != nil:
		return err
	case e == nil:
		return fmt.Errorf("answered ok, want %s", strings.Join(codes, " or "))
	}
	for _, c := range codes {
		if e.Code == c {
			return nil
		}
	}
	return fmt.Errorf("answered %s (%s), want %s", e.Code, e.Message, strings.Join(codes, " or "))
}
