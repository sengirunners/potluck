package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
)

func TestShelfBindings(t *testing.T) {
	box := func(name string, cfg map[string]string) computeapi.RunTarget {
		return computeapi.RunTarget{Name: name, Kind: "box", Driver: "tart", Config: cfg}
	}
	ts := []computeapi.RunTarget{
		box("a", map[string]string{"vm": "oci", "shelf_max_gb": "10", "state_dir": "/s1"}),
		box("b", map[string]string{"vm": "oci", "shelf_max_gb": "10", "state_dir": "/s1"}), // same state directory as a
		box("c", map[string]string{"vm": "oci", "shelf_max_gb": "10", "state_dir": "/s2"}),
		box("d", map[string]string{"vm": "oci", "shelf_max_gb": "0", "state_dir": "/s3"}),
		box("e", map[string]string{"vm": "oci", "state_dir": "/s4"}),
		box("f", map[string]string{"vm": "tart", "shelf_max_gb": "10", "state_dir": "/s5"}),
		{Name: "g", Kind: "job", Config: map[string]string{"vm": "oci", "shelf_max_gb": "10", "state_dir": "/s6"}},
		{Name: "h@m", Kind: "box", Host: "m", Config: map[string]string{"vm": "oci", "shelf_max_gb": "10", "state_dir": "/s7"}},
	}
	got := shelfBindings(ts)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("got %v", got)
	}
}

func TestOCIBindings(t *testing.T) {
	box := func(name string, cfg map[string]string) computeapi.RunTarget {
		return computeapi.RunTarget{Name: name, Kind: "box", Driver: "tart", Config: cfg}
	}
	ts := []computeapi.RunTarget{
		box("a", map[string]string{"vm": "oci", "state_dir": "/s1"}),
		box("b", map[string]string{"vm": "oci", "shelf_max_gb": "10", "state_dir": "/s1"}), // same state directory as a
		box("c", map[string]string{"vm": "oci", "shelf_max_gb": "0", "state_dir": "/s2"}),
		box("d", map[string]string{"vm": "tart", "state_dir": "/s3"}),
		{Name: "h@m", Kind: "box", Host: "m", Config: map[string]string{"vm": "oci", "state_dir": "/s4"}},
	}
	got := ociBindings(ts)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("got %v", got)
	}
}

// host shelf status asks each oci size (one per state directory) for its
// status, and warns where sizes of one engine differ.
func TestShelfStatus(t *testing.T) {
	old := hostShelfRun
	t.Cleanup(func() { hostShelfRun = old })
	var calls []string
	hostShelfRun = func(_ context.Context, _ string, tg computeapi.RunTarget, args []string, stdout, _ io.Writer) error {
		calls = append(calls, tg.Name+": "+strings.Join(args, " "))
		fmt.Fprintln(stdout, "shelf: /opt/potluck-cache, 1 bytes, 1 files")
		return nil
	}
	box := func(name, vm, gb, dir string) computeapi.RunTarget {
		return computeapi.RunTarget{Name: name, Kind: "box", Config: map[string]string{"vm": vm, "oci_cli": "docker", "shelf_max_gb": gb, "state_dir": dir}}
	}
	ts := []computeapi.RunTarget{box("a", "oci", "5", "/s1"), box("b", "oci", "9", "/s1"), box("c", "oci", "0", "/s2"), box("d", "tart", "", "/s3")}
	var out, errs bytes.Buffer
	a := &app{stdout: &out, stderr: &errs}
	if err := a.shelfStatus(context.Background(), t.TempDir(), ts); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a: shelf status", "c: shelf status"}; !slices.Equal(calls, want) { // c has no shelf cap, and still reports it
		t.Errorf("calls %v, want %v", calls, want)
	}
	if w := out.String(); !strings.Contains(w, "warning: shelf_max_gb=5 on a but 9 on b") {
		t.Errorf("no warning for sizes that differ: %q", w)
	}
	if err := a.shelfStatus(context.Background(), t.TempDir(), ts[3:]); err == nil || !strings.Contains(err.Error(), "vm=oci") {
		t.Errorf("no oci size: %v", err)
	}
}

// potluck host --shelf-gb, --shelf-fill and --volume-max-gb: a plan names
// them on every size; box fill is said plainly and never comes by itself.
func TestHostShelfFlags(t *testing.T) {
	oldRunner, oldDetect, oldDriver := shareRunner, detectHost, shareDriver
	t.Cleanup(func() { shareRunner, detectHost, shareDriver = oldRunner, oldDetect, oldDriver })
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Fatal("dry run executed a command")
		return hostprobe.Result{}, nil
	})
	detectHost = func(_ context.Context, dir string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true, StateDir: dir,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"
	t.Setenv(EnvHome, t.TempDir())
	host := func(flags ...string) (string, string, int) {
		args := append([]string{"host", "--dry-run", "--image", refPin, "--sizes", "c1m2,c2m4", "--name-prefix", "lender", "--no-sudo", "--reclaim-idle-s", "0"}, flags...)
		var out, errs bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &errs)
		return out.String(), errs.String(), code
	}

	out, errs, code := host("--shelf-gb", "5", "--shelf-fill", "boxes", "--volume-max-gb", "40")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"shelf and volumes, on every size that passes: shelf_fill=boxes, shelf_max_gb=5, volume_max_gb=40",
		"would make the shelf volume if missing: docker volume create potluck-cache",
		"someone on this host recently used a given public package",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}

	// JSON: both sizes carry the same keys, and no flag adds none.
	out, errs, code = host("--json", "--shelf-gb", "5")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var res hostprobe.ShareResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sizes) != 2 {
		t.Fatalf("sizes %d", len(res.Sizes))
	}
	for _, s := range res.Sizes {
		if s.Config["shelf_max_gb"] != "5" || s.Config["shelf_fill"] != "" || s.Config["volume_max_gb"] != "" {
			t.Errorf("%s: %v", s.Binding, s.Config)
		}
	}
	out, _, code = host("--shelf-gb", "5")
	if code != 0 || strings.Contains(out, "recently used a given public package") {
		t.Errorf("--shelf-gb alone says box fill: exit %d: %s", code, out)
	}
	out, _, code = host("--json")
	if code != 0 || strings.Contains(out, "shelf_max_gb") || strings.Contains(out, "shelf_fill") {
		t.Errorf("no flag: exit %d: %s", code, out)
	}

	// Box fill needs a shelf.
	if _, errs, code = host("--shelf-fill", "boxes"); code == 0 || !strings.Contains(errs, "--shelf-gb N") {
		t.Errorf("--shelf-fill boxes alone: exit %d: %s", code, errs)
	}
	if _, errs, code = host("--shelf-gb", "0"); code != 0 {
		t.Errorf("--shelf-gb 0: exit %d: %s", code, errs)
	}
}

// Without a go, the settings come from the environment: the private
// patterns joined, and a refusal for settings that are not the public
// proxy and checksum database, or patterns that are not plain globs.
func TestGoPrivateFromEnv(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOPROXY", "")
	t.Setenv("GOSUMDB", "")
	t.Setenv("GOPRIVATE", "corp.example/*")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GONOSUMDB", "other.example")
	t.Setenv("GOINSECURE", "")
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "env"))
	if got, err := goPrivate(); err != nil || got != "corp.example/*,other.example" {
		t.Errorf("goPrivate() = %q, %v", got, err)
	}
	// A go env file it cannot read: refused.
	if err := os.WriteFile(os.Getenv("GOENV"), []byte("GOPRIVATE=x.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := goPrivate(); err == nil {
		t.Errorf("with an unread go env file: %q", got)
	}
	t.Setenv("GOENV", "off")
	for k, v := range map[string]string{"GOPROXY": "https://athens.corp.example,direct", "GOSUMDB": "off", "GOPRIVATE": "[a-z].example"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if got, err := goPrivate(); err == nil {
				t.Errorf("%s=%s: %q", k, v, got)
			}
		})
	}
	t.Setenv("GOPROXY", "file:///opt/potluck-cache/goproxy,https://proxy.golang.org,direct")
	if _, err := goPrivate(); err != nil {
		t.Errorf("the shelf's GOPROXY: %v", err)
	}
}

// With a go on PATH, its go env answers, one line a key; unset keys at the
// end are empty lines, not missing ones.
func TestGoPrivateFromGoEnv(t *testing.T) {
	bin := t.TempDir()
	goEnv := "#!/bin/sh\nprintf '%s\\n' https://proxy.golang.org,direct sum.golang.org corp.example '' '' ''\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goEnv), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got, err := goPrivate(); err != nil || got != "corp.example" {
		t.Errorf("goPrivate() = %q, %v", got, err)
	}
}
