package protocol

import (
	"slices"
	"strings"
	"testing"
)

func TestVolumePathOK(t *testing.T) {
	for _, path := range []string{"a", "a/b", "x.y/z@1+2", strings.Repeat("x", 256)} {
		if !VolumePathOK(path) {
			t.Errorf("expected %q to be accepted", path)
		}
	}
	for _, path := range []string{"", "/abs", "a//b", ".", "..", "a/../b", "a,b", "a=b", " ", strings.Repeat("x", 257)} {
		if VolumePathOK(path) {
			t.Errorf("expected %q to be rejected", path)
		}
	}
}

func TestVolumePathsOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b    string
		overlap bool
	}{
		{"a/b", "a/b", true}, {"a", "a/b", true}, {"a/b", "a", true},
		{"a/b", "a/bc", false}, {"a/b", "a/c", false},
	} {
		if got := VolumePathsOverlap(tc.a, tc.b); got != tc.overlap {
			t.Errorf("overlap(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.overlap)
		}
	}
}

func TestKeepPathsOK(t *testing.T) {
	if err := KeepPathsOK([]string{"a/b", "c"}); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{nil, make([]string, KeepMax+1)} {
		if err := KeepPathsOK(paths); err == nil {
			t.Errorf("accepted list of length %d", len(paths))
		}
	}
	if err := KeepPathsOK([]string{"good", "a/../b"}); err == nil || !strings.Contains(err.Error(), "a/../b") {
		t.Fatalf("invalid path error = %v", err)
	}
}

func TestKeepDirs(t *testing.T) {
	got := KeepDirs([]string{"a/b/c", "a/b/d", "plain", "e/f", "a/g/h"})
	want := []string{"a", "a/b", "e", "a/g"}
	if !slices.Equal(got, want) {
		t.Errorf("KeepDirs = %v, want %v", got, want)
	}
}
