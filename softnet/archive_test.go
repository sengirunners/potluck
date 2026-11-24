package softnet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarGz is a gzipped tar of these entries (a link when its body starts
// with "->").
func tarGz(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e[0], Mode: 0o755, Size: int64(len(e[1])), Typeflag: tar.TypeReg}
		if target, ok := strings.CutPrefix(e[1], "->"); ok {
			h = &tar.Header{Name: e[0], Linkname: target, Typeflag: tar.TypeSymlink}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e[1]))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// archived is a build whose executable is exe, pinned in the archive data.
func archived(exe string, data []byte) Build {
	return Build{ID: "0.99.0", SHA256: sum([]byte(exe)), Bytes: int64(len(exe)),
		Archive: &Archive{SHA256: sum(data), Bytes: int64(len(data)), Member: "softnet"}}
}

func TestUnpackWritesThePinnedMemberOfThePinnedArchive(t *testing.T) {
	exe := "#!/bin/sh\necho softnet\n"
	data := tarGz(t, [2]string{"LICENSE", License}, [2]string{"softnet", exe})
	b := archived(exe, data)
	dir := t.TempDir()
	src := filepath.Join(dir, "softnet.tar.gz")
	os.WriteFile(src, data, 0o600)
	dst := filepath.Join(dir, "softnet")
	if err := b.Unpack(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != exe {
		t.Fatalf("unpacked %q", got)
	}
	if err := b.Unpack(src, dst); err == nil {
		t.Fatal("Unpack wrote over an existing file")
	}
}

// Any archive but the pinned one is refused before anything in it is read,
// and nothing is written.
func TestUnpackRefusesAnyOtherArchive(t *testing.T) {
	exe := "#!/bin/sh\necho softnet\n"
	data := tarGz(t, [2]string{"softnet", exe})
	b := archived(exe, data)
	flipped := bytes.Clone(data)
	flipped[len(flipped)-10] ^= 1
	for name, c := range map[string][]byte{
		"one byte changed": flipped,
		"one byte longer":  append(bytes.Clone(data), 0),
		"one byte shorter": data[:len(data)-1],
		"another archive":  tarGz(t, [2]string{"softnet", "#!/bin/sh\necho other\n"}),
	} {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "softnet")
		os.WriteFile(src, c, 0o600)
		if err := b.Unpack(src, dst); err == nil || !strings.Contains(err.Error(), "is not Softnet 0.99.0's archive") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Lstat(dst); !os.IsNotExist(err) {
			t.Errorf("%s: wrote %s", name, dst)
		}
	}
}

// A pinned archive that holds no plain file under the member's name gives
// nothing; one whose member is longer than the build gives at most one byte
// more than the build's size, for the caller's size check to refuse.
func TestUnpackTakesOnlyAPlainMember(t *testing.T) {
	for name, c := range map[string]struct {
		data []byte
		says string
	}{
		"missing": {tarGz(t, [2]string{"README.md", "x"}), "has no softnet"},
		"a link":  {tarGz(t, [2]string{"softnet", "->/etc/passwd"}), "not a regular file"},
	} {
		b := archived("x", c.data)
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "softnet")
		os.WriteFile(src, c.data, 0o600)
		if err := b.Unpack(src, dst); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Lstat(dst); !os.IsNotExist(err) {
			t.Errorf("%s: wrote %s", name, dst)
		}
	}
	data := tarGz(t, [2]string{"softnet", strings.Repeat("a", 100)})
	b := archived("short", data)
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "softnet")
	os.WriteFile(src, data, 0o600)
	if err := b.Unpack(src, dst); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() != b.Bytes+1 {
		t.Fatalf("a long member: %v %v", fi, err)
	}
}
