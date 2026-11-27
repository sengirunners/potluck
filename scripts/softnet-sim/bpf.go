//go:build darwin

package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// tap is a BPF device on one Ethernet interface: it reads every frame of the
// interface, both ways, and can write frames to it.
type tap struct {
	f    *os.File
	len  int
	mu   sync.Mutex
	fail error // why reading ended
}

func openTap(ifname string) (*tap, error) {
	for i := 0; i < 256; i++ {
		f, err := os.OpenFile(fmt.Sprintf("/dev/bpf%d", i), os.O_RDWR, 0)
		if err != nil {
			if pe, ok := err.(*os.PathError); ok && pe.Err == syscall.EBUSY {
				continue
			}
			return nil, err
		}
		t := &tap{f: f}
		if err := t.setup(ifname); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %v", ifname, err)
		}
		return t, nil
	}
	return nil, fmt.Errorf("no free /dev/bpf")
}

func (t *tap) ioctl(req uintptr, p unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, t.f.Fd(), req, uintptr(p)); e != 0 {
		return e
	}
	return nil
}

func (t *tap) setup(ifname string) error {
	size, one := uint32(1<<19), uint32(1)
	if err := t.ioctl(syscall.BIOCSBLEN, unsafe.Pointer(&size)); err != nil {
		return err
	}
	var ifr [32]byte
	copy(ifr[:15], ifname)
	if err := t.ioctl(syscall.BIOCSETIF, unsafe.Pointer(&ifr)); err != nil {
		return err
	}
	// Frames as they arrive, the source address left as written, what this
	// device itself writes seen too, and everything on the interface.
	for _, req := range []uintptr{syscall.BIOCIMMEDIATE, syscall.BIOCSHDRCMPLT, syscall.BIOCSSEESENT} {
		if err := t.ioctl(req, unsafe.Pointer(&one)); err != nil {
			return err
		}
	}
	if err := t.ioctl(syscall.BIOCPROMISC, nil); err != nil {
		return err
	}
	var n uint32
	if err := t.ioctl(syscall.BIOCGBLEN, unsafe.Pointer(&n)); err != nil {
		return err
	}
	t.len = int(n)
	return nil
}

func (t *tap) write(f []byte) error { _, err := t.f.Write(f); return err }

// each calls fn with every frame until the device is closed.
func (t *tap) each(fn func([]byte)) {
	buf := make([]byte, t.len)
	for {
		n, err := t.f.Read(buf)
		if err != nil {
			t.mu.Lock()
			t.fail = err
			t.mu.Unlock()
			return
		}
		for b := buf[:n]; len(b) >= 18; {
			capl, hdr := int(le32(b[8:])), int(le16(b[16:]))
			if hdr+capl > len(b) {
				break
			}
			fn(append([]byte(nil), b[hdr:hdr+capl]...))
			next := (hdr + capl + 3) &^ 3
			if next >= len(b) {
				break
			}
			b = b[next:]
		}
	}
}

// lost is what this tap did not see, "" when it saw every frame.
func (t *tap) lost() string {
	t.mu.Lock()
	err := t.fail
	t.mu.Unlock()
	if err != nil {
		return "reading the interface ended: " + err.Error()
	}
	var st struct{ recv, drop uint32 }
	if err := t.ioctl(syscall.BIOCGSTATS, unsafe.Pointer(&st)); err != nil {
		return "the interface's dropped frames could not be counted: " + err.Error()
	}
	if st.drop > 0 {
		return fmt.Sprintf("%d frames of the interface were dropped unread", st.drop)
	}
	return ""
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 { return uint32(le16(b)) | uint32(le16(b[2:]))<<16 }

// record is every frame an interface carried, kept for the checks.
type record struct {
	mu     sync.Mutex
	frames [][]byte
}

func (r *record) add(f []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, f)
}

// any is true once a recorded frame satisfies ok.
func (r *record) any(ok func([]byte) bool) bool { return r.count(ok) > 0 }

func (r *record) count(ok func([]byte) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, f := range r.frames {
		if ok(f) {
			n++
		}
	}
	return n
}
