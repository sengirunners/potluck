package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// A missing UDP capability or a failed send must not certify isolation just
// because nothing reached the host. Exercise the published guest script.
func TestMulticastProbeRequiresSuccessfulSend(t *testing.T) {
	for _, senderExit := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(senderExit), func(t *testing.T) {
			bin := t.TempDir()
			stub := "#!/bin/sh\ncase \"$1\" in -h) exit 0;; -u) exit " + strconv.Itoa(senderExit) + ";; esac\nexit 99\n"
			if err := os.WriteFile(filepath.Join(bin, "nc"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", ProbeScript, "probe", "net", "mcast", "239.255.77.77", "47123", "test-token")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			want := 0
			if senderExit != 0 {
				want = exitUnreliable
			}
			if got := cmd.ProcessState.ExitCode(); got != want {
				t.Fatalf("sender exits %d: probe exit %d, want %d (%v): %s", senderExit, got, want, err, out)
			}
		})
	}
}
