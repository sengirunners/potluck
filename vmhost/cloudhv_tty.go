package vmhost

// A terminal in a cloud-hypervisor box (pty.go; docs/cloudhv.md): the
// --cloudhv-tty process is a command's client, as --cloudhv-exec is, started
// on a host terminal the process's holder made raw. The command runs on a
// terminal the guest's agent opens for it (cloudhvguest.OpPTY); this process
// carries the host terminal's size to it, at the start and at each resize,
// and the bytes both ways.

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/sengirunners/potluck/vmhost/cloudhvguest"
)

func (l cloudhvCLI) ExecTTY(vm string, argv ...string) *exec.Cmd {
	return l.client(flagCloudHVTTY, vm, argv)
}

// rawTTY: the client changes nothing of the terminal it stands on, so the
// holder makes that one raw (pty.go).
func (cloudhvCLI) rawTTY() {}

// cloudhvOnTerminal is a guest command on a terminal in the guest, which
// follows the terminal this process stands on, its stdin: that one's size
// at the start and after each resize, what is typed on it, and what the
// guest's shows. The guest's terminal is the one that echoes, edits a line
// and turns a Ctrl-C into a signal, so this one must be raw: its bytes pass
// as they are, both ways.
func cloudhvOnTerminal(g io.ReadWriter, req cloudhvguest.Request) (int, error) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	resized := make(chan struct{}, 1)
	go func() {
		for range winch {
			resized <- struct{}{}
		}
	}()
	return cloudhvguest.DoPTY(g, req, cloudhvTerminalSize, resized, os.Stdin, os.Stdout)
}

// cloudhvTerminalSize is the size of the terminal this process stands on:
// 24 by 80 when it stands on none.
func cloudhvTerminalSize() cloudhvguest.Size {
	cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil || rows < 1 || cols < 1 || rows > 0xffff || cols > 0xffff {
		return cloudhvguest.Size{Rows: 24, Cols: 80}
	}
	return cloudhvguest.Size{Rows: uint16(rows), Cols: uint16(cols)}
}
