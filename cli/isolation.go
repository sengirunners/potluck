package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runtarget"
)

// bindingIsolation is one binding on this machine as doctor and potluck
// isolation show it: whether it may run anyone's code, in plain words, and
// (a box) its latest block test.
type bindingIsolation struct {
	Binding  string `json:"binding"`
	Kind     string `json:"kind"`
	Isolated bool   `json:"isolated"`
	Says     string `json:"says"`
	// BlockTest is a box binding's latest recorded block test; BlockTestError
	// why its record could not be read (never shown as a pass). Stale: the
	// binding changed since that test (its driver program, config or
	// images), or its driver's pin cannot be read, so the record proves
	// nothing about it now. BlockTestOK is the one answer a reader takes: an
	// isolated binding with a readable record that passed and is not stale.
	BlockTest      *hostprobe.BlockTestResult `json:"block_test,omitempty"`
	BlockTestError string                     `json:"block_test_error,omitempty"`
	Stale          bool                       `json:"block_test_stale,omitempty"`
	BlockTestOK    bool                       `json:"block_test_ok"`
	// Testable: isolation test can test it (a size this machine serves: one
	// that host shares, or a pool's own size served here).
	Testable bool `json:"block_test_runnable"`
}

// isolationOf says what v's class lets it run. A binding whose driver did
// not answer counts as not isolated: its class is unknown. A box binding
// is testable: potluck isolation test tests every size this machine serves.
func isolationOf(v computeapi.RunTargetView, home string) bindingIsolation {
	bi := bindingIsolation{Binding: v.Name, Kind: v.Kind, Testable: v.Kind == computeapi.RunTargetKindBox}
	switch {
	case v.Describe == nil:
		why := strings.TrimSpace(v.Error)
		if why == "" {
			why = "no answer"
		}
		bi.Says = "not checked: its driver did not answer (" + why + "), so it takes no work until it does"
	case !v.Effective.UntrustedCapable():
		bi.Says = "not isolated: " + v.Effective.NotIsolated() + "; so it takes only this machine's owner's own work and refuses anyone else's"
		if v.Kind == computeapi.RunTargetKindBox {
			bi.Says = "not isolated: " + v.Effective.NotIsolated() + "; so it makes no sandbox, for anyone"
		}
	default:
		bi.Isolated = true
		bi.Says = "isolated: each piece of work runs in a throwaway machine walled off from this machine, its LAN and its tailnet, with no credentials"
	}
	if v.Kind == computeapi.RunTargetKindBox && home != "" {
		r, ok, err := hostprobe.LoadBlockTest(home, v.Name)
		switch {
		case err != nil:
			bi.BlockTestError = err.Error()
		case ok:
			bi.BlockTest = &r
			pin, pinned, perr := runtarget.PinOf(home, v.Driver)
			bi.Stale = perr != nil || !pinned || r.Fingerprint != hostprobe.BindingFingerprint(v.Driver, pin.SHA256, v.Config, v.Images)
			bi.BlockTestOK = r.Passed && !bi.Stale && bi.Isolated
		}
	}
	return bi
}

// blockTestLine is a box binding's block test in plain words.
func blockTestLine(bi bindingIsolation, p prefix) string {
	again := p.cmd("isolation test " + bi.Binding)
	switch r := bi.BlockTest; {
	case bi.BlockTestError != "":
		return "block test: its record cannot be read (" + bi.BlockTestError + "); run " + again
	case r == nil:
		return "block test: never run on this machine; run " + again
	case r.Passed && bi.Stale:
		return fmt.Sprintf("block test out of date: the size or its driver changed since the test of %s, so it proves nothing now; run %s", r.At.Format("2006-01-02 15:04 UTC"), again)
	case r.Passed && !bi.BlockTestOK:
		return fmt.Sprintf("block test passed %s, but the size is not isolated now, so the test counts for nothing", r.At.Format("2006-01-02 15:04 UTC"))
	case r.Passed:
		line := fmt.Sprintf("block test passed %s: a sandbox reached none of this machine, %s, or another sandbox", r.At.Format("2006-01-02 15:04 UTC"), strings.Join(r.Forbid, ", "))
		for _, n := range r.Notes {
			line += "; " + n
		}
		return line
	default:
		return fmt.Sprintf("block test FAILED %s: %s; fix it, then run %s", r.At.Format("2006-01-02 15:04 UTC"), r.Error, again)
	}
}

// localIsolation is every binding on this machine: the sizes potluck host
// shares, and each pool's own bindings this machine serves (servedHere;
// --pool: that pool's alone),
// each with its class asked of its driver. It reads without starting the
// daemon; err when the daemon does not answer. A pool whose list cannot be
// read is named in partial, never left out silently.
func (a *app) localIsolation(ctx context.Context) (out []bindingIsolation, partial string, err error) {
	cfg, err := a.loadClient()
	if err != nil {
		return nil, "", err
	}
	c, err := a.readClient()
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	host, err := computeclient.HostRunTargets(ctx, c)
	if err != nil {
		return nil, "", err
	}
	for _, v := range host.Targets {
		out = append(out, isolationOf(v, cfg.Home))
	}
	pools, unread, err := servedHere(ctx, c, a.pool())
	if err != nil {
		return nil, "", err
	}
	for _, pb := range pools {
		out = append(out, isolationOf(pb.View, cfg.Home))
	}
	if len(unread) > 0 {
		partial = strings.Join(unread, "; and ") + " could not be read"
	}
	slices.SortStableFunc(out, func(x, y bindingIsolation) int { return strings.Compare(x.Binding, y.Binding) })
	return out, partial, nil
}

// poolBinding is one of a pool's own bindings, as the pool lists it.
type poolBinding struct {
	Pool string
	View computeapi.RunTargetView
}

// servedHere is each pool's own bindings this machine serves, each with
// its class asked of its driver: only a pool this machine runs (the local
// registry) has them, since a joined pool's own bindings are its
// authority's. only: that pool alone (--pool), else every one. A pool's
// list holds other machines' bindings too (adverts): only those served
// here are kept, by the daemon's own machine name, not this shell's
// (ZM_MACHINE or a changed host name can make them differ). unread names
// each list that could not be read; err: the daemon did not answer.
func servedHere(ctx context.Context, c *client.Client, only string) (out []poolBinding, unread []string, err error) {
	h, err := c.Health(ctx)
	if err != nil {
		return nil, nil, err
	}
	if h.Machine == "" {
		return nil, nil, fmt.Errorf("the daemon did not say its machine name")
	}
	ps, err := c.LocalProjects(ctx)
	if err != nil {
		return nil, []string{"the pools this machine runs (" + oneLineErr(err) + ")"}, nil
	}
	for _, p := range ps {
		if only != "" && p.Slug != only && p.ID != only {
			continue
		}
		l, err := computeclient.RunTargets(ctx, inPool(c, p.Slug), true)
		if err != nil {
			unread = append(unread, "pool "+p.Slug+"'s own sizes and targets ("+oneLineErr(err)+")")
			continue
		}
		for _, v := range l.Targets {
			if v.HostBinding != "" || v.Host != h.Machine {
				continue // another machine's binding, or this machine's size as the pool sees it
			}
			out = append(out, poolBinding{Pool: p.Slug, View: v})
		}
	}
	return out, unread, nil
}

func oneLineErr(err error) string { return strings.Join(strings.Fields(err.Error()), " ") }

// printIsolation is doctor's and potluck isolation's per-binding lines,
// and the list it could not read, if any.
func printIsolation(w io.Writer, bs []bindingIsolation, partial string, p prefix) {
	if partial != "" {
		fmt.Fprintf(w, "isolation: %s, so it is not shown\n", partial)
	}
	if len(bs) == 0 {
		return
	}
	fmt.Fprintln(w, "isolation, per size and target this machine lends:")
	for _, bi := range bs {
		kind := bi.Kind
		if kind == computeapi.RunTargetKindBox {
			kind = "sandbox size"
		}
		fmt.Fprintf(w, "  %s (%s): %s\n", bi.Binding, kind, bi.Says)
		if bi.Kind == computeapi.RunTargetKindBox {
			fmt.Fprintf(w, "    %s\n", blockTestLine(bi, p))
		}
	}
}

func (a *app) isolationCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "isolation", Short: "Which sizes and targets this machine lends may run anyone's code, and each size's latest block test", Args: cobra.NoArgs,
		Long: `Shows, for each size and target this machine lends, whether it may run
anyone's code: isolated (each piece of work in a throwaway machine walled
off from this machine, its LAN and its tailnet, with no credentials), or
not, in which case it takes only this machine's owner's own work. Each
size also shows its latest block test (` + a.cmd("isolation test") + `).
docs/isolation.md.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			bs, partial, err := a.localIsolation(cmd.Context())
			if err != nil {
				return fmt.Errorf("what this machine lends: %v (is %s running?)", err, a.cmd("daemon"))
			}
			if a.asJSON() {
				if err := a.printJSON(bs); err != nil {
					return err
				}
			} else if len(bs) == 0 && partial == "" {
				fmt.Fprintln(a.host.Stdout, "this machine lends nothing yet")
			} else {
				printIsolation(words{a.host.Stdout}, bs, partial, prefix(a.host.Prefix))
			}
			if partial != "" {
				fmt.Fprintf(a.host.Stderr, "%s: %s\n", a.cmd("isolation"), partial)
				return exitSilently(1)
			}
			return nil
		}}
	cmd.AddCommand(a.isolationTestCmd())
	return cmd
}

func (a *app) isolationTestCmd() *cobra.Command {
	var forbid []string
	var timeout time.Duration
	cmd := &cobra.Command{Use: "test <size> [--net-forbid host:port]...", Short: "Prove a size's sandboxes cannot reach this machine, its LAN or its tailnet, and record the result",
		Long: `Runs conformance's network checks on a size this machine serves (one
` + a.cmd("host") + ` shares, or a pool's own size served here), with its own driver and
config: a sandbox must not reach this machine on a port that is open (the
test listens on one), the LAN's router or Tailscale's resolver (each on a
port that opens from here), any --net-forbid target, or another sandbox at
the same time. Any check that could not run fails the test. The result is
kept in $POTLUCK_HOME/isolation/<size>.json, and doctor shows it.
docs/isolation.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.notWhileAttached(a.cmd("isolation test")); err != nil {
				return err
			}
			c, cfg, err := a.client()
			if err != nil {
				return err
			}
			b, err := localBoxBinding(ctx, c, args[0], prefix(a.host.Prefix))
			if err != nil {
				return err
			}
			// The file the daemon would run, by its pin: the walling-off
			// lives in the program, so the record names its sum.
			pin, err := runtarget.PinnedDriver(cfg.Home, a.driverDirs(cfg), b.Driver)
			if err != nil {
				return err
			}
			if err := idleForBlockTest(ctx, c); err != nil {
				return err
			}
			r := hostprobe.BlockTest(ctx, hostprobe.ExecRunner{}, hostprobe.BlockTestOptions{
				Binding: b.Name, DriverName: b.Driver, DriverSHA256: pin.SHA256, Driver: pin.Path, Config: b.Config, Images: b.Images, NetForbid: forbid, Timeout: timeout})
			if err := hostprobe.RecordBlockTest(cfg.Home, r); err != nil {
				return fmt.Errorf("record the result: %v", err)
			}
			if a.asJSON() {
				if err := a.printJSON(r); err != nil {
					return err
				}
			} else {
				w := words{a.host.Stdout}
				for _, c := range r.Checks {
					state := "PASS"
					switch {
					case !c.OK:
						state = "FAIL"
					case c.Skipped:
						state = "SKIP"
					}
					fmt.Fprintf(w, "%-4s  %s\n", state, strings.TrimSpace(c.Name+" "+c.Detail))
				}
				fmt.Fprintln(w, blockTestLine(bindingIsolation{Binding: b.Name, BlockTest: &r, BlockTestOK: r.Passed, Testable: true}, prefix(a.host.Prefix)))
			}
			if !r.Passed {
				return exitSilently(1)
			}
			return nil
		}}
	cmd.Flags().StringArrayVar(&forbid, "net-forbid", nil, "another host:port a sandbox must not reach (a NAS, a printer); it must open from here (repeatable)")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long the checks may take")
	return cmd
}

// localBoxBinding is the size name this machine serves, its config
// included (the owner on the socket sees it): one potluck host shared, or a
// pool's own size served here. The block test's record is kept by name, so
// a name that names different sizes here (a shared size and a pool's, or
// two pools') is refused: one record cannot speak for both.
func localBoxBinding(ctx context.Context, c *client.Client, name string, p prefix) (computeapi.RunTarget, error) {
	type found struct {
		where string
		t     computeapi.RunTarget
	}
	var all []found
	var why []string
	host, err := computeclient.HostRunTargets(ctx, c)
	if err != nil {
		why = append(why, "the sizes "+p.cmd("host")+" shares ("+oneLineErr(err)+")")
	}
	for _, t := range targetsOf(host) {
		if t.Name == name {
			all = append(all, found{"shared by " + p.cmd("host"), t})
		}
	}
	pools, unread, err := servedHere(ctx, c, "")
	if err != nil {
		return computeapi.RunTarget{}, err
	}
	why = append(why, unread...)
	for _, pb := range pools {
		if pb.View.Name == name {
			all = append(all, found{"pool " + pb.Pool + "'s own", pb.View.RunTarget})
		}
	}
	if len(all) == 0 {
		if len(why) > 0 {
			return computeapi.RunTarget{}, fmt.Errorf("this machine serves no size %s that could be read (%s could not be read)", name, strings.Join(why, "; and "))
		}
		return computeapi.RunTarget{}, fmt.Errorf("this machine serves no size %s (%s lists them)", name, p.cmd("isolation"))
	}
	t := all[0].t
	for _, f := range all {
		if f.t.Kind != computeapi.RunTargetKindBox {
			return f.t, fmt.Errorf("%s is a %s target (%s), not a size: the block test is for sizes", name, f.t.Kind, f.where)
		}
	}
	for _, f := range all[1:] {
		if !sameSize(f.t, t) {
			return computeapi.RunTarget{}, fmt.Errorf("%s names two different sizes here (%s, and %s), and the test's record is kept by name: give one another name", name, all[0].where, f.where)
		}
	}
	return t, nil
}

// sameSize reports whether x and y run the same driver, config and images:
// the block test of one is the test of the other.
func sameSize(x, y computeapi.RunTarget) bool {
	return x.Driver == y.Driver && maps.Equal(x.Config, y.Config) && slices.Equal(x.Images, y.Images)
}

// idleForBlockTest refuses while sandboxes of any size this machine serves
// run here (the sizes potluck host shares, and each pool's own sizes served
// here), or while it cannot tell: the test boots two of its own beside them,
// past the sizes' slot limits. It is a check when the test starts, not a
// hold: a sandbox asked for during the test still starts (docs/isolation.md).
func idleForBlockTest(ctx context.Context, c *client.Client) error {
	var l computeapi.RunTargetList
	if err := c.Do(ctx, "GET", "/v0/run_targets?host=1&live=1", nil, &l); err != nil {
		return fmt.Errorf("which sandboxes run here: %v", err)
	}
	n := 0
	for _, t := range l.Targets {
		if t.Error != "" { // unknown is not none
			return fmt.Errorf("cannot tell whether sandboxes run on %s (%s), so the test does not start beside them", t.Name, t.Error)
		}
		n += len(t.Live)
	}
	pools, unread, err := servedHere(ctx, c, "") // every pool, whatever --pool says
	if err != nil {
		return fmt.Errorf("which sandboxes run here: %v", err)
	}
	if len(unread) > 0 {
		return fmt.Errorf("cannot tell whether sandboxes run here: %s could not be read, so the test does not start beside them", strings.Join(unread, "; and "))
	}
	for _, pb := range pools {
		if pb.View.Kind != computeapi.RunTargetKindBox {
			continue
		}
		// The pool's records of the size's boxes not yet ended, acquiring
		// ones too: a pool's own box has a record from its first ask.
		hs, err := computeclient.RunTargetHandles(ctx, inPool(c, pb.Pool), pb.View.Name, true)
		if err != nil {
			return fmt.Errorf("cannot tell whether sandboxes run on %s in pool %s (%s), so the test does not start beside them", pb.View.Name, pb.Pool, oneLineErr(err))
		}
		n += len(hs)
	}
	if n > 0 {
		return fmt.Errorf("%s still running on this machine: the test boots two sandboxes of its own beside them, so run it when the machine is idle", sandboxCount(n))
	}
	return nil
}
