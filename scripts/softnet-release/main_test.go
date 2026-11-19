package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/softnet"
)

func TestReleaseFilesCarryTheLicenseAndOnlyThePinnedBuild(t *testing.T) {
	content := []byte("a softnet")
	sum := sha256.Sum256(content)
	old := softnet.Builds
	t.Cleanup(func() { softnet.Builds = old })
	b := old[0]
	b.ID, b.SHA256, b.Bytes = "0.0.1-test", hex.EncodeToString(sum[:]), int64(len(content))
	softnet.Builds = []softnet.Build{b}
	dir := t.TempDir()
	bin := filepath.Join(dir, "softnet")
	os.WriteFile(bin, content, 0o755)
	wrong := filepath.Join(dir, "other")
	os.WriteFile(wrong, []byte("a softnot"), 0o755)
	var w bytes.Buffer
	if err := run(&w, wrong, filepath.Join(dir, "refused"), ""); err == nil || !strings.Contains(err.Error(), "is not Softnet 0.0.1-test") {
		t.Fatalf("wrong binary: %v", err)
	}
	out := filepath.Join(dir, "out")
	if err := run(&w, bin, out, ""); err != nil {
		t.Fatal(err)
	}
	lic, _ := os.ReadFile(filepath.Join(out, "LICENSE"))
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if string(lic) != softnet.License || !strings.Contains(string(sums), b.SHA256+"  softnet-0.0.1-test-darwin-arm64") {
		t.Fatalf("license or sums wrong:\n%s", sums)
	}
	if !strings.Contains(w.String(), "gh release create softnet-0.0.1-test --repo sengirunners/potluck --latest=false --prerelease") {
		t.Fatalf("publish line: %s", w.String())
	}
}
