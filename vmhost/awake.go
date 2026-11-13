package vmhost

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// awakeKeeper is a layer whose VMs stop when the host sleeps (Tart on a Mac:
// a sleeping Mac pauses its VMs, and on wake their runs end expired). Its
// KeepAwake answers the command that holds the host awake until the process
// with the given pid exits, or nil when this host has no such command.
type awakeKeeper interface{ KeepAwake(pid int) *exec.Cmd }

// KeepAwake is `caffeinate -i -w <pid>`: no idle system sleep while that
// process lives (the display may still sleep). -w ends caffeinate with the
// supervisor even when the supervisor is killed and cannot clean up. On a
// host with no caffeinate (Linux) it answers nil; potluck doctor is where a Mac
// that sleeps is flagged (hostprobe's detectSleep).
func (tartCLI) KeepAwake(pid int) *exec.Cmd {
	path, err := exec.LookPath("caffeinate")
	if err != nil {
		return nil
	}
	return exec.Command(path, "-i", "-w", strconv.Itoa(pid))
}

// holdAwake starts the layer's hold on the host's sleep, for as long as this
// supervisor serves its VM. The hold has no stop of its own: caffeinate's -w
// ends it with the supervisor on every exit path, a kill included. A hold that
// cannot start is logged and the run carries on: doctor already tells the
// owner whether this Mac sleeps.
func (s *sup) holdAwake() {
	k, ok := s.vm.(awakeKeeper)
	if !ok {
		return
	}
	cmd := k.KeepAwake(os.Getpid())
	if cmd == nil {
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(s.log, "potluck-driver-tart: could not keep the Mac awake:", err)
		return
	}
	go func() { _ = cmd.Wait() }() // reaped if it ends first
}
