package hostmem

import (
	"context"
	"errors"
	"testing"

	"github.com/sengirunners/potluck/hostswap"
)

type answers map[string]hostswap.Result

func (a answers) Run(_ context.Context, name string, args ...string) (hostswap.Result, error) {
	r, ok := a[args[len(args)-1]]
	if !ok {
		return hostswap.Result{}, errors.New("no " + name)
	}
	return r, nil
}

func TestReadDarwin(t *testing.T) {
	for _, c := range []struct {
		level, swap string
		want        Reading
	}{
		{"1\n", "total = 11264.00M  used = 9818.62M  free = 1445.38M  (encrypted)\n", Reading{Normal, 9819, 11264, true}},
		{"2", "total = 2.00G  used = 512.00K  free = 2.00G  (encrypted)", Reading{Warn, 1, 2048, true}},
		{"4", "total = 0.00M  used = 0.00M  free = 0.00M", Reading{Critical, 0, 0, true}},
		{"9", "garbage", Reading{Unknown, 0, 0, false}},
		{"", "total = 1.00M", Reading{Unknown, 0, 0, false}},
	} {
		run := answers{"kern.memorystatus_vm_pressure_level": {Stdout: c.level}, "vm.swapusage": {Stdout: c.swap}}
		if got := Read(context.Background(), run, "darwin", nil); got != c.want {
			t.Errorf("%q %q: %+v, want %+v", c.level, c.swap, got, c.want)
		}
	}
	if got := Read(context.Background(), answers{}, "darwin", nil); got != (Reading{Pressure: Unknown}) {
		t.Errorf("nothing answers: %+v", got)
	}
	failed := answers{"kern.memorystatus_vm_pressure_level": {Stdout: "4", Exit: 1}}
	if got := Read(context.Background(), failed, "darwin", nil); got.Pressure != Unknown {
		t.Errorf("a failed sysctl: %+v", got)
	}
}

func TestReadLinux(t *testing.T) {
	mi := func(s string) func() ([]byte, error) { return func() ([]byte, error) { return []byte(s), nil } }
	got := Read(context.Background(), answers{}, "linux", mi("MemTotal: 100 kB\nSwapTotal:  2097152 kB\nSwapFree:  1048576 kB\n"))
	if got != (Reading{Unknown, 1024, 2048, true}) {
		t.Errorf("%+v", got)
	}
	if got := Read(context.Background(), answers{}, "linux", mi("SwapTotal: 10 kB\nSwapFree: 20 kB\n")); got.Swap {
		t.Errorf("more free than total: %+v", got)
	}
	if got := Read(context.Background(), answers{}, "plan9", nil); got != (Reading{Pressure: Unknown}) {
		t.Errorf("%+v", got)
	}
}
