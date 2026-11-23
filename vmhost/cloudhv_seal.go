package vmhost

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
)

// A nap's saved memory is sealed. A guest's memory holds its secrets (they
// are files in its memory filesystem), and the protocol keeps those off
// every disk. So the hypervisor writes its snapshot into the host's memory
// (cloudhvMemDir), and what reaches the VM's directory on disk is that
// snapshot encrypted (cloudhvSeal) under a key the monitor makes for the nap
// and keeps in its own memory only. A wake decrypts it back into memory for
// the hypervisor to restore from. The key goes with the monitor: a napping
// VM whose monitor is gone cannot be woken, and what is left of it on disk
// cannot be read.
//
// That is what an encrypted swap device does with a box's memory, which the
// authority already counts as safe (hostswap). The memory directory and the
// monitor's own memory can be paged out as any memory can, so they are as
// safe as the host's swap, which the authority asks of every host it sends
// secrets to.
//
// A sealed snapshot is one file: a magic line, then frames. Each frame is a
// length and an AES-256-GCM ciphertext whose nonce is the frame's number, so
// frames cannot be reordered, dropped or repeated, and the magic is every
// frame's additional data. A frame's plaintext starts with its kind:
//
//	F <size> <name>    a file of the snapshot begins
//	D <offset> <data>  data of that file; what no D covers is zeros
//	E                  the end: a file without it was cut short

const (
	cloudhvSealMagic = "potluck-cloudhv-nap-1\n"
	cloudhvSealChunk = 1 << 20
	cloudhvSealFile  = "sealed" // in the VM's snap directory
)

// cloudhvSealKey is a new key for one nap.
func cloudhvSealKey() ([]byte, error) {
	k := make([]byte, 32)
	_, err := rand.Read(k)
	return k, err
}

func cloudhvSealAEAD(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func cloudhvSealNonce(n uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], n)
	return nonce
}

// cloudhvSeal writes the files of the directory src, which hold a snapshot
// in the clear, as one sealed file dst. src holds regular files only. It
// answers the bytes of plaintext it sealed.
func cloudhvSeal(src, dst string, key []byte) (sealed int64, err error) {
	aead, err := cloudhvSealAEAD(key)
	if err != nil {
		return 0, err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	if len(ents) == 0 {
		return 0, errors.New("the snapshot holds no file")
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	w := bufio.NewWriterSize(out, 4<<20)
	if _, err := w.WriteString(cloudhvSealMagic); err != nil {
		return 0, err
	}
	var n uint64
	frame := func(plain []byte) error {
		ct := aead.Seal(nil, cloudhvSealNonce(n), plain, []byte(cloudhvSealMagic))
		n++
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}
		_, err := w.Write(ct)
		return err
	}
	buf := make([]byte, 9+cloudhvSealChunk)
	for _, e := range ents {
		fi, err := os.Lstat(filepath.Join(src, e.Name()))
		if err != nil {
			return 0, err
		}
		if !fi.Mode().IsRegular() {
			return 0, fmt.Errorf("the snapshot holds %s, which is no regular file", e.Name())
		}
		f, err := os.Open(filepath.Join(src, e.Name()))
		if err != nil {
			return 0, err
		}
		head := append(binary.BigEndian.AppendUint64([]byte{'F'}, uint64(fi.Size())), e.Name()...)
		if err := frame(head); err != nil {
			f.Close()
			return 0, err
		}
		// A snapshot's memory file is as long as the guest's memory and
		// holds data only where the guest used it: the rest is skipped.
		err = cloudhvDataRanges(f, fi.Size(), func(off, end int64) error {
			for off < end {
				m := int(min(end-off, cloudhvSealChunk))
				if _, err := io.ReadFull(io.NewSectionReader(f, off, int64(m)), buf[9:9+m]); err != nil {
					return err
				}
				if !allZero(buf[9 : 9+m]) {
					buf[0] = 'D'
					binary.BigEndian.PutUint64(buf[1:9], uint64(off))
					if err := frame(buf[:9+m]); err != nil {
						return err
					}
					sealed += int64(m)
				}
				off += int64(m)
			}
			return nil
		})
		f.Close()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	if err := frame([]byte{'E'}); err != nil {
		return 0, err
	}
	return sealed, w.Flush()
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if binary.LittleEndian.Uint64(b) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// cloudhvNoMem is an unsealing's error when the host's memory did not take
// the snapshot: the sealed file is not at fault, and is as it was.
type cloudhvNoMem struct{ error }

func (e cloudhvNoMem) Unwrap() error { return e.error }

// cloudhvUnseal writes the snapshot sealed in the file src into the empty
// directory dst, in the clear. Anything but the whole file that was sealed
// under key is an error: a changed byte, a missing or added frame, another
// key. A file of dst that cannot be written is a cloudhvNoMem.
func cloudhvUnseal(src, dst string, key []byte) error {
	aead, err := cloudhvSealAEAD(key)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r := bufio.NewReaderSize(in, 4<<20)
	magic := make([]byte, len(cloudhvSealMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != cloudhvSealMagic {
		return errors.New("the saved memory is no sealed snapshot")
	}
	damaged := errors.New("the saved memory is damaged, or was sealed under another key")
	var (
		n    uint64
		out  *os.File
		size int64
		ct   = make([]byte, 0, 9+cloudhvSealChunk+aead.Overhead())
		pt   = make([]byte, 0, 9+cloudhvSealChunk)
	)
	closeOut := func() error {
		if out == nil {
			return nil
		}
		err := out.Close()
		out = nil
		return err
	}
	defer closeOut()
	for {
		var l [4]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return damaged // the end comes as a frame, never as the file's end
		}
		ln := binary.BigEndian.Uint32(l[:])
		if uint64(ln) > uint64(cap(ct)) {
			return damaged
		}
		ct = ct[:ln]
		if _, err := io.ReadFull(r, ct); err != nil {
			return damaged
		}
		plain, err := aead.Open(pt[:0], cloudhvSealNonce(n), ct, []byte(cloudhvSealMagic))
		n++
		if err != nil || len(plain) == 0 {
			return damaged
		}
		switch plain[0] {
		case 'F':
			if len(plain) < 10 {
				return damaged
			}
			if err := closeOut(); err != nil {
				return cloudhvNoMem{err}
			}
			name := string(plain[9:])
			size = int64(binary.BigEndian.Uint64(plain[1:9]))
			if name != filepath.Base(name) || name == "." || name == ".." || bytes.IndexByte(plain[9:], 0) >= 0 || size < 0 {
				return damaged
			}
			if out, err = os.OpenFile(filepath.Join(dst, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); errors.Is(err, os.ErrExist) {
				return damaged // dst was empty: the name came twice
			} else if err != nil {
				return cloudhvNoMem{err}
			}
			if err := out.Truncate(size); err != nil {
				return cloudhvNoMem{err}
			}
		case 'D':
			if out == nil || len(plain) < 9 {
				return damaged
			}
			off := int64(binary.BigEndian.Uint64(plain[1:9]))
			if off < 0 || off > size || int64(len(plain)-9) > size-off {
				return damaged
			}
			if _, err := out.WriteAt(plain[9:], off); err != nil {
				return cloudhvNoMem{err}
			}
		case 'E':
			if len(plain) != 1 {
				return damaged
			}
			if _, err := r.ReadByte(); err != io.EOF {
				return damaged
			}
			if err := closeOut(); err != nil {
				return cloudhvNoMem{err}
			}
			return nil
		default:
			return damaged
		}
	}
}

// cloudhvMemRoot is a directory of the host that is memory, not disk, or
// why the host has none. Tests replace it.
var cloudhvMemRoot = cloudhvShm

// cloudhvMemDir is where the VM in dir has its snapshot in the clear, for
// the moments a nap or a wake needs it: a directory of this user's alone, in
// the host's memory. It is not made here. Anything about it that is not as
// it must be is an error, and then no memory is saved: a nap is off. The
// VM's monitor names its directory "." and the driver by the binding's path:
// both come to one directory here.
func cloudhvMemDir(dir string) (string, error) { return cloudhvMemPath(dir, true) }

// cloudhvMemPath is cloudhvMemDir; with base false the user's directory is
// not made when it is not there.
func cloudhvMemPath(dir string, base bool) (string, error) {
	root, err := cloudhvMemRoot()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	// The root is shared with every user of the host: the directory is
	// taken only when it is this user's own and closed to all others, in a
	// root where no other user can put another in its place.
	if fi, err := os.Stat(root); err != nil {
		return "", err
	} else if !ownedByUs(fi) && !ownedByRoot(fi) || fi.Mode()&os.ModeSticky == 0 && fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s lets other users replace what is in it (it is another user's, or others can write to it and it is not sticky)", root)
	}
	mine := filepath.Join(root, "potluck-cloudhv-"+strconv.Itoa(os.Getuid()))
	if base {
		if err := os.Mkdir(mine, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	fi, err := os.Lstat(mine)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 || !ownedByUs(fi) {
		return "", fmt.Errorf("%s is not a directory of this user's alone (mode 0700)", mine)
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(mine, hex.EncodeToString(sum[:12])), nil
}

// cloudhvMemClear removes what the VM in dir has in the host's memory.
func cloudhvMemClear(dir string) error {
	mem, err := cloudhvMemPath(dir, false)
	if err != nil {
		return nil // there is no such place, so nothing is in it
	}
	return os.RemoveAll(mem)
}

// cloudhvOwner is the user a file is of. Tests replace it.
var cloudhvOwner = func(fi os.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func ownedByRoot(fi os.FileInfo) bool {
	uid, ok := cloudhvOwner(fi)
	return ok && uid == 0
}

func ownedByUs(fi os.FileInfo) bool {
	uid, ok := cloudhvOwner(fi)
	return ok && uid == os.Getuid()
}

// cloudhvNoCore keeps this process and the hypervisors it starts from
// leaving a core dump: a monitor's would hold its naps' keys, and a
// hypervisor's the guest's memory. The limit is 1 byte, not 0: Linux writes
// no core file that small, and it hands a core to a crash handler (a
// core_pattern that is a pipe) whatever the limit is, except when the limit
// is 1. A process whose hard limit is 0 cannot have 1: that is the error.
// Tests replace it.
var cloudhvNoCore = func() error {
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 1, Max: 1})
}
