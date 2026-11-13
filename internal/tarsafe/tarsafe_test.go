package tarsafe

import (
	"archive/tar"
	"testing"
)

func TestName(t *testing.T) {
	for n, want := range map[string]string{"a": "a", "a/b/": "a/b", "./a": "a", "a//b": "a/b", ".": "."} {
		if got, ok := Name(n); !ok || got != want {
			t.Errorf("%q: %q %v, want %q", n, got, ok, want)
		}
	}
	for _, n := range []string{"", "/a", "..", "../a", "a/../b", "a/..", "a\x00b"} {
		if got, ok := Name(n); ok {
			t.Errorf("%q accepted as %q", n, got)
		}
	}
}

func TestSparse(t *testing.T) {
	if !Sparse(&tar.Header{Typeflag: tar.TypeGNUSparse}) || !Sparse(&tar.Header{Typeflag: tar.TypeReg, PAXRecords: map[string]string{"GNU.sparse.map": "0,1"}}) {
		t.Error("a sparse entry was not seen")
	}
	if Sparse(&tar.Header{Typeflag: tar.TypeReg, PAXRecords: map[string]string{"path": "a"}}) {
		t.Error("a plain entry was taken as sparse")
	}
}
