package menubar

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The app's pictures are kept beside the drawings they were made from; a
// build refuses a set that does not match its record, so a change that left
// one out must not reach main.
func TestAppPicturesAreMadeFromTheirDrawings(t *testing.T) {
	dir := filepath.Join("..", "macos", "PotluckMenu", "icon")
	record, err := os.ReadFile(filepath.Join(dir, "made.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(record)), "\n") {
		want, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("made.sha256: %q is not a sha256 and a file name", line)
		}
		have[name] = true
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s is not the one made.sha256 records: run macos/PotluckMenu/icon/make.sh on a Mac and look at what it made", name)
		}
	}
	for _, pattern := range []string{"*.svg", "*.png", "*.icns"} {
		files, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v, %v", pattern, files, err)
		}
		for _, f := range files {
			if !have[filepath.Base(f)] {
				t.Errorf("made.sha256 does not record %s", filepath.Base(f))
			}
		}
	}
}
