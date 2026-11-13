package vmhost

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

// shelfFilled is what one fill did. Drops are counted by reason only: no
// package name is kept or printed.
type shelfFilled struct {
	Skipped   string         // why nothing was done (shelf_max_gb 0, shelf_fill off), or ""
	Wanted    int            // distinct wants
	Present   int            // already on the shelf
	Fetched   int            // fetched and verified into staging
	Bytes     int64          // the bytes of what was fetched
	Dropped   map[string]int // reason -> wants dropped (shelf_npm.go's drop*)
	Published shelfPublished // the publish after the fetches (zero when none ran)
	Touched   int            // present blobs whose mtime moved forward
	Evicted   shelfEvicted   // the eviction after the publish
}

// shelfNamesMax bounds the names one hash is tried under.
const shelfNamesMax = 4

// dropReason is a fetch's drop reason, or "" for none.
func dropReason(err error) string {
	var d shelfDrop
	switch {
	case err == nil:
		return ""
	case errors.As(err, &d):
		return d.reason
	}
	return dropError
}

// shelfGiB is shelf_max_gb's unit (tests lower it).
var shelfGiB int64 = 1 << 30

// shelfFill fetches each npm want not on the shelf (shelf_npm.go) and
// publishes what was verified (shelf_publish.go). It does nothing unless the
// owner set shelf_max_gb above 0 and shelf_fill is not off. The filler that
// reads boxes' wants and the owner's seed command both call it; it holds no
// queue of its own.
func shelfFill(ctx context.Context, cfg config, sh shelfer, wants []shelfWant) (shelfFilled, error) {
	return shelfFillUpTo(ctx, cfg, sh, wants, int64(cfg.Shelf.MaxGB)*shelfGiB)
}

// shelfFillUpTo is shelfFill staging at most limit bytes of new tarballs
// (the last one may pass it); the rest are dropped as cap. A limit above
// shelf_max_gb is lowered to it.
func shelfFillUpTo(ctx context.Context, cfg config, sh shelfer, wants []shelfWant, limit int64) (shelfFilled, error) {
	limit = min(limit, int64(cfg.Shelf.MaxGB)*shelfGiB)
	res := shelfFilled{Dropped: map[string]int{}}
	switch {
	case cfg.Shelf.MaxGB <= 0:
		res.Skipped = "shelf_max_gb is 0"
		return res, nil
	case cfg.Shelf.Fill == "off":
		res.Skipped = "shelf_fill is off"
		return res, nil
	case cfg.StateDir == "":
		return res, errors.New("no state directory for the shelf's staging")
	case !sh.HasShelf():
		return res, fmt.Errorf("no %s volume: the owner has not made the shelf", ShelfVolume)
	}
	var gos []shelfWant
	wants = slices.DeleteFunc(slices.Clone(wants), func(w shelfWant) bool {
		if w.Kind == shelfKindGo {
			gos = append(gos, w)
		}
		return w.Kind == shelfKindGo
	})
	// By hash: one blob, one fetch. The first shelfNamesMax distinct names a
	// hash came with are tried in turn, so a few wants naming the wrong
	// package for a hash do not keep the right one from filling it. The
	// caller orders wants (the owner's first) and bounds how many it passes.
	uniq := map[string][]shelfWant{}
	for _, w := range wants {
		if len(uniq[w.Hex]) < shelfNamesMax && !slices.Contains(uniq[w.Hex], w) {
			uniq[w.Hex] = append(uniq[w.Hex], w)
		}
	}
	res.Wanted = len(uniq)
	hexes := slices.Sorted(maps.Keys(uniq))
	has, err := sh.ShelfHas(cfg.Image, hexes)
	if err != nil {
		return res, err
	}
	var present []string
	for _, h := range hexes {
		if has[h] {
			present = append(present, h)
		}
	}
	// A missed touch only lets the blob go sooner, so it does not stop the fill.
	res.Touched, _ = sh.TouchShelf(cfg.Image, present, cfg.Shelf.KeepS)
	// What staging already holds (a publish that failed or met the floor)
	// is published with this fill's, or alone: it is never fetched again.
	staged := shelfStagedBytes(cfg.StateDir)
	if res.Present = len(present); res.Present == len(hexes) && staged == 0 && len(gos) == 0 {
		return res, nil
	}
	// The room under shelf_max_gb, after what the keep time lets go. A
	// tarball that would pass it is dropped, so a blob still kept is never
	// pushed out to make room: wanting junk cannot churn the shelf.
	ev, err := evictShelf(cfg, sh)
	if err != nil {
		return res, err
	}
	floor := shelfFloor(cfg, sh)
	clearFetching(cfg.StateDir)
	f, err := newShelfNpm(cfg.StateDir, cfg.Shelf.FillMbit)
	if err != nil {
		return res, err
	}
	f.room = int64(cfg.Shelf.MaxGB)*shelfGiB - ev.Bytes - staged
	for _, h := range hexes {
		if has[h] || shelfIsStaged(cfg.StateDir, h) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if f.kept >= limit { // a fill stops once it staged its limit (the last tarball may pass it)
			res.Dropped[dropCap]++
			continue
		}
		if f.kept >= f.room { // no room: wants wait for the keep time to make it
			res.Dropped[dropFull]++
			continue
		}
		if floor() != nil {
			res.Dropped[dropFloor]++
			continue
		}
		why := dropError
		for _, w := range uniq[h] {
			if why = dropReason(f.fetch(ctx, w)); why == "" {
				break
			}
		}
		if why == "" {
			res.Fetched++
			res.Bytes = f.kept
		} else {
			res.Dropped[why]++
		}
	}
	g, gw, gerr := fillGo(ctx, cfg, sh, gos, goRoom{limit: limit - f.kept, room: f.room - f.kept, floor: floor})
	res.Wanted += gw
	res.Present += g.present
	res.Touched += g.touched
	res.Fetched += g.fetched
	res.Bytes = f.kept + g.bytes
	for k, n := range g.dropped {
		res.Dropped[k] += n
	}
	if res.Fetched == 0 && staged == 0 {
		return res, gerr
	}
	if err := floor(); err != nil { // staging stays for a later fill
		return res, errors.Join(gerr, err)
	}
	if res.Published, err = sh.PublishShelf(cfg.Image); err != nil {
		return res, errors.Join(gerr, err)
	}
	res.Evicted, err = evictShelf(cfg, sh)
	return res, errors.Join(gerr, err)
}

// String is the fill as one line for the owner, with no package names.
func (r shelfFilled) String() string {
	if r.Skipped != "" {
		return "shelf: nothing filled (" + r.Skipped + ")"
	}
	s := fmt.Sprintf("shelf: %d wanted, %d already on the shelf, %d fetched", r.Wanted, r.Present, r.Fetched)
	if len(r.Dropped) > 0 {
		var ds []string
		for _, k := range slices.Sorted(maps.Keys(r.Dropped)) {
			ds = append(ds, fmt.Sprintf("%d %s", r.Dropped[k], k))
		}
		s += ", dropped " + strings.Join(ds, ", ")
	}
	if r.Fetched > 0 {
		s += fmt.Sprintf("; published %d (%d were there, %d refused); evicted %d", r.Published.Published, r.Published.Present, r.Published.Dropped, r.Evicted.Deleted)
	}
	if r.Touched > 0 {
		s += fmt.Sprintf("; %d kept longer", r.Touched)
	}
	return s
}

// shelfUsage is `potluck-driver-tart host shelf`'s usage.
const shelfUsage = `usage: potluck-driver-tart host shelf fill [--npm-lock FILE]... [--go-sum FILE]... [--go-private GLOBS] < request
       potluck-driver-tart host shelf status < request

Fetches the packages the package-lock.json files name (only those resolved
from registry.npmjs.org with a sha512 integrity) and the Go modules the go.sum
files name (from proxy.golang.org only, checked against sum.golang.org; none
that GLOBS, in GOPRIVATE's syntax, match) onto the shelf of the binding whose
config is in the request on stdin ({"config": {...}}, as the daemon sends it).
status prints the shelf's size (no names), fill mode, limits and queue.
potluck host shelf fill and potluck host shelf status run these for you.
`

// shelfHostCmd is `potluck-driver-tart host shelf ...`.
func shelfHostCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "status" {
		return shelfStatusCmd(args[1:], stdin, stdout, stderr)
	}
	if len(args) == 0 || args[0] != "fill" {
		fmt.Fprint(stderr, shelfUsage)
		return 2
	}
	fs := flag.NewFlagSet("host shelf fill", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var locks, sums []string
	fs.Func("npm-lock", "", func(s string) error { locks = append(locks, s); return nil })
	fs.Func("go-sum", "", func(s string) error { sums = append(sums, s); return nil })
	private := fs.String("go-private", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 || len(locks)+len(sums) == 0 {
		fmt.Fprint(stderr, shelfUsage)
		return 2
	}
	var req struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&req); err != nil {
		fmt.Fprintln(stderr, "potluck-driver-tart host shelf fill: the request on stdin:", err)
		return 2
	}
	cfg, err := parseConfig(req.Config)
	if err != nil {
		fmt.Fprintln(stderr, "potluck-driver-tart host shelf fill:", err)
		return 1
	}
	sh, ok := newLayer(cfg).(shelfer)
	if !ok {
		fmt.Fprintf(stderr, "potluck-driver-tart host shelf fill: vm=%s has no shelf (only vm=oci does)\n", cfg.VM)
		return 1
	}
	var wants []shelfWant
	read := func(p string, parse func(io.Reader) ([]shelfWant, error)) bool {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(stderr, "potluck-driver-tart host shelf fill:", err)
			return false
		}
		ws, err := parse(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(stderr, "potluck-driver-tart host shelf fill: %s: %v\n", p, err)
			return false
		}
		wants = append(wants, ws...)
		return true
	}
	for _, p := range locks {
		if !read(p, npmLockWants) {
			return 1
		}
	}
	for _, p := range sums {
		if !read(p, func(r io.Reader) ([]shelfWant, error) { return goSumWants(r, *private) }) {
			return 1
		}
	}
	if err := (store{cfg.StateDir}).create(); err != nil {
		fmt.Fprintln(stderr, "potluck-driver-tart host shelf fill:", err)
		return 1
	}
	res, err := shelfFill(context.Background(), cfg, sh, wants)
	fmt.Fprintln(stdout, res)
	if err != nil {
		fmt.Fprintln(stderr, "potluck-driver-tart host shelf fill:", err)
		return 1
	}
	return 0
}
