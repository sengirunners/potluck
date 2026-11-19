package cfsetup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// Probe is tunnel.Probe: one outside look at a home, as setup's check and
// an invite's (tunnel.Reachable) both make it.
var Probe = tunnel.Probe

// ExecRunner runs the cloudflared at Path with a minimal environment (PATH,
// the user's, and env), its stderr streamed to out.
type ExecRunner struct{ Path string }

// Run implements Runner.
func (e ExecRunner) Run(ctx context.Context, env []string, out io.Writer, args ...string) ([]byte, error) {
	if e.Path == "" {
		return nil, errors.New("cloudflared is not provisioned in $ZM_HOME/tools (" + config.Command("tunnel setup") + " fetches the pinned one)")
	}
	cmd := exec.CommandContext(ctx, e.Path, args...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER")}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, out)
	cmd.Stderr = io.MultiWriter(&stderr, out)
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return stdout.Bytes(), &RunError{Args: args, Err: err, Stderr: msg}
	}
	return stdout.Bytes(), nil
}

// RunError is a cloudflared call that failed, with what it said.
type RunError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *RunError) Error() string {
	return "cloudflared " + strings.Join(e.Args, " ") + ": " + e.Err.Error() + ": " + e.Stderr
}
