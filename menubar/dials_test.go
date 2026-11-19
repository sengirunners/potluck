package menubar

import (
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostmem"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
)

// sized is box with memory.
func sized(name string, cpu, memMB, slots int, live []string) computeapi.RunTargetView {
	v := box(name, "", "linux", cpu, slots, live, nil)
	v.Describe.Size.MemoryMB = memMB
	return v
}

func dial(ds []Dial, id string) *Dial {
	for i := range ds {
		if ds[i].ID == id {
			return &ds[i]
		}
	}
	return nil
}

// The dials bound what is lent, in steps of the smallest size; the one in
// force is checked and runs nothing; jobs over a lowered dial are said to
// finish first.
func TestDials(t *testing.T) {
	targets := []computeapi.RunTargetView{sized("c2", 2, 4096, 3, []string{"a", "b"}), sized("c3", 3, 8192, 3, nil)}
	f := Facts{Cores: 12, Targets: targets, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"},
		Lend: vmhost.HostLend{Cores: 2, MemoryMB: 16384}, MemoryMB: 48 << 10, DiskFreeGB: 300, DiskTotalGB: 460}
	st := Build(f)
	l := st.Lending
	if l.Shared != 9 || l.Lent != 2 || l.InUse != 4 || l.MemoryShared != 24576 || l.MemoryLent != 16384 || l.MemoryInUse != 8192 {
		t.Fatalf("lending %+v", l)
	}
	m := st.Menu
	if m.Title != "Lending 2 of 12 cores" {
		t.Errorf("title %q", m.Title)
	}
	if !strings.Contains(m.Note, "running jobs hold 4 cores (2 lent now): they finish, and no new job starts until they are under it; Stop now ends them") {
		t.Errorf("note %q", m.Note)
	}
	cores := dial(m.Dials, "cores")
	if cores == nil || cores.Label != "Cores lent: 2 cores of 9 cores shared" || len(cores.Choices) != 5 {
		t.Fatalf("cores dial %+v", cores)
	}
	for i, want := range []string{"2 cores", "4 cores", "6 cores", "8 cores", "All shared (9 cores)"} {
		c := cores.Choices[i]
		if c.Label != want || c.Checked != (i == 0) || (c.Args == nil) != c.Checked {
			t.Errorf("choice %d: %+v", i, c)
		}
	}
	if cores.Choices[0].Sub != "too small for size c3: its jobs do not start" || cores.Choices[1].Sub != "" {
		t.Errorf("too small: %q %q", cores.Choices[0].Sub, cores.Choices[1].Sub)
	}
	if got := strings.Join(cores.Choices[1].Args, " "); got != "host lend --cores 4" {
		t.Errorf("args %q", got)
	}
	if got := strings.Join(cores.Choices[4].Args, " "); got != "host lend --cores all" {
		t.Errorf("all args %q", got)
	}
	mem := dial(m.Dials, "memory")
	if mem == nil || mem.Label != "Memory lent: 16 GB of 24 GB shared" || mem.Choices[0].Label != "4 GB" || !mem.Choices[3].Checked ||
		strings.Join(mem.Choices[1].Args, " ") != "host lend --memory-gb 8" {
		t.Errorf("memory dial %+v", mem)
	}
	disk := dial(m.Dials, "disk")
	if disk == nil || disk.Label != "Disk kept free for you: none set (300 GB free now)" || len(disk.Choices) != 6 || !disk.Choices[0].Checked ||
		disk.Choices[5].Label != "Keep 200 GB free" || strings.Join(disk.Choices[3].Args, " ") != "host lend --keep-free-gb 50" {
		t.Errorf("disk dial %+v", disk)
	}

	// All shared, and an amount set by the command that is no step.
	f.Lend = vmhost.HostLend{Cores: 5, KeepFreeGB: 75}
	m = Build(f).Menu
	cores = dial(m.Dials, "cores")
	if cores.Choices[2].Label != "5 cores" || !cores.Choices[2].Checked {
		t.Errorf("an amount set by the command: %+v", cores.Choices)
	}
	if mem := dial(m.Dials, "memory"); mem.Label != "Memory lent: all shared, 24 GB" || !mem.Choices[len(mem.Choices)-1].Checked {
		t.Errorf("memory all shared: %+v", mem)
	}
	if disk := dial(m.Dials, "disk"); disk.Label != "Disk kept free for you: 75 GB (300 GB free now)" || !disk.Choices[4].Checked ||
		strings.Join(disk.Choices[0].Args, " ") != "host lend --keep-free-gb 0" {
		t.Errorf("disk 75: %+v", disk)
	}
	if strings.Contains(m.Note, "they finish") {
		t.Errorf("under the dial, no note: %q", m.Note)
	}

	// Dials that cannot be read hold new work and offer no dials.
	f.LendErr = "host-lend.json: bad"
	if m := Build(f).Menu; len(m.Dials) != 0 || !strings.Contains(m.Note, "no new jobs start until your limits can be read: host-lend.json: bad") {
		t.Errorf("unreadable dials: %+v", m)
	}
}

// The memory line says the pressure, Potluck's share and swap; at high
// pressure, whether lending is part of it.
func TestMemoryLine(t *testing.T) {
	busy := []computeapi.RunTargetView{sized("c2", 2, 6144, 2, []string{"a"})}
	host := &protocol.HostStatus{Mode: "auto", State: "accepting"}
	for _, c := range []struct {
		name    string
		r       *hostmem.Reading
		targets []computeapi.RunTargetView
		daemon  string
		want    string
	}{
		{"none read", nil, busy, "", ""},
		{"normal", &hostmem.Reading{Pressure: hostmem.Normal, Swap: true, SwapUsedMB: 512, SwapTotalMB: 2048}, busy, "",
			"Memory pressure normal; Potluck's jobs may use up to 6 GB of 48 GB (13%); swap 0.5 GB used of 2 GB"},
		{"high, lending", &hostmem.Reading{Pressure: hostmem.Warn}, busy, "",
			"Memory pressure high; Potluck's jobs may use up to 6 GB of 48 GB (13%). Lending is part of it: turn Memory lent down, or Wind down, to give memory back"},
		{"critical, idle", &hostmem.Reading{Pressure: hostmem.Critical, Swap: true}, nil, "",
			"Memory pressure critical; Potluck's jobs use none. Lending is not the cause: no Potluck job is running"},
		{"no daemon", &hostmem.Reading{Pressure: hostmem.Unknown}, nil, "down", "Memory pressure not known"},
		{"sizes of no memory", &hostmem.Reading{Pressure: hostmem.Warn}, []computeapi.RunTargetView{sized("x", 2, 0, 2, []string{"a"})}, "",
			"Memory pressure high; how much memory Potluck's jobs use is not known. Lending is part of it: turn Memory lent down, or Wind down, to give memory back"},
	} {
		got := Build(Facts{Cores: 8, Targets: c.targets, Host: host, Memory: c.r, MemoryMB: 48 << 10, Daemon: c.daemon}).Menu.Memory
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
