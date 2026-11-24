// Package tartpintest makes stand-in Tart releases for tests: a
// tart.tar.gz in a temporary directory and the tartpin.Build that pins it.
package tartpintest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/tartpin"
)

// Entry is one entry of a stand-in tart.tar.gz.
type Entry struct {
	Name     string
	Type     byte // tar.TypeReg when 0
	Body     string
	Mode     int64 // 0o644 when 0
	Linkname string
}

// Program stands for the tart program in an Entry's Body; Release writes
// ProgramOf its version instead.
const Program = "the tart program"

// ProgramOf is the stand-in tart program of version.
func ProgramOf(version string) string { return "#!/bin/sh\necho " + version + "\n" }

// Whole is a release laid out as upstream's is: LICENSE and tart.app with
// its program.
func Whole() []Entry {
	return []Entry{
		{Name: "LICENSE", Body: "licence"},
		{Name: "tart.app/Contents/Info.plist", Body: "<plist/>"},
		{Name: "tart.app/Contents/MacOS/tart", Body: Program, Mode: 0o755},
	}
}

// Release writes entries as a tart.tar.gz in a new temporary directory and
// returns its path, its bytes and a Build of version that pins it (its
// ProgramSHA256 is ProgramOf version's).
func Release(t testing.TB, version string, entries []Entry) (string, []byte, tartpin.Build) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.Body == Program {
			e.Body = ProgramOf(version)
		}
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.Mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.Name, Typeflag: typ, Mode: mode, Linkname: e.Linkname}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	path := filepath.Join(t.TempDir(), "tart.tar.gz")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, data, tartpin.Build{
		Version:       version,
		URL:           "https://example.invalid/" + version + "/tart.tar.gz",
		Bytes:         int64(len(data)),
		SHA256:        sum(data),
		ProgramSHA256: sum([]byte(ProgramOf(version))),
		MinMacOS:      13,
		Arches:        []string{"arm64"},
	}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
