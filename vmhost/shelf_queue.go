package vmhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Boxes' wants (shelf_fill=boxes). The in-box helper (shelfhelper) appends
// what a box's lockfile names to the box's own wants file. The host reads
// that file by exec inside the box, never from its own filesystem: on every
// layer that has a shelf the box's $HOME is the guest's, so the host's own
// $HOME/.potluck (the daemon's home on a machine running the local driver)
// is never read. Each line is checked with parseShelfWant and appended to a
// per-box file in the host's queue; a detached filler, one per state
// directory, drains the queue into shelfFill. Package names stay in the
// queue: no log line, answer or volume file holds one.
//
//	<stateDir>/shelf/queue/<record id>        validated wants lines of one box
//	<stateDir>/shelf/queue/<record id>.done   "<offset> <bytes>": how far the filler read, and what it fetched for the box
//	<stateDir>/shelf/queue.lock               held while a box's lines are appended or read
//	<stateDir>/shelf/fill.lock                held by the one filler (shelf/lock is the publisher's)

const (
	// shelfWantsFile is the wants file in the guest; the guest's shell
	// expands $HOME.
	shelfWantsFile = `$HOME/` + protocol.ShelfWants
	// shelfWantsReadMax and shelfWantsReadFor bound one read of it;
	// shelfWantsEndFor bounds the read as a box ends (release, expiry, a
	// run's end), which waits for it.
	shelfWantsReadMax = 4 << 20
	shelfWantsReadFor = 5 * time.Second
	shelfWantsEndFor  = 2 * time.Second
	// shelfFillSlice is the most of one box's lines a filler takes at a
	// time: boxes take turns, and a killed filler loses at most one slice.
	shelfFillSlice = 200
	// shelfLinkFor bounds the helper's link step after a checkout.
	shelfLinkFor = 30 * time.Second
	// shelfBoxLines is the most wants one box queues.
	shelfBoxLines = 20000
	// shelfDoneFor is how long a drained box's queue file stays after the box
	// ended, so a late read (the supervisor's, after the driver's release)
	// adds no line twice and no cap starts over.
	shelfDoneFor = 10 * time.Minute
	// shelfFillerFor bounds one filler's life; the next sweep starts another.
	shelfFillerFor = time.Hour
)

// shelfQueueMax bounds the whole queue's bytes on the host's disk: wants
// that would pass it are dropped (tests lower it).
var shelfQueueMax int64 = 64 << 20

// shelfBoxFetchMax is the most new tarball bytes one box's wants fetch
// (tests lower it).
var shelfBoxFetchMax int64 = 2 << 30

// shelfFillPoll is how often a filler waiting on live boxes looks again.
var shelfFillPoll = 5 * time.Second

// flagShelfFill re-executes the driver, detached, as a state directory's
// filler; the binding's config comes on stdin.
const flagShelfFill = "--shelf-fill"

func shelfQueueDir(stateDir string) string  { return filepath.Join(shelfDir(stateDir), "queue") }
func shelfQueueLock(stateDir string) string { return filepath.Join(shelfDir(stateDir), "queue.lock") }
func shelfFillLock(stateDir string) string  { return filepath.Join(shelfDir(stateDir), "fill.lock") }

// shelfFromBoxes says whether the owner lets boxes' wants fill the shelf:
// a shelf to fill (shelf_max_gb above 0, so vm=oci) and shelf_fill=boxes.
func (c config) shelfFromBoxes() bool { return c.Shelf.MaxGB > 0 && c.Shelf.Fill == "boxes" }

// shelfLink runs the in-box helper's link step in the checkout, as the
// box's user, when the layer has a shelf; it does nothing in a box without
// the shelf mounted. Its failure is the run log's, never the box's. It
// names no Go module (POTLUCK_DEPS_GO=off): it runs before the run's script,
// which may set GOPRIVATE itself, so it cannot know which modules are
// private. A run that calls the helper names its own.
func shelfLink(vm vmLayer, name, dir string, log io.Writer) {
	if _, ok := vm.(shelfer); !ok {
		return
	}
	const prog = `base=$2
` + baseInHome + `h=$1/` + shelfHelperPath + `; [ -x "$h" ] || exit 0; POTLUCK_DEPS_GO=off exec "$h" link "$base"`
	ctx, cancel := context.WithTimeout(context.Background(), shelfLinkFor)
	defer cancel()
	out := &capped{max: 4 << 10}
	if err := guestRun(ctx, vm, name, out, "sh", "-c", prog+" 2>&1", "sh", ociCacheDir, dir); err != nil {
		fmt.Fprintf(log, "potluck-driver-tart: shelf: linking failed (ignored): %v: %s\n", err, strings.TrimSpace(out.String()))
	}
}

// collectShelfWants reads the box's wants file by exec, for at most wait,
// and queues its valid lines, when the owner set shelf_fill=boxes. Only a
// regular file is read (a FIFO or a device is not), and at most
// shelfWantsReadMax of it. Counts go to log, never names.
func collectShelfWants(cfg config, vm vmLayer, r record, log io.Writer, wait time.Duration) {
	if !cfg.shelfFromBoxes() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	out := &capped{max: shelfWantsReadMax, stop: true}
	prog := fmt.Sprintf(`f="%s"; [ -f "$f" ] && exec head -c %d -- "$f" 2>/dev/null; exit 0`, shelfWantsFile, shelfWantsReadMax+1)
	err := guestRun(ctx, vm, r.VM, cutEnds{out, cancel}, "sh", "-c", prog)
	if err != nil && out.buf.Len() == 0 {
		return // the box is gone, or has no shell
	}
	_, dropped, err := queueShelfWants(cfg.StateDir, r.ID, out.buf.Bytes(), out.cut || err != nil)
	if err != nil {
		fmt.Fprintln(log, "potluck-driver-tart: shelf: queueing wants:", err)
		return
	}
	if dropped > 0 { // the box's own counts: nothing the whole queue's room shaped (not even whether a line was queued)
		fmt.Fprintf(log, "potluck-driver-tart: shelf: wants read; %d lines invalid or past the box's cap\n", dropped)
	}
}

// cutEnds ends the read (cancel) once its capped writer cut, so a box that
// writes past the cap is not waited for.
type cutEnds struct {
	*capped
	cancel func()
}

func (c cutEnds) Write(p []byte) (int, error) {
	n, err := c.capped.Write(p)
	if c.cut {
		c.cancel()
	}
	return n, err
}

// queueShelfWants appends the valid, new lines of raw to box id's queue
// file, up to shelfBoxLines for the box and shelfQueueMax for the queue.
// cut says raw may end in a partial line, which is dropped. It answers how
// many lines it queued and how many of the box's own it dropped (invalid,
// cut, past the box's cap); lines dropped because the queue is full are not
// counted, since that says how much other boxes asked for.
func queueShelfWants(stateDir, id string, raw []byte, cut bool) (queued, dropped int, err error) {
	lines := strings.Split(string(raw), "\n")
	if last := len(lines) - 1; lines[last] == "" {
		lines = lines[:last]
	} else if cut {
		lines, dropped = lines[:last], 1
	}
	var valid []string
	for _, l := range lines {
		if _, ok := parseShelfWant(l); ok {
			valid = append(valid, l)
		} else {
			dropped++
		}
	}
	if len(valid) == 0 {
		return 0, dropped, nil
	}
	if !idRe.MatchString(id) {
		return 0, dropped, fmt.Errorf("record id %q", id)
	}
	if err := os.MkdirAll(shelfQueueDir(stateDir), 0o700); err != nil {
		return 0, dropped, err
	}
	err = flocked(shelfQueueLock(stateDir), func() error {
		p := filepath.Join(shelfQueueDir(stateDir), id)
		had := map[string]bool{}
		if b, err := os.ReadFile(p); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if l != "" {
					had[l] = true
				}
			}
		}
		room := shelfQueueMax - shelfQueueBytes(stateDir)
		var add []byte
		for _, l := range valid {
			switch {
			case had[l]:
			case len(had) >= shelfBoxLines:
				dropped++
			case int64(len(add)+len(l)+1) > room:
			default:
				had[l] = true
				add = append(append(add, l...), '\n')
				queued++
			}
		}
		if len(add) == 0 {
			return nil
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(add)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
	return queued, dropped, err
}

// shelfQueueBytes is the queue's unread bytes, what shelfQueueMax bounds.
// Lines a filler read stay in a box's file (so a later read of the same box
// adds none twice) and do not count: a box kept stopped for weeks holds no
// room other boxes need. Each box file is at most shelfBoxLines lines.
func shelfQueueBytes(stateDir string) (n int64) {
	for _, q := range shelfQueue(stateDir) {
		n += max(q.size-q.offset, 0)
	}
	return n
}

// writeShelfDone records how far the filler read box file qp and what it
// fetched for the box, atomically: a torn write would start the box's caps
// over.
func writeShelfDone(qp string, offset, fetched int64) error {
	tmp := qp + ".done.tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d %d\n", offset, fetched)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, qp+".done")
}

// shelfQueued is one box's queue file as the filler sees it.
type shelfQueued struct {
	id      string
	size    int64     // the file's bytes
	offset  int64     // read up to here
	fetched int64     // new tarball bytes already fetched for this box
	mod     time.Time // its last append
}

func readShelfDone(p string) (offset, fetched int64) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, 0
	}
	offset, _ = strconv.ParseInt(f[0], 10, 64)
	fetched, _ = strconv.ParseInt(f[1], 10, 64)
	return offset, fetched
}

// shelfQueue lists the queue's box files, oldest append first (a pass
// takes one slice of each, so the order only says who goes first).
func shelfQueue(stateDir string) []shelfQueued {
	es, _ := os.ReadDir(shelfQueueDir(stateDir))
	var qs []shelfQueued
	for _, e := range es {
		if !idRe.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		q := shelfQueued{id: e.Name(), size: fi.Size(), mod: fi.ModTime()}
		q.offset, q.fetched = readShelfDone(filepath.Join(shelfQueueDir(stateDir), e.Name()+".done"))
		qs = append(qs, q)
	}
	slices.SortFunc(qs, func(a, b shelfQueued) int { return a.mod.Compare(b.mod) })
	return qs
}

// shelfPending says whether the queue has lines no filler has read.
func shelfPending(stateDir string) bool {
	for _, q := range shelfQueue(stateDir) {
		if q.offset < q.size {
			return true
		}
	}
	return false
}

// shelfFillPass fills what is ready in the queue once: for each box that has
// ended, or whose lines are shelf_fill_delay_s old, the next shelfFillSlice
// lines past its offset, under the box's remaining fetch allowance. The
// offset moves before the fetch, so a line is tried at most once. It removes
// the files of boxes long ended and fully read (a stopped persistent box's
// stay: its caps hold across starts). It stops when the queue is dropped
// (shelf_fill no longer boxes; a slice already fetching finishes, at most
// shelfFillSlice wants). waiting says whether lines remain that are
// not ready yet.
func shelfFillPass(ctx context.Context, cfg config, sh shelfer, log io.Writer) (waiting bool) {
	st := store{cfg.StateDir}
	delay := time.Duration(cfg.Shelf.FillDelayS) * time.Second
	for _, q := range shelfQueue(cfg.StateDir) {
		if ctx.Err() != nil {
			return false
		}
		if _, err := os.Stat(shelfQueueDir(cfg.StateDir)); err != nil {
			return false
		}
		r, rerr := st.load(q.id)
		live := rerr == nil && r.live()
		qp := filepath.Join(shelfQueueDir(cfg.StateDir), q.id)
		if q.offset >= q.size {
			if rerr != nil || !live && r.State != stStopped && time.Since(r.End) > shelfDoneFor {
				_ = flocked(shelfQueueLock(cfg.StateDir), func() error {
					if fi, err := os.Stat(qp); err == nil && fi.Size() <= q.offset {
						_ = os.Remove(qp)
						_ = os.Remove(qp + ".done")
						_ = os.Remove(qp + ".done.tmp")
					}
					return nil
				})
			}
			continue
		}
		if live && time.Since(q.mod) < delay {
			waiting = true
			continue
		}
		var chunk []byte
		_ = flocked(shelfQueueLock(cfg.StateDir), func() error {
			f, err := os.Open(qp)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := f.Seek(q.offset, io.SeekStart); err != nil {
				return err
			}
			chunk, err = io.ReadAll(io.LimitReader(f, shelfFillSlice*(shelfWantMax+1)))
			return err
		})
		end := 0 // past the slice's last whole line
		for n := 0; n < shelfFillSlice; n++ {
			i := bytes.IndexByte(chunk[end:], '\n')
			if i < 0 {
				break
			}
			end += i + 1
		}
		if end == 0 {
			continue
		}
		chunk = chunk[:end]
		offset := q.offset + int64(end)
		if writeShelfDone(qp, offset, q.fetched) != nil {
			continue
		}
		var wants []shelfWant
		sc := bufio.NewScanner(bytes.NewReader(chunk))
		sc.Buffer(make([]byte, shelfWantMax+1), shelfWantMax+1)
		for sc.Scan() {
			if w, ok := parseShelfWant(sc.Text()); ok { // checked when queued; checked again here
				wants = append(wants, w)
			}
		}
		res, err := shelfFillUpTo(ctx, cfg, sh, wants, shelfBoxFetchMax-q.fetched)
		fmt.Fprintln(log, "potluck-driver-tart:", res)
		if err != nil {
			fmt.Fprintln(log, "potluck-driver-tart: shelf fill:", err) // the shelf's paths, never a name
		} // a failed fill's lines are forgotten, like any dropped want
		_ = writeShelfDone(qp, offset, q.fetched+res.Bytes)
	}
	return waiting
}

// shelfFiller is a state directory's one filler: it drains the queue until
// nothing is left or waiting, or shelfFillerFor passed. A second one finds
// the lock held and exits at once.
func shelfFiller(stateDir string, stdin io.Reader, log io.Writer) int {
	var m map[string]string
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&m); err != nil {
		fmt.Fprintln(log, "potluck-driver-tart: shelf filler: the config on stdin:", err)
		return 2
	}
	cfg, err := parseConfig(m)
	if err != nil || cfg.StateDir != stateDir {
		fmt.Fprintln(log, "potluck-driver-tart: shelf filler: the config is not this state directory's:", err)
		return 2
	}
	sh, ok := newLayer(cfg).(shelfer)
	if !ok {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), shelfFillerFor)
	defer cancel()
	if err := os.MkdirAll(shelfDir(stateDir), 0o700); err != nil {
		fmt.Fprintln(log, "potluck-driver-tart: shelf filler:", err)
		return 1
	}
	err = tryFlocked(shelfFillLock(stateDir), func() error {
		if cfg.Shelf.MaxGB > 0 { // the keep time, though nothing was published (shelf_evict.go)
			logShelfEvict(cfg, sh, log)
		}
		for cfg.shelfFromBoxes() && ctx.Err() == nil {
			if !shelfFillPass(ctx, cfg, sh, log) && !shelfPending(stateDir) {
				return nil
			}
			select {
			case <-ctx.Done():
			case <-time.After(shelfFillPoll):
			}
		}
		return nil
	})
	if errors.Is(err, errLockHeld) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(log, "potluck-driver-tart: shelf filler:", err)
		return 1
	}
	return 0
}

// sweepShelfQueue is the sweep's part (lock held): with shelf_fill=boxes it
// starts the filler when the queue has unread lines and none runs; with any
// other setting it drops the queue, so nothing a box asked for is fetched.
// With any setting it starts the filler, which evicts as it starts, every
// shelfEvictEvery while shelf_max_gb is above 0.
func (d *drv) sweepShelfQueue() {
	boxes := d.cfg.shelfFromBoxes()
	if !boxes {
		if _, err := os.Stat(shelfQueueDir(d.cfg.StateDir)); err == nil {
			_ = flocked(shelfQueueLock(d.cfg.StateDir), func() error { return os.RemoveAll(shelfQueueDir(d.cfg.StateDir)) })
		}
	}
	evict := shelfEvictDue(d.cfg)
	if !evict && !(boxes && shelfPending(d.cfg.StateDir)) || flockHeld(shelfFillLock(d.cfg.StateDir)) {
		return
	}
	if evict {
		stampShelfEvict(d.cfg.StateDir)
	}
	d.spawnShelfFiller()
}

// spawnShelfFiller starts the filler detached, as spawnSupervisor starts a
// supervisor: the binding's config on stdin, output to logs/shelf-fill.log.
func (d *drv) spawnShelfFiller() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	logf, err := os.OpenFile(filepath.Join(d.st.dir, "logs", "shelf-fill.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer logf.Close()
	b, _ := json.Marshal(d.rawConfig())
	cmd := exec.Command(self, flagShelfFill, d.st.dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(b), logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// errLockHeld is tryFlocked's answer when another process holds the lock.
var errLockHeld = errors.New("held by another process")

// tryFlocked is flocked without waiting: errLockHeld when the lock is taken.
func tryFlocked(path string, f func() error) error {
	l, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errLockHeld
		}
		return err
	}
	defer syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
	return f()
}

// flockHeld says whether another process holds the lock at path.
func flockHeld(path string) bool {
	return errors.Is(tryFlocked(path, func() error { return nil }), errLockHeld)
}

// guestRun runs argv in the guest with stdout to out and stderr dropped,
// ended when ctx is done (and its output given up a second later, so a
// child that keeps the pipe open cannot hold the host).
func guestRun(ctx context.Context, vm vmLayer, name string, out io.Writer, argv ...string) error {
	cmd := vm.Exec(name, nil, out, io.Discard, argv...)
	cmd.WaitDelay = time.Second
	stop := context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer stop()
	if err := cmd.Run(); err != nil {
		return err
	}
	return ctx.Err()
}
