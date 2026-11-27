package hostprobe

// Tart when Potluck is removed from a Mac (`potluck uninstall --with-tart`):
// which of Tart's images are Potluck's to take along, and whether Tart then
// holds anything else. A removal of one home's Potluck differs from
// `potluck host images --remove`: nothing of that home will be lent again,
// so even its current images may go.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/vmhost"
)

// TartLeave is the Tart of a Mac Potluck leaves.
type TartLeave struct {
	CLI  string // the Tart program (Layer.CLI)
	Dir  string // where Tart keeps its images (Report.TartDir)
	Home string // the Potluck home being removed
	// OnlyDir is that Dir is where Tart keeps everything when nothing says
	// otherwise, and nothing says Tart was used in another directory: with
	// nothing in Dir, Tart then holds nothing. False proves nothing: what a
	// TART_HOME names may be on a disk that is not mounted.
	OnlyDir bool
	// KeepHome is that Home stays, and may share again.
	KeepHome bool
	// Others reads what every Potluck daemon here but Home's serves.
	Others BindingsReader
}

// TartLeaveAt is the Tart program cli's, as this user's Tart keeps it.
func TartLeaveAt(cli, home string, keepHome bool, others BindingsReader) TartLeave {
	return TartLeave{CLI: cli, Dir: tartDir(SystemFacts{}), OnlyDir: os.Getenv("TART_HOME") == "", Home: home, KeepHome: keepHome, Others: others}
}

// TartLeaving is what Tart's list holds, sorted for a Potluck that leaves:
// removable images are Potluck's; every other entry stays.
type TartLeaving struct {
	TartInventory
	// Others are the other Potluck homes this Mac was shared from, or that a
	// daemon here runs on: they may need this Tart.
	Others []string
	// Known is that what Tart holds is what this directory holds: Potluck's
	// record is in it, or it is the only one (OnlyDir) and blank. Any other
	// (TART_HOME was set elsewhere, its disk is not mounted) may be empty
	// while the VMs are where Potluck ran.
	Known bool
	// Busy is that a Tart process runs, on whatever directory: Tart must not
	// go then. One may run for a moment (a list), so it is for the removal
	// itself to heed.
	Busy bool
	// Unread is why some of it could not be read; then nothing is removable.
	Unread error
}

// Kept is what stays in Tart.
func (l TartLeaving) Kept() []TartItem {
	return slices.DeleteFunc(slices.Clone(l.Items), func(it TartItem) bool { return it.State == TartRemovable })
}

// Empty reports that with the removable images gone nothing here uses Tart.
func (l TartLeaving) Empty() bool {
	return l.Unread == nil && l.Known && len(l.Others) == 0 && len(l.Kept()) == 0
}

// Read sorts what Tart's list holds. An image is removable only when
// Potluck's record says a share of Potluck's pulled it, it is a digest (a
// tag is a person's word for it), no other Potluck home is here, no tag of
// its repository is there (keepUnproven) and it has not changed since that
// pull: an owner who deleted it and pulled it again owns it. A VM folder
// Tart's list leaves out (one being made) counts as a VM. It changes nothing.
func (l TartLeave) Read(ctx context.Context, run Runner) (TartLeaving, error) {
	// Tart's list makes the directory: it is not run for one that is not there.
	absent := l.OnlyDir && tartAbsent(l.Dir)
	var entries []tartEntry
	if !absent {
		var err error
		if entries, err = tartList(ctx, run, l.CLI); err != nil {
			return TartLeaving{}, fmt.Errorf("could not read Tart's list: %v", err)
		}
	}
	rec, recErr := readTartPulls(l.Dir)
	rec.forget(l.Home)
	served, servedErr := l.Others(ctx)
	if servedErr != nil {
		served = Served{}
	}
	lent, _ := tartLent(served.Targets)
	tags := tartTags(entries)
	out := TartLeaving{TartInventory: TartInventory{Items: []TartItem{}}, Others: slices.Clone(served.Homes), Unread: errors.Join(recErr, servedErr)}
	for h := range rec.homes() {
		if !slices.ContainsFunc(out.Others, func(o string) bool { return daemonrecord.SameHome(o, h) }) {
			out.Others = append(out.Others, h)
		}
	}
	slices.Sort(out.Others)
	_, statErr := os.Stat(filepath.Join(l.Dir, TartPullsFile))
	out.Known = !absent && statErr == nil || l.blank()
	vms, vmsErr := vmFolders(l.Dir)
	busy, busyErr := tartRunning(ctx, run)
	out.Busy, out.Unread = busy, errors.Join(out.Unread, vmsErr, busyErr)
	for _, n := range vms {
		if !slices.ContainsFunc(entries, func(e tartEntry) bool { return !e.OCI && e.Name == n }) {
			out.Items = append(out.Items, TartItem{Name: n, VM: true, State: TartNotPotluck, Why: "a VM folder Tart does not list (one being made, or a damaged one)"})
		}
	}
	for _, e := range entries {
		it := TartItem{Name: e.Name, VM: !e.OCI, GB: e.SizeGB, Measured: e.SizeOK}
		switch {
		case it.VM && vmhost.PotluckVMName(e.Name):
			it.State, it.Why = TartInUse, "a Potluck sandbox's VM"
		case it.VM:
			it.State, it.Why = TartNotPotluck, "a VM Potluck did not make"
		case !strings.Contains(e.Name, "@sha256:"):
			it.State, it.Why = TartNotPotluck, "a tag, which Potluck never pulls (it pulls by digest)"
		case len(lent[e.Name]) > 0:
			it.State, it.Why = TartInUse, "lent by "+strings.Join(lent[e.Name], ", ")+" of another Potluck daemon here"
		default:
			it.State, it.Why, it.Unclaimed = keepUnproven(e.Name, "an image Tart holds", tags, rec, recErr, served.Homes, servedErr)
			if it.State == TartRemovable && len(out.Others) > 0 {
				it.State, it.Why = TartRetained, "another Potluck on this Mac may need it ("+strings.Join(out.Others, ", ")+")"
			} else if it.State == TartRemovable {
				if err := unchangedSince(l.Dir, e.Name, rec.Images[e.Name].Pulled); err != nil {
					it.State, it.Why = TartRetained, "Potluck's record says it pulled it, but "+err.Error()
					it.Unclaimed = true
					if errors.Is(err, errConfigUnread) {
						out.Unread = errors.Join(out.Unread, err)
					}
				}
			}
		}
		if !it.VM && !it.Measured {
			it.GB = pullGBEstimate(e.Name)
		}
		out.Items = append(out.Items, it)
	}
	if out.Unread != nil {
		for i := range out.Items {
			if it := &out.Items[i]; it.State == TartRemovable {
				it.State, it.Why = TartRetained, "what may still need it could not be read"
			}
		}
	}
	slices.SortStableFunc(out.Items, func(a, b TartItem) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Pulled are the images the record says Potluck pulled, whether or not a Tart
// program is here to list them; one whose files cannot be read may be there. An unreadable record names none.
func (l TartLeave) Pulled() []string {
	rec, _ := readTartPulls(l.Dir)
	var out []string
	for im, u := range rec.Images {
		if _, err := os.Stat(tartConfig(l.Dir, im)); !errors.Is(err, fs.ErrNotExist) && !u.Pulled.IsZero() {
			out = append(out, im)
		}
	}
	slices.Sort(out)
	return out
}

// MayHold reports that Potluck's record says it pulled images that may
// still be here: an unreadable record may say so too.
func (l TartLeave) MayHold() bool {
	_, err := readTartPulls(l.Dir)
	return err != nil || len(l.Pulled()) > 0
}

// Records are Potluck's record of its pulls and the lock beside it, those of
// them that are in Tart's directory.
func (l TartLeave) Records() []string {
	if l.Dir == "" {
		return nil
	}
	out := []string{filepath.Join(l.Dir, TartPullsFile), filepath.Join(l.Dir, tartPullsLock)}
	return slices.DeleteFunc(out, func(p string) bool { _, err := os.Lstat(p); return err != nil })
}

// tartAbsent reports that dir is not there, in a directory that is: the name
// itself is missing (a link to nothing is there, and what it leads to may
// come back with its disk), and that could be read.
func tartAbsent(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	fi, err := os.Stat(filepath.Dir(dir))
	return err == nil && fi.IsDir()
}

// tartMakes are the directories in Tart's that any Tart command makes.
var tartMakes = []string{"cache", "tmp"}

// blank reports that Tart holds nothing: its one directory is not there, or
// is as Tart makes it before anything is in it.
func (l TartLeave) blank() bool {
	if !l.OnlyDir {
		return false
	}
	if tartAbsent(l.Dir) {
		return true
	}
	if fi, err := os.Lstat(l.Dir); err != nil || !fi.IsDir() || !filepath.IsAbs(l.Dir) {
		return false
	}
	ents, err := os.ReadDir(l.Dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() || !slices.Contains(tartMakes, e.Name()) {
			return false
		}
		if in, err := os.ReadDir(filepath.Join(l.Dir, e.Name())); err != nil || len(in) > 0 {
			return false
		}
	}
	return true
}

// DropBlank deletes Tart's directory when it is blank: the empty directories
// Potluck's own Tart made, and nothing that holds anything.
func (l TartLeave) DropBlank() {
	if !l.blank() {
		return
	}
	for _, n := range tartMakes {
		_ = os.Remove(filepath.Join(l.Dir, n)) // only when still empty
	}
	_ = os.Remove(l.Dir)
}

// RemoveImages deletes the removable images (Read) and nothing else, each
// read again just before its delete (removeTart). One that stays because
// something could not be read is an error, so that a caller can run it
// again; one that is no longer Potluck's to remove is not. Then, unless
// it stays, drops Home from Potluck's record of its pulls. A second run removes nothing.
func (l TartLeave) RemoveImages(ctx context.Context, run Runner) (TartCleanup, error) {
	now, err := l.Read(ctx, run)
	if err == nil && now.Unread != nil {
		err = fmt.Errorf("could not read what may still need them: %w", now.Unread)
	}
	if err != nil {
		return TartCleanup{}, err
	}
	again := func(ctx context.Context) (TartInventory, error) {
		r, err := l.Read(ctx, run)
		return r.TartInventory, errors.Join(err, r.Unread)
	}
	c, err := removeTart(ctx, run, l.CLI, l.Dir, now.TartInventory, now.Removable(), again, func(it TartItem) bool { return it.State == TartRemovable })
	for _, rm := range c.Removals {
		if rm.unread != nil {
			err = errors.Join(err, fmt.Errorf("%s was not removed: %s", rm.Name, rm.Note))
		}
	}
	if err == nil && !l.KeepHome {
		_ = editTartPulls(l.Dir, func(rec *tartPullRecord) error { rec.forget(l.Home); return nil }) // a home left in it only keeps images
	}
	return c, err
}

// errConfigUnread is that an image's files are there and could not be read.
var errConfigUnread = errors.New("its files in Tart's cache could not be read")

// unchangedSince is nil when the image's config in Tart's cache was written
// by pulled, when Potluck's pull of it had ended: only a pull that downloads
// the image writes it (a clone may touch its folder).
func unchangedSince(dir, image string, pulled time.Time) error {
	fi, err := os.Stat(tartConfig(dir, image))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("its files are not in Tart's cache")
	case err != nil:
		return fmt.Errorf("%w (%v)", errConfigUnread, err)
	case fi.ModTime().After(pulled.Add(2 * time.Second)): // a disk's clock may round up
		return errors.New("it was downloaded again since, and not by Potluck")
	}
	return nil
}

// tartConfig is the config of image, a digest, in Tart's cache.
func tartConfig(dir, image string) string {
	repo, digest, _ := strings.Cut(image, "@")
	return filepath.Join(dir, "cache", "OCIs", filepath.FromSlash(repo), digest, "config.json")
}

// vmFolders are the folders of Tart's VMs, those its list leaves out too.
func vmFolders(dir string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(dir, "vms"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// tartRunning reports that a program named tart runs on this Mac.
func tartRunning(ctx context.Context, run Runner) (bool, error) {
	r, err := run.Run(ctx, "/bin/ps", "-axo", "comm=")
	if err != nil {
		return false, fmt.Errorf("could not list what runs: %v", err)
	}
	if r.Exit != 0 {
		return false, fmt.Errorf("could not list what runs: ps exited %d", r.Exit)
	}
	for line := range strings.Lines(r.Stdout) {
		if filepath.Base(strings.TrimSpace(line)) == "tart" {
			return true, nil
		}
	}
	return false, nil
}

// tartTags is the tags in Tart's list, by repository.
func tartTags(entries []tartEntry) map[string][]string {
	tags := map[string][]string{}
	for _, e := range entries {
		if e.OCI && !strings.Contains(e.Name, "@") {
			tags[imageRepository(e.Name)] = append(tags[imageRepository(e.Name)], e.Name)
		}
	}
	return tags
}

// forget drops home's shares and marks from the record.
func (rec *tartPullRecord) forget(home string) {
	same := func(h string, _ time.Time) bool { return daemonrecord.SameHome(h, home) }
	maps.DeleteFunc(rec.Shares, same)
	for _, u := range rec.Images {
		maps.DeleteFunc(u.Homes, same)
	}
}

// homes are the homes the record names.
func (rec tartPullRecord) homes() map[string]bool {
	out := map[string]bool{}
	for h := range rec.Shares {
		out[h] = true
	}
	for _, u := range rec.Images {
		for h := range u.Homes {
			out[h] = true
		}
	}
	return out
}
