package hostprobe

// The record of the Tart images Potluck's shares used on this Mac. A digest
// in Tart's cache whose repository Potluck pins may still be the owner's own
// (the pinned repositories are public ones anyone pulls), so the record is
// the only proof that an image is Potluck's: `potluck host images --remove`
// deletes no image the record does not say a share of Potluck's pulled.
//
// It also says which images each Potluck home (a daemon's home: potluck's,
// or another program's built on Potluck, each with its own pins) needs: the
// ones its latest share uses, marked when that share starts, before any
// pull. A failed share's images are what its retry lends, and a share still
// running holds its images from its start; an image stays removable only
// while no home's latest share uses it, whatever this program's own pins
// say. The record lives in Tart's own directory (Report.TartDir), beside the
// cache it describes, so every program that shares this Tart writes and
// reads the same one; Tart reads only its vms and cache directories there.
// Writers take a lock on a file beside it.
//
// An image Potluck pulled before the record existed, or one Tart pulled by
// itself, is not in it, and so is kept unless the owner names it to
// `potluck host images --remove` (TartItem.Unclaimed). While any home the
// record names is unread, no image is removable (a share there before the
// record began may have used any of them). A home whose shares all ran on a
// build older than the record is not in it: its daemon's sizes are read
// while it runs, but a stopped one's are not, and its latest share's images
// are known only once it shares again.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
)

// TartPullsFile is the record's file name in Tart's directory.
const TartPullsFile = "potluck-pulls.json"

// TartUse is what the record says of one image.
type TartUse struct {
	// Pulled is when a share of Potluck's pulled it, Tart having read its
	// list and found it missing, and the pull having succeeded; zero when
	// Potluck found it already there (then it may be the owner's, and is
	// removed only when named: TartItem.Unclaimed), or when a list read since
	// (a share's, or a removal's) lacked it.
	Pulled time.Time `json:"pulled,omitzero"`
	// Homes are the Potluck homes whose shares used it, each with when the
	// last of them did (its start).
	Homes map[string]time.Time `json:"homes"`
}

// tartPullRecord is the file.
type tartPullRecord struct {
	Images map[string]TartUse `json:"images"`
	// Shares is when each home's latest share started: an image a home's
	// share used at or after that time is one its latest share uses.
	Shares map[string]time.Time `json:"shares"`
}

// readTartPulls reads the record in tartDir. No file is an empty record; an
// unreadable one is an error.
func readTartPulls(tartDir string) (tartPullRecord, error) {
	rec := tartPullRecord{Images: map[string]TartUse{}, Shares: map[string]time.Time{}}
	if tartDir == "" {
		return rec, errors.New("where Tart keeps its images is unknown")
	}
	b, err := os.ReadFile(filepath.Join(tartDir, TartPullsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, fmt.Errorf("%s: %v", filepath.Join(tartDir, TartPullsFile), err)
	}
	if rec.Images == nil {
		rec.Images = map[string]TartUse{}
	}
	if rec.Shares == nil {
		rec.Shares = map[string]time.Time{}
	}
	return rec, nil
}

// homeKey is the key a home goes under: as given, or the one already in m
// that names the same directory.
func homeKey[V any](m map[string]V, home string) string {
	for h := range m {
		if daemonrecord.SameHome(h, home) {
			return h
		}
	}
	return home
}

// StartTartShare notes in tartDir's record that a share on home starts at
// start and will use images: from now they are what home's latest share
// uses, and no image an earlier share there used is. It makes no
// directory: when tartDir does not exist it returns an error that matches
// fs.ErrNotExist (then nothing is recorded, and what Tart pulls into it is
// not taken for Potluck's).
func StartTartShare(tartDir, home string, images []string, start time.Time) error {
	if home == "" {
		return errors.New("no Potluck home to record")
	}
	return editTartPulls(tartDir, func(rec *tartPullRecord) error {
		start = start.UTC()
		rec.Shares[homeKey(rec.Shares, home)] = start
		for _, im := range images {
			u := rec.Images[im]
			if u.Homes == nil {
				u.Homes = map[string]time.Time{}
			}
			u.Homes[homeKey(u.Homes, home)] = start
			rec.Images[im] = u
		}
		return nil
	})
}

// RecordTartPull notes in tartDir's record that the share on home that
// started at start pulled image at now, Tart's list having been read and
// lacked it, and the pull having succeeded: the image is Potluck's, and
// that share uses it (StartTartShare's marks, made here too when the Tart
// directory did not exist yet at its start).
func RecordTartPull(tartDir, image, home string, start, now time.Time) error {
	if home == "" {
		return errors.New("no Potluck home to record")
	}
	return editTartPulls(tartDir, func(rec *tartPullRecord) error {
		start = start.UTC()
		if k := homeKey(rec.Shares, home); rec.Shares[k].Before(start) {
			rec.Shares[k] = start
		}
		u := rec.Images[image]
		u.Pulled = now.UTC()
		if u.Homes == nil {
			u.Homes = map[string]time.Time{}
		}
		if k := homeKey(u.Homes, home); u.Homes[k].Before(start) {
			u.Homes[k] = start
		}
		rec.Images[image] = u
		return nil
	})
}

// ForgottenHome is what ForgetTartHome dropped from the record.
type ForgottenHome struct {
	Home   string    `json:"home"`             // as the record named it
	Shared time.Time `json:"shared,omitzero"`  // when its latest share started
	Images []string  `json:"images,omitempty"` // the images whose mark of it was dropped
}

// ForgetTartHome drops home, a daemon home gone for good, from tartDir's
// record: its latest share and its mark on every image, so its being
// unread no longer keeps every image. It deletes no image, no VM and no
// file: an image only it marked keeps its claim (Pulled), and is judged
// again by what the homes still here need. It refuses, changing nothing,
// a home that is not an absolute path, one that exists or whose existence
// cannot be read, one under a directory that is missing too (an unmounted
// disk comes back: homeGone), one a daemon in running (their homes, read by
// the caller before this) runs on, and one the record does not name. The
// existence check and the edit hold the record's lock, as every share's
// write does.
func ForgetTartHome(tartDir, home string, running []string) (ForgottenHome, error) {
	if !filepath.IsAbs(home) {
		return ForgottenHome{}, fmt.Errorf("%q is not an absolute path: name the home as the record does", home)
	}
	home = filepath.Clean(home)
	for _, r := range running {
		if daemonrecord.SameHome(r, home) {
			return ForgottenHome{}, fmt.Errorf("a daemon runs on %s: it is not gone, so it is not forgotten", home)
		}
	}
	var out ForgottenHome
	err := editTartPulls(tartDir, func(rec *tartPullRecord) error {
		var keys []string
		for h := range rec.Shares {
			if filepath.Clean(h) == home {
				keys = append(keys, h)
			}
		}
		for _, u := range rec.Images {
			for h := range u.Homes {
				if filepath.Clean(h) == home && !slices.Contains(keys, h) {
					keys = append(keys, h)
				}
			}
		}
		if len(keys) == 0 {
			known := slices.Sorted(maps.Keys(rec.Shares))
			if len(known) == 0 {
				return fmt.Errorf("the record names no home, so %s is not in it", home)
			}
			return fmt.Errorf("the record does not name %s (it names %s)", home, strings.Join(known, ", "))
		}
		for _, k := range keys {
			if ok, why := homeGone(k); !ok {
				return errors.New(why)
			}
		}
		out.Home = keys[0]
		for _, k := range keys {
			if t, ok := rec.Shares[k]; ok && t.After(out.Shared) {
				out.Shared = t
			}
			delete(rec.Shares, k)
		}
		for _, im := range slices.Sorted(maps.Keys(rec.Images)) {
			u := rec.Images[im]
			n := len(u.Homes)
			for _, k := range keys {
				delete(u.Homes, k)
			}
			if len(u.Homes) < n {
				out.Images = append(out.Images, im)
				rec.Images[im] = u
			}
		}
		return nil
	})
	return out, err
}

// homeGone says whether home is gone in the way ForgetTartHome accepts: the
// name itself is missing (a link to nothing is still there) while the
// directory that held it is there, and it is not a volume's mount point.
// Otherwise why not, in plain words: a home under a disk that is not
// mounted is missing too, and comes back with the disk.
func homeGone(home string) (bool, string) {
	home = filepath.Clean(home)
	_, err := os.Lstat(home)
	switch {
	case err == nil:
		return false, home + " exists: a home that is still here is never forgotten (start its daemon instead, so what it shares is read)"
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Sprintf("whether %s exists cannot be read (%v): it is not forgotten", home, err)
	}
	parent := filepath.Dir(home)
	if filepath.Dir(parent) == parent || filepath.Clean(parent) == "/Volumes" {
		return false, home + " is where a disk mounts: it comes back with the disk, so it is not forgotten"
	}
	if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return false, fmt.Sprintf("%s is missing too (a disk that is not mounted?): %s may come back with it, so it is not forgotten", parent, home)
	}
	return true, ""
}

// ForgetHomeCommand is the command that forgets home, for a person to copy.
func ForgetHomeCommand(home string) string {
	q := home
	if strings.Trim(home, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") != "" {
		q = "'" + strings.ReplaceAll(home, "'", `'\''`) + "'"
	}
	return config.PotluckCommand("host images", "--forget-home "+q)
}

// lockTartPulls takes the record's lock in tartDir (it makes no directory),
// held until unlock: every write of the record holds it, and a removal holds
// it across its last read and its delete, so a share that starts meanwhile
// marks its images after the delete, and finds them gone.
func lockTartPulls(tartDir string) (unlock func(), err error) {
	if tartDir == "" {
		return nil, errors.New("where Tart keeps its images is unknown")
	}
	if st, err := os.Stat(tartDir); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", tartDir)
	}
	lock, err := os.OpenFile(filepath.Join(tartDir, "."+TartPullsFile+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, err
	}
	return func() { lock.Close() }, nil
}

// editTartPulls applies edit to the record under its lock and writes it;
// an error from edit writes nothing.
func editTartPulls(tartDir string, edit func(*tartPullRecord) error) error {
	unlock, err := lockTartPulls(tartDir)
	if err != nil {
		return err
	}
	defer unlock()
	rec, err := readTartPulls(tartDir)
	if err != nil {
		return err
	}
	if err := edit(&rec); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tartDir, "."+TartPullsFile+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(tartDir, TartPullsFile)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
