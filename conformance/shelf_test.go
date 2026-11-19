package conformance

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tryReplace runs shelfReplace through shelfTry as the box user, on shelf s
// with mark "mark" and PATH starting at bin when bin is not "". It answers
// the targets the op worked on.
func tryReplace(t *testing.T, s, bin string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", shelfLook+shelfTry, "sh", s, "0", "d", shelfReplace, "mark")
	path := os.Getenv("PATH")
	if bin != "" {
		path = bin + string(os.PathListSeparator) + path
	}
	cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir()}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("shelfTry: %v: %s", err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// shelfDir makes a shelf with one non-empty file and answers its physical
// path, as shelfLook resolves it.
func shelfDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := filepath.Join(d, "shelf")
	if err := os.Mkdir(s, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s, "blob"), []byte("shelf bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

// Where replacing the shelf would take a copy (a stub mv that copies, as mv
// does when rename(2) answers EXDEV), the op writes no file content and
// works on nothing.
func TestShelfReplaceNeverCopies(t *testing.T) {
	bin, out := t.TempDir(), t.TempDir()
	stub := "#!/bin/sh\nset -e\nmkdir -p \"" + out + "/copy\"\nfor f in \"$1\"/*; do cat \"$f\" > \"" + out + "/copy/${f##*/}\"; done\n"
	if err := os.WriteFile(filepath.Join(bin, "mv"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	s := shelfDir(t)
	if did := tryReplace(t, s, bin); did != "" {
		t.Errorf("the op reports it replaced %q though mv could only copy", did)
	}
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if i, err := d.Info(); err != nil {
			return err
		} else if !d.IsDir() && i.Size() > 0 {
			t.Errorf("the op's mv wrote %d bytes to %s", i.Size(), p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(s, "blob")); err != nil || string(b) != "shelf bytes" {
		t.Errorf("the shelf changed: %q, %v", b, err)
	}
}

// Where rename works in place, the op still replaces the shelf and says so.
func TestShelfReplaceRenames(t *testing.T) {
	s := shelfDir(t)
	if did := tryReplace(t, s, ""); did != s {
		t.Fatalf("the op worked on %q, want %q", did, s)
	}
	if b, err := os.ReadFile(filepath.Join(s+".mark", "blob")); err != nil || string(b) != "shelf bytes" {
		t.Errorf("the shelf was not renamed aside: %q, %v", b, err)
	}
	if i, err := os.Stat(filepath.Join(s, "mark")); err != nil || i.Size() != 0 {
		t.Errorf("no empty marker in the box's directory: %v", err)
	}
}
