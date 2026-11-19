package menubar

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostmem"
)

// The lender's dials and the memory line (zm-eakh.12.97.13): how hard the
// Mac's memory is pressed and how much of it Potluck's jobs hold, and three
// submenus that turn down, or back up, the cores and memory lent and the
// disk kept free (potluck host lend, vmhost lend.go). docs/menubar.md.

// Dial is one of the lender's dials as the menu draws it: a submenu titled
// with the limit in force, holding one choice per step; the checked choice
// is in force, and has no Args.
type Dial struct {
	ID      string `json:"id"` // cores, memory or disk
	Label   string `json:"label"`
	Choices []Item `json:"choices"`
}

// keepFreeSteps are the disk dial's choices, in GB, those under the disk's
// size offered.
var keepFreeSteps = []int{10, 25, 50, 100, 200, 500, 1000}

// memoryLine says how hard the memory is pressed, how much of it Potluck's
// running jobs may use, and swap; at high pressure, whether lending is part
// of it and what gives memory back. "" when nothing was read.
func memoryLine(f Facts, l Lending) string {
	if f.Memory == nil {
		return ""
	}
	m := *f.Memory
	line := map[string]string{hostmem.Normal: "Memory pressure normal", hostmem.Warn: "Memory pressure high",
		hostmem.Critical: "Memory pressure critical"}[m.Pressure]
	if line == "" {
		line = "Memory pressure not known"
	}
	switch {
	case f.Daemon != "":
	case l.Jobs == 0:
		line += "; Potluck's jobs use none"
	case l.MemoryInUse == 0:
		line += "; how much memory Potluck's jobs use is not known"
	case f.MemoryMB > 0:
		line += fmt.Sprintf("; Potluck's jobs may use up to %s of %s (%d%%)", gb(l.MemoryInUse), gb(int(f.MemoryMB)), int(float64(l.MemoryInUse)*100/float64(f.MemoryMB)+0.5))
	default:
		line += "; Potluck's jobs may use up to " + gb(l.MemoryInUse)
	}
	if m.Swap && m.SwapTotalMB > 0 {
		line += fmt.Sprintf("; swap %s used of %s", gb(int(m.SwapUsedMB)), gb(int(m.SwapTotalMB)))
	}
	if m.Pressure == hostmem.Warn || m.Pressure == hostmem.Critical {
		switch {
		case f.Daemon != "":
		case l.Jobs == 0:
			line += ". Lending is not the cause: no Potluck job is running"
		default:
			line += ". Lending is part of it: turn Memory lent down, or Wind down, to give memory back"
		}
	}
	return line
}

// dials are the three submenus. A dial for what nothing shared lends (no
// box sizes) is left out; the disk dial needs the disk's size.
func dials(f Facts, l Lending) []Dial {
	var out []Dial
	if l.Shared > 0 && l.minCPU > 0 {
		out = append(out, stepDial("cores", "Cores lent", f.Lend.Cores, l.Shared, l.minCPU, l.Groups, "--cores", func(n int) string { return plural(n, "core") },
			func(n int) string { return strconv.Itoa(n) }, tooSmall(l.sizes, func(s computeapi.RunTargetSize) int { return s.CPU })))
	}
	if l.MemoryShared > 0 && l.minMemory > 0 {
		out = append(out, stepDial("memory", "Memory lent", f.Lend.MemoryMB, l.MemoryShared, l.minMemory, l.Groups, "--memory-gb", gb,
			func(n int) string { return strconv.FormatFloat(float64(n)/1024, 'f', -1, 64) }, tooSmall(l.sizes, func(s computeapi.RunTargetSize) int { return s.MemoryMB })))
	}
	if f.DiskTotalGB > 0 {
		out = append(out, diskDial(f))
	}
	return out
}

// stepDial is a dial of multiples of step up to shared (the smallest size's
// cores or memory, so every choice fits a whole box), then "all shared"; a
// dial set to some other amount (by the command) is a choice too.
func stepDial(id, title string, set, shared, step, groups int, flag string, words func(int) string, arg func(int) string, small func(int) string) Dial {
	var steps []int
	for n := step; n < shared; n += step {
		steps = append(steps, n)
	}
	if set > 0 && set < shared && !slices.Contains(steps, set) {
		steps = append(steps, set)
		slices.Sort(steps)
	}
	all := set <= 0 || set >= shared
	d := Dial{ID: id}
	if all {
		d.Label = fmt.Sprintf("%s: all shared, %s", title, words(shared))
	} else {
		d.Label = fmt.Sprintf("%s: %s of %s shared", title, words(set), words(shared))
	}
	if groups > 1 {
		d.Label += fmt.Sprintf(" (each of %d groups of sizes)", groups)
	}
	for _, n := range steps {
		it := Item{ID: id + "-" + strconv.Itoa(n), Label: words(n), Sub: small(n), Checked: !all && n == set}
		if !it.Checked {
			it.Args = []string{"host", "lend", flag, arg(n)}
		}
		d.Choices = append(d.Choices, it)
	}
	it := Item{ID: id + "-all", Label: "All shared (" + words(shared) + ")", Checked: all}
	if !all {
		it.Args = []string{"host", "lend", flag, "all"}
	}
	return Dial{ID: d.ID, Label: d.Label, Choices: append(d.Choices, it)}
}

// tooSmall says which sizes a dial of n leaves out, by what of a size it
// bounds ("" when none): their jobs do not start under it.
func tooSmall(sizes []computeapi.RunTargetSize, need func(computeapi.RunTargetSize) int) func(int) string {
	return func(n int) string {
		var out []string
		for _, s := range sizes {
			if need(s) > n && !slices.Contains(out, s.Name) {
				out = append(out, s.Name)
			}
		}
		if len(out) == 0 {
			return ""
		}
		return "too small for size " + strings.Join(out, ", ") + ": its jobs do not start"
	}
}

// diskDial keeps disk free for the lender: no box starts, and no kept disk,
// volume or shelf grows, below it.
func diskDial(f Facts) Dial {
	set := f.Lend.KeepFreeGB
	d := Dial{ID: "disk"}
	free := ""
	if f.DiskFreeGB >= 0 {
		free = fmt.Sprintf(" (%.0f GB free now)", f.DiskFreeGB)
	}
	if set > 0 {
		d.Label = fmt.Sprintf("Disk kept free for you: %d GB%s", set, free)
	} else {
		d.Label = "Disk kept free for you: none set" + free
	}
	steps := []int{}
	for _, n := range keepFreeSteps {
		if float64(n) < f.DiskTotalGB {
			steps = append(steps, n)
		}
	}
	if set > 0 && !slices.Contains(steps, set) {
		steps = append(steps, set)
		slices.Sort(steps)
	}
	none := Item{ID: "disk-none", Label: "None set", Sub: "Potluck's own floor for kept disks still applies", Checked: set <= 0}
	if !none.Checked {
		none.Args = []string{"host", "lend", "--keep-free-gb", "0"}
	}
	d.Choices = append(d.Choices, none)
	for _, n := range steps {
		it := Item{ID: "disk-" + strconv.Itoa(n), Label: fmt.Sprintf("Keep %d GB free", n), Checked: n == set}
		if !it.Checked {
			it.Args = []string{"host", "lend", "--keep-free-gb", strconv.Itoa(n)}
		}
		d.Choices = append(d.Choices, it)
	}
	return d
}

// gb is MiB as GB in words: whole when it is, else one decimal.
func gb(mb int) string {
	if mb%1024 == 0 {
		return fmt.Sprintf("%d GB", mb/1024)
	}
	return fmt.Sprintf("%.1f GB", float64(mb)/1024)
}

// overNote is said when running jobs hold more than a dial now lends: they
// finish, and nothing new starts until they are under it.
func overNote(l Lending) string {
	var what []string
	if l.InUse > l.Lent {
		what = append(what, fmt.Sprintf("%s (%d lent now)", plural(l.InUse, "core"), l.Lent))
	}
	if l.MemoryLent > 0 && l.MemoryInUse > l.MemoryLent {
		what = append(what, fmt.Sprintf("%s of memory (%s lent now)", gb(l.MemoryInUse), gb(l.MemoryLent)))
	}
	if len(what) == 0 {
		return ""
	}
	s := what[0]
	if len(what) == 2 {
		s += " and " + what[1]
	}
	return "running jobs hold " + s + ": they finish, and no new job starts until they are under it; Stop now ends them"
}
