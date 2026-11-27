package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/release"
)

// The release packs signed Mac programs only when they are its own build with
// another signature.
func TestSignedProgramsAreThisBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the Mac programs")
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("no codesign: the signing is macOS's")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	const version = "main-aaaaaaaaaaaa"
	targets := []string{"darwin/arm64", "darwin/amd64"}
	dir := t.TempDir()
	if err := buildDarwin(version, "unknown", dir, targets); err != nil {
		t.Fatal(err)
	}
	// The release's own build, untouched by the signing below.
	built := copyDir(t, filepath.Join(dir, "darwin_arm64"), filepath.Join(t.TempDir(), "built"))
	names := commandsFor("darwin", "arm64")
	take := func() error {
		return takeSigned(filepath.Join(dir, "darwin_arm64"), copyDir(t, built, filepath.Join(t.TempDir(), "taken")), names)
	}
	// Go signs its arm64 programs ad hoc, its amd64 ones not at all.
	amd := filepath.Join(dir, "darwin_amd64")
	if err := takeSigned(amd, copyDir(t, amd, filepath.Join(t.TempDir(), "amd")), commandsFor("darwin", "amd64")); err == nil || !strings.Contains(err.Error(), "no signature") {
		t.Fatalf("took an unsigned program: %v", err)
	}
	for _, p := range []string{"darwin_arm64", "darwin_amd64"} {
		ents, err := os.ReadDir(filepath.Join(dir, p))
		if err != nil || len(ents) == 0 {
			t.Fatalf("%s: %v", p, err)
		}
		for _, e := range ents {
			f := filepath.Join(dir, p, e.Name())
			if o, err := exec.Command("codesign", "--force", "--options", "runtime", "--prefix", "sh.potluck.", "--sign", "-", f).CombinedOutput(); err != nil {
				t.Fatalf("codesign: %v\n%s", err, o)
			}
		}
	}
	if err := take(); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := run(version, "unknown", out, targets, "", dir); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "darwin_arm64", "potluck")
	signed, err := os.ReadFile(prog)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := os.ReadFile(filepath.Join(out, release.Potluck.Tarball(version, "darwin", "arm64")))
	if err != nil {
		t.Fatal(err)
	}
	got := t.TempDir()
	if _, err := release.Potluck.Extract(tb, version, "darwin", "arm64", got); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(got, "potluck")); string(b) != string(signed) {
		t.Error("the tarball does not carry the signed potluck")
	}

	changed := append([]byte(nil), signed...)
	changed[len(changed)/2] ^= 1
	other, err := os.ReadFile(filepath.Join(dir, "darwin_arm64", "potluck-e2b"))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := os.ReadFile(filepath.Join(dir, "darwin_amd64", "potluck"))
	if err != nil {
		t.Fatal(err)
	}
	for what, b := range map[string][]byte{"a changed byte": changed, "another program": other, "another CPU's": foreign, "no Mach-O": []byte("#!/bin/sh\n")} {
		if err := os.WriteFile(prog, b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := take(); err == nil {
			t.Errorf("took %s", what)
		}
	}
	if err := os.Remove(prog); err != nil {
		t.Fatal(err)
	}
	if err := take(); err == nil {
		t.Error("took a missing program")
	}
}
