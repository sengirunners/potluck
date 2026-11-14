package vmhost

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The in-box helper (shelfHelper) reaches the shelf only through the
// publisher, which writes it with every publish. A shelf that is on must have
// it before any box asks for anything: boxes write wants only through the
// helper, so with shelf_fill=boxes a fresh shelf with no helper would never
// fill. So the filler, as it starts, reads the helper's sha512 on the volume
// (read-only) and publishes when it is not the current helper's; with
// nothing staged, that publish carries only the helper. The sweep starts a
// filler for it (shelfHelperDue) when this state directory has not seen the
// current helper on the shelf, and again every shelfHelperCheck, so a fresh
// shelf gets its helper on the driver's first sweep, an upgraded driver's
// replaces the old one, and a volume made again (or a helper another size's
// older driver published) is put right within shelfHelperCheck.

// shelfHelperStamp records the helper this state directory last saw on the
// shelf (published or read back): its sha512 in hex; its mtime is when. It
// is the host's, never the volume's, and only says when to look again: the
// filler decides by what the volume holds.
func shelfHelperStamp(stateDir string) string { return filepath.Join(shelfDir(stateDir), "helper") }

// shelfHelperTry is touched each time the sweep starts a filler for the
// helper, so a shelf that cannot take it is not retried on every sweep.
func shelfHelperTry(stateDir string) string { return filepath.Join(shelfDir(stateDir), "helper.try") }

// shelfHelperRetry is how long the sweep waits before starting another
// filler for a helper that is still not published.
var shelfHelperRetry = 10 * time.Minute

// shelfHelperCheck is how often the sweep has the filler read the helper
// on the volume back, though the stamp says it is current.
var shelfHelperCheck = time.Hour

func shelfHelperSum() string {
	h := sha512.Sum512(shelfHelper)
	return hex.EncodeToString(h[:])
}

// shelfHelperOn says whether the shelf should carry the helper: the owner
// set shelf_max_gb above 0 and shelf_fill is not off.
func shelfHelperOn(cfg config) bool {
	return cfg.Shelf.MaxGB > 0 && cfg.Shelf.Fill != "off" && shelfHelper != nil
}

// stampShelfHelper records that the current helper is on the shelf now (a
// publish just wrote it, or the filler read it back).
func stampShelfHelper(stateDir string) {
	if shelfHelper == nil {
		return
	}
	tmp := shelfHelperStamp(stateDir) + ".tmp"
	if os.WriteFile(tmp, []byte(shelfHelperSum()+"\n"), 0o600) == nil {
		_ = os.Rename(tmp, shelfHelperStamp(stateDir))
	}
}

// shelfHelperFresh says whether this state directory saw the current
// helper on the shelf within shelfHelperCheck.
func shelfHelperFresh(stateDir string) bool {
	p := shelfHelperStamp(stateDir)
	b, err := os.ReadFile(p)
	if err != nil || string(b) != shelfHelperSum()+"\n" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && time.Since(fi.ModTime()) < shelfHelperCheck
}

// shelfHelperDue says whether the sweep should start a filler to check and
// publish the helper: it should be on the shelf, this state directory has
// not seen the current one there lately, and no filler was started for it
// within shelfHelperRetry.
func shelfHelperDue(cfg config) bool {
	if !shelfHelperOn(cfg) || shelfHelperFresh(cfg.StateDir) {
		return false
	}
	fi, err := os.Stat(shelfHelperTry(cfg.StateDir))
	return err != nil || time.Since(fi.ModTime()) >= shelfHelperRetry
}

func stampShelfHelperTry(stateDir string) {
	_ = os.MkdirAll(shelfDir(stateDir), 0o700)
	_ = os.WriteFile(shelfHelperTry(stateDir), nil, 0o600)
	now := time.Now()
	_ = os.Chtimes(shelfHelperTry(stateDir), now, now)
}

// ensureShelfHelper publishes (whatever staging holds, which may be
// nothing) when the shelf should carry the helper and the volume's is not
// the current one (missing, or other bytes). It does nothing without the
// volume or below the free-disk floor. Counts and errors go to log.
func ensureShelfHelper(cfg config, sh shelfer, log io.Writer) {
	if !shelfHelperOn(cfg) || cfg.StateDir == "" || !sh.HasShelf() {
		return
	}
	if sum, err := sh.ShelfHelperSum(cfg.Image); err == nil && sum == shelfHelperSum() {
		stampShelfHelper(cfg.StateDir)
		return
	}
	if err := shelfFloor(cfg, sh)(); err != nil {
		fmt.Fprintln(log, "potluck-driver-tart: shelf: the helper is not published:", err)
		return
	}
	res, err := sh.PublishShelf(cfg.Image)
	if err != nil {
		fmt.Fprintln(log, "potluck-driver-tart: shelf: publishing the helper:", err)
		return
	}
	fmt.Fprintf(log, "potluck-driver-tart: shelf: helper published (with %d blobs or files staged before)\n", res.Published)
}

// shelfHelperSumSh prints the sha512 (hex) of the helper on the shelf, or
// nothing when it is not a regular file there; $1 is the shelf.
const shelfHelperSumSh = `f="$1/` + shelfHelperPath + `"
if [ -f "$f" ] && [ ! -L "$f" ]; then sha512sum < "$f"; fi
exit 0
`

// ShelfHelperSum reads the helper's sha512 on the shelf through a read-only
// helper container: "" when there is none.
func (o ociCLI) ShelfHelperSum(image string) (string, error) {
	out, err := o.shelfRun(image, false, []string{"DAC_READ_SEARCH"}, shelfHelperSumSh, nil, shelfMeasureFor)
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return "", nil
	}
	return f[0], nil
}
