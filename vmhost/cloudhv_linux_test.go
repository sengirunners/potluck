package vmhost

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The host's memory filesystem is /dev/shm when it is a tmpfs or a ramfs,
// and nothing else: /proc is a filesystem that is neither.
func TestCloudHVMemoryFilesystem(t *testing.T) {
	if err := cloudhvMemFS("/proc"); err == nil {
		t.Error("/proc was taken for a memory filesystem")
	}
	if err := cloudhvMemFS("/no/such/directory"); err == nil {
		t.Error("a directory that is not there was taken for a memory filesystem")
	}
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("this host has no /dev/shm")
	}
	if err := cloudhvMemFS("/dev/shm"); err != nil {
		t.Skipf("this host's /dev/shm is no memory filesystem: %v", err)
	}
	if d, err := cloudhvShm(); err != nil || d != "/dev/shm" {
		t.Errorf("the host's memory filesystem: %q, %v", d, err)
	}
}

// A VM's monitor, and the hypervisor it starts, can leave no core dump:
// their core limit is 1 byte (cloudhvNoCore).
func TestCloudHVAVMsProcessesLeaveNoCore(t *testing.T) {
	if !coreLimitCanBeOne() {
		t.Skip("this test's own hard core limit is 0: no process it starts can have 1")
	}
	c := cloudhvFixture(t, 0)
	c.boot("vm1", false)
	st := c.state("vm1")
	for who, pid := range map[string]int{"monitor": st.PID, "hypervisor": st.HypervisorPID} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
		if err != nil {
			t.Fatalf("the %s's limits: %v", who, err)
		}
		var got []string
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Max core file size") {
				got = strings.Fields(strings.TrimPrefix(line, "Max core file size"))
			}
		}
		if len(got) < 2 || got[0] != "1" || got[1] != "1" {
			t.Errorf("the %s's core limit is %v, want 1 and 1", who, got)
		}
	}
}
