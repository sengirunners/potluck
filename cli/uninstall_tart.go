package cli

// Tart when Potluck is removed (uninstall --with-tart): the images
// Potluck pulled and the tart.app Potluck installed, and only those. What is
// Potluck's is a recorded fact (hostprobe's record of pulls, tartpin's of
// installs), never a guess.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/attach"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/tartpin"
	"github.com/sengirunners/potluck/vmhost"
)

// uninstallTartWait bounds a plan's reading of Tart (the app waits for it),
// or of a removal that takes none of it, and uninstallTartRunWait that of one
// that does.
const (
	uninstallTartWait    = 10 * time.Second
	uninstallTartRunWait = time.Minute
)

// tartScope is how much of Tart --with-tart takes. A window's removal names
// the one its plan showed, so a run never takes more.
type tartScope string

const (
	tartNone   tartScope = ""
	tartImages tartScope = "images" // the images Potluck pulled
	tartAll    tartScope = "all"    // and the tart.app it installed
)

// The steps of --with-tart: tartReadStep is that of a plan that could not
// read Tart.
const (
	tartReadStep   = "tart-read"
	tartImagesStep = "images"
	tartAppStep    = "tart"
)

// orWithoutTart ends what a --with-tart step that failed says.
const orWithoutTart = "run it again, or without --with-tart to leave Tart as it is"

const tartFolderStays = "Potluck's leftovers from installing Tart: a folder in Applications couldn't be deleted, or may hold something of yours"

var errNoTart = errors.New("no Tart program is here")

// tartPart is this Mac's Tart as uninstall reads it (nil: it has none).
type tartPart struct {
	leave hostprobe.TartLeave
	now   hostprobe.TartLeaving
	err   error // now could not be read
	place tartpin.Place
	app   tartpin.Record
	ours  bool // the tart.app at place is Potluck's, app
	// unfinished is that an earlier removal of it was interrupted.
	unfinished bool
	// strays are the images Potluck pulled that no Tart here can delete.
	strays []string
}

// readTartPart reads this Mac's Tart, changing nothing.
func (a *app) readTartPart(ctx context.Context, home string, keepHome bool, wait time.Duration) (*tartPart, error) {
	if setupGOOS != "darwin" {
		return nil, nil
	}
	t := &tartPart{}
	var err error
	if t.place, err = tartPlace(); err != nil {
		return nil, err
	}
	t.app, t.ours = t.place.Installed()
	t.unfinished = t.place.Unfinished()
	cli := vmhost.FindTart(tartSearch()).Path
	t.leave = hostprobe.TartLeaveAt(cli, home, keepHome, func(ctx context.Context) (hostprobe.Served, error) { return othersServed(ctx, home) })
	t.leave.OnlyDir = t.leave.OnlyDir && !tartUsedElsewhere(t.place, home)
	if cli == "" {
		t.strays = t.leave.Pulled()
		if !t.ours && !t.unfinished && len(t.strays) == 0 {
			return nil, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if t.err = errNoTart; cli != "" {
		t.now, t.err = t.leave.Read(ctx, tartRunner)
	}
	return t, nil
}

// images are the images --with-tart removes, and gb their size.
func (t *tartPart) images() (names []string, gb int) {
	if t == nil || t.err != nil {
		return nil, 0
	}
	for _, it := range t.now.Removable() {
		names = append(names, it.Name)
		gb += it.GB
	}
	return names, gb
}

// leftovers reports that --with-tart finishes an interrupted removal of
// tart.app: only when all of Tart could be read, so that what a run takes is
// what its plan named.
func (t *tartPart) leftovers() bool {
	return t != nil && t.unfinished && (t.err == errNoTart || t.err == nil && t.now.Unread == nil)
}

// scope is what a run asked for asked of Tart should be asked for: no more
// than there is, but all of it when that could not be read, so that no run
// goes on without it.
func (t *tartPart) scope(asked tartScope) tartScope {
	if t == nil || asked == tartNone {
		return tartNone
	}
	names, _ := t.images()
	switch {
	case t.unread(asked) != nil:
		return asked
	case asked == tartAll && (t.takesApp() || t.leftovers()):
		return tartAll
	case len(names) > 0:
		return tartImages
	}
	return tartNone
}

// takesApp reports that --with-tart removes tart.app.
func (t *tartPart) takesApp() bool {
	return t != nil && t.ours && t.err == nil && t.now.Empty()
}

// unread is why a removal of scope must not go on: what Tart holds, or what
// may need it, could not be read, and something here may be Potluck's.
func (t *tartPart) unread(scope tartScope) error {
	if t == nil {
		return nil
	}
	err := t.err
	if err == nil {
		err = t.now.Unread
	}
	if err == nil || err == errNoTart {
		return nil
	}
	if t.leave.MayHold() || scope == tartAll && (t.ours || t.unfinished) {
		return err
	}
	return nil
}

// tartUsedElsewhere reports a sign that Potluck's Tart was used in another
// directory than the one Tart takes by itself (TART_HOME, in a shell this run
// does not see): with that one blank, what it holds is there. A setup on home
// that reached the images recorded itself where it pulled them, the driver's
// records say it made VMs, and the daily update job keeps the TART_HOME of
// the shell that turned it on. What cannot be read is such a sign.
func tartUsedElsewhere(place tartpin.Place, home string) bool {
	if place.UsedElsewhere() || tartDriverState() != "" {
		return true
	}
	run, err := hostsetup.Read(home)
	if err != nil || run != nil && slices.ContainsFunc(run.Stages, func(s hostsetup.Stage) bool { return s.Name != hostsetup.Preflight }) {
		return true
	}
	s, err := newUpdateSchedule()
	if err != nil {
		return true
	}
	for _, f := range s.Files() {
		b, err := os.ReadFile(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || strings.Contains(string(b), "TART_HOME") {
			return true
		}
	}
	return false
}

// keepElsewhere records that Tart was used elsewhere before a run takes what
// shows it, for the next run. No setup on home goes on any more (held is why
// one may: then nothing is taken).
func (p uninstallPlan) keepElsewhere(a *app, home string, held error) {
	place, err := tartPlace()
	if held != nil || setupGOOS != "darwin" || err != nil || !tartUsedElsewhere(place, home) {
		return
	}
	if err := place.MarkUsedElsewhere(); err != nil {
		words := fmt.Sprintf("could not note in %s that Tart may have been used in another folder (%v): a later --with-tart may take Tart for unused", attach.Tilde(place.RecordFile), err)
		*p.notes = append(*p.notes, words)
		a.say("%s", words)
	}
}

// records are Potluck's files about Tart that stay after uninstall with f.
func (t *tartPart) records(f uninstallFlags) []string {
	if setupGOOS != "darwin" {
		return nil
	}
	files := hostprobe.TartLeaveAt("", "", false, nil).Records()
	// The install's record stays with the tart.app it is of.
	if place, err := tartPlace(); err == nil && lexists(place.RecordFile) && !(f.withTart == tartAll && (t.takesApp() || t.leftovers())) {
		for _, p := range []string{place.RecordFile, place.RecordFile + ".lock"} {
			if lexists(p) {
				files = append(files, p)
			}
		}
	}
	return files
}

// tartLockSteps delete what an install of Tart left that was cut off before
// it had a record, or a removal after (tartpin's lock file): nothing of a Tart.
func (p uninstallPlan) tartLockSteps(note func(string, ...any)) []uninstallStep {
	if setupGOOS != "darwin" {
		return nil
	}
	place, err := tartPlace()
	if err != nil || !place.HasSpareLock() {
		return nil
	}
	lock := place.RecordFile + ".lock"
	if !lexists(lock) {
		lock = filepath.Dir(lock) // the empty folder
	}
	return []uninstallStep{{
		id: "tart-lock", will: "Remove what is left from installing Tart", did: "Removed what was left from installing Tart",
		plan: "remove what an interrupted install or removal of Tart left: " + attach.Tilde(lock) + " and any folder it unpacked into",
		do: func(context.Context) error {
			if err := place.DropSpareLock(); err != nil { // the rest can go without it
				note("%s stays: %v", attach.Tilde(lock), err)
				*p.kept = "A small file or folder left from installing Tart"
			}
			return nil
		},
	}}
}

// offers is what --with-tart with scope removes (none yet: all there is),
// in the app's words ("": nothing).
func (t *tartPart) offers(scope tartScope) string {
	if t.unread(cmp.Or(scope, tartAll)) != nil {
		if scope == tartImages {
			return "any disk images Potluck downloaded to run jobs (it can't read which there are right now)"
		}
		return "what Potluck downloaded or installed to run jobs in Tart (it can't read what there is right now)"
	}
	names, gb := t.images()
	size := ""
	if gb > 0 {
		size = fmt.Sprintf(" (about %d GB)", gb)
	}
	app := scope != tartImages
	switch {
	case app && len(names) > 0 && t.takesApp():
		return "Tart, which Potluck installed to run jobs, and everything Potluck downloaded for it" + size
	case app && t.takesApp():
		return "Tart, which Potluck installed to run jobs"
	case app && len(names) > 0 && t.leftovers():
		return "Potluck's leftovers from installing Tart, and the disk images Potluck downloaded to run jobs" + size
	case len(names) > 0:
		return "the disk images Potluck downloaded to run jobs" + size
	case app && t.leftovers():
		return "Potluck's leftovers from installing Tart"
	}
	return ""
}

// kept is why Tart's tart.app, or its images, stay with --with-tart, for
// the terminal and in the app's words ("": neither stays).
func (t *tartPart) kept() (why, plain string) {
	switch {
	case t == nil, !t.ours && t.err == errNoTart:
		return "", ""
	case t.err != nil:
		return "what Tart holds could not be read (" + oneLineErr(t.err) + ")", "Potluck couldn't read what is in it"
	case t.now.Unread != nil:
		return "what may still need it could not be read (" + oneLineErr(t.now.Unread) + ")", "Potluck couldn't tell what else uses it"
	case len(t.now.Others) > 0:
		return "another Potluck on this Mac may need it (" + strings.Join(tildeAll(t.now.Others), ", ") + ")", "another Potluck on this Mac may need it"
	}
	var why1, plain1 []string
	if t.ours && !t.now.Known {
		noted := ""
		if t.place.UsedElsewhere() {
			noted = " (" + attach.Tilde(t.place.RecordFile) + " says it may have been used in another folder)"
		}
		why1, plain1 = append(why1, "Potluck's record of its pulls is not in "+attach.Tilde(t.leave.Dir)+", so what Tart holds may be elsewhere"+noted), append(plain1, "Potluck couldn't tell where Tart keeps its virtual machines")
	}
	vms, jobs, unproven, other := 0, 0, 0, 0
	for _, it := range t.now.Kept() {
		why1 = append(why1, it.Name+": "+it.Why)
		switch {
		case it.VM && it.State == hostprobe.TartInUse:
			jobs++
		case it.VM:
			vms++
		case it.Unclaimed:
			unproven++
		default:
			other++
		}
	}
	for _, c := range []struct {
		n    int
		what string
	}{
		{vms, "virtual machines Potluck didn't make are in it"},
		{jobs, "virtual machines that jobs ran in are still in it"},
		{unproven, "it holds disk images Potluck may not have downloaded (it has no record that it did)"},
		{other, "it holds other disk images that may be yours"},
	} {
		if c.n > 0 {
			plain1 = append(plain1, c.what)
		}
	}
	return strings.Join(why1, "; "), strings.Join(plain1, ", and ")
}

// refusal is what a run asked for --with-tart is told it will not remove.
func (t *tartPart) refusal(f uninstallFlags) string {
	if f.withTart == tartNone {
		return ""
	}
	if t == nil {
		return "--with-tart: there is no Tart on this machine."
	}
	if why, _ := t.kept(); why != "" && t.unread(f.withTart) == nil {
		return "--with-tart leaves what is not Potluck's to remove: " + why
	}
	return ""
}

// left is what uninstall with f leaves of Tart, as people-words and in the
// app's words ("": nothing).
func (t *tartPart) left(a *app, f uninstallFlags) (left, stays string) {
	if t == nil {
		return "", ""
	}
	why, plain := t.kept()
	names, gb := t.images()
	unread := t.unread(tartAll) != nil
	app := ""
	if t.ours {
		app = fmt.Sprintf("the tart.app Potluck installed (%s, Tart %s; its record is %s)", attach.Tilde(t.place.App()), t.app.Version, attach.Tilde(t.place.RecordFile))
	}
	var parts []string
	if app != "" && (f.withTart != tartAll || !t.takesApp()) {
		parts = append(parts, app)
	}
	if len(names) > 0 && f.withTart == tartNone {
		parts = append(parts, fmt.Sprintf("the Tart images Potluck pulled (%s, about %d GB)", strings.Join(names, ", "), gb))
	}
	if why != "" {
		parts = append(parts, "what else Tart holds, which Potluck never removes ("+why+")")
	}
	itself, plainItself := "Tart itself, which Potluck did not install", "Tart, a program that runs virtual machines: Potluck didn't install it"
	switch {
	case t.ours:
		itself, plainItself = "", ""
	case t.unfinished && t.err == errNoTart:
		itself, plainItself = "", ""
	case t.unfinished:
		itself, plainItself = "a Tart that is not where Potluck put one", "Tart, a program that runs virtual machines: it isn't where Potluck put one"
	}
	if t.unfinished {
		if f.withTart != tartAll || !t.leftovers() {
			parts = append(parts, "what is left of the tart.app Potluck installed (its record is "+attach.Tilde(t.place.RecordFile)+")")
		}
	}
	if !t.ours && !t.unfinished && lexists(t.place.RecordFile) {
		itself = "the tart.app at " + attach.Tilde(t.place.App()) + ", which changed since Potluck installed it (its record is " + attach.Tilde(t.place.RecordFile) + ")"
		plainItself = "Tart, the program jobs ran in: it changed since Potluck installed it"
	}
	if len(t.strays) > 0 {
		stray := "The disk images Potluck downloaded to run jobs: there is no Tart on this Mac to delete them with"
		if f.withTart == tartNone && t.unfinished {
			stray += ". Potluck's leftovers from installing Tart stay too"
		}
		parts = append(parts, "the images Potluck pulled into "+attach.Tilde(t.leave.Dir)+" ("+strings.Join(t.strays, ", ")+"), which no Tart program is here to delete")
		return strings.Join(parts, "; "), stray
	}
	if len(parts) == 0 {
		return itself, plainItself
	}
	if itself != "" {
		parts = append(parts, itself)
	}
	left = strings.Join(parts, "; ")
	if offer := t.offers(f.withTart); offer != "" && f.withTart == tartNone && !unread {
		stays = strings.ToUpper(offer[:1]) + offer[1:]
		if f.dryRun {
			left += ". " + a.cmd("uninstall --with-tart") + " removes what of it Potluck installed and pulled, and nothing of yours"
		} else {
			left += ". `tart delete <name>` removes an image, and tart.app can go to the Trash"
		}
		if plain != "" {
			stays += ". Nothing else in Tart is touched: " + plain
		}
		if plainItself != "" {
			stays += ". Tart itself stays too"
		}
		return left, stays
	}
	stays = cmp.Or(plainItself, "Tart, the program jobs ran in")
	if plainItself == "" && plain != "" {
		stays += ": " + plain
	}
	if unread && t.leave.MayHold() {
		stays += ". It may hold disk images Potluck downloaded"
		left += ". It may hold images Potluck pulled"
	}
	return left + " (`tart list` shows its images and VMs, and `tart delete <name>` removes one)", stays
}

// tartSteps are --with-tart's steps. Each reads Tart again before it removes
// anything, and keeps what is no longer Potluck's alone, saying so. What
// cannot be read stops the run instead, while the home and the programs are
// here to run it again with: without them nothing tells Potluck's images
// from anyone's.
func (p uninstallPlan) tartSteps(a *app, f uninstallFlags, note func(string, ...any)) (steps []uninstallStep) {
	t := p.tart
	if t == nil || f.withTart == tartNone {
		return nil
	}
	if unread := t.unread(f.withTart); unread != nil {
		return []uninstallStep{{
			id: tartReadStep, will: "Check what Tart holds", did: "Checked what Tart holds",
			plan: "read what Tart holds (it could not be read: " + oneLineErr(unread) + ")",
			do: func(context.Context) error {
				return fmt.Errorf("%w: "+orWithoutTart, unread)
			},
		}}
	}
	if names, gb := t.images(); len(names) > 0 {
		steps = append(steps, uninstallStep{
			id:   tartImagesStep,
			will: fmt.Sprintf("Delete the disk images Potluck downloaded (about %d GB)", gb), did: fmt.Sprintf("Deleted the disk images Potluck downloaded (about %d GB)", gb),
			plan: fmt.Sprintf("remove the Tart images Potluck pulled (about %d GB): %s", gb, strings.Join(names, ", ")),
			do: func(ctx context.Context) error {
				c, err := t.leave.RemoveImages(ctx, tartRunner)
				kept, gone := 0, 0
				for _, rm := range c.Removals {
					if rm.Kept {
						kept++
						note("%s: %s", rm.Name, rm.Note)
					} else if rm.Removed {
						gone++
						a.say("removed %s", rm.Name)
					}
				}
				if err == nil && gone < len(names) && kept == 0 {
					note("%d of %d images were no longer Potluck's to remove: kept", len(names)-gone, len(names))
					kept = len(names) - gone
				}
				if kept > 0 {
					*p.kept = "Some of the disk images Potluck downloaded: Potluck couldn't tell that nothing else needs them"
					if gone > 0 {
						*p.said = "Deleted some of the disk images Potluck downloaded"
					}
				}
				if err != nil {
					err = fmt.Errorf("%w: "+orWithoutTart, err)
				}
				return err
			},
		})
	}
	if t.leftovers() && f.withTart == tartAll {
		steps = append(steps, uninstallStep{
			id: tartAppStep, will: "Remove Potluck's leftovers from installing Tart", did: "Removed Potluck's leftovers from installing Tart",
			plan: "remove what an interrupted removal of the tart.app Potluck installed left, and remove the record of it, " + attach.Tilde(t.place.RecordFile),
			do: func(context.Context) error {
				err := t.place.Finish()
				if tartLeft(err, note) {
					*p.kept = tartFolderStays
					return nil
				}
				if errors.Is(err, tartpin.ErrNotOurs) {
					note("%s is there again: kept", attach.Tilde(t.place.App()))
					*p.kept = "Tart: it was installed again"
					return nil
				}
				return err
			},
		})
	}
	if t.takesApp() && f.withTart == tartAll {
		steps = append(steps, uninstallStep{
			id: tartAppStep, will: "Remove Tart, which Potluck installed", did: "Removed Tart, which Potluck installed",
			plan: fmt.Sprintf("remove %s (Tart %s, which Potluck installed) and the record of it, %s", attach.Tilde(t.place.App()), t.app.Version, attach.Tilde(t.place.RecordFile)),
			do: func(ctx context.Context) error {
				// A setup stopped since the plan may have got further.
				t.leave.OnlyDir = t.leave.OnlyDir && !tartUsedElsewhere(t.place, t.leave.Home)
				now, err := t.leave.Read(ctx, tartRunner)
				if err = errors.Join(err, now.Unread); err != nil {
					return fmt.Errorf("could not read whether something still uses Tart: %w: "+orWithoutTart, err)
				}
				if now.Busy || !now.Empty() || len(now.Removable()) > 0 {
					note("%s stays: something on this Mac uses it now", attach.Tilde(t.place.App()))
					*p.kept = "Tart: something on this Mac may use it now"
					return nil
				}
				if t.leave.CLI == t.place.Program() { // its empty folders, no other Tart's
					t.leave.DropBlank()
				}
				_, err = t.place.Remove()
				if tartLeft(err, note) {
					*p.said = "Removed Tart, which Potluck installed"
					*p.kept = tartFolderStays
					return nil
				}
				if errors.Is(err, tartpin.ErrNotOurs) {
					note("%s stays: %v", attach.Tilde(t.place.App()), err)
					*p.kept = "Tart: it changed since Potluck installed it"
					return nil
				}
				return err
			},
		})
	}
	return steps
}

// tartLeft reports that err is only that folders of interrupted installs
// stay, and notes it.
func tartLeft(err error, note func(string, ...any)) bool {
	var left *tartpin.LeftError
	if !errors.As(err, &left) {
		return false
	}
	note("%v", left)
	return true
}
