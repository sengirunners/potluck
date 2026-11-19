package cli

// potluck host images: what Tart holds on this Mac after a failed or
// superseded setup, which of it Potluck still needs, and, with --remove, the
// removal of only what nothing needs (hostprobe.ReadTartInventory).

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Test hooks: the runner tart list and tart delete go through, and the free
// disk read before and after a removal.
var (
	hostImagesRunner hostprobe.Runner = hostprobe.ExecRunner{}
	hostImagesFreeGB                  = func(dir string) (float64, error) { return hostprobe.SystemFacts{}.FreeDiskGB(dir) }
)

func (a *app) hostImagesCmd() *cobra.Command {
	var remove bool
	var forget string
	cmd := &cobra.Command{
		Use:   "images [--remove [image...] | --forget-home <dir>]",
		Short: "What Tart holds on this Mac, which of it Potluck still needs, and what is safe to remove",
		Long: `Images reads Tart's list once and says, for each image and VM, whether it
is in use (a size this machine shares lends it, or it is a Potluck sandbox's VM),
retained (Potluck's published image for this Mac: a retry lends it without
downloading it again; or one Potluck cannot show is its own and unused),
removable (an image Potluck pulled, by its record of its pulls, that nothing
here needs: an earlier version of a pinned image, or one this Mac's macOS
cannot lend), absent (a retry or a shared size needs it and Tart does not hold
it), or not Potluck's (a tag or a VM Potluck did not make). Sizes are what Tart
measured on disk, or an estimate where it says none. It changes nothing and
starts no daemon.

What this machine shares is read from every Potluck daemon running here (this
command's, and another program's built on Potluck), in every pool and at
machine level. The pinned repositories are public, so an image in one may be
yours: an image is removable only if Potluck's record says a share of
Potluck's pulled it, every daemon home whose share used it answered, no
home's latest share uses it (a retry there lends it, or that share is still
running), and Tart holds no tag of its repository (that tag may name the
same download). An image that passes every check but the record (one Potluck
pulled before it kept a record, or your own) is retained and listed as
unclaimed: name it to --remove to remove it.

With --remove it runs tart delete on the removable images only, reading
everything again just before each delete, then reads Tart's list again and
says what is gone, what was already gone, what it kept because it was no
longer removable, and what could not be removed and why (then it exits
non-zero). Running it again removes nothing more. It deletes no retained or
in-use image, no VM, and nothing not Potluck's; Tart itself may drop its own
leftovers on a delete (a tag whose download was already gone), which are
listed as also gone. When any list of what this machine shares cannot be
read, no image is removable.

With --remove and image names it deletes exactly those, each removable or
unclaimed; if any named image is neither, it deletes nothing. Naming an
unclaimed image says it is not yours: every other check still holds, again
just before its delete.

While any daemon home the record names is not running, no image is removable:
its sizes may lend one, even an image a share there used before the record
began. A home that has not shared since the record began (an older build's) is
not in it: while its daemon runs, what it shares is read, but a stopped one's
sizes are not, so an image it lends can look removable or unclaimed. Start
every Potluck daemon on this Mac before --remove.

With --forget-home and a daemon home the record names that is gone for good
(an uninstalled program's, or a temporary home that shared this Mac), it
takes that home out of the record: its latest share and its mark on each
image, which it lists. It deletes nothing: no image, no VM, no file. An image
that home alone used keeps Potluck's claim and is judged again by what the
homes still here need. It refuses, changing nothing, a home that exists, one
whose existence cannot be read (a disk that is not mounted comes back), one a
running daemon uses, and one the record does not name.`,
		Example: "  " + a.cmd("host images") + "\n  " + a.cmd("host images --remove") + "\n  " + a.cmd("host images --remove <image>") +
			"\n  " + a.cmd("host images --forget-home <dir>"),
		RunE: func(cmd *cobra.Command, names []string) error {
			forgetting := cmd.Flags().Changed("forget-home")
			if forgetting && strings.TrimSpace(forget) == "" {
				return exitWith{2, errors.New("--forget-home needs the home's directory, as the listing names it")}
			}
			if forgetting && (remove || len(names) > 0) {
				return exitWith{2, errors.New("--forget-home goes alone: forget the home first, then " + a.cmd("host images --remove"))}
			}
			if len(names) > 0 && !remove {
				return errors.New("image names go with --remove: " + a.cmd("host images --remove <image>..."))
			}
			var f hostFlags
			dir, err := f.dir(a)
			if err != nil {
				return err
			}
			r := detectHost(cmd.Context(), dir)
			if forgetting {
				return a.forgetTartHome(cmd.Context(), r.TartDir, forget)
			}
			read := func(ctx context.Context) (hostprobe.Served, error) { return a.servedTargets(ctx, dir) }
			return a.hostImages(cmd.Context(), hostImagesRunner, r, read, remove, names...)
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "delete the removable images (only those), or the named ones, each removable or unclaimed")
	cmd.Flags().StringVar(&forget, "forget-home", "", "take a daemon home that is gone for good out of the record of Tart's images (deletes nothing)")
	return cmd
}

// forgetTartHome is host images --forget-home: home leaves the record of
// Tart's images (hostprobe.ForgetTartHome) unless it exists or a running
// daemon uses it; it says exactly what was forgotten.
func (a *app) forgetTartHome(ctx context.Context, tartDir, home string) error {
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	live, err := daemonrecord.Live(ctx, nil)
	if err != nil {
		return fmt.Errorf("this machine's daemon records cannot be read (%v), so whether a daemon runs on %s is unknown: nothing forgotten", err, home)
	}
	var running []string
	for _, x := range live {
		running = append(running, x.Home)
	}
	f, err := hostprobe.ForgetTartHome(tartDir, home, running)
	if err != nil {
		return err
	}
	if a.asJSON() {
		return a.printJSON(f)
	}
	shared := ""
	if !f.Shared.IsZero() {
		shared = ", whose latest share started " + f.Shared.Local().Format("Jan 2 15:04")
	}
	a.say("forgot %s%s, from the record of Tart's images in %s", f.Home, shared, tartDir)
	if len(f.Images) > 0 {
		a.say("its mark is off: %s", strings.Join(f.Images, ", "))
	}
	a.say("Nothing was deleted. %s says what is removable now.", a.cmd("host images"))
	return nil
}

// servedTargets is every binding this machine serves, in every pool and
// at machine level (servedViews), from the daemon this command uses (on
// home) and from every other Potluck daemon running here (another
// program's, kit/daemonrecord), without starting any; any list it could not
// read is an error, so the inventory keeps every image. Homes are the
// daemons' homes it read, which the record of Tart's images is checked
// against.
func (a *app) servedTargets(ctx context.Context, home string) (hostprobe.Served, error) {
	vs, running, err := a.servedViews(ctx, false)
	if err != nil {
		if !running {
			return hostprobe.Served{}, fmt.Errorf("%s; is %s running?", oneLineErr(err), a.cmd("daemon"))
		}
		return hostprobe.Served{}, errors.New(oneLineErr(err))
	}
	s := hostprobe.Served{Homes: []string{home}}
	others, err := daemonrecord.Live(ctx, func(r daemonrecord.Record) bool {
		return r.Serves(rt.Name) && !daemonrecord.SameHome(r.Home, home) // any daemon with bindings, whatever else it serves
	})
	if err != nil {
		return hostprobe.Served{}, fmt.Errorf("read this machine's daemon records: %v", err)
	}
	for _, r := range others {
		ovs, _, err := servedViewsOn(ctx, client.NewUnix(r.Socket), false)
		if err != nil {
			return hostprobe.Served{}, fmt.Errorf("%s's daemon at %s: %s", r.Program, tildePath(r.Home), oneLineErr(err))
		}
		vs = append(vs, ovs...)
		s.Homes = append(s.Homes, r.Home)
	}
	for _, v := range vs {
		s.Targets = append(s.Targets, v.RunTarget)
	}
	return s, nil
}

// hostImages prints the inventory, or with remove removes the removable
// images (or the named ones) and says what happened to each. read is how it
// reads the bindings this machine serves, each time it needs them.
func (a *app) hostImages(ctx context.Context, run hostprobe.Runner, r hostprobe.Report, read hostprobe.BindingsReader, remove bool, names ...string) error {
	var bindErr error // the first read's, for the warning
	first := true
	reader := func(ctx context.Context) (hostprobe.Served, error) {
		s, err := read(ctx)
		if first {
			bindErr, first = err, false
		}
		return s, err
	}
	if !remove {
		inv, err := hostprobe.ReadTartInventory(ctx, run, r, reader)
		if err != nil {
			return err
		}
		if a.asJSON() {
			return a.printJSON(inv)
		}
		a.sayTartInventory(r, inv, bindErr)
		if gb, est := inv.ReclaimGB(); len(inv.Removable()) > 0 {
			a.say("Removing the removable images frees up to %s. To remove them, and nothing else, run: %s", gbWords(gb, !est), a.cmd("host images --remove"))
		} else {
			a.say("Nothing is removable.")
		}
		if un := inv.Unclaimed(); len(un) > 0 {
			var ns []string
			gb, measured := 0, true
			for _, it := range un {
				ns = append(ns, it.Name)
				gb += it.GB
				measured = measured && it.Measured
			}
			a.say("Unclaimed (%s): no running Potluck daemon here needs them, but Potluck has no record that it pulled them, so they may be yours, or a stopped daemon's that shared before the record began. If they are neither, this removes them and nothing else: %s",
				gbWords(gb, measured), a.cmd("host images --remove "+strings.Join(ns, " ")))
		}
		return nil
	}
	before, beforeErr := hostImagesFreeGB(r.TartDir)
	c, err := hostprobe.RemoveTartImages(ctx, run, r, reader, names...)
	if len(c.Removals) == 0 && err != nil {
		return err // nothing was read, or a named image was refused: nothing was tried
	}
	if a.asJSON() {
		if perr := a.printJSON(c); perr != nil {
			return perr
		}
		return exitIf(err)
	}
	if bindErr != nil {
		a.say("warning: what this machine shares could not be read (%v): every image a size might lend is kept", bindErr)
	}
	if len(c.Removals) == 0 && bindErr != nil {
		a.say("Nothing removed: what this machine shares could not be read, so every image is kept (%s lists what Tart holds).", a.cmd("host images"))
		return nil
	}
	if len(c.Removals) == 0 {
		a.say("Nothing to remove: Tart holds no image Potluck pulled that nothing here needs (%s lists what it holds).", a.cmd("host images"))
		return nil
	}
	for _, rm := range c.Removals {
		switch {
		case rm.Kept:
			a.say("kept %s: %s", rm.Name, rm.Note)
		case rm.Removed && rm.Note == "":
			a.say("removed %s (%s)", rm.Name, gbWords(rm.GB, rm.Measured))
		case rm.Removed:
			a.say("removed %s: %s", rm.Name, rm.Note)
		default:
			a.say("NOT removed %s: %s", rm.Name, rm.Note)
		}
	}
	for _, g := range c.AlsoGone {
		a.say("also gone from Tart's list: %s", g)
	}
	after, afterErr := hostImagesFreeGB(r.TartDir)
	if beforeErr == nil && afterErr == nil {
		a.say("free disk in %s: %.0f GB before, %.0f GB after", r.TartDir, before, after)
	}
	return err
}

// exitIf is a quiet non-zero exit when err is set (the JSON said why).
func exitIf(err error) error {
	if err != nil {
		return exitSilently(1)
	}
	return nil
}

// sayTartInventory prints the inventory grouped by state.
func (a *app) sayTartInventory(r hostprobe.Report, inv hostprobe.TartInventory, bindErr error) {
	a.say("Tart on this Mac (macOS %s, images in %s, %.0f GB free there):", r.MacOS, r.TartDir, r.TartFreeGB)
	if bindErr != nil {
		a.say("warning: what this machine shares could not be read (%v): every image a size might lend is kept", bindErr)
	}
	var last hostprobe.TartState
	for _, it := range inv.Items {
		if it.State != last {
			a.say("%s:", it.State)
			last = it.State
		}
		kind := ""
		if it.VM {
			kind = " (VM)"
		}
		size := gbWords(it.GB, it.Measured)
		if it.State == hostprobe.TartAbsent && it.GB > 0 {
			size = fmt.Sprintf("about %d GB to download", it.GB)
		}
		a.say("  %s%s, %s: %s", it.Name, kind, size, it.Why)
	}
	if len(inv.Items) == 0 {
		a.say("  Tart holds nothing, and nothing a retry needs is missing.")
	}
}

// gbWords is a size for people: measured, an estimate, or unknown.
func gbWords(gb int, measured bool) string {
	switch {
	case measured:
		return fmt.Sprintf("%d GB on disk", gb)
	case gb > 0:
		return fmt.Sprintf("about %d GB (an estimate: Tart gave no size)", gb)
	default:
		return "size unknown"
	}
}
