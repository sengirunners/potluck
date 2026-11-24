// Command softnet-release assembles one pinned Softnet build's release
// files (softnet.Builds): the executable, checked against its pinned
// SHA-256 and size, the upstream LICENSE, provenance.json, SHA256SUMS and
// the release notes. It publishes nothing; docs/softnet.md says who
// publishes them and how.
//
//	go run ./scripts/softnet-release -binary <reviewed softnet> -out <new dir> [-id <build>]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/softnet"
)

func main() {
	binary := flag.String("binary", "", "the reviewed Softnet executable")
	out := flag.String("out", "", "a new directory for the release files")
	id := flag.String("id", "", "the build (default: the newest of Potluck's own in softnet.Builds)")
	flag.Parse()
	if err := run(os.Stdout, *binary, *out, *id); err != nil {
		fmt.Fprintln(os.Stderr, "softnet-release:", err)
		os.Exit(1)
	}
}

func run(w io.Writer, binary, out, id string) error {
	if binary == "" || out == "" {
		return errors.New("-binary and -out are required")
	}
	var b softnet.Build
	if id != "" {
		var ok bool
		if b, ok = softnet.ByID(id); !ok {
			return fmt.Errorf("softnet.Builds pins no build %s", id)
		}
		if b.UpstreamRelease {
			return fmt.Errorf("Softnet %s is an upstream release, published by upstream at %s, not by Potluck", id, b.URL)
		}
	} else {
		for _, p := range softnet.Builds {
			if !p.UpstreamRelease {
				b = p
				break
			}
		}
		if b.ID == "" {
			return errors.New("softnet.Builds pins no build of Potluck's own")
		}
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != b.SHA256 || int64(len(data)) != b.Bytes {
		return fmt.Errorf("%s is not Softnet %s: %d bytes, SHA-256 %x; pinned %d bytes, SHA-256 %s", binary, b.ID, len(data), sum, b.Bytes, b.SHA256)
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		return fmt.Errorf("-out must be a new directory: %w", err)
	}
	prov, _ := json.MarshalIndent(map[string]any{"id": b.ID, "version": b.Version, "sha256": b.SHA256, "bytes": b.Bytes, "arch": b.Arch,
		"min_macos": b.MinMacOS, "max_macos": b.MaxMacOS, "validated": b.Validated, "source": b.Source,
		"license": "FSL-1.1-ALv2 (upstream Softnet, OpenAI; see LICENSE)", "license_sha256": b.LicenseSHA256}, "", "  ")
	status := "a candidate, not yet validated on a real Mac of each macOS version it covers"
	if b.Validated {
		status = "validated on a real Mac of each macOS version it covers"
	}
	notes := fmt.Sprintf(`Softnet %s for macOS %d-%d on Apple silicon: %s.

Upstream Softnet %s and newer need macOS %d; this build carries upstream's protocol and port rules onto
upstream %s. Install it with `+"`potluck softnet install`"+` (docs/softnet.md), which accepts only the file whose
SHA-256 the potluck release pins:

    %s  %s

Source: %s, commit %s (tree %s), from %s with %s.
Build: %s.

Softnet is under the Functional Source License 1.1, ALv2 Future License (LICENSE in this release), not under
Potluck's Apache-2.0.
`, b.ID, b.MinMacOS, b.MaxMacOS, status, softnet.UpstreamMinVersion, softnet.UpstreamMinMacOS, b.Source.Base,
		b.SHA256, softnet.AssetName(b.ID, b.Arch), b.Source.Upstream, b.Source.Commit, b.Source.Tree, b.Source.Base, b.Source.Patch, b.Source.Recipe)
	files := map[string][]byte{
		softnet.AssetName(b.ID, b.Arch): data,
		"LICENSE":                       []byte(softnet.License),
		"provenance.json":               append(prov, '\n'),
		"NOTES.md":                      []byte(notes),
	}
	var sums []string
	for name, d := range files {
		mode := os.FileMode(0o644)
		if name == softnet.AssetName(b.ID, b.Arch) {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(out, name), d, mode); err != nil {
			return err
		}
		if name != "NOTES.md" {
			s := sha256.Sum256(d)
			sums = append(sums, hex.EncodeToString(s[:])+"  "+name)
		}
	}
	sort.Strings(sums)
	if err := os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	pre := ""
	if !b.Validated {
		pre = " --prerelease"
	}
	fmt.Fprintf(w, "wrote Softnet %s's release files to %s\n", b.ID, out)
	fmt.Fprintf(w, "publish (an owner's step, docs/softnet.md):\n  gh release create %s --repo %s --latest=false%s --title 'Softnet %s' --notes-file %s %s %s %s %s\n",
		softnet.ReleaseTag(b.ID), softnet.ReleaseRepo, pre, b.ID, filepath.Join(out, "NOTES.md"),
		filepath.Join(out, softnet.AssetName(b.ID, b.Arch)), filepath.Join(out, "LICENSE"), filepath.Join(out, "provenance.json"), filepath.Join(out, "SHA256SUMS"))
	return nil
}
