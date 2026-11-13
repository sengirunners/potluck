package boxcp

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// unpack keeps a running total of the bytes it writes and refuses past the
// copy's limit, whatever the entries' headers claim: nothing written.
func TestUnpackStopsAtTheLimit(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, n := range []string{"a", "b"} {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Size: 600, Mode: 0o644})
		tw.Write(bytes.Repeat([]byte("x"), 600))
	}
	tw.Close()
	final := filepath.Join(t.TempDir(), "copy")
	if _, _, err := unpack(bytes.NewReader(b.Bytes()), final, 1000); !api.IsCode(err, api.CodeTooLarge) || !strings.Contains(err.Error(), "nothing written") {
		t.Fatalf("past the limit: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(final)); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	if files, _, err := unpack(bytes.NewReader(b.Bytes()), final, 1200); err != nil || files != 2 {
		t.Fatalf("at the limit: %d %v", files, err)
	}
}
