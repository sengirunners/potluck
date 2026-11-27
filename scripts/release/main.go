// Command release builds Potluck's release tarballs: per OS and
// architecture, every program an install needs, packed as
// dist/potluck_<version>_<os>_<arch>.tar.gz, and dist/SHA256SUMS in the
// format `shasum -a 256 -c` reads. scripts/release-sign signs them; it
// publishes nothing.
//
//	go run ./scripts/release -version main-1eacf23e722f -commit <sha> [-out dist] [-targets darwin/arm64,linux/amd64] [-menubar Potluck-Menu.zip] [-signed dir]
//	go run ./scripts/release -version main-1eacf23e722f -commit <sha> -programs dir
//
// -menubar names a zip of Potluck.app (macos/PotluckMenu, built on a
// macOS runner: swiftc is not Go); it goes into each darwin tarball as
// MenubarZip, so the signed manifest's hash covers it and install.sh
// --menubar unpacks it from there. No -menubar, no app in the tarballs.
//
// -programs only builds the Mac programs, into dir/<os>_<arch>/, for
// macos/PotluckMenu/sign.sh to sign; -signed takes them back, and refuses one
// that differs from this build's in anything but its signature (signed.go).
//
// Builds are CGO_ENABLED=0 and -trimpath, stamped with -ldflags so
// `potluck --version` names the release; archive entries are sorted and
// carry one fixed time (SOURCE_DATE_EPOCH, else the commit's time), so the
// same commit and Go version give byte-identical tarballs (the darwin ones
// only without -signed: a signature carries its time). It refuses to
// build while internal/upgrade/release_keys.txt is empty or malformed: such
// a build would refuse every upgrade. A local or test release signed with
// another key is trusted through config update_keys.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/internal/upgrade"
	"github.com/sengirunners/potluck/kit/release"
)

// Targets are the platforms a release ships for.
var Targets = []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64"}

// Commands are the programs in every tarball: potluck, the drivers the
// daemon runs from its own directory (protocol.DriverDirs), the conformance
// suite `potluck host` runs (hostprobe), and the E2B gateway.
var Commands = []string{"potluck", "potluck-driver-local", "potluck-conformance", "potluck-e2b"}

// TargetCommands are the programs one platform adds: the tart driver, the
// driver `potluck host` binds on every layer (Tart VMs on Apple-silicon
// Macs, none on Intel; Incus or an OCI runtime on Linux).
var TargetCommands = map[string][]string{
	"darwin/arm64": {"potluck-driver-tart"},
	"linux/amd64":  {"potluck-driver-tart"},
	"linux/arm64":  {"potluck-driver-tart"},
}

// Docs are copied next to the programs.
var Docs = []string{"README.md", "LICENSE", "NOTICE"}

// MenubarZip is the name of Potluck.app's zip inside a darwin tarball
// (docs/menubar.md); install.sh --menubar reads it by this name.
const MenubarZip = "Potluck-Menu.zip"

const module = "github.com/sengirunners/potluck"

var versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func main() {
	version := flag.String("version", "v0.0.0-dev", "version stamped into the programs and file names")
	commit := flag.String("commit", "unknown", "commit stamped into the programs")
	out := flag.String("out", "dist", "output directory (its old tarballs, SHA256SUMS, manifest and .build are replaced; nothing else is touched)")
	targets := flag.String("targets", strings.Join(Targets, ","), "comma list of os/arch")
	menubar := flag.String("menubar", "", "a zip of Potluck.app to put in each darwin tarball as "+MenubarZip)
	signed := flag.String("signed", "", "a directory of this build's Mac programs (as -programs lays them out), signed, to pack in place of the built ones")
	programs := flag.String("programs", "", "only build the darwin targets' programs, into this directory")
	flag.Parse()
	list := strings.Split(*targets, ",")
	do := func() error { return run(*version, *commit, *out, list, *menubar, *signed) }
	if *programs != "" {
		do = func() error { return buildDarwin(*version, *commit, *programs, list) }
	}
	if err := do(); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

// commandsFor is the list of programs in the tarball for goos/goarch.
func commandsFor(goos, goarch string) []string {
	return append(append([]string(nil), Commands...), TargetCommands[goos+"/"+goarch]...)
}

func run(version, commit, out string, targets []string, menubar, signed string) error {
	if !versionRE.MatchString(version) {
		return fmt.Errorf("bad version %q", version)
	}
	if _, err := upgrade.ReleaseKeys(); err != nil {
		return err
	}
	mtime, err := sourceTime()
	if err != nil {
		return err
	}
	if err := clean(out); err != nil {
		return err
	}
	build := filepath.Join(out, ".build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(build)
	var sums []string
	for _, t := range targets {
		goos, goarch, err := splitTarget(t)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(release.Potluck.Tarball(version, goos, goarch), ".tar.gz")
		dir := filepath.Join(build, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := buildPrograms(version, commit, goos, goarch, dir); err != nil {
			return err
		}
		if signed != "" && goos == "darwin" {
			if err := takeSigned(filepath.Join(signed, goos+"_"+goarch), dir, commandsFor(goos, goarch)); err != nil {
				return fmt.Errorf("-signed: %s: %w", t, err)
			}
		}
		for _, d := range Docs {
			if err := copyFile(d, filepath.Join(dir, d)); err != nil {
				return err
			}
		}
		if menubar != "" && goos == "darwin" {
			if err := copyFile(menubar, filepath.Join(dir, MenubarZip)); err != nil {
				return fmt.Errorf("-menubar: %w", err)
			}
		}
		tarball := filepath.Join(out, name+".tar.gz")
		if err := pack(tarball, dir, name, mtime); err != nil {
			return err
		}
		sum, err := sha256File(tarball)
		if err != nil {
			return err
		}
		sums = append(sums, sum+"  "+filepath.Base(tarball))
		fmt.Println(sum, tarball)
	}
	sort.Strings(sums)
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644)
}

func splitTarget(t string) (goos, goarch string, err error) {
	goos, goarch, ok := strings.Cut(strings.TrimSpace(t), "/")
	if !ok || goos == "" || goarch == "" {
		return "", "", fmt.Errorf("bad target %q (want os/arch)", t)
	}
	return goos, goarch, nil
}

// buildPrograms builds the platform's programs into dir.
func buildPrograms(version, commit, goos, goarch, dir string) error {
	ldflags := fmt.Sprintf("-s -w -X %s/kit/api.Version=%s -X %s/kit/api.Commit=%s", module, version, module, commit)
	// -buildvcs=false: the stamp says whether the checkout is clean, which the output directory alone changes.
	args := []string{"build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", dir + "/"}
	for _, c := range commandsFor(goos, goarch) {
		args = append(args, module+"/cmd/"+c)
	}
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s/%s: %w", goos, goarch, err)
	}
	return nil
}

// clean removes only what a release writes into out, never out itself.
func clean(out string) error {
	old, err := filepath.Glob(filepath.Join(out, release.Potluck.Name+"_*.tar.gz"))
	if err != nil {
		return err
	}
	for _, p := range append(old, filepath.Join(out, "SHA256SUMS"), filepath.Join(out, release.ManifestFile),
		filepath.Join(out, release.SignatureFile), filepath.Join(out, release.SSHSIGFile), filepath.Join(out, ".build")) {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// sourceTime is SOURCE_DATE_EPOCH, else HEAD's commit time, else now.
func sourceTime() (time.Time, error) {
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH: %w", err)
		}
		return time.Unix(n, 0).UTC(), nil
	}
	if b, err := exec.Command("git", "log", "-1", "--format=%ct").Output(); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			return time.Unix(n, 0).UTC(), nil
		}
	}
	return time.Now().UTC().Truncate(time.Second), nil
}

// pack writes dir as a gzip'd tar whose entries live under prefix/, sorted,
// owned by root, with one fixed time.
func pack(path, dir, prefix string, mtime time.Time) error {
	var files []string
	if err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return err
	}); err != nil {
		return err
	}
	sort.Strings(files)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755, ModTime: mtime, Uname: "root", Gname: "root", Format: tar.FormatPAX}); err != nil {
		return err
	}
	for _, p := range files {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		mode := int64(0o644)
		if fi.Mode()&0o111 != 0 {
			mode = 0o755
		}
		h := &tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/" + filepath.ToSlash(rel), Mode: mode, Size: fi.Size(),
			ModTime: mtime, Uname: "root", Gname: "root", Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
