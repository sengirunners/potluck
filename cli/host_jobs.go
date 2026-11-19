package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/runtarget/rt"
	"github.com/sengirunners/potluck/vmhost"
)

// daemonWait bounds what host jobs asks the daemon.
const daemonWait = 8 * time.Second

// potluck host jobs: every job this machine's sizes ran, the counts and the
// electricity estimate (the menu bar item's job window, docs/menubar.md).

func (a *app) hostJobsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "jobs",
		Short: "Every job this machine ran: when, how long, its size and pool, how it ended; counts and estimated electricity",
		Long: `Jobs lists every job the sizes this machine shares have run, newest
first: when it started, how long it was up, its size, the pool it came from
and how it ended. Then the counts (running now, started in the last 12 hours,
per day for two weeks) and an estimate of the electricity they used, with how
it is worked out. It reads the sizes' own state directories (each driver
keeps a job's record for a week, then a line for it in jobs.jsonl), so it
needs no daemon; with one running it also reads the sizes of pools this
machine runs, and names each pool. With --json it prints the window the
macOS menu bar item draws (docs/menubar.md).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := menubar.BuildJobs(a.jobFacts(cmd.Context()))
			if a.asJSON() {
				return a.printJSON(v)
			}
			a.say("%s", jobsText(v))
			return nil
		},
	}
}

// jobFacts reads every job of the box sizes this machine serves: those in
// its own bindings file, and, when the daemon answers, those of pools it
// runs; each state directory once.
func (a *app) jobFacts(ctx context.Context) menubar.JobFacts {
	now := time.Now()
	f := menubar.JobFacts{Loc: time.Local, Now: now, Pools: map[string]string{}}
	var configs []map[string]string
	home := ""
	if cfg, err := a.loadClient(); err == nil {
		home = cfg.Home
		b, err := os.ReadFile(filepath.Join(home, runtarget.HostFile))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			f.Unread = append(f.Unread, err.Error())
		default:
			ts, _, err := rt.ParseHostRunTargets(string(b))
			if err != nil {
				f.Unread = append(f.Unread, err.Error())
			}
			for _, t := range ts {
				if t.Kind == computeapi.RunTargetKindBox {
					configs = append(configs, t.Config)
				}
			}
		}
	} else {
		f.Unread = append(f.Unread, err.Error())
	}
	names, named := map[string]string{}, false // project id -> slug; named: both lists answered
	// The daemon adds what it knows; the state directories are local, so a
	// slow daemon or pool never keeps the window (which waits 25 s) empty.
	// The pool names first: they are the daemon's own lists.
	dctx, cancel := context.WithTimeout(ctx, daemonWait)
	defer cancel()
	if c, err := a.readClient(); err == nil {
		rs, rerr := c.Remotes(dctx)
		ps, perr := c.LocalProjects(dctx)
		for _, r := range rs {
			names[r.Project.ID] = r.Project.Slug
		}
		for _, p := range ps {
			names[p.ID] = p.Slug
		}
		named = rerr == nil && perr == nil
		views, running, verr := servedViewsOn(dctx, c, false)
		for _, v := range views {
			if v.Kind == computeapi.RunTargetKindBox {
				configs = append(configs, v.Config)
			}
		}
		switch {
		case dctx.Err() != nil:
			f.Unread = append(f.Unread, fmt.Sprintf("Potluck did not answer within %s, so some pools may not be named and the sizes of pools this Mac runs may be missing", daemonWait))
		case !running:
			f.Unread = append(f.Unread, "Potluck is not running, so pools are not named and the sizes of pools this Mac runs are not read")
		case !named:
			f.Unread = append(f.Unread, "the pools this Mac is in could not be listed, so they are not named")
		case verr != nil:
			f.Unread = append(f.Unread, "the sizes of pools this Mac runs: "+verr.Error())
		}
	}
	if home != "" {
		held, err := runtarget.HostHandleProjects(home)
		if err != nil {
			f.Unread = append(f.Unread, "which pool each job came from: "+err.Error())
		} else {
			// The ledger forgets a handle HostLedgerKeep after a call last
			// touched it; a day's margin covers a job that ran on untouched.
			f.PoolsFrom = now.Add(-runtarget.HostLedgerKeep + 24*time.Hour)
		}
		for h, id := range held {
			switch {
			case names[id] != "":
				f.Pools[h] = names[id]
			case named:
				f.Pools[h] = "a pool this Mac is not in now"
			default:
				f.Pools[h] = "a pool (its name could not be read now)"
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range configs {
		dir, err := vmhost.StateDir(c)
		if err != nil {
			f.Unread = append(f.Unread, err.Error())
			continue
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		js, err := vmhost.Jobs(dir, now)
		if err != nil {
			f.Unread = append(f.Unread, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		for _, j := range js {
			if j.Kind == computeapi.RunTargetKindBox { // what this machine lends; a run binding may share the directory
				f.Jobs = append(f.Jobs, j)
			}
		}
	}
	return f
}

// jobsText is the window as text: the counts, the estimate, the days with
// jobs, then the jobs.
func jobsText(v menubar.JobsView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n%s\n%s\n", v.Title, v.Summary, v.Energy.Line, v.Energy.How)
	if v.Note != "" {
		fmt.Fprintf(&b, "%s\n", v.Note)
	}
	var days []string
	for _, d := range v.Days {
		if d.Jobs > 0 {
			days = append(days, fmt.Sprintf("  %-12s %d, %s", d.Label, d.Jobs, d.Energy))
		}
	}
	if len(days) > 0 {
		fmt.Fprintf(&b, "\njobs per day (the last two weeks):\n%s\n", strings.Join(days, "\n"))
	}
	if len(v.Jobs) == 0 {
		b.WriteString("\nno jobs yet\n")
		return strings.TrimRight(b.String(), "\n")
	}
	b.WriteString("\n")
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STARTED\tUP\tSIZE\tPOOL\tENDED\tELECTRICITY")
	for _, j := range v.Jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", j.When, j.Took, j.Size, j.Pool, j.Ended, j.Energy)
	}
	_ = tw.Flush()
	return strings.TrimRight(b.String(), "\n")
}
