package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/kit/atomicfile"
)

func TestWritePrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.token")
	if err := atomicfile.WritePrivate(filepath.Join(dir, "missing", "f"), []byte("x")); err == nil {
		t.Fatal("wrote into a missing directory")
	}
	for _, want := range []string{"one\n", "two\n"} {
		if err := atomicfile.WritePrivate(p, []byte(want)); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil || string(b) != want {
			t.Fatalf("read %q %v, want %q", b, err, want)
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v, want 0600", fi.Mode(), err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("dir holds %v %v, want the file alone (no temp file left)", ents, err)
	}
}
