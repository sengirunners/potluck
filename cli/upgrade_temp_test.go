package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// install.sh's release directory is temporary and is not named in the
// verified line; a directory the person gave elsewhere is.
func TestInTempDir(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "tmp")
	other := filepath.Join(t.TempDir(), "other")
	for _, d := range []string{tmp, other, filepath.Join(tmp, "rel")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tmp)
	if !inTempDir(filepath.Join(tmp, "rel")) {
		t.Error("a directory inside the temp directory is not in it")
	}
	if inTempDir(other) || inTempDir("/") {
		t.Error("a directory outside the temp directory is in it")
	}
}
