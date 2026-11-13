package runtarget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
)

// HostCommand runs `<driver> host <args...>` for binding t, a binding this
// machine serves, as the owner's own command (potluck host shelf fill): the
// driver is found and checked against its pin as the daemon does (pin.go),
// (it must already have a pin), and t's config goes on stdin in the request envelope. The driver's output
// goes to stdout and stderr as is: it is the owner's terminal, not a log.
func HostCommand(ctx context.Context, home string, t computeapi.RunTarget, args []string, stdout, stderr io.Writer) error {
	if t.Host != "" {
		return errors.New("run target " + t.Name + " is served by another machine")
	}
	p := newPins(home, nil)
	p.noFirstPin = true // only a driver already pinned (by the daemon's first use, or potluck driver pin)
	exe, findErr := FindDriver(DriverDirs(home), t.Driver)
	v, err := p.open(t.Name, t.Driver, exe, findErr)
	if err != nil {
		return err
	}
	defer v.Close()
	in, err := json.Marshal(computeapi.RunTargetEnvelope{Potluck: protocol.Protocol, Zm: protocol.ProtocolOld, Verb: "host", Config: driverConfig(t)})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, v.path, append([]string{"host"}, args...)...)
	cmd.Env = driverEnv()
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	if err := execVerified(cmd, v); err != nil {
		return err
	}
	return cmd.Run()
}
