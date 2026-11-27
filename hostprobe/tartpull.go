package hostprobe

// Potluck's own pulls of Tart images, so that one that stopped partway (the
// app quit, the Mac restarted, the network went) goes on where it stopped.
//
// Tart keeps an unfinished download in the tmp folder of its home, under a
// name made from the image's, and the next pull of that image resumes it.
// But every other Tart command, even `tart list`, first deletes what is in
// that folder and no process locks, and Potluck, its driver and the person
// all run Tart. So Potluck pulls in a Tart home of its own inside Tart's
// directory, where nothing but a pull runs: its tmp is its own, and its
// cache is a link to Tart's, where the finished image goes.
//
// Only images named by digest are pulled (Check): for a tag Tart would also
// write a link with the home's own path in it, which breaks when it goes.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// TartPullHome is the name of that home in Tart's directory.
const TartPullHome = "potluck-pull"

// readyTartPull makes the pull home in Tart's directory dir and is the command
// that does plain, a "tart pull", there: plain itself when the home could not
// be made (why; such a pull works, and starts over when it stops) or dir is
// not known. In the home Tart deletes no image to make room: it counts a
// resumed download as all still to come, and would take the person's own
// images for room the download already has. Potluck's disk check asked for
// the room.
func readyTartPull(dir string, plain []string) (ready []string, why error) {
	if dir == "" {
		return plain, nil
	}
	home := filepath.Join(dir, TartPullHome)
	err := errors.Join(os.MkdirAll(filepath.Join(dir, "cache"), 0o755), os.MkdirAll(home, 0o755))
	if err == nil {
		if err = os.Symlink(cacheLink, filepath.Join(home, "cache")); errors.Is(err, fs.ErrExist) {
			err = nil
		}
	}
	if err == nil {
		err = checkPullHome(dir)
	}
	if err != nil {
		return plain, err
	}
	return append([]string{"/usr/bin/env", "TART_HOME=" + home, "TART_NO_AUTO_PRUNE=1"}, plain...), nil
}

// cacheLink is what the pull home's cache links to: Tart's.
var cacheLink = filepath.Join("..", "cache")

// checkPullHome refuses a pull home that is not as Potluck makes it: a link
// in the place of it or its tmp leads to files Potluck must not delete, and
// with another cache an image pulled there is not Tart's. What is not there
// yet is fine.
func checkPullHome(dir string) error {
	for _, p := range []string{filepath.Join(dir, TartPullHome), tartPartials(dir)} {
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", p)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	link := filepath.Join(dir, TartPullHome, "cache")
	if to, err := os.Readlink(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is not a link to Tart's cache", link)
	} else if err == nil && to != cacheLink {
		return fmt.Errorf("%s leads to %s", link, to)
	}
	return nil
}

// tartPartials is the pull home's unfinished downloads.
func tartPartials(dir string) string { return filepath.Join(dir, TartPullHome, "tmp") }

// partialName is the name Tart gives an image's unfinished download
// (VMDirectory.temporaryDeterministic).
func partialName(image string) string {
	sum := md5.Sum([]byte(image))
	return hex.EncodeToString(sum[:])
}

// partials is what the pull home holds of unfinished downloads nothing wants:
// neither of images (what is still to pull) nor of an image another home than
// home uses.
type partials struct {
	dead []string // no pull works on these
	busy int      // how many a pull does
	rec  tartPullRecord
	held []*os.File
}

// release lets go of the dead ones.
func (p partials) release() {
	for _, f := range p.held {
		f.Close()
	}
}

// unwantedPartials reads them. Each dead one is locked as a pull locks it, so
// that none starts on it, until release. With the record unread none is dead.
func unwantedPartials(dir, home string, images []string) (p partials, err error) {
	if err := checkPullHome(dir); err != nil {
		return p, err
	}
	ents, err := os.ReadDir(tartPartials(dir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return p, err
	}
	if p.rec, err = readTartPulls(dir); err != nil {
		return p, err
	}
	p.rec.forget(home)
	wanted := map[string]bool{}
	for _, im := range images {
		wanted[partialName(im)] = true
	}
	for im, u := range p.rec.Images {
		for h, at := range u.Homes {
			if _, err := os.Lstat(h); errors.Is(err, fs.ErrNotExist) {
				continue // gone: no setup on it goes on
			}
			if !at.Before(p.rec.Shares[homeKey(p.rec.Shares, h)]) {
				wanted[partialName(im)] = true
			}
		}
	}
	for _, e := range ents {
		path := filepath.Join(tartPartials(dir), e.Name())
		if wanted[e.Name()] {
			continue
		}
		if !e.IsDir() { // no download
			p.dead = append(p.dead, path)
			continue
		}
		f, err := lockPartial(path)
		switch {
		case errors.Is(err, syscall.EWOULDBLOCK):
			p.busy++
			continue
		case errors.Is(err, fs.ErrNotExist): // a pull that just ended
			continue
		case err != nil:
			p.release()
			return partials{}, err
		}
		p.held, p.dead = append(p.held, f), append(p.dead, path)
	}
	return p, nil
}

// lockPartial takes the lock a pull holds on its download, or fails at once
// when one does.
func lockPartial(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// dropDeadPartials deletes them, under the record's lock: another home's
// share marks its images before it pulls.
func dropDeadPartials(dir, home string, images []string) error {
	if _, err := os.Lstat(tartPartials(dir)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	unlock, err := lockTartPulls(dir)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := unwantedPartials(dir, home, images)
	if err != nil {
		return err
	}
	defer p.release()
	return p.drop()
}

// drop deletes the dead ones.
func (p partials) drop() error {
	var errs []error
	for _, path := range p.dead {
		errs = append(errs, os.RemoveAll(path))
	}
	return errors.Join(errs...)
}

// partialNeedGB is the free disk, in GB, the pull of image still needs: its
// budget less what its unfinished download already takes.
func partialNeedGB(dir, image string, budget int) int {
	if checkPullHome(dir) != nil { // a plain pull, which starts over
		return budget
	}
	return max(0, budget-int(diskTaken(filepath.Join(tartPartials(dir), partialName(image)))/gib))
}

// deadPartialsGB is the disk, in GB, the dead partials take, which the next
// pull frees first.
func deadPartialsGB(dir, home string, images []string) int {
	if home == "" { // a share on no home deletes none
		return 0
	}
	p, err := unwantedPartials(dir, home, images)
	if err != nil {
		return 0
	}
	defer p.release()
	var b int64
	for _, path := range p.dead {
		b += diskTaken(path)
	}
	return int(b / gib)
}

const gib = 1 << 30 // what TartFreeGB counts in

// diskTaken is the disk the files in path, a directory, take (a download's
// are sparse), in bytes; tests stand in for it.
var diskTaken = func(path string) int64 {
	var b int64
	_ = filepath.WalkDir(path, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := e.Info(); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && fi.Mode().IsRegular() {
				b += st.Blocks * 512
			}
		}
		return nil
	})
	return b
}

// partialWait is how long DropPartials waits for a pull that is ending; tests
// shorten it.
var partialWait = 5 * time.Second

// PullHome is Potluck's pull home in Tart's directory, there or not.
func (l TartLeave) PullHome() string { return filepath.Join(l.Dir, TartPullHome) }

// HasPullHome reports that it is there.
func (l TartLeave) HasPullHome() bool {
	_, err := os.Lstat(l.PullHome())
	return l.Dir != "" && err == nil
}

// DropPartials deletes the pull home's unfinished downloads that no other
// home's share wants, and with no other home in the record the pull home
// itself: a Mac Potluck leaves will not go on with them. A pull still going
// on is an error, and its download stays. It never touches what is in Tart's
// cache or its own tmp.
func (l TartLeave) DropPartials(ctx context.Context) error {
	if !l.HasPullHome() {
		return nil
	}
	for end := time.Now().Add(partialWait); ; {
		stays, err := l.dropPartials()
		if err != nil || !stays {
			return err
		}
		if time.Now().After(end) {
			return errors.New("a download there still goes on")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dropPartials is one try; stays is that a pull holds one it would delete.
func (l TartLeave) dropPartials() (stays bool, err error) {
	unlock, err := lockTartPulls(l.Dir)
	if err != nil {
		return false, err
	}
	defer unlock()
	p, err := unwantedPartials(l.Dir, l.Home, nil)
	if err != nil {
		return false, err
	}
	defer p.release()
	if err := p.drop(); err != nil || p.busy > 0 {
		return p.busy > 0, err
	}
	if len(p.rec.homes()) > 0 {
		return false, nil
	}
	return false, l.dropPullHome()
}

// dropPullHome deletes the pull home with what a pull makes in it: its tmp,
// its link to Tart's cache, never the cache, and empty directories. Anything
// else there stays, and so does the home.
func (l TartLeave) dropPullHome() error {
	if err := os.RemoveAll(tartPartials(l.Dir)); err != nil {
		return err
	}
	ents, err := os.ReadDir(l.PullHome())
	for _, e := range ents {
		if e.IsDir() || (e.Name() == "cache" && e.Type() == fs.ModeSymlink) {
			_ = os.Remove(filepath.Join(l.PullHome(), e.Name())) // a directory only when empty
		}
	}
	_ = os.Remove(l.PullHome())
	return err
}

// UnfinishedPulls is the downloads in the pull home that a pull works on, or
// that stopped partway: the next setup that needs one goes on with it.
type UnfinishedPulls struct {
	Dir string `json:"dir"`
	GB  int    `json:"gb"` // the disk they take, rounded up
}

// readUnfinishedPulls is those in Tart's directory dir (nil: none).
func readUnfinishedPulls(dir string) *UnfinishedPulls {
	if dir == "" || checkPullHome(dir) != nil {
		return nil
	}
	at := tartPartials(dir)
	b := diskTaken(at)
	if b == 0 {
		return nil
	}
	return &UnfinishedPulls{Dir: at, GB: int((b + gib - 1) / gib)}
}
