package images_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedChainUsesTheReturnedImmutableParents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TAG", "staging-"+strings.Repeat("a", 40))
	t.Setenv("SOURCE_HEAD", strings.Repeat("a", 40))
	t.Setenv("SOURCE_REPOSITORY", "sengirunners/potluck")
	calls := filepath.Join(dir, "calls")
	output := filepath.Join(dir, "output")
	t.Setenv("CALLS", calls)
	t.Setenv("GITHUB_OUTPUT", output)
	for name, script := range map[string]string{
		"docker": "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$CALLS\"\n",
		"jq":     "#!/bin/sh\nprintf '%s\\n' \"$TEST_DIGEST\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	parent := "-"
	for i, name := range []string{"linux", "agent", "browser"} {
		digest := "sha256:" + strings.Repeat(string(rune('1'+i)), 64)
		t.Setenv("TEST_DIGEST", digest)
		image := "ghcr.io/sengirunners/potluck-box-" + name
		if err := os.WriteFile(output, nil, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "publish-box-image.sh", image, "context/"+name, filepath.Join(dir, name+".json"), parent)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("publish %s: %v: %s", name, err, out)
		}
		b, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		published := string(b)
		want := "pin=" + image + "@" + digest + "\n"
		if string(b) != want {
			t.Fatalf("output %q, want %q", b, want)
		}
		b, err = os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		args := string(b)
		if parent != "-" && !strings.Contains(args, "--build-arg\nBASE="+parent+"\n") {
			t.Fatalf("%s did not inherit exact parent %s: %s", name, parent, args)
		}
		for _, arg := range []string{"--platform\nlinux/amd64,linux/arm64\n", "--push\n", "org.opencontainers.image.revision=" + strings.Repeat("a", 40)} {
			if !strings.Contains(args, arg) {
				t.Fatalf("build omitted %q: %s", arg, args)
			}
		}
		parent = strings.TrimPrefix(strings.TrimSpace(published), "pin=")
		if err := os.Remove(calls); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []struct{ base, digest string }{
		{"mutable:latest", "sha256:" + strings.Repeat("0", 64)},
		{parent, "not-a-digest"},
	} {
		if err := os.WriteFile(output, nil, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TEST_DIGEST", bad.digest)
		cmd := exec.Command("bash", "publish-box-image.sh", "registry/browser", "context", filepath.Join(dir, "bad.json"), bad.base)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("accepted invalid publication base=%q digest=%q: %s", bad.base, bad.digest, out)
		}
		if b, err := os.ReadFile(output); err != nil || len(b) != 0 {
			t.Fatalf("invalid publication returned a pin: %q, %v", b, err)
		}
	}
}
