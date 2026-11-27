package swap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tree is dir's files by name.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func fill(t *testing.T, files map[string]string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	for n, s := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A name that cannot be copied changes nothing, the .prev files neither.
func TestPlaceRefusedChangesNothing(t *testing.T) {
	src, was := fill(t, map[string]string{"a": "new a"}), map[string]string{"a": "old a", "a.prev": "older a"}
	dst := fill(t, was)
	if _, err := Place(src, dst, []string{"a", "missing"}); err == nil {
		t.Fatal("placed a name that is not there")
	}
	if got := tree(t, dst); !maps.Equal(got, was) {
		t.Fatalf("a refused Place left %v", got)
	}
}

var placed = []string{"new-driver", "same", "driver", "potluck"}

var (
	oldTree = map[string]string{"same": "same", "driver": "A driver", "potluck": "A"}
	oldPrev = map[string]string{"same.prev": "Z same", "driver.prev": "Z driver", "potluck.prev": "Z"}
	newTree = map[string]string{"new-driver": "B new", "same": "same", "driver": "B driver", "potluck": "B"}
)

// placeKilledAt runs TestPlaceKilled's process, which kills itself at its
// at-th change of dst; false: it ended before that many.
func placeKilledAt(t *testing.T, src, dst, then string, at int) bool {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestPlaceKilled$")
	c.Env = append(os.Environ(), "SWAP_TEST_KILL_AT="+strconv.Itoa(at), "SWAP_TEST_SRC="+src, "SWAP_TEST_DST="+dst, "SWAP_TEST_THEN="+then)
	out, err := c.CombinedOutput()
	if err == nil {
		return false
	}
	if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("%s, kill %d: %v\n%s", then, at, err, out)
	}
	return true
}

// TestPlaceKilled really kills a process at every change it makes of the
// directory, in the placing, the finish and the restore: potluck is there
// new only with all the rest, and the next Place ends it, with the files
// before it to go back to, never a mix.
func TestPlaceKilled(t *testing.T) {
	if at, err := strconv.Atoi(os.Getenv("SWAP_TEST_KILL_AT")); err == nil {
		n := 0
		step = func() {
			if n++; n == at {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
		p, err := Begin(t.Context(), os.Getenv("SWAP_TEST_SRC"), os.Getenv("SWAP_TEST_DST"), placed, func() {})
		if err == nil && os.Getenv("SWAP_TEST_THEN") == "restore" {
			err = p.Restore()
		} else if err == nil {
			err = p.Finish()
		}
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	src := fill(t, newTree)
	for _, then := range []string{"finish", "restore"} {
		killed := 0
		for at := 1; ; at++ {
			was := maps.Clone(oldTree)
			maps.Copy(was, oldPrev)
			dst := fill(t, was)
			if !placeKilledAt(t, src, dst, then, at) {
				break
			}
			killed++
			got := tree(t, dst)
			mixed := false
			for n, s := range newTree {
				mixed = mixed || got[n] != s
			}
			if got["potluck"] == "B" && mixed && then == "finish" {
				t.Errorf("kill %d: potluck is there without the rest: %v", at, got)
			}
			if same := maps.Equal(got, newTree) || func() bool {
				for n, s := range oldTree {
					if got[n] != s {
						return false
					}
				}
				return got["new-driver"] == ""
			}(); !same && !Placing(dst) && mixed {
				t.Errorf("%s, kill %d: a mix that does not say so: %v", then, at, got)
			}
			again := Placing(dst) || mixed
			p, err := Begin(t.Context(), src, dst, placed, func() {})
			if err != nil {
				t.Fatal(err)
			}
			if got := tree(t, dst); !again && Placing(dst) {
				t.Errorf("nothing to place: %v", got)
			}
			for n, s := range newTree {
				if got := tree(t, dst)[n]; got != s {
					t.Errorf("%s, kill %d, then Begin: %s is %q", then, at, n, got)
				}
			}
			if !again {
				continue // it had finished
			}
			if err := p.Restore(); err != nil {
				t.Fatal(err)
			}
			if got := tree(t, dst); !maps.Equal(got, oldTree) {
				t.Errorf("%s, kill %d, then Begin and Restore: %v", then, at, got)
			}
			if _, err := Place(src, dst, placed); err != nil {
				t.Fatal(err)
			}
			want := maps.Clone(newTree)
			for n, s := range oldTree {
				want[n+PrevSuffix] = s
			}
			if got := tree(t, dst); !maps.Equal(got, want) {
				t.Errorf("%s, kill %d, then Place: %v", then, at, got)
			}
		}
		if killed < 10 {
			t.Errorf("%s: %d kills: too few changes were made", then, killed)
		}
	}
}

// A third build over a second one cut off goes back to the first, and the
// same build again keeps what it replaced.
func TestPlaceOverACutOffPlace(t *testing.T) {
	dst := fill(t, oldTree)
	if cut, err := Begin(t.Context(), fill(t, newTree), dst, placed, func() {}); err != nil {
		t.Fatal(err)
	} else {
		cut.unlock() // cut off after it
	}
	third := map[string]string{"potluck": "C", "extra": "C extra"} // it ships no driver
	p, err := Begin(t.Context(), fill(t, third), dst, slices.Sorted(maps.Keys(third)), func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, dst); !maps.Equal(got, oldTree) {
		t.Errorf("went back to %v", got)
	}
	// Cut off in the middle, some files are still the second's: no one else's.
	dst = fill(t, oldTree)
	for _, b := range []map[string]string{newTree, {"new-driver": "C", "same": "C", "driver": "C", "potluck": "C"}} {
		if cut, err := Begin(t.Context(), fill(t, b), dst, placed, func() {}); err != nil {
			t.Fatal(err)
		} else {
			cut.unlock()
		}
	}
	for _, n := range placed[2:] {
		if err := os.WriteFile(filepath.Join(dst, n), []byte(newTree[n]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if p, err = Begin(t.Context(), fill(t, third), dst, slices.Sorted(maps.Keys(third)), func() {}); err != nil {
		t.Fatal(err)
	}
	if err := p.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, dst); !maps.Equal(got, oldTree) {
		t.Errorf("from a mix of two, went back to %v", got)
	}
	dst = fill(t, oldTree)
	src := fill(t, newTree)
	for range 2 {
		if _, err := Place(src, dst, placed); err != nil {
			t.Fatal(err)
		}
	}
	if got := tree(t, dst); got["potluck.prev"] != "A" {
		t.Errorf("the same build again: potluck.prev is %q", got["potluck.prev"])
	}
}

// A file of the same bytes that nobody can run, or a link to one, is placed
// again.
func TestPlaceRepairs(t *testing.T) {
	src := fill(t, map[string]string{"a": "a", "b": "b", "c": "c"})
	dst := fill(t, map[string]string{"a": "a", "b": "b"})
	if err := os.Chmod(filepath.Join(dst, "a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "c"), filepath.Join(dst, "c")); err != nil {
		t.Fatal(err)
	}
	if _, err := Place(src, dst, []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if fi, err := os.Lstat(filepath.Join(dst, n)); err != nil || fi.Mode() != 0o755 {
			t.Errorf("%s: %v %v", n, fi.Mode(), err)
		}
	}
}

// Files put there some other way after a cut-off Place are what the next one
// replaces: it goes back to them, and never takes them away.
func TestPlaceOverOthersFiles(t *testing.T) {
	mine := map[string]string{"new-driver": "MINE new", "same": "MINE same", "driver": "MINE driver", "potluck": "MINE"}
	for name, c := range map[string]struct {
		before, since map[string]string
		want          map[string]string // of mine and what the cut-off Place left
	}{
		"a first install cut off, then all of it":   {nil, mine, mine},
		"an upgrade cut off, then all of it":        {oldTree, mine, mine},
		"a first install cut off, then the program": {nil, map[string]string{"potluck": "MINE"}, map[string]string{"new-driver": "B new", "same": "same", "driver": "B driver", "potluck": "MINE"}},
		"an upgrade cut off, then one driver":       {oldTree, map[string]string{"driver": "MINE driver"}, map[string]string{"same": "same", "driver": "MINE driver", "potluck": "A"}},
	} {
		dst := fill(t, c.before)
		for _, b := range []map[string]string{newTree, {"new-driver": "B new", "same": "same", "driver": "B driver", "potluck": "B"}} { // cut off, and again
			p, err := Begin(t.Context(), fill(t, b), dst, placed, func() {})
			if err != nil {
				t.Fatal(err)
			}
			p.unlock()
		}
		for n, s := range c.since {
			at := filepath.Join(dst, n)
			if err := os.Remove(at); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(at, []byte(s), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 { // the one that goes back, and one after it
			if err := Settle(dst, "potluck"); err != nil {
				t.Fatal(err)
			}
			if c.since["potluck"] != "" && Placing(dst) {
				t.Errorf("%s: it runs, and is not settled", name)
			}
			d, err := Begin(t.Context(), fill(t, map[string]string{"new-driver": "D", "same": "D", "driver": "D", "potluck": "D"}), dst, placed, func() {})
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Restore(); err != nil {
				t.Fatal(err)
			}
			if got := tree(t, dst); !maps.Equal(got, c.want) {
				t.Errorf("%s: went back to %v", name, got)
			}
		}
	}
}

// The only build there was, run after another's Place was cut off over it, is
// what the next one goes back to; one that runs from a file Settle cannot read
// blocks nothing.
func TestSettleTheOnlyBuild(t *testing.T) {
	src := fill(t, map[string]string{"new-driver": "C", "same": "C", "driver": "C", "potluck": "C"})
	for at := 1; ; at++ {
		dst := t.TempDir()
		b, err := Begin(t.Context(), fill(t, newTree), dst, placed, func() {})
		if err != nil {
			t.Fatal(err)
		}
		b.unlock() // cut off after it
		if !placeKilledAt(t, src, dst, "finish", at) {
			break
		}
		want := tree(t, dst)["potluck"]
		if err := os.Chmod(filepath.Join(dst, "potluck"), 0o111); err != nil {
			t.Fatal(err)
		}
		if err := Settle(dst, "potluck"); err != nil || Placing(dst) {
			t.Fatalf("killed at %d: %v, placing %t", at, err, Placing(dst))
		}
		if err := os.Chmod(filepath.Join(dst, "potluck"), 0o755); err != nil {
			t.Fatal(err)
		}
		p, err := Begin(t.Context(), fill(t, map[string]string{"new-driver": "D", "same": "D", "driver": "D", "potluck": "D"}), dst, placed, func() {})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Restore(); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, dst)["potluck"]; got != want {
			t.Errorf("killed at %d: went back to %q, not %q", at, got, want)
		}
	}
}

// A build put there by hand after a first install of it was cut off, and run,
// is what the next Place goes back to, whatever its mode, and whatever else
// of that install is there.
func TestSettleOverTheSameBuildPutByHand(t *testing.T) {
	d := map[string]string{"new-driver": "D", "same": "D", "driver": "D", "potluck": "D"}
	for at := 1; ; at++ {
		dst := t.TempDir()
		if !placeKilledAt(t, fill(t, newTree), dst, "finish", at) {
			break
		}
		if !Placing(dst) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dst, "hand"), []byte("B"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dst, "hand"), filepath.Join(dst, "potluck")); err != nil {
			t.Fatal(err)
		}
		want := tree(t, dst)
		maps.DeleteFunc(want, func(n, _ string) bool { return n[0] == '.' })
		if err := Settle(dst, "potluck"); err != nil {
			t.Fatal(err)
		}
		p, err := Begin(t.Context(), fill(t, d), dst, placed, func() {})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Restore(); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, dst); !maps.Equal(got, want) {
			t.Errorf("killed at %d: went back to %v, not %v", at, got, want)
		}
	}
}

// Files put in some other way stay the ones gone back to when the Place that
// found that is killed at any step.
func TestPlaceOverOthersFilesKilled(t *testing.T) {
	mine := map[string]string{"new-driver": "B new", "same": "MINE same", "driver": "MINE driver", "potluck": "MINE"}
	src := fill(t, map[string]string{"new-driver": "D", "same": "D", "driver": "D", "potluck": "D"})
	for at := 1; ; at++ {
		dst := fill(t, oldTree)
		p, err := Begin(t.Context(), fill(t, newTree), dst, placed, func() {})
		if err != nil {
			t.Fatal(err)
		}
		p.unlock()
		for n, s := range mine {
			if err := os.Remove(filepath.Join(dst, n)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, n), []byte(s), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if !placeKilledAt(t, src, dst, "finish", at) {
			break
		}
		if !Placing(dst) {
			continue
		}
		e, err := Begin(t.Context(), src, dst, placed, func() {})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Restore(); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, dst); !maps.Equal(got, mine) {
			t.Errorf("killed at %d: went back to %v", at, got)
		}
	}
}

// One that goes on is never settled by another.
func TestSettleLeavesAPlaceThatGoesOn(t *testing.T) {
	dst := t.TempDir()
	p, err := Begin(t.Context(), fill(t, newTree), dst, placed, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := Settle(dst, "potluck"); err != nil || !Placing(dst) {
		t.Errorf("%v, placing %t", err, Placing(dst))
	}
	p.unlock()
	if err := Settle(dst, "potluck"); err != nil || Placing(dst) {
		t.Errorf("cut off: %v, placing %t", err, Placing(dst))
	}
}

// What a cut-off Place put there and a daemon then ran is what the next one
// replaces, though nothing was there before the first; so is what one over
// it put there.
func TestPlaceOverASettledPlace(t *testing.T) {
	second := map[string]string{"new-driver": "C new", "same": "same", "driver": "C driver", "potluck": "C"}
	for _, builds := range [][]map[string]string{{newTree}, {newTree, second}, {oldTree, newTree}, {oldTree, newTree, second}} {
		dst := t.TempDir()
		if maps.Equal(builds[0], oldTree) {
			dst = fill(t, oldTree)
		}
		for _, b := range builds {
			if maps.Equal(b, oldTree) {
				continue
			}
			p, err := Begin(t.Context(), fill(t, b), dst, placed, func() {})
			if err != nil {
				t.Fatal(err)
			}
			p.unlock() // cut off after it
		}
		if err := Settle(dst, "potluck"); err != nil {
			t.Fatal(err)
		}
		d, err := Begin(t.Context(), fill(t, map[string]string{"potluck": "D"}), dst, []string{"potluck"}, func() {})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Restore(); err != nil {
			t.Fatal(err)
		}
		got := tree(t, dst)
		maps.DeleteFunc(got, func(n, _ string) bool { return strings.HasSuffix(n, PrevSuffix) })
		if !maps.Equal(got, builds[len(builds)-1]) {
			t.Errorf("went back to %v", got)
		}
	}
}

// A Place cut off before its last file put no build there to go back to:
// settling it keeps what it replaced.
func TestSettleHalfPlaced(t *testing.T) {
	src, half := fill(t, newTree), 0
	for at := 1; ; at++ {
		dst := fill(t, oldTree)
		if !placeKilledAt(t, src, dst, "finish", at) {
			break
		}
		got := tree(t, dst)
		if !Placing(dst) || got["potluck"] == "B" {
			continue
		}
		half++
		if err := Settle(dst, "potluck"); err != nil {
			t.Fatal(err)
		}
		p, err := Begin(t.Context(), src, dst, placed, func() {})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Restore(); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, dst); !maps.Equal(got, oldTree) {
			t.Errorf("killed at %d, settled: went back to %v", at, got)
		}
	}
	if half < 3 {
		t.Errorf("%d half placed", half)
	}
}

// A third build half placed over a second one that runs and is not settled:
// the first is still what there is to go back to.
func TestSettleHalfPlacedOverAnother(t *testing.T) {
	third, half := map[string]string{"new-driver": "C new", "same": "C same", "driver": "C driver", "potluck": "C"}, 0
	src := fill(t, third)
	for at := 1; ; at++ {
		dst := fill(t, oldTree)
		if b, err := Begin(t.Context(), fill(t, newTree), dst, placed, func() {}); err != nil {
			t.Fatal(err)
		} else {
			b.unlock() // cut off after it
		}
		if !placeKilledAt(t, src, dst, "finish", at) {
			break
		}
		got := tree(t, dst)
		if got["potluck"] == "C" || got["new-driver"] != "C new" {
			continue
		}
		half++
		if err := Settle(dst, "potluck"); err != nil {
			t.Fatal(err)
		}
		p, err := Begin(t.Context(), src, dst, placed, func() {})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Restore(); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, dst); !maps.Equal(got, oldTree) {
			t.Errorf("killed at %d, settled: went back to %v", at, got)
		}
	}
	if half < 3 {
		t.Errorf("%d half placed", half)
	}
}

// A Place that fails over one cut off part way leaves no mix: all of it is as
// it was before that one.
func TestPlaceFailsOverACutOffPlace(t *testing.T) {
	src, cut := fill(t, newTree), 0
	for at := 1; ; at++ {
		dst := fill(t, oldTree)
		if !placeKilledAt(t, src, dst, "finish", at) {
			break
		}
		if got := tree(t, dst); !Placing(dst) || got["potluck"] == "B" {
			continue
		}
		cut++
		n := 0
		step = func() { // every copy is made: the last one's rename fails
			if n++; n > len(placed) {
				_ = os.Remove(filepath.Join(dst, stagedName("potluck")))
			}
		}
		_, err := Begin(t.Context(), src, dst, placed, func() {})
		step = func() {}
		if err == nil || errors.Is(err, ErrNotRestored) {
			t.Fatal(err)
		}
		if got := tree(t, dst); !maps.Equal(got, oldTree) {
			t.Errorf("killed at %d: went back to %v", at, got)
		}
	}
	if cut < 3 {
		t.Errorf("%d cut off", cut)
	}
}

// Waiting for another's turn ends when the one who waits is ended.
func TestPlaceWaitEnds(t *testing.T) {
	dst := fill(t, oldTree)
	first, err := Begin(t.Context(), fill(t, newTree), dst, placed, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer first.unlock()
	ctx, cancel := context.WithCancel(t.Context())
	_, err = Begin(ctx, fill(t, map[string]string{"potluck": "C"}), dst, []string{"potluck"}, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

// Two at once in a directory take turns, to the end of each.
func TestPlacesTakeTurns(t *testing.T) {
	dst := fill(t, oldTree)
	first, err := Begin(t.Context(), fill(t, newTree), dst, placed, func() {})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error)
	go func() {
		_, err := Place(fill(t, map[string]string{"potluck": "C"}), dst, []string{"potluck"})
		ended <- err
	}()
	select {
	case <-ended:
		t.Fatal("the second went ahead in the middle of the first")
	case <-time.After(200 * time.Millisecond):
	}
	if err := first.Restore(); err != nil {
		t.Fatal(err)
	}
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	if got := tree(t, dst); got["potluck"] != "C" || got["potluck.prev"] != "A" {
		t.Errorf("%v", got)
	}
}

// What a Place cut off leaves beside the programs is known.
func TestLeftovers(t *testing.T) {
	dst := fill(t, map[string]string{".a.new": "", ".placing": "", "a": "", ".mine.new": ""})
	if got := Leftovers(dst, []string{"a"}); len(got) != 2 {
		t.Errorf("%v", got)
	}
}
