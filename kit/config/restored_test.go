package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
)

func TestRestoreGeneration(t *testing.T) {
	home := t.TempDir()
	if gen, err := config.RestoreGeneration(home); err != nil || gen != "" {
		t.Fatalf("a never-restored home: %q %v", gen, err)
	}
	gen, err := config.NewRestoreGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteRestoreGeneration(home, gen); err != nil {
		t.Fatal(err)
	}
	if got, err := config.RestoreGeneration(home); err != nil || got != gen {
		t.Fatalf("read %q %v, wrote %q", got, err, gen)
	}
	// Damaged is an error, never "never restored".
	for _, bad := range []string{"", "short", "zz" + gen[2:]} {
		os.WriteFile(filepath.Join(home, config.FileRestored), []byte(bad), 0o600)
		if got, err := config.RestoreGeneration(home); err == nil {
			t.Fatalf("damaged %q read as %q", bad, got)
		}
	}
}

func TestRestoringMarker(t *testing.T) {
	home := t.TempDir()
	if err := config.CheckNotRestoring(home); err != nil {
		t.Fatalf("no marker: %v", err)
	}
	if err := config.MarkRestoring(home, "20261102T010000Z"); err != nil {
		t.Fatal(err)
	}
	err := config.CheckNotRestoring(home)
	if err == nil || !strings.Contains(err.Error(), "20261102T010000Z") || !strings.Contains(err.Error(), "fresh home") {
		t.Fatalf("marker present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, config.FileRestoring+".tmp")); err == nil {
		t.Fatal("temp file left behind")
	}
	if err := config.ClearRestoring(home); err != nil {
		t.Fatal(err)
	}
	if err := config.CheckNotRestoring(home); err != nil {
		t.Fatalf("cleared: %v", err)
	}
	if err := config.ClearRestoring(home); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
}

func TestRestoringMarkerFailsClosed(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "blank": "\n", "spaces": "a b\n"} {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, config.FileRestoring), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := config.CheckNotRestoring(home); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Unreadable: a directory in its place.
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, config.FileRestoring), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.CheckNotRestoring(home); err == nil {
		t.Fatal("an unreadable marker passed")
	}
}
