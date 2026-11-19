package cli

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/vmhost"
)

// potluck host lend: the lender's dials (vmhost lend.go), which the menu
// bar item's Cores lent, Memory lent and Disk kept free submenus run
// (docs/menubar.md).

// lendNow and lendSet read and write the dials' file; tests stand in for
// them, as for the host switch.
var (
	lendNow = vmhost.LendNow
	lendSet = vmhost.SetLend
)

// lendAnswer is host lend's --json answer.
type lendAnswer struct {
	vmhost.HostLend
	Summary string `json:"summary"`
	// Over is said when running jobs hold more than a dial now lends.
	Over string `json:"over,omitempty"`
	// Ignored is said when a shared size's driver does not obey the dials
	// (menubar.DialsIgnored), as the running daemon describes its sizes.
	Ignored string `json:"ignored,omitempty"`
}

func (a *app) hostLendCmd() *cobra.Command {
	var cores, memory, keep string
	cmd := &cobra.Command{
		Use:   "lend [--cores N|all] [--memory-gb N|all] [--keep-free-gb N]",
		Short: "Turn down, or back up, the cores and memory this machine lends and the disk it keeps free for you",
		Long: `Lend sets this machine's dials, which every size it shares obeys from its
next job on, with no setup run again: --cores, the most cores Potluck's jobs
hold at once; --memory-gb, the most memory; --keep-free-gb, the disk kept
free for you (no job starts, and nothing Potluck keeps grows, below it).
"all" (or 0 for the disk) is as shared. A dial not given is kept. Jobs
running when a dial goes down finish as they would; no new one starts until
they are under it, and ` + a.cmd("host set closed") + ` ends them now. Raising a
dial past what was shared needs ` + a.cmd("host") + ` again with more (--share,
--linux-slots). With no flags it prints the dials. The macOS menu bar item
runs this (docs/menubar.md).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fl := cmd.Flags()
			changed := fl.Changed("cores") || fl.Changed("memory-gb") || fl.Changed("keep-free-gb")
			l, err := lendNow(os.Getuid())
			if err != nil && !(fl.Changed("cores") && fl.Changed("memory-gb") && fl.Changed("keep-free-gb")) {
				return fmt.Errorf("the dials cannot be read, so none is changed: %v (give all three of --cores, --memory-gb and --keep-free-gb to write them anew)", err)
			}
			if changed {
				if fl.Changed("cores") {
					if l.Cores, err = lendCount("--cores", cores); err != nil {
						return err
					}
				}
				if fl.Changed("memory-gb") {
					if l.MemoryMB, err = lendMemory(memory); err != nil {
						return err
					}
				}
				if fl.Changed("keep-free-gb") {
					n, err := strconv.Atoi(keep)
					if err != nil || n < 0 {
						return fmt.Errorf("--keep-free-gb %q is not a whole number of GB (0: none)", keep)
					}
					l.KeepFreeGB = n
				}
				l.Since = time.Time{} // SetLend stamps it
				if l, err = lendSet(l); err != nil {
					return err
				}
			}
			ans := lendAnswer{HostLend: l, Summary: l.Summary(), Over: a.lendOver(cmd, l), Ignored: a.lendIgnored(cmd, l)}
			if a.asJSON() {
				return a.printJSON(ans)
			}
			a.say("%s", ans.Summary)
			for _, s := range []string{ans.Over, ans.Ignored} {
				if s != "" {
					a.say("%s", s)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cores, "cores", "", "the most cores Potluck's jobs hold at once, or all")
	cmd.Flags().StringVar(&memory, "memory-gb", "", "the most memory Potluck's jobs hold at once, in GB, or all")
	cmd.Flags().StringVar(&keep, "keep-free-gb", "", "GB of disk kept free for you; 0 for none")
	return cmd
}

// lendCount reads a cores dial: a whole number, or all (0).
func lendCount(flag, s string) (int, error) {
	if s == "all" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s %q is a whole number of cores, or all", flag, s)
	}
	return n, nil
}

// lendMemory reads the memory dial in GB (halves allowed), as MiB, or all (0).
func lendMemory(s string) (int, error) {
	if s == "all" {
		return 0, nil
	}
	gb, err := strconv.ParseFloat(s, 64)
	if err != nil || gb <= 0 || math.IsInf(gb, 0) || gb*1024 > math.MaxInt32 {
		return 0, errors.New("--memory-gb " + strconv.Quote(s) + " is a number of GB, or all")
	}
	return int(math.Round(gb * 1024)), nil
}

// lendIgnored says which shared sizes' drivers do not obey the dials, from
// the running daemon's description of them ("" when all do, or when no
// daemon answers: then nothing is known of its drivers, and host status
// says so once it runs).
func (a *app) lendIgnored(cmd *cobra.Command, l vmhost.HostLend) string {
	targets, running, _ := a.liveBindings(cmd.Context())
	if !running {
		return ""
	}
	return menubar.DialsIgnored(menubar.Lend(targets, l))
}

// lendOver says what running boxes hold past the dials ("" when nothing),
// read from the sizes' own state directories.
func (a *app) lendOver(cmd *cobra.Command, l vmhost.HostLend) string {
	if l.Cores == 0 && l.MemoryMB == 0 {
		return ""
	}
	cpu, mem, n := 0, 0, 0
	for _, j := range a.jobFacts(cmd.Context()).Jobs {
		if j.Running() {
			cpu, mem, n = cpu+j.CPU, mem+j.MemoryMB, n+1
		}
	}
	if (l.Cores == 0 || cpu <= l.Cores) && (l.MemoryMB == 0 || mem <= l.MemoryMB) {
		return ""
	}
	jobs := "1 running job holds"
	if n != 1 {
		jobs = fmt.Sprintf("%d running jobs hold", n)
	}
	return fmt.Sprintf("%s %d cores and %.1f GB of memory: they finish, and no new job starts until they are under the dials; %s ends them now",
		jobs, cpu, float64(mem)/1024, a.cmd("host set closed"))
}
