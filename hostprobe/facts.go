package hostprobe

import (
	"bufio"
	"github.com/sengirunners/potluck/vmhost"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// SystemFacts reads the real machine.
type SystemFacts struct{}

func (SystemFacts) OS() string   { return runtime.GOOS }
func (SystemFacts) Arch() string { return runtime.GOARCH }
func (SystemFacts) Cores() int   { return runtime.NumCPU() }

func (SystemFacts) RAMMB() int64 {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
				kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
				return kb / 1024
			}
		}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		b, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return b >> 20
	}
	return 0
}

func (SystemFacts) FreeDiskGB(dir string) (float64, error) {
	free, _, err := DiskGB(dir)
	return free, err
}

// DiskGB is the free and total size, in GiB, of the disk dir is on (a
// directory not made yet: the nearest parent that exists).
func DiskGB(dir string) (free, total float64, err error) {
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(dir, &st)
		if err == nil {
			return float64(st.Bavail) * float64(st.Bsize) / (1 << 30), float64(st.Blocks) * float64(st.Bsize) / (1 << 30), nil
		}
		parent := dir[:max(strings.LastIndex(dir, "/"), 0)]
		if parent == dir || parent == "" || !os.IsNotExist(err) {
			return 0, 0, err
		}
		dir = parent
	}
}

func (SystemFacts) Exists(path string) bool { _, err := os.Stat(path); return err == nil }

func (SystemFacts) LookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	return p, err == nil
}

func (SystemFacts) OpenRW(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func (SystemFacts) OCIEngine(cli, runtime string) error { return vmhost.VerifyOCIEngine(cli, runtime) }

func (SystemFacts) Getenv(k string) string { return os.Getenv(k) }
