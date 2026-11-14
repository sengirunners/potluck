package vmhost

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/vmhost/shelfhelper"
)

// The shelf (zm-eakh.12.30) is the owner's cache volume (ShelfVolume),
// mounted read-only in every box at ociCacheDir, holding public packages a
// box would otherwise download. Its layout:
//
//	tools/<tool>/<version>/<arch>{,.complete}       the owner's fill script's
//	goproxy/<module>/@v/<version>.{zip,ziphash,mod,info}  a GOPROXY tree (paths escaped as Go escapes them), files 0444
//	npm/content-v2/sha512/<aa>/<bb>/<rest of hex>    one file per tarball, mode 0444, named by its sha512
//	npm/.tmp/                                        the publisher's staging, same filesystem, root 0700
//	bin/potluck-deps                                 the in-box helper, written by the publisher (shelf_helper.go)
//
// A blob's path is its hash, so presence means complete: there is no index
// and no marker, and nothing in the volume says who asked for what. In the driver only
// the publisher (shelf_publish.go) mounts the volume writable; no box ever
// does. What the host
// keeps about the shelf (the files waiting to be published, the lock, the
// last measurement) is under shelfDir in the state directory, never in the
// volume.

// shelfBlobDir is where npm tarballs live in the volume, and shelfTmpDir the
// publisher's staging beside them (one filesystem, so a rename is atomic).
const shelfBlobDir, shelfTmpDir = "npm/content-v2/sha512", "npm/.tmp"

// shelfHelperPath is the in-box helper's path in the volume.
const shelfHelperPath = "bin/" + shelfhelper.Name

// shelfBlobName is an npm blob's path in the volume: its sha512 in lowercase
// hex, split 2/2/124 as npm's own cache is.
var shelfBlobName = regexp.MustCompile(`^` + regexp.QuoteMeta(shelfBlobDir) + `/([0-9a-f]{2})/([0-9a-f]{2})/([0-9a-f]{124})$`)

// shelfBlobHex is the sha512 a blob path names, or "" when p is not one.
func shelfBlobHex(p string) string {
	m := shelfBlobName.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	return m[1] + m[2] + m[3]
}

// shelfDir is the host's own state about the shelf: staging (files fetched
// and verified, waiting for the publisher; a file enters it by a rename
// under lock, and only npm blobs and GOPROXY files at their volume paths:
// anything else there refuses every publish until it is removed), lock (one
// publisher at a time) and stats (the last measurement, for describe).
func shelfDir(stateDir string) string { return filepath.Join(stateDir, "shelf") }

func shelfStaging(stateDir string) string { return filepath.Join(shelfDir(stateDir), "staging") }

func shelfLock(stateDir string) string { return filepath.Join(shelfDir(stateDir), "lock") }

// shelfCfg is the owner's shelf keys. Fetching and eviction read them;
// parseConfig only checks them.
type shelfCfg struct {
	MaxGB      int    // shelf_max_gb: the shelf's size cap; 0 (the default) fills nothing
	KeepS      int    // shelf_keep_s: a blob no box asked for in this long goes
	Fill       string // shelf_fill: off, seed (only what the owner seeds) or boxes (also what boxes ask for)
	FillMbit   int    // shelf_fill_mbit: the host's download rate for fills
	FillDelayS int    // shelf_fill_delay_s: a live box's wants wait this long, or until it ends
}

const (
	shelfKeepDefault      = 30 * 24 * 3600
	shelfFillMbitDefault  = 16
	shelfFillDelayDefault = 300
	shelfFillDefault      = "seed"
)

// shelfInts are the shelf's whole-number keys, for parseConfig's ints table.
func (s *shelfCfg) ints() []struct {
	key  string
	into *int
	def  int
} {
	return []struct {
		key  string
		into *int
		def  int
	}{{"shelf_max_gb", &s.MaxGB, 0}, {"shelf_keep_s", &s.KeepS, shelfKeepDefault},
		{"shelf_fill_mbit", &s.FillMbit, shelfFillMbitDefault}, {"shelf_fill_delay_s", &s.FillDelayS, shelfFillDelayDefault}}
}

// parse reads shelf_fill and checks the keys ints read.
func (s *shelfCfg) parse(m map[string]string, vm string, bad *[]string) {
	switch s.Fill = m["shelf_fill"]; s.Fill {
	case "":
		s.Fill = shelfFillDefault
	case "off", "seed", "boxes":
	default:
		*bad = append(*bad, "shelf_fill is off, seed or boxes")
	}
	if s.KeepS < 60 {
		*bad = append(*bad, "shelf_keep_s must be at least 60")
	}
	if s.FillMbit < 1 {
		*bad = append(*bad, "shelf_fill_mbit must be at least 1")
	}
	if s.MaxGB > 0 && vm != "oci" {
		*bad = append(*bad, "shelf_max_gb is for vm=oci: only its boxes mount the shelf")
	}
}

// raw echoes the keys for rawConfig.
func (s shelfCfg) raw(m map[string]string) {
	m["shelf_max_gb"], m["shelf_keep_s"], m["shelf_fill"] = strconv.Itoa(s.MaxGB), strconv.Itoa(s.KeepS), s.Fill
	m["shelf_fill_mbit"], m["shelf_fill_delay_s"] = strconv.Itoa(s.FillMbit), strconv.Itoa(s.FillDelayS)
}

// shelfer is a layer with a shelf (OCI; Tart and Incus have no volumes).
type shelfer interface {
	// HasShelf says whether the shelf's volume exists (ShelfMounted, an
	// unreadable state counted as none).
	HasShelf() bool
	// ShelfMounted says whether a box created now gets the shelf, or that
	// its state cannot be read (an error): what create and describe decide by.
	ShelfMounted() (bool, error)
	// BoxShelf says whether an existing box has the shelf mounted, an error
	// for a mount that is not the one every box gets.
	BoxShelf(vm string) (bool, error)
	// MeasureShelf counts the shelf's bytes and files, read-only.
	MeasureShelf(image string) (shelfStats, error)
	// PublishShelf moves what is in the state directory's staging onto the
	// shelf (shelf_publish.go).
	PublishShelf(image string) (shelfPublished, error)
	// ShelfHas says which of the blobs named by hexes (sha512s in hex) are
	// on the shelf, read-only.
	ShelfHas(image string, hexes []string) (map[string]bool, error)
	// ListShelf lists the shelf's files with their sizes and mtimes,
	// read-only (shelf_evict.go).
	ListShelf(image string) (shelfListing, error)
	// DeleteShelf deletes npm blobs whose mtime is still the one listed,
	// through the publisher's container, and answers which went.
	DeleteShelf(image string, blobs []shelfBlobAt) (map[string]bool, error)
	// TouchShelf moves forward the mtime of the blobs named that are older
	// than shelfTouchAge(keepS), through the publisher's container.
	TouchShelf(image string, hexes []string, keepS int) (int, error)
	// ShelfHasFiles says which of the paths (relative to the shelf, built
	// by the host) are regular files on the shelf, read-only.
	ShelfHasFiles(image string, rels []string) (map[string]bool, error)
	// DeleteShelfGo deletes whole Go module versions whose recency is still
	// the one listed, through the publisher's container, and answers which
	// went (by Base).
	DeleteShelfGo(image string, gos []shelfGoAt) (map[string]bool, error)
	// TouchShelfFiles moves forward the mtime of the files named (paths
	// relative to the shelf) that are older than shelfTouchAge(keepS),
	// through the publisher's container.
	TouchShelfFiles(image string, rels []string, keepS int) (int, error)
	// ShelfHelperSum reads the sha512 (hex) of the helper on the shelf,
	// read-only: "" when there is none (shelf_helper.go).
	ShelfHelperSum(image string) (string, error)
	// FetchGo runs the Go fetch container (shelf_go.go): lines in, a tar of
	// module cache files out.
	FetchGo(ctx context.Context, image string, lines io.Reader, out io.Writer) error
}

// shelfStats is a measurement of the whole volume: its disk use in bytes
// and its regular files (the publisher's staging left out), never a name.
type shelfStats struct {
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
	At    int64 `json:"at"` // unix seconds
}

// shelfStatsFor is how long describe answers from the last measurement
// before it measures again.
var shelfStatsFor = 10 * time.Minute

func shelfStatsFile(stateDir string) string { return filepath.Join(shelfDir(stateDir), "stats") }

func saveShelfStats(stateDir string, s shelfStats) error {
	if err := os.MkdirAll(shelfDir(stateDir), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	tmp := shelfStatsFile(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, shelfStatsFile(stateDir))
}

func loadShelfStats(stateDir string) (shelfStats, bool) {
	var s shelfStats
	b, err := os.ReadFile(shelfStatsFile(stateDir))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, false
	}
	return s, true
}

// shelfDescribe is describe.shelf: where boxes find the shelf, its bytes and
// its files; no names. ok is false when the layer has no shelf or the volume
// does not exist. A measurement that fails, with none before it, says why
// instead of a count.
func (d *drv) shelfDescribe() (out map[string]any, ok bool) {
	sh, isShelfer := d.vm.(shelfer)
	if !isShelfer || !sh.HasShelf() {
		return nil, false
	}
	out = map[string]any{"path": ociCacheDir}
	s, have := loadShelfStats(d.cfg.StateDir)
	if !have || time.Since(time.Unix(s.At, 0)) > shelfStatsFor {
		fresh, err := sh.MeasureShelf(d.cfg.Image)
		switch {
		case err == nil:
			s, have = fresh, true
			_ = saveShelfStats(d.cfg.StateDir, s)
		case !have:
			fmt.Fprintln(os.Stderr, "potluck-driver-tart: measuring the shelf:", err) // its paths stay in the host's log
			out["why"] = "the shelf could not be measured"
			return out, true
		}
	}
	out["bytes"], out["files"] = s.Bytes, s.Files
	return out, true
}

// shelfWant is one validated want: a package a box or the owner asked to
// have on the shelf. Kind says which registry: shelfKindNpm or shelfKindGo.
//
// For npm, Name and Version are safe to put in a registry URL as they are
// (shelfNpmName, shelfSemver) and Hex is the tarball's sha512 in lowercase
// hex. For Go, Name is a module path and Version its canonical version, both
// checked by goModulePath and goVersion (shelf_go.go); GoMod says only the
// version's go.mod is wanted, as a go.sum line ending /go.mod says. Hex is
// empty for Go: the Go checksum database, not the want, vouches for the
// bytes.
type shelfWant struct {
	Kind, Name, Version, Hex string
	GoMod                    bool
}

// The kinds of want, as the first field of a wants line says.
const (
	shelfKindNpm = "npm"
	shelfKindGo  = "go"
)

// shelfWantMax bounds one wants line.
const shelfWantMax = 512

// shelfNpmName is an npm package name the registry can hold: an optional
// @scope/ and a base, each of URL-unreserved characters, not starting with
// a dot or an underscore. Upper case is allowed for the registry's old
// names. 214 characters at most (npm's own limit).
var shelfNpmName = regexp.MustCompile(`^(?:@[A-Za-z0-9~-][A-Za-z0-9._~-]*/)?[A-Za-z0-9~-][A-Za-z0-9._~-]*$`)

// shelfSemver is a semantic version (semver.org's own grammar).
var shelfSemver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
	`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// parseShelfWant reads one wants line (single spaces): either
//
//	npm <name> <version> sha512-<b64>    the hash in canonical padded base64 of 64 bytes
//	go <module> <version>[/go.mod]       as a go.sum line names them
//
// Anything else is not a want. This is the one validator for wants,
// wherever they come from (npmLockWants and goSumWants build their lines and
// pass them through it).
func parseShelfWant(line string) (shelfWant, bool) {
	if len(line) > shelfWantMax {
		return shelfWant{}, false
	}
	f := strings.Split(line, " ")
	switch {
	case len(f) == 3 && f[0] == shelfKindGo:
		return parseGoWant(f[1], f[2])
	case len(f) != 4 || f[0] != shelfKindNpm:
		return shelfWant{}, false
	}
	name, version, integrity := f[1], f[2], f[3]
	if len(name) > 214 || !shelfNpmName.MatchString(name) || len(version) > 256 || !shelfSemver.MatchString(version) {
		return shelfWant{}, false
	}
	hx := shelfSRIHex(integrity)
	if hx == "" {
		return shelfWant{}, false
	}
	return shelfWant{Kind: shelfKindNpm, Name: name, Version: version, Hex: hx}, true
}

// shelfSRIHex is the sha512 an integrity part `sha512-<b64>` names, in hex,
// or "" when it is not exactly one canonical sha512.
func shelfSRIHex(s string) string {
	b64, ok := strings.CutPrefix(s, "sha512-")
	if !ok || len(b64) != 88 {
		return ""
	}
	b, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil || len(b) != sha512.Size || base64.StdEncoding.EncodeToString(b) != b64 {
		return ""
	}
	return hex.EncodeToString(b)
}

// blob is the want's path in the volume (and in staging).
func (w shelfWant) blob() string {
	return shelfBlobDir + "/" + w.Hex[:2] + "/" + w.Hex[2:4] + "/" + w.Hex[4:]
}

// StorageKeyDefault is what a binding that leaves key out gets, for the oci
// storage keys potluck host sets (hostprobe's shelf and volume flags), so
// the one table of defaults stays here.
func StorageKeyDefault(key string) (string, bool) {
	switch key {
	case "shelf_max_gb":
		return "0", true
	case "shelf_keep_s":
		return strconv.Itoa(shelfKeepDefault), true
	case "shelf_fill":
		return shelfFillDefault, true
	case "volume_max_gb":
		return strconv.Itoa(volumeMaxGBDefault), true
	}
	return "", false
}
