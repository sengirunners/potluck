package hostprobe

// Tart's disk after a failed or superseded setup (`potluck host images`):
// what Tart's list holds on this Mac, which of it Potluck still needs, and
// the removal, asked for explicitly, of only what nothing needs.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/vmhost"
)

// TartState is what the inventory makes of one image or VM.
type TartState string

const (
	// TartInUse: a size this Mac shares lends the image, or the VM is a
	// Potluck box's (its driver deletes it).
	TartInUse TartState = "in use"
	// TartRetained: Potluck's published image for this Mac, which a retry
	// lends without downloading it again; or an image whose use could not be
	// ruled out, or that Potluck cannot show is its own (TartItem.Unclaimed).
	TartRetained TartState = "retained"
	// TartRemovable: an image Potluck pulled (its record says so) that
	// nothing here needs: an earlier version of a pin, or a pin this Mac's
	// macOS cannot lend; and no tag in Tart's list may name it.
	TartRemovable TartState = "removable"
	// TartAbsent: an image a retry or a shared size needs that Tart does not
	// hold; it is downloaded when needed.
	TartAbsent TartState = "absent"
	// TartNotPotluck: an image or VM Potluck did not make: left alone.
	TartNotPotluck TartState = "not Potluck's"
)

// tartStateOrder is the order the inventory lists states in.
var tartStateOrder = []TartState{TartInUse, TartRetained, TartRemovable, TartAbsent, TartNotPotluck}

// TartItem is one image or VM of the inventory.
type TartItem struct {
	Name  string    `json:"name"`
	VM    bool      `json:"vm,omitempty"` // a local VM (a clone), not a pulled image
	State TartState `json:"state"`
	Why   string    `json:"why"`
	// GB is its size: on disk as Tart's list measured it (Measured), else an
	// estimate from its pin's pull_gb (what a download takes, which is less
	// than the unpacked image; for an absent image, what a retry downloads);
	// 0 and not Measured when nothing says.
	GB       int  `json:"gb"`
	Measured bool `json:"measured"`
	// Unclaimed: retained only because Potluck's record does not say a share
	// of Potluck's pulled it (one pulled before the record existed, or the
	// owner's own), every other check having found nothing that needs it.
	// RemoveTartImages removes it only when it is named.
	Unclaimed bool `json:"unclaimed,omitempty"`
}

// TartInventory is everything Tart's list holds, plus the images a retry or
// a shared size needs that it does not.
type TartInventory struct {
	Items []TartItem `json:"items"`
}

// Removable is the inventory's removable items.
func (inv TartInventory) Removable() []TartItem {
	var out []TartItem
	for _, it := range inv.Items {
		if it.State == TartRemovable {
			out = append(out, it)
		}
	}
	return out
}

// Unclaimed is the inventory's unclaimed items (TartItem.Unclaimed).
func (inv TartInventory) Unclaimed() []TartItem {
	var out []TartItem
	for _, it := range inv.Items {
		if it.Unclaimed {
			out = append(out, it)
		}
	}
	return out
}

// ReclaimGB is what removing the removable items frees at most, in GB, and
// whether any of it is an estimate. At most: a clone of an image shares its
// blocks on APFS, so while a box cloned from it exists less comes back.
func (inv TartInventory) ReclaimGB() (gb int, estimated bool) {
	for _, it := range inv.Removable() {
		gb += it.GB
		estimated = estimated || !it.Measured
	}
	return gb, estimated
}

// Served is what a BindingsReader read: the bindings this machine serves,
// from every Potluck daemon on it that answered, and those daemons' homes.
type Served struct {
	Targets []computeapi.RunTarget
	Homes   []string
}

// BindingsReader reads every binding this machine serves (in every pool and
// at machine level, from every Potluck daemon running here). An error means
// some could not be read, so an image one of them lends would look unused.
type BindingsReader func(context.Context) (Served, error)

// ReadTartInventory reads Tart's list once and sorts each entry by what
// still needs it on this Mac (r, as Detect read it):
//
//   - an image a binding served here lends (bindings: its images, or a run
//     binding's config image) is in use;
//   - Potluck's published image this Mac can lend (CheckTartImages) is
//     retained: a retry, or --with its name, lends it without a download;
//   - an image of a pinned repository that this Mac cannot lend, or an
//     earlier digest of a pin, is removable, but only when Potluck's record
//     says Potluck pulled it (tartpulls.go: the pinned repositories are
//     public, so the owner may have pulled it too), every home the record
//     names was read (a stopped daemon's sizes may lend it), and no
//     tag in Tart's list is of its repository (a tag may name that very
//     download, and Tart drops a tag whose download is deleted); otherwise
//     it is retained;
//   - a VM with a Potluck driver's name (vmhost.PotluckVMName) is in use: a
//     box's, or a kept box's disk, which its driver deletes;
//   - anything else, a tag Potluck never pulls or a VM it did not make, is
//     not Potluck's and is left alone.
//
// The images a retry needs (the host's default, the small Linux image) and
// the bindings' images that Tart does not hold are listed as absent. When
// the bindings cannot be read no image is removable, since one a binding
// lends would look unused. It refuses a Mac whose macOS version or
// architecture is unknown, since which pins it can lend is unknown too.
func ReadTartInventory(ctx context.Context, run Runner, r Report, read BindingsReader) (TartInventory, error) {
	if err := CheckTartImages(r, nil); err != nil {
		return TartInventory{}, fmt.Errorf("cannot tell which images this Mac can lend: %v", err)
	}
	layer, _ := r.Layer("tart")
	entries, err := tartList(ctx, run, layer.CLI)
	if err != nil {
		return TartInventory{}, fmt.Errorf("could not read Tart's list: %v", err)
	}
	var served Served
	bindingsErr := errors.New("no way to read what this machine shares")
	if read != nil {
		served, bindingsErr = read(ctx)
	}
	if bindingsErr != nil {
		served = Served{}
	}
	lent, lentTart := tartLent(served.Targets)
	rec, recErr := readTartPulls(r.TartDir)
	tags := map[string][]string{} // Tart's tags, by repository
	for _, e := range entries {
		if e.OCI && !strings.Contains(e.Name, "@") {
			tags[imageRepository(e.Name)] = append(tags[imageRepository(e.Name)], e.Name)
		}
	}
	inv := TartInventory{Items: []TartItem{}} // never null in JSON
	held := map[string]bool{}
	for _, e := range entries {
		it := TartItem{Name: e.Name, VM: !e.OCI, GB: e.SizeGB, Measured: e.SizeOK}
		if e.OCI {
			held[e.Name] = true
			it.State, it.Why = classifyTartImage(r, lent, e.Name)
			if it.State == TartRemovable {
				it.State, it.Why, it.Unclaimed = keepUnproven(it.Name, it.Why, tags, rec, recErr, served.Homes, bindingsErr)
			}
			if !it.Measured {
				it.GB = pullGBEstimate(e.Name)
			}
		} else if vmhost.PotluckVMName(e.Name) {
			state := "stopped: a kept sandbox's disk, or one its driver deletes at its next sweep"
			if e.Running {
				state = "running"
			}
			it.State, it.Why = TartInUse, "a Potluck sandbox's VM ("+state+"); its driver deletes it when the sandbox ends"
		} else {
			it.State, it.Why = TartNotPotluck, fmt.Sprintf("a VM Potluck did not make: left alone (if it is yours and you no longer need it, `tart delete %s` removes it)", e.Name)
		}
		inv.Items = append(inv.Items, it)
	}
	for _, need := range tartNeeded(r, lentTart) {
		if held[need.image] {
			continue
		}
		it := TartItem{Name: need.image, State: TartAbsent, Why: need.why}
		it.GB = pullGBEstimate(need.image)
		inv.Items = append(inv.Items, it)
	}
	slices.SortStableFunc(inv.Items, func(a, b TartItem) int {
		return cmp.Or(cmp.Compare(slices.Index(tartStateOrder, a.State), slices.Index(tartStateOrder, b.State)), cmp.Compare(a.Name, b.Name))
	})
	return inv, nil
}

// tartLent is the images the bindings served here lend: by any layer's
// binding (never removed), and by a Tart one (what a shared size needs).
// A binding's images are its Images, its config's image (a run binding's
// one image) and its config's images list.
func tartLent(bindings []computeapi.RunTarget) (lent, lentTart map[string][]string) {
	lent, lentTart = map[string][]string{}, map[string][]string{}
	for _, t := range bindings {
		if t.Host != "" {
			continue // another machine's (a reader of a daemon's views filters those: a view's Host hides this one on the wire)
		}
		ims := slices.Clone(t.Images)
		ims = append(ims, t.Config["image"])
		ims = append(ims, strings.Split(t.Config["images"], ",")...)
		seen := map[string]bool{}
		for _, im := range ims {
			if im = strings.TrimSpace(im); im == "" || seen[im] {
				continue
			}
			seen[im] = true
			lent[im] = append(lent[im], t.Name)
			if cmp.Or(t.Config["vm"], "tart") == "tart" {
				lentTart[im] = append(lentTart[im], t.Name)
			}
		}
	}
	return lent, lentTart
}

// keepUnproven is a removable image's state once what could make removing
// it wrong is ruled out (ReadTartInventory): it stays removable only when
// no tag of its repository is in Tart's list, Potluck's record could be
// read, the bindings were read, every home the record names (for this
// image or any other: a share before the record began may have used this
// one there) is one whose daemon was read (read), no home's latest share
// uses it (a retry
// there lends it, or that share is still running: each program judges by
// its own pins, so this one's say nothing of another's), and the record
// says a share of Potluck's pulled it; otherwise it is retained, saying
// why. Unclaimed is set when only that last is missing.
func keepUnproven(image, why string, tags map[string][]string, rec tartPullRecord, recErr error, read []string, bindingsErr error) (state TartState, reason string, unclaimed bool) {
	manual := fmt.Sprintf(" (if nothing of yours needs it, `tart delete %s` removes it)", image)
	use := rec.Images[image]
	var unread, needed []string
	homes := slices.Sorted(maps.Keys(rec.Shares))
	for h := range use.Homes {
		if !slices.ContainsFunc(homes, func(k string) bool { return daemonrecord.SameHome(k, h) }) {
			homes = append(homes, h)
		}
	}
	for _, h := range homes {
		if !slices.ContainsFunc(read, func(r string) bool { return daemonrecord.SameHome(r, h) }) {
			unread = append(unread, h)
		}
	}
	for _, h := range slices.Sorted(maps.Keys(use.Homes)) {
		if !use.Homes[h].Before(rec.Shares[homeKey(rec.Shares, h)]) {
			needed = append(needed, h)
		}
	}
	switch {
	case len(tags[imageRepository(image)]) > 0:
		return TartRetained, fmt.Sprintf("%s; but Tart also holds %s, a tag of its repository that may name this very download (Tart drops a tag whose download is deleted): kept%s",
			why, strings.Join(tags[imageRepository(image)], ", "), manual), false
	case recErr != nil:
		return TartRetained, fmt.Sprintf("%s; but Potluck's record of what it pulled could not be read (%v), so it may be yours: kept%s", why, recErr, manual), false
	case bindingsErr != nil:
		return TartRetained, fmt.Sprintf("%s; but what this machine shares could not be read (%v), so a size may lend it: kept", why, bindingsErr), false
	case len(unread) > 0:
		var gone []string
		for _, h := range unread {
			if _, err := os.Lstat(h); errors.Is(err, fs.ErrNotExist) {
				gone = append(gone, h)
			}
		}
		if len(gone) == len(unread) {
			var forget []string
			for _, h := range gone {
				if ok, whyNot := homeGone(h); ok {
					forget = append(forget, ForgetHomeCommand(h))
				} else {
					forget = append(forget, "not "+h+": "+whyNot)
				}
			}
			return TartRetained, fmt.Sprintf("%s; but this Mac was shared from %s, which no longer exists here, and Potluck cannot tell whether it is gone for good (an unmounted disk comes back): kept (if it is gone for good, %s forgets it and deletes nothing)",
				why, strings.Join(unread, ", "), strings.Join(forget, "; then ")), false
		}
		return TartRetained, fmt.Sprintf("%s; but this Mac was shared from %s, whose Potluck daemon is not running here, so a size there may lend it: kept (start that daemon and run this again)",
			why, strings.Join(unread, ", ")), false
	case len(needed) > 0:
		return TartRetained, fmt.Sprintf("%s; but the latest share from %s uses it (a retry there lends it, or that share is still running): kept",
			why, strings.Join(needed, ", ")), false
	case use.Pulled.IsZero():
		return TartRetained, why + "; but Potluck has no record that it pulled this image (it may be yours, or one Potluck pulled before it kept a record), and nothing else here needs it: kept unless you name it to remove it", true
	}
	return TartRemovable, why, false
}

// pinOf is the pin of a Tart image (PinForImage on the tart layer).
func pinOf(image string) (DefaultPin, bool) {
	p, ok := PinForImage(image)
	return p, ok && p.Layer == "tart"
}

// pullGBEstimate is an image's size where Tart gives none: its pin's
// pull_gb, else that of the current pin of its repository (an earlier
// version is about as big); 0 when no pin says. It is the download, so the
// unpacked image is bigger.
func pullGBEstimate(image string) int {
	if p, ok := pinOf(image); ok {
		return p.PullGB
	}
	if p, ok := imagepins.PinForRepository(image); ok && p.Layer == "tart" {
		return p.PullGB
	}
	return 0
}

// pinLabel names a pin for the inventory's words: its repository's last
// path element, and the --with name of a named image.
func pinLabel(p DefaultPin) string {
	l := path.Base(imageRepository(p.Image))
	if p.Name != "" {
		l += " (--with " + p.Name + ")"
	}
	return l
}

// classifyTartImage sorts one OCI image Tart holds (ReadTartInventory).
func classifyTartImage(r Report, lent map[string][]string, image string) (TartState, string) {
	if by := lent[image]; len(by) > 0 {
		return TartInUse, "lent by " + strings.Join(by, ", ")
	}
	if p, ok := pinOf(image); ok {
		if CheckTartImages(r, []string{image}) == nil {
			return TartRetained, "Potluck's published " + pinLabel(p) + " image for this Mac: a retry lends it without downloading it again"
		}
		if p.MinHostMacOS == 0 || p.Arch == "" {
			return TartRemovable, "Potluck's " + pinLabel(p) + " image has no host compatibility data, so Potluck lends it on no Mac; a retry does not need it"
		}
		return TartRemovable, fmt.Sprintf("Potluck's %s image needs macOS %d+ on %s, and this Mac is macOS %s on %s: Potluck never lends it here, and a retry does not need it",
			pinLabel(p), p.MinHostMacOS, p.Arch, r.MacOS, r.Arch)
	}
	if cur, ok := imagepins.PinForRepository(image); ok && cur.Layer == "tart" && strings.Contains(image, "@sha256:") {
		return TartRemovable, fmt.Sprintf("an earlier version of Potluck's %s image (the pin is now %s); no size this Mac shares lends it", pinLabel(cur), cur.Image)
	}
	if !strings.Contains(image, "@") {
		return TartNotPotluck, "a tag, which Potluck never pulls (it pulls by digest): left alone"
	}
	return TartNotPotluck, "not an image Potluck pins: left alone"
}

// tartNeed is an image a retry or a shared size needs, and why.
type tartNeed struct{ image, why string }

// tartNeeded is what a retry lends on this Mac (the host's default, and the
// small Linux image a first try lends) and every image a Tart binding lends.
func tartNeeded(r Report, lent map[string][]string) []tartNeed {
	var out []tartNeed
	if p, err := HostDefaultImage(r, "tart"); err == nil {
		out = append(out, tartNeed{p.Image, "Potluck's " + pinLabel(p) + " image, this Mac's default: a retry downloads it"})
	}
	if p, ok := namedSmallLinux(); ok && CheckTartImages(r, []string{p.Image}) == nil && !slices.ContainsFunc(out, func(n tartNeed) bool { return n.image == p.Image }) {
		out = append(out, tartNeed{p.Image, "Potluck's " + pinLabel(p) + " image, what a first try lends: a retry downloads it"})
	}
	for _, im := range slices.Sorted(func(yield func(string) bool) {
		for im := range lent {
			if !yield(im) {
				return
			}
		}
	}) {
		if !slices.ContainsFunc(out, func(n tartNeed) bool { return n.image == im }) {
			out = append(out, tartNeed{im, "lent by " + strings.Join(lent[im], ", ") + ": its sandboxes are refused until it is pulled again (`tart pull " + im + "`, or share again)"})
		}
	}
	return out
}

// TartRemoval is what happened to one removable image.
type TartRemoval struct {
	TartItem
	// Removed: Tart's list no longer has it (or, when the list could not be
	// read again, tart delete exited 0).
	Removed bool `json:"removed"`
	// Kept: read again just before its delete, it was no longer removable
	// (Note says why), so it was not deleted. That is no failure.
	Kept bool `json:"kept,omitempty"`
	// Note says what Tart answered when that is not a plain removal.
	Note string `json:"note,omitempty"`
}

// TartCleanup is a removal's account: the inventory it acted on, what
// happened to each removable image, and the other entries Tart's list lost
// meanwhile (a tag of a removed download, or a box its driver deleted).
type TartCleanup struct {
	Before   TartInventory `json:"before"`
	Removals []TartRemoval `json:"removals"`
	AlsoGone []string      `json:"also_gone,omitempty"`
}

// tartDeleteTimeout bounds one tart delete.
var tartDeleteTimeout = 2 * time.Minute

// RemoveTartImages reads the inventory (ReadTartInventory) and runs tart
// delete on each removable image, and nothing else; then it reads Tart's list
// again to say what is gone. With names it deletes those images only, each
// removable or unclaimed (TartItem.Unclaimed: the owner, naming it, says it
// is not theirs); it refuses, deleting nothing, when any named image is
// neither. Just before each delete, holding the record's lock until the
// delete is done (lockTartPulls), it reads the inventory again, bindings and
// all, and keeps an image that is no longer removable (a size began lending
// it, a share began using it, a tag of it appeared). A delete that failed
// for an image the list no longer has says so (it was already gone); one
// that failed for an image still there is an error, naming what tart said.
// A removed image leaves Potluck's record of its pulls. A second run, with
// nothing left to remove, removes nothing.
func RemoveTartImages(ctx context.Context, run Runner, r Report, read BindingsReader, names ...string) (TartCleanup, error) {
	inv, err := ReadTartInventory(ctx, run, r, read)
	if err != nil {
		return TartCleanup{}, err
	}
	c := TartCleanup{Before: inv}
	named := len(names) > 0
	mayRemove := func(it TartItem) bool { return it.State == TartRemovable || named && it.Unclaimed }
	todo := inv.Removable()
	if named {
		todo = nil
		var refused []string
		for _, n := range slices.Compact(slices.Sorted(slices.Values(names))) {
			i := slices.IndexFunc(inv.Items, func(it TartItem) bool { return it.Name == n && it.State != TartAbsent })
			switch {
			case i < 0:
				refused = append(refused, fmt.Sprintf("Tart holds no image %s", n))
			case inv.Items[i].VM:
				refused = append(refused, fmt.Sprintf("%s is a VM, not an image: %s", n, inv.Items[i].Why))
			case !mayRemove(inv.Items[i]):
				refused = append(refused, fmt.Sprintf("%s is %s: %s", n, inv.Items[i].State, inv.Items[i].Why))
			default:
				todo = append(todo, inv.Items[i])
			}
		}
		if len(refused) > 0 {
			return c, fmt.Errorf("nothing removed: %s", strings.Join(refused, "; "))
		}
	}
	layer, _ := r.Layer("tart")
	cli := cmp.Or(layer.CLI, "tart")
	failed := map[string]string{}
	for _, it := range todo {
		rm := TartRemoval{TartItem: it}
		unlock, err := lockTartPulls(r.TartDir)
		if err != nil {
			rm.Kept, rm.Note = true, "kept: Potluck's record of what it pulled could not be locked just before its delete ("+err.Error()+")"
			c.Removals = append(c.Removals, rm)
			continue
		}
		now, err := ReadTartInventory(ctx, run, r, read)
		if i := slices.IndexFunc(now.Items, func(n TartItem) bool { return n.Name == it.Name && n.State != TartAbsent }); err != nil || i < 0 || !mayRemove(now.Items[i]) {
			unlock()
			rm.Kept = true
			switch {
			case err != nil:
				rm.Note = "kept: what Tart holds could not be read again just before its delete (" + err.Error() + ")"
			case i < 0:
				rm.Kept, rm.Removed, rm.Note = false, true, "Tart's list no longer had it just before its delete: it was already gone"
			default:
				rm.Note = "kept: read again just before its delete, it is " + string(now.Items[i].State) + ": " + now.Items[i].Why
			}
			c.Removals = append(c.Removals, rm)
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, tartDeleteTimeout)
		res, err := run.Run(dctx, cli, "delete", it.Name)
		cancel()
		unlock()
		if err != nil || res.Exit != 0 {
			failed[it.Name] = commandFailure(res, err)
		}
		c.Removals = append(c.Removals, rm)
	}
	if len(c.Removals) == 0 {
		held := map[string]bool{}
		for _, it := range inv.Items {
			held[it.Name] = it.State != TartAbsent
		}
		forgetGone(r.TartDir, held)
		return c, nil
	}
	after, listErr := tartList(ctx, run, layer.CLI)
	still := map[string]bool{}
	for _, e := range after {
		still[e.Name] = true
	}
	var errs []error
	for i := range c.Removals {
		rm := &c.Removals[i]
		if rm.Kept || rm.Note != "" {
			continue // decided before any delete
		}
		why, deleteFailed := failed[rm.Name]
		switch {
		case listErr != nil && deleteFailed:
			rm.Note = "tart delete failed: " + why + "; and Tart's list could not be read again (" + listErr.Error() + ")"
		case listErr != nil:
			rm.Removed, rm.Note = true, "tart delete exited 0, but Tart's list could not be read again to confirm ("+listErr.Error()+")"
		case !still[rm.Name] && deleteFailed:
			rm.Removed, rm.Note = true, "tart delete failed ("+why+"), and Tart's list no longer has it: it was already gone"
		case !still[rm.Name]:
			rm.Removed = true
		case deleteFailed:
			rm.Note = "tart delete failed: " + why
		default:
			rm.Note = "tart delete exited 0, but Tart's list still has it"
		}
		if !rm.Removed {
			errs = append(errs, fmt.Errorf("%s was not removed: %s", rm.Name, rm.Note))
		}
	}
	if listErr == nil {
		for _, it := range inv.Items {
			if it.State != TartAbsent && !slices.ContainsFunc(c.Removals, func(rm TartRemoval) bool { return rm.Name == it.Name }) && !still[it.Name] {
				c.AlsoGone = append(c.AlsoGone, it.Name)
			}
		}
		forgetGone(r.TartDir, still)
	}
	return c, errors.Join(errs...)
}

// forgetGone takes Potluck's claim off every image of the record Tart's
// list (held) no longer has, so a later pull of the same digest by the owner
// is not taken for Potluck's; a pull of Potluck's claims it again (one that
// raced the list loses its claim, and its image is kept). The homes that
// used it stay: a stopped daemon's sizes may still lend it once it is back.
// A failed write only leaves claims a later removal drops.
func forgetGone(tartDir string, held map[string]bool) {
	editTartPulls(tartDir, func(rec *tartPullRecord) error {
		for im, u := range rec.Images {
			if !held[im] && !u.Pulled.IsZero() {
				u.Pulled = time.Time{}
				rec.Images[im] = u
			}
		}
		return nil
	})
}
