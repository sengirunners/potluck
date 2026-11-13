package tunnel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// nonceJournal remembers each member key's nonces so a signed request runs
// at most once, across restarts and clock corrections (SPEC 17.11).
//
//   - An entry lives until its request's own Zm-Date plus 2×MaxSkew plus a
//     minute. A request passes the date check only within MaxSkew of the
//     home's clock, so a home clock that steps back by up to MaxSkew after an
//     entry was dropped still refuses the request for its date.
//   - Every nonce is appended to the journal ($ZM_HOME/tunnel/nonces) before
//     its request runs; when that append fails the request is refused (503)
//     and the next one retries opening the file. The next start reloads the
//     live entries.
//   - If the journal cannot be read at start (missing while the key is not
//     new, unreadable, a read error), writes over the tunnel are refused for
//     2×MaxSkew plus a minute, by the monotonic clock: long enough for any
//     request signed before the start to fail its date, even across a clock
//     step of MaxSkew. A durable sidecar keeps that uncertainty across
//     restarts: each restart waits a full monotonic window before removing
//     it. Reads go on when the recovery state can be recorded.
//   - The journal is rewritten with only live entries once dead lines
//     outnumber live ones by compactSlack, off the request path: the live
//     set is copied under the lock; the big write and its fsync happen
//     without it; then, under it, the lines appended meanwhile are written
//     and fsynced, the file is renamed over the journal and appends move to
//     it (compact). A failed rewrite backs off.
//   - Appends are not fsynced (F_FULLFSYNC per request costs milliseconds): a
//     process crash keeps them, a power loss can lose the last ones, which
//     the authority's write-id records still cover for writes.
//
// Each key has its own budget (MaxNoncesPerKey), and only token callers
// reach it (anyone can make a key).
type nonceJournal struct {
	path string
	log  *slog.Logger
	// fresh: the daemon's key was made by this process, so no earlier run
	// can have accepted a request and a missing journal is expected.
	fresh func() bool

	mu           sync.Mutex
	keys         map[string]map[string]int64 // key -> nonce -> expiry (Unix seconds)
	live         int                         // entries in keys
	lines        int                         // lines in the journal file
	pruned       time.Time
	loaded       bool
	degradedTo   time.Time // writes refused until then (monotonic)
	holdRecorded bool      // the recovery sidecar was written and synced
	jf           *os.File
	compacting   bool
	pending      []byte // lines appended while a compaction runs
	wg           sync.WaitGroup

	retryAt     time.Time // after a failed compaction, not before this
	retryLines  int       // ... nor before the journal has this many lines
	compactions int       // done (tests)
}

const (
	nonceNew = iota
	nonceSeen
	nonceFull
	nonceNoJournal
)

// compactSlack: dead lines a journal may hold beyond its live ones before
// it is rewritten; compactBackoff: how long a failed rewrite waits.
var (
	compactSlack   = 4096
	compactBackoff = time.Minute
)

// beforeSwap, when set (tests), runs after a compaction's big write and
// before it takes the lock: appends made then must survive the swap.
var beforeSwap func()

// maxLine bounds one journal line when reading (a line is under 150 bytes).
const maxLine = 1 << 20

func keepFor() int64 { return int64((2*MaxSkew + time.Minute) / time.Second) }

func (c *nonceJournal) add(key, n string, date int64, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loadLocked(now)
	}
	if now.Sub(c.pruned) > time.Minute {
		c.pruneLocked(now) // before taking seen: it drops empty maps
	}
	seen := c.keys[key]
	if seen == nil {
		seen = map[string]int64{}
		c.keys[key] = seen
	}
	if len(seen) >= MaxNoncesPerKey {
		c.live -= prune(seen, now)
	}
	if e, ok := seen[n]; ok && e >= now.Unix() {
		return nonceSeen
	}
	if len(seen) >= MaxNoncesPerKey {
		return nonceFull
	}
	exp := date + keepFor()
	line := fmt.Appendf(nil, "%s %s %d\n", key, n, exp)
	if err := c.appendLocked(line); err != nil {
		c.log.Error("tunnel: the replay journal cannot be written; refusing tunnel requests until it can", "path", c.path, "err", err)
		return nonceNoJournal
	}
	if _, ok := seen[n]; !ok {
		c.live++
	}
	seen[n] = exp
	c.maybeCompactLocked()
	return nonceNew
}

// appendLocked writes one line, reopening the file first if an earlier
// write or open failed.
func (c *nonceJournal) appendLocked(line []byte) error {
	// A failed initial rewrite must not let this retry create a journal that
	// looks complete on restart while the earlier replay history is missing.
	if !c.degradedTo.IsZero() && !c.holdRecorded {
		if err := c.recordHoldLocked(); err != nil {
			return err
		}
	}
	if c.jf == nil {
		f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		c.jf = f
	}
	if _, err := c.jf.Write(line); err != nil {
		c.jf.Close()
		c.jf = nil
		return err
	}
	c.lines++
	if c.compacting {
		c.pending = append(c.pending, line...)
	}
	return nil
}

func prune(m map[string]int64, now time.Time) int {
	n := 0
	for k, e := range m {
		if e < now.Unix() {
			delete(m, k)
			n++
		}
	}
	return n
}

func (c *nonceJournal) pruneLocked(now time.Time) {
	for k, m := range c.keys {
		if c.live -= prune(m, now); len(m) == 0 {
			delete(c.keys, k)
		}
	}
	c.pruned = now
}

// load reads the journal at start (Service.Start), so the window of refused
// writes counts from there.
func (c *nonceJournal) load(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loadLocked(now)
	}
}

func (c *nonceJournal) loadLocked(now time.Time) {
	c.loaded = true
	c.keys = map[string]map[string]int64{}
	c.live, c.lines = 0, 0
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		c.degrade(err)
		return
	}
	if _, err := os.Stat(c.path + ".hold"); !errors.Is(err, fs.ErrNotExist) {
		// Presence, including an unreadable marker, means the earlier run
		// could not prove its replay history complete. Never trust a saved
		// wall-clock deadline: a clock correction must not shorten the hold.
		if err == nil {
			err = errors.New("an earlier replay recovery has not completed")
		}
		c.degrade(err)
	}
	f, err := os.Open(c.path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && c.fresh != nil && c.fresh():
	case err != nil:
		c.degrade(err)
	default:
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 4096), maxLine)
		for sc.Scan() {
			c.lines++
			var key, n string
			var exp int64
			if _, err := fmt.Sscanf(sc.Text(), "%s %s %d", &key, &n, &exp); err != nil || exp < now.Unix() {
				continue
			}
			if c.keys[key] == nil {
				c.keys[key] = map[string]int64{}
			}
			if _, ok := c.keys[key][n]; !ok {
				c.live++
			}
			c.keys[key][n] = exp
		}
		if err := sc.Err(); err != nil {
			c.degrade(err)
		}
		f.Close()
	}
	// Do not replace uncertain history until the recovery marker is durable.
	// Otherwise a successful later append could hide this hold on restart.
	if !c.degradedTo.IsZero() && !c.holdRecorded {
		return
	}
	// Start from a clean file holding only the live entries.
	if err := writeSynced(c.path, c.snapshotLocked()); err != nil {
		c.degrade(err)
		return
	}
	c.lines = c.live
	if c.jf != nil {
		c.jf.Close()
	}
	c.jf = nil
}

// degrade refuses tunnel writes from now for as long as an entry is kept
// (2×MaxSkew plus a minute): any request signed before this start has
// expired by then, even across a clock step of MaxSkew.
func (c *nonceJournal) degrade(err error) {
	c.degradedTo = time.Now().Add(time.Duration(keepFor()) * time.Second)
	if err := c.recordHoldLocked(); err != nil {
		c.log.Error("tunnel: recording the replay recovery hold", "path", c.path, "err", err)
	}
	c.log.Error("tunnel: the replay journal could not be read; refusing tunnel writes for a few minutes (reads go on)",
		"path", c.path, "err", err, "until", c.degradedTo.Format(time.TimeOnly))
}

// recordHoldLocked makes uncertainty durable before the journal can be
// rewritten or reopened for appends. Compaction only replaces c.path, so
// it cannot erase this marker. Its contents deliberately carry no deadline.
func (c *nonceJournal) recordHoldLocked() error {
	if c.holdRecorded {
		return nil
	}
	if err := writeSynced(c.path+".hold", []byte("replay recovery pending\n")); err != nil {
		return err
	}
	c.holdRecorded = true
	return nil
}

// degraded reports whether writes are refused now (monotonic clock).
func (c *nonceJournal) degraded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loadLocked(nowFunc())
	}
	if c.degradedTo.IsZero() {
		return false
	}
	if time.Now().Before(c.degradedTo) {
		return true
	}
	// This process waited the whole window. Only now can a later restart
	// safely rely on the rebuilt journal, without a saved wall-clock time.
	if err := os.Remove(c.path + ".hold"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.log.Error("tunnel: clearing the replay recovery hold", "path", c.path, "err", err)
		return true
	}
	if err := syncDir(filepath.Dir(c.path)); err != nil {
		c.log.Error("tunnel: syncing the cleared replay recovery hold", "path", c.path, "err", err)
		return true
	}
	c.degradedTo, c.holdRecorded = time.Time{}, false
	return false
}

func (c *nonceJournal) snapshotLocked() []byte {
	var b bytes.Buffer
	for key, m := range c.keys {
		for n, e := range m {
			fmt.Fprintf(&b, "%s %s %d\n", key, n, e)
		}
	}
	return b.Bytes()
}

func (c *nonceJournal) maybeCompactLocked() {
	if c.compacting || c.lines-c.live < c.live+compactSlack {
		return
	}
	if !c.retryAt.IsZero() && (time.Now().Before(c.retryAt) || c.lines < c.retryLines) {
		return // a compaction failed: not again until a minute and compactSlack more lines
	}
	c.compacting = true
	snap := c.snapshotLocked()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.compact(snap)
	}()
}

// compact writes snap to a temp file and fsyncs it without the lock;
// then, under the lock, it adds the lines appended meanwhile (fsynced
// again), renames the file over the journal, and moves appends to it.
// Once the rename has happened the old file is gone, so the append handle
// is always moved, whatever follows; a failed directory sync is only
// logged. A failure before the rename leaves the journal as it was and
// backs off (compactBackoff).
func (c *nonceJournal) compact(snap []byte) {
	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err == nil {
		_, err = f.Write(snap)
	}
	if err == nil {
		err = f.Sync()
	}
	if beforeSwap != nil {
		beforeSwap()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.compacting, c.pending = false, nil }()
	if err == nil && len(c.pending) > 0 {
		if _, err = f.Write(c.pending); err == nil {
			err = f.Sync()
		}
	}
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = os.Rename(tmp, c.path)
	}
	if err != nil {
		os.Remove(tmp)
		c.log.Warn("tunnel: compacting the replay journal (will retry later)", "err", err)
		c.retryAt, c.retryLines = time.Now().Add(compactBackoff), c.lines+compactSlack
		return
	}
	if c.jf != nil {
		c.jf.Close()
	}
	c.jf = nil
	if nf, err := os.OpenFile(c.path, os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		c.jf = nf
	} // else the next append opens it, or refuses its request
	c.lines = bytes.Count(snap, []byte("\n")) + bytes.Count(c.pending, []byte("\n"))
	c.compactions++
	c.retryAt, c.retryLines = time.Time{}, 0 // an old failure leaves no raised threshold
	if err := syncDir(filepath.Dir(c.path)); err != nil {
		c.log.Warn("tunnel: syncing the replay journal's directory", "err", err)
	}
}

// writeSynced replaces path with b: a temp file, fsynced, renamed, and the
// directory fsynced, so a crash leaves the old file or the new one.
func writeSynced(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (c *nonceJournal) close() {
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jf != nil {
		c.jf.Close()
		c.jf = nil
	}
}
