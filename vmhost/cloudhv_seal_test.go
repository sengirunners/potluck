package vmhost

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// sealFixture is a snapshot in the clear: a small file, an empty one, and a
// memory file that is mostly hole, with data in three places (one of them
// 8 MiB of zeros that were written) and a marker in it.
func sealFixture(t *testing.T) (src string, files map[string][]byte) {
	t.Helper()
	src = filepath.Join(t.TempDir(), "clear")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	mem := make([]byte, 64<<20)
	put := func(off int, b []byte) { copy(mem[off:], b) }
	rnd := make([]byte, 3<<19)
	_, _ = rand.Read(rnd)
	put(3<<20+17, rnd)
	put(40<<20+7, []byte("PLANTED-SECRET-in-the-guest's-memory"))
	files = map[string][]byte{"config.json": []byte(`{"cpus":2}`), "empty": {}, "memory-ranges": mem}
	for name, b := range files {
		f, err := os.Create(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "memory-ranges" { // sparse, as a hypervisor writes it
			if err := f.Truncate(int64(len(b))); err != nil {
				t.Fatal(err)
			}
			for _, r := range [][2]int{{3 << 20, 5 << 20}, {10 << 20, 18 << 20}, {40 << 20, 40<<20 + 4096}} {
				if _, err := f.WriteAt(b[r[0]:r[1]], int64(r[0])); err != nil {
					t.Fatal(err)
				}
			}
		} else if _, err := f.Write(b); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	return src, files
}

func sealKey(t *testing.T) []byte {
	t.Helper()
	k, err := cloudhvSealKey()
	if err != nil || len(k) != 32 {
		t.Fatalf("key: %d bytes, %v", len(k), err)
	}
	return k
}

// unsealed unseals the file into a new directory and answers what it holds.
func unsealed(t *testing.T, sealed string, key []byte) (map[string][]byte, error) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "back")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cloudhvUnseal(sealed, dst, key); err != nil {
		return nil, err
	}
	got := map[string][]byte{}
	ents, _ := os.ReadDir(dst)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dst, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got[e.Name()] = b
	}
	return got, nil
}

// A sealed snapshot unseals to the same files, byte for byte, holes
// included; it holds nothing of them in the clear; and it is about as long
// as the data in it, not as the memory file.
func TestCloudHVSealRoundTrip(t *testing.T) {
	src, files := sealFixture(t)
	key := sealKey(t)
	sealed := filepath.Join(t.TempDir(), "sealed")
	n, err := cloudhvSeal(src, sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
	// The data is 2 MiB and a page, sealed in chunks of up to 1 MiB that
	// start where the filesystem says data does: the written zeros (8 MiB)
	// and the holes are not kept.
	if least, most := int64(len(`{"cpus":2}`)+2<<20+4096), int64(4<<20); n < least || n > most {
		t.Errorf("%d bytes sealed, want %d to %d", n, least, most)
	}
	if int64(len(b)) < n || len(b) > 4<<20+1<<16 {
		t.Errorf("the sealed file is %d bytes for %d bytes of data", len(b), n)
	}
	for _, clear := range []string{"PLANTED-SECRET", `"cpus"`, "memory-ranges", "config.json"} {
		if bytes.Contains(b, []byte(clear)) {
			t.Errorf("the sealed file holds %q in the clear", clear)
		}
	}
	if fi, _ := os.Stat(sealed); fi.Mode().Perm() != 0o600 {
		t.Errorf("the sealed file's mode is %v", fi.Mode().Perm())
	}
	got, err := unsealed(t, sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(files) {
		t.Fatalf("%d files back, want %d", len(got), len(files))
	}
	for name, want := range files {
		if !bytes.Equal(got[name], want) {
			t.Errorf("%s came back different (%d bytes, want %d)", name, len(got[name]), len(want))
		}
	}
	if _, err := cloudhvSeal(src, sealed, key); err == nil {
		t.Error("a second sealing wrote over the first")
	}
	// A snapshot with no file in it is no snapshot: what the hypervisor
	// wrote is gone, and a nap of it could never be woken.
	if _, err := cloudhvSeal(t.TempDir(), filepath.Join(t.TempDir(), "sealed"), key); err == nil {
		t.Error("an empty snapshot was sealed")
	}
	// Two sealings of one snapshot share no key, so no bytes.
	again := filepath.Join(t.TempDir(), "sealed")
	if _, err := cloudhvSeal(src, again, sealKey(t)); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(again); bytes.Equal(b2[len(cloudhvSealMagic)+4:len(cloudhvSealMagic)+40], b[len(cloudhvSealMagic)+4:len(cloudhvSealMagic)+40]) {
		t.Error("two naps' sealed files begin with the same ciphertext")
	}
}

// sealFrames reads a sealed file's frames, each with its length prefix.
func sealFrames(t *testing.T, b []byte) (frames [][]byte) {
	t.Helper()
	for b = b[len(cloudhvSealMagic):]; len(b) > 0; {
		n := 4 + int(binary.BigEndian.Uint32(b))
		frames = append(frames, b[:n])
		b = b[n:]
	}
	return frames
}

// Anything but the whole sealed file, under its key, does not unseal.
func TestCloudHVSealedSnapshotThatWasChangedDoesNotUnseal(t *testing.T) {
	src, _ := sealFixture(t)
	key := sealKey(t)
	sealed := filepath.Join(t.TempDir(), "sealed")
	if _, err := cloudhvSeal(src, sealed, key); err != nil {
		t.Fatal(err)
	}
	whole, _ := os.ReadFile(sealed)
	fr := sealFrames(t, whole)
	if len(fr) < 7 {
		t.Fatalf("%d frames", len(fr))
	}
	join := func(frames ...[]byte) []byte { return append([]byte(cloudhvSealMagic), bytes.Join(frames, nil)...) }
	flip := func(at int) []byte { b := bytes.Clone(whole); b[at] ^= 1; return b }
	cases := map[string][]byte{
		"empty":                        {},
		"the magic alone":              []byte(cloudhvSealMagic),
		"a changed magic":              flip(3),
		"a changed length":             flip(len(cloudhvSealMagic) + 3),
		"a changed byte of a name":     flip(len(cloudhvSealMagic) + 4 + 12),
		"a changed byte of data":       flip(len(whole) / 2),
		"a changed last byte":          flip(len(whole) - 1),
		"cut in a frame":               whole[:len(whole)/2],
		"cut before the end frame":     join(fr[:len(fr)-1]...),
		"a byte after the end":         append(bytes.Clone(whole), 0),
		"the end frame twice":          join(append(slicesClone(fr), fr[len(fr)-1])...),
		"a frame left out":             join(append(slicesClone(fr[:2]), fr[3:]...)...),
		"two frames the other way":     join(append([][]byte{fr[0], fr[2], fr[1]}, fr[3:]...)...),
		"a frame twice":                join(append([][]byte{fr[0], fr[1], fr[1]}, fr[2:]...)...),
		"a length longer than a frame": append(append([]byte(cloudhvSealMagic), 0x7f, 0xff, 0xff, 0xff), whole[len(cloudhvSealMagic)+4:]...),
	}
	for name, b := range cases {
		p := filepath.Join(t.TempDir(), "sealed")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := unsealed(t, p, key); err == nil || errors.As(err, new(cloudhvNoMem)) {
			t.Errorf("%s: %v, want it refused as a sealed file that is not whole", name, err)
		}
	}
	if _, err := unsealed(t, sealed, sealKey(t)); err == nil || !strings.Contains(err.Error(), "another key") || errors.As(err, new(cloudhvNoMem)) {
		t.Errorf("under another key: %v", err)
	}
	if _, err := unsealed(t, sealed, key); err != nil {
		t.Errorf("the whole file under its key: %v", err)
	}
}

func slicesClone(f [][]byte) [][]byte { return append([][]byte(nil), f...) }

// Frames sealed under the right key that say something no sealing writes
// are refused too, and none of them writes outside the directory.
func TestCloudHVUnsealRefusesWhatNoSealingWrites(t *testing.T) {
	key := sealKey(t)
	aead, err := cloudhvSealAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	file := func(size uint64, name string) []byte {
		return append(binary.BigEndian.AppendUint64([]byte{'F'}, size), name...)
	}
	data := func(off uint64, b string) []byte {
		return append(binary.BigEndian.AppendUint64([]byte{'D'}, off), b...)
	}
	end := []byte{'E'}
	cases := map[string][][]byte{
		"a name with a directory":  {file(1, "../out"), end},
		"a name below":             {file(1, "a/b"), end},
		"the directory itself":     {file(1, "."), end},
		"the directory above":      {file(1, ".."), end},
		"no name":                  {file(1, ""), end},
		"a name with a zero byte":  {file(1, "a\x00b"), end},
		"a size past any file":     {file(1<<63, "a"), end},
		"data before any file":     {data(0, "x"), end},
		"data past the file's end": {file(4, "a"), data(2, "xyz"), end},
		"data at a place past it":  {file(4, "a"), data(5, ""), end},
		"data at a place far past": {file(4, "a"), data(1<<63+5, "x"), end},
		"a file named twice":       {file(1, "a"), file(1, "a"), end},
		"an end that says more":    {file(1, "a"), {'E', 0}},
		"a kind no sealing writes": {file(1, "a"), {'X'}, end},
		"a frame with nothing":     {file(1, "a"), {}, end},
		"a file frame cut short":   {{'F', 0, 0}, end},
		"a data frame cut short":   {file(1, "a"), {'D', 0}, end},
		"no end":                   {file(1, "a")},
	}
	for name, frames := range cases {
		var b bytes.Buffer
		b.WriteString(cloudhvSealMagic)
		for i, plain := range frames {
			ct := aead.Seal(nil, cloudhvSealNonce(uint64(i)), plain, []byte(cloudhvSealMagic))
			_ = binary.Write(&b, binary.BigEndian, uint32(len(ct)))
			b.Write(ct)
		}
		root := t.TempDir()
		dst, p := filepath.Join(root, "in", "dst"), filepath.Join(root, "sealed")
		if err := os.MkdirAll(dst, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		// Refused as a sealed file that is not whole, which loses its VM:
		// never as a host whose memory took nothing, which would be asked
		// again for good.
		if err := cloudhvUnseal(p, dst, key); err == nil || errors.As(err, new(cloudhvNoMem)) {
			t.Errorf("%s: %v, want it refused as a sealed file that is not whole", name, err)
		}
		if ents, _ := os.ReadDir(filepath.Join(root, "in")); len(ents) != 1 {
			t.Errorf("%s: wrote beside the directory: %v", name, ents)
		}
	}
}

// A snapshot that holds anything but regular files is not sealed.
func TestCloudHVSealTakesRegularFilesOnly(t *testing.T) {
	for name, plant := range map[string]func(dir string) error{
		"a link":      func(d string) error { return os.Symlink("/etc/hostname", filepath.Join(d, "link")) },
		"a directory": func(d string) error { return os.Mkdir(filepath.Join(d, "sub"), 0o700) },
	} {
		src, _ := sealFixture(t)
		if err := plant(src); err != nil {
			t.Fatal(err)
		}
		if _, err := cloudhvSeal(src, filepath.Join(t.TempDir(), "sealed"), sealKey(t)); err == nil {
			t.Errorf("a snapshot with %s was sealed", name)
		}
	}
}

// The VM's directory in the host's memory is this user's alone, one per VM,
// and is refused when it is not.
func TestCloudHVMemDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv(envCloudHVMem, root)
	a, err := cloudhvMemDir("/vms/a")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := cloudhvMemDir("/vms/a/")
	b, _ := cloudhvMemDir("/vms/b")
	if a != a2 || a == b || filepath.Dir(a) != filepath.Dir(b) || !strings.HasPrefix(a, root+string(filepath.Separator)) {
		t.Fatalf("directories %q, %q, %q", a, a2, b)
	}
	base := filepath.Dir(a)
	if fi, err := os.Lstat(base); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the user's directory: %v, %v", fi, err)
	}
	// A VM's directory is one VM however it is named: through a link, and
	// as "." from inside it, which is how its monitor names it.
	vm, link := filepath.Join(t.TempDir(), "vm"), filepath.Join(t.TempDir(), "link")
	if err := os.Mkdir(vm, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(vm, link); err != nil {
		t.Fatal(err)
	}
	byPath, _ := cloudhvMemDir(vm)
	byLink, _ := cloudhvMemDir(link)
	t.Chdir(link)
	fromInside, _ := cloudhvMemDir(".")
	if byPath == "" || byPath != byLink || byPath != fromInside {
		t.Errorf("one VM's directories: %q by its path, %q through a link, %q from inside", byPath, byLink, fromInside)
	}
	if _, err := os.Stat(a); err == nil {
		t.Error("the VM's directory was made by asking where it is")
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudhvMemDir("/vms/a"); err == nil {
		t.Error("a directory others can read was taken")
	}
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // a directory of this user's alone: only its being a link is wrong
	if err := os.Chmod(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, base); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudhvMemDir("/vms/a"); err == nil {
		t.Error("a link in the directory's place was taken")
	}
	t.Setenv(envCloudHVMem, filepath.Join(root, "gone"))
	if _, err := cloudhvMemDir("/vms/a"); err == nil {
		t.Error("a host with no memory filesystem gave a directory")
	}
	if err := cloudhvMemClear("/vms/a"); err != nil {
		t.Errorf("clearing where there is nothing: %v", err)
	}
	// Clearing makes nothing: a host whose VMs never napped has no
	// directory of the layer's in its memory.
	fresh := t.TempDir()
	t.Setenv(envCloudHVMem, fresh)
	if err := cloudhvMemClear("/vms/a"); err != nil {
		t.Errorf("clearing on a host that never napped: %v", err)
	}
	if ents, _ := os.ReadDir(fresh); len(ents) != 0 {
		t.Errorf("clearing made %v", ents)
	}
}

// holds says whether any file under root holds the bytes.
func holds(t *testing.T, root string, what string) (where []string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		b, _ := io.ReadAll(bufio.NewReader(f))
		if bytes.Contains(b, []byte(what)) {
			where = append(where, p)
		}
		return nil
	})
	return where
}

// A nap leaves the guest's memory on the host sealed only: no file of the
// state directory or of the host's memory holds it in the clear, the VM's
// directory in the host's memory is gone, and the wake brings the same guest
// back and leaves nothing behind either.
func TestCloudHVANapSavesNoMemoryInTheClear(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.boot("vm1", false)
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	c.until("the idle VM naps", func() bool { return c.state("vm1").State == "napping" })
	if where := holds(t, c.l.state, mem); len(where) != 0 {
		t.Fatalf("the napping guest's memory is in the clear in %v", where)
	}
	memDir, err := cloudhvMemDir(c.l.dir("vm1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(memDir); err == nil {
		t.Error("the snapshot in the clear is still in the host's memory")
	}
	if ents, _ := os.ReadDir(filepath.Join(c.l.dir("vm1"), "snap")); len(ents) != 1 || ents[0].Name() != cloudhvSealFile {
		t.Errorf("the saved nap holds %v, want the sealed file alone", ents)
	}
	sealedNap, err := os.ReadFile(filepath.Join(c.l.dir("vm1"), "snap", cloudhvSealFile))
	if err != nil {
		t.Fatal(err)
	}
	if e := c.events("vm1", "nap"); len(e) != 1 || e[0]["seal_ms"] == nil {
		t.Errorf("nap events %v", e)
	}
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Fatalf("after the wake the guest's memory is %q, was %q", got, mem)
	}
	if _, err := os.Stat(memDir); err == nil || c.has("vm1", "snap") {
		t.Errorf("after the wake: the snapshot in the clear is there: %v; the sealed one: %v", err == nil, c.has("vm1", "snap"))
	}
	if e := c.events("vm1", "wake"); len(e) != 1 || e[0]["unseal_ms"] == nil {
		t.Errorf("wake events %v", e)
	}
	// The next nap of the same memory is sealed under another key: the two
	// files share no frame.
	first := sealedNap
	c.until("the VM naps again", func() bool { return c.state("vm1").State == "napping" })
	second, err := os.ReadFile(filepath.Join(c.l.dir("vm1"), "snap", cloudhvSealFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("two naps of one memory: %d and %d bytes", len(first), len(second))
	}
	for i, f := range sealFrames(t, first) {
		if bytes.Equal(f, sealFrames(t, second)[i]) {
			t.Errorf("frame %d of two naps is the same: they were sealed under one key", i)
		}
	}
}

// lockedBuffer is a monitor's log a test reads while the monitor writes it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// On a host with no memory filesystem a VM does not nap: its memory would
// have nowhere to go but a disk. It stays awake and whole, and the hypervisor
// is never asked for a snapshot.
func TestCloudHVNoMemoryFilesystemNoNap(t *testing.T) {
	c := cloudhvFixture(t, 1)
	t.Setenv(envCloudHVMem, filepath.Join(c.l.state, "no-such-memory"))
	if err := c.l.Clone("img", "vm1", 2, 512); err != nil {
		t.Fatal(err)
	}
	var log lockedBuffer
	cmd, held, err := c.l.Boot("vm1", true, &log)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() { c.l.Destroy("vm1", cmd.Process.Pid); held.Close(); <-done }()
	c.until("the guest answers", func() bool { return c.l.Exec("vm1", nil, io.Discard, io.Discard, "true").Run() == nil })
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	c.until("the monitor has given up napping", func() bool {
		return strings.Count(log.String(), "nowhere to be saved but a disk") >= cloudhvNapRetries
	})
	time.Sleep(1500 * time.Millisecond)
	if st := c.state("vm1"); st.State != "awake" || st.Naps != 0 || c.has("vm1", "snap") || c.has("vm1", "snap.tmp") || c.has("vm1", "snapshots") {
		t.Fatalf("%+v; snap %v, snap.tmp %v, snapshots asked for %v", st, c.has("vm1", "snap"), c.has("vm1", "snap.tmp"), c.has("vm1", "snapshots"))
	}
	if c.has("vm1", "pauses") || c.has("vm1", "squeezed") {
		t.Errorf("the VM was paused (%v) or its guest squeezed (%v) for a nap that cannot be", c.has("vm1", "pauses"), c.has("vm1", "squeezed"))
	}
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Fatalf("the guest's memory is %q, was %q", got, mem)
	}
	if where := holds(t, c.l.state, mem); len(where) != 0 {
		t.Fatalf("the guest's memory is in the clear in %v", where)
	}
}

// A nap whose sealed memory was changed is not woken: the command is told,
// the VM ends, and nothing of the snapshot is left in the host's memory.
func TestCloudHVAChangedSealedNapIsNotWoken(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.boot("vm1", false)
	c.until("the idle VM naps", func() bool { return c.state("vm1").State == "napping" })
	p := filepath.Join(c.l.dir("vm1"), "snap", cloudhvSealFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-3] ^= 1
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	memDir, _ := cloudhvMemDir(c.l.dir("vm1"))
	if code, out, said := c.ask("vm1"); code != 255 || out != "" || !strings.Contains(said, "could not be read back") {
		t.Fatalf("a command for it: exit %d, printed %q, said %q", code, out, said)
	}
	c.until("the lost VM is removed", func() bool { return !c.has("vm1", "") })
	if _, err := os.Stat(memDir); err == nil {
		t.Error("the VM's directory in the host's memory is still there")
	}
}

// What a monitor that was killed in the middle of a nap or a wake left in
// the host's memory goes when the VM is stopped or destroyed.
func TestCloudHVAStoppedVMLeavesNothingInTheHostsMemory(t *testing.T) {
	c := cloudhvFixture(t, 0)
	plant := func(vm string) string {
		t.Helper()
		memDir, err := cloudhvMemDir(c.l.dir(vm))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(memDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(memDir, "memory-ranges"), []byte("the guest's memory"), 0o600); err != nil {
			t.Fatal(err)
		}
		return memDir
	}
	kept, _ := c.boot("kept", true)
	gone, _ := c.boot("gone", false)
	keptMem, goneMem := plant("kept"), plant("gone")
	c.l.PowerOff("kept", kept.Process.Pid)
	if _, err := os.Stat(keptMem); err == nil {
		t.Error("a stopped VM's snapshot is still in the host's memory")
	}
	if !c.has("kept", "disk.qcow2") {
		t.Error("the stop took the kept VM's disk")
	}
	c.l.Destroy("gone", gone.Process.Pid)
	if _, err := os.Stat(goneMem); err == nil {
		t.Error("a destroyed VM's snapshot is still in the host's memory")
	}
	// A monitor that was killed clears nothing: the destroy does.
	killed, _ := c.boot("killed", false)
	if err := syscall.Kill(c.state("killed").PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	c.until("the killed monitor is gone", func() bool { return !cloudhvMonAlive(c.l.dir("killed")) })
	killedMem := plant("killed")
	c.l.Destroy("killed", killed.Process.Pid)
	if _, err := os.Stat(killedMem); err == nil {
		t.Error("the snapshot of a destroyed VM whose monitor was killed is still in the host's memory")
	}
}

// A wake the host's memory has no room for leaves the VM napping, whole and
// without the slot it took: its command is refused as on a full host, with no
// path of the host, and the next command, once there is room, runs in the
// same guest.
func TestCloudHVAWakeWithNoHostMemoryNapsOn(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	c.until("the idle VM naps, its slot given up", c.gaveUp("vm1"))
	memDir, err := cloudhvMemDir(c.l.dir("vm1"))
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(memDir)
	if err := os.Chmod(base, 0o500); err != nil { // the host's memory takes nothing of this user's
		t.Fatal(err)
	}
	defer os.Chmod(base, 0o700)
	code, out, said := c.ask("vm1")
	if code != 255 || out != "" || !strings.Contains(said, cloudhvNapsOn) || !strings.Contains(said, "did not take its saved memory") || strings.Contains(said, base) {
		t.Fatalf("a command for it: exit %d, printed %q, said %q", code, out, said)
	}
	// The slot it took for the wake is given back: it holds none.
	if st := c.state("vm1"); st.State != "napping" || st.Waking || !c.gaveUp("vm1")() || !c.has("vm1", "snap/"+cloudhvSealFile) {
		t.Fatalf("%+v; sealed nap there %v", st, c.has("vm1", "snap/"+cloudhvSealFile))
	}
	if e := c.events("vm1", "wake_refused"); len(e) != 1 {
		t.Errorf("wake_refused events %v", e)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Fatalf("after the wake the guest's memory is %q, was %q", got, mem)
	}
}

// A sealed file that is whole and cannot be written into the host's memory
// is the memory's fault, said apart from a sealed file that is not whole: a
// wake of the first naps on, a wake of the second loses the VM.
func TestCloudHVUnsealSaysWhenTheMemoryTookNothing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into any directory")
	}
	src, _ := sealFixture(t)
	key := sealKey(t)
	sealed := filepath.Join(t.TempDir(), "sealed")
	if _, err := cloudhvSeal(src, sealed, key); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "full")
	if err := os.Mkdir(dst, 0o500); err != nil { // nothing can be made in it
		t.Fatal(err)
	}
	if err := cloudhvUnseal(sealed, dst, key); !errors.As(err, new(cloudhvNoMem)) {
		t.Errorf("a directory that takes no file: %v, want it said that the memory took nothing", err)
	}
	if err := cloudhvUnseal(filepath.Join(t.TempDir(), "gone"), t.TempDir(), key); err == nil || errors.As(err, new(cloudhvNoMem)) {
		t.Errorf("no sealed file: %v, want an error that is not the memory's", err)
	}
}

// Whose a file is: the layer takes a directory in the host's memory only
// when it is this user's.
func TestCloudHVOwnedByUs(t *testing.T) {
	mine, err := os.Stat(t.TempDir())
	if err != nil || !ownedByUs(mine) {
		t.Errorf("a directory this test made is not this user's: %v", err)
	}
	if os.Getuid() == 0 {
		t.Skip("root owns the root directory")
	}
	if root, err := os.Stat("/"); err != nil || ownedByUs(root) || !ownedByRoot(root) {
		t.Errorf("the root directory is taken for this user's, or not for root's: %v", err)
	}
	if ownedByRoot(mine) {
		t.Error("a directory this test made is taken for root's")
	}
}

// The directory in the host's memory is taken only where no other user can
// put another in its place: in a root that is sticky, or that only this user
// can write to. And only when it is this user's own.
func TestCloudHVMemDirIsThisUsersWhereNoOtherCanSwapIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv(envCloudHVMem, root)
	for _, mode := range []os.FileMode{0o777, 0o770, 0o757} { // every user, its group, the others
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if d, err := cloudhvMemDir("/vms/a"); err == nil {
			t.Errorf("a root other users can rename in (mode %v) gave %s", mode, d)
		}
		if ents, _ := os.ReadDir(root); len(ents) != 0 {
			t.Errorf("a root that was refused (mode %v) had %v made in it", mode, ents)
		}
	}
	if err := os.Chmod(root, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	// Whose the root and the directory in it are, as the test says: the
	// user's own directory is the one named for it, the root every other.
	const other = 54321 // a user that is neither this one nor root
	mine := filepath.Join(root, "potluck-cloudhv-"+strconv.Itoa(os.Getuid()))
	rootOf, mineOf := os.Getuid(), os.Getuid()
	owner := cloudhvOwner
	defer func() { cloudhvOwner = owner }()
	cloudhvOwner = func(fi os.FileInfo) (int, bool) {
		if fi.Name() == filepath.Base(mine) {
			return mineOf, true
		}
		return rootOf, true
	}
	taken := func() error {
		_, err := cloudhvMemDir("/vms/a")
		return err
	}
	// A sticky root of root's, as /dev/shm is, and one of this user's.
	for _, rootOf = range []int{0, os.Getuid()} {
		if err := taken(); err != nil {
			t.Fatalf("a sticky root of user %d: %v", rootOf, err)
		}
	}
	// The directory itself, made there by another user before this one.
	rootOf, mineOf = 0, other
	if err := taken(); err == nil {
		t.Error("a directory another user made in this user's place was taken")
	}
	// And a root of another user's, who can rename in it, sticky or closed
	// to all others as it may be.
	rootOf, mineOf = other, os.Getuid()
	if err := os.Remove(mine); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o777 | os.ModeSticky, 0o755} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := taken(); err == nil {
			t.Errorf("a root of another user's (mode %v) was taken", mode)
		}
		if _, err := os.Lstat(mine); err == nil {
			t.Errorf("a root of another user's (mode %v) that was refused had a directory made in it", mode)
		}
	}
}

// A monitor that cannot keep itself from leaving a core dump does not nap
// its VM: the dump would hold the nap's key, beside the nap it sealed.
func TestCloudHVAMonitorThatMayLeaveACoreDoesNotNap(t *testing.T) {
	c := cloudhvFixture(t, 1)
	t.Setenv(envCloudHVNoCore, "fails")
	if err := c.l.Clone("img", "vm1", 2, 512); err != nil {
		t.Fatal(err)
	}
	var log lockedBuffer
	cmd, held, err := c.l.Boot("vm1", true, &log)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() { c.l.Destroy("vm1", cmd.Process.Pid); held.Close(); <-done }()
	c.until("the guest answers", func() bool { return c.l.Exec("vm1", nil, io.Discard, io.Discard, "true").Run() == nil })
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	time.Sleep(3 * time.Second) // three times what an idle VM of this fixture naps after
	if st := c.state("vm1"); st.State != "awake" || st.Naps != 0 || c.has("vm1", "snap") || c.has("vm1", "pauses") || c.has("vm1", "squeezed") {
		t.Fatalf("%+v; snap %v, paused %v, squeezed %v: want the VM awake, never asked to nap", st, c.has("vm1", "snap"), c.has("vm1", "pauses"), c.has("vm1", "squeezed"))
	}
	if !strings.Contains(log.String(), "this VM does not nap") {
		t.Errorf("the monitor did not say why its VM does not nap: %q", log.String())
	}
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Fatalf("the guest's memory is %q, was %q", got, mem)
	}
}
