// Package hostmem reads how hard this machine's memory is pressed now: the
// system's own pressure level and how much swap is in use. The menu bar
// item (potluck host status, docs/menubar.md) shows it beside Potluck's
// share of the memory, so a lender sees whether lending gets in the way of
// their own apps.
package hostmem

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/hostswap"
)

// Pressure levels, as macOS names them (kern.memorystatus_vm_pressure_level:
// 1, 2, 4). Unknown is a machine that would not say, or one Potluck does
// not read it on.
const (
	Normal   = "normal"
	Warn     = "warn"
	Critical = "critical"
	Unknown  = "unknown"
)

// Reading is the memory now. SwapTotalMB is 0 with no swap; Swap says
// whether the swap figures were read.
type Reading struct {
	Pressure    string `json:"pressure"`
	SwapUsedMB  int64  `json:"swap_used_mb"`
	SwapTotalMB int64  `json:"swap_total_mb"`
	Swap        bool   `json:"swap_read"`
}

// Read reads the machine's memory pressure and swap. osName is
// runtime.GOOS's word for it; meminfo reads /proc/meminfo on Linux.
func Read(ctx context.Context, run hostswap.Runner, osName string, meminfo func() ([]byte, error)) Reading {
	out := Reading{Pressure: Unknown}
	switch osName {
	case "darwin":
		if r, err := run.Run(ctx, "sysctl", "-n", "kern.memorystatus_vm_pressure_level"); err == nil && r.Exit == 0 {
			switch strings.TrimSpace(r.Stdout) {
			case "1":
				out.Pressure = Normal
			case "2":
				out.Pressure = Warn
			case "4":
				out.Pressure = Critical
			}
		}
		if r, err := run.Run(ctx, "sysctl", "-n", "vm.swapusage"); err == nil && r.Exit == 0 {
			out.SwapTotalMB, out.SwapUsedMB, out.Swap = darwinSwap(r.Stdout)
		}
	case "linux":
		if b, err := meminfo(); err == nil {
			out.SwapTotalMB, out.SwapUsedMB, out.Swap = linuxSwap(string(b))
		}
	}
	return out
}

// darwinSwap reads vm.swapusage: "total = 11264.00M  used = 9818.62M  free =
// 1445.38M  (encrypted)".
func darwinSwap(s string) (total, used int64, ok bool) {
	f := strings.Fields(s)
	var gotT, gotU bool
	for i := 0; i+2 < len(f); i++ {
		if f[i+1] != "=" {
			continue
		}
		mb, good := megabytes(f[i+2])
		switch {
		case !good:
		case f[i] == "total":
			total, gotT = mb, true
		case f[i] == "used":
			used, gotU = mb, true
		}
	}
	if !gotT || !gotU || used > total {
		return 0, 0, false
	}
	return total, used, true
}

// megabytes reads sysctl's size: a number with a K, M or G suffix.
func megabytes(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	scale := map[byte]float64{'K': 1.0 / 1024, 'M': 1, 'G': 1024}[s[len(s)-1]]
	if scale == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return int64(n*scale + 0.5), true
}

// linuxSwap reads /proc/meminfo's SwapTotal and SwapFree (kB).
func linuxSwap(s string) (total, used int64, ok bool) {
	kb := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || (f[0] != "SwapTotal:" && f[0] != "SwapFree:") {
			continue
		}
		if n, err := strconv.ParseInt(f[1], 10, 64); err == nil && n >= 0 {
			kb[f[0]] = n
		}
	}
	t, okT := kb["SwapTotal:"]
	fr, okF := kb["SwapFree:"]
	if !okT || !okF || fr > t {
		return 0, 0, false
	}
	return t / 1024, (t - fr) / 1024, true
}

// Local reads this machine. Tests replace it.
var Local = func(ctx context.Context) Reading {
	return Read(ctx, hostswap.ExecRunner{}, runtime.GOOS, func() ([]byte, error) { return os.ReadFile("/proc/meminfo") })
}
