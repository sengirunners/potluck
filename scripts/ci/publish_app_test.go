package ci

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in for gh: one release, its assets a line each in $GH_FAKE/assets
// ("id name state digest"). $GH_FAKE/bad is how many uploads break off, as
// GitHub leaves them: an asset that is not "uploaded".
const fakeGH = `#!/bin/bash
set -euo pipefail
echo "$*" >>"$GH_FAKE/calls"
f="$GH_FAKE/assets"
case "$*" in
"release upload "*)
	sum=$( (sha256sum "$4" 2>/dev/null || shasum -a 256 "$4") | cut -d' ' -f1)
	n=$(($(wc -l <"$GH_FAKE/calls") + 100))
	bad=$(cat "$GH_FAKE/bad" 2>/dev/null || echo 0)
	if [ "$bad" -gt 0 ]; then
		echo $((bad - 1)) >"$GH_FAKE/bad"
		if [ -e "$GH_FAKE/wrong" ]; then echo "$n $(basename "$4") uploaded sha256:other" >>"$f"; exit 0; fi
		echo "$n $(basename "$4") starter null" >>"$f"
		echo "connection reset" >&2
		exit 1
	fi
	echo "$n $(basename "$4") uploaded sha256:$sum" >>"$f"
	;;
"api repos/o/r/releases/tags/"*) [ -e "$GH_FAKE/release" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }; cat "$GH_FAKE/release" ;;
"api repos/o/r/releases/7/assets?"*) jq -Rn '[inputs | split(" ") | {id: (.[0] | tonumber), name: .[1], state: .[2], digest: .[3]}]' <"$f" ;;
"api --method DELETE repos/o/r/releases/assets/"*)
	[ ! -e "$GH_FAKE/nodelete" ] || exit 1
	awk -v id="${4##*/}" '$1 != id' "$f" >"$f.new" && mv "$f.new" "$f" ;;
"api --method PATCH repos/o/r/releases/assets/"*)
	[ ! -e "$GH_FAKE/norename" ] || exit 1
	awk -v id="${4##*/}" -v n="${6#name=}" '$1 == id { $2 = n } { print }' "$f" >"$f.new" && mv "$f.new" "$f"
	[ ! -e "$GH_FAKE/lostanswer" ] ;;
*) echo "fake gh: $*" >&2; exit 2 ;;
esac
`

func TestPublishApp(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq here")
	}
	const published = `{"id": 7, "draft": false, "prerelease": false}`
	sum := sha256.Sum256([]byte("the app"))
	good := "uploaded sha256:" + hex.EncodeToString(sum[:])
	other := "1 manifest.json uploaded sha256:m\n"
	cases := []struct {
		name    string
		release string
		assets  string
		files   map[string]string
		ok      bool
		want    string // the assets left, without their ids
	}{
		{"a new release", published, other, nil, true, "manifest.json uploaded sha256:m\nPotluck.zip " + good + "\n"},
		{"an upload that breaks off once", published, other, map[string]string{"bad": "1"}, true, "manifest.json uploaded sha256:m\nPotluck.zip " + good + "\n"},
		{"every upload breaks off", published, other, map[string]string{"bad": "9"}, false, "manifest.json uploaded sha256:m\n"},
		{"GitHub holds other bytes", published, other, map[string]string{"bad": "9", "wrong": ""}, false, "manifest.json uploaded sha256:m\n"},
		{"a rename that fails", published, other, map[string]string{"norename": ""}, false, "manifest.json uploaded sha256:m\n"},
		{"a rename whose answer is lost", published, other, map[string]string{"lostanswer": ""}, true, "manifest.json uploaded sha256:m\nPotluck.zip " + good + "\n"},
		{"a leftover that cannot be removed", published, other, map[string]string{"bad": "9", "nodelete": ""}, false, "manifest.json uploaded sha256:m\nPotluck-upload.zip starter null\n"},
		{"a leftover of an earlier run", published, other + "2 Potluck-upload.zip starter null\n", nil, true, "manifest.json uploaded sha256:m\nPotluck.zip " + good + "\n"},
		{"published already", published, other + "2 Potluck.zip uploaded sha256:old\n", nil, true, "manifest.json uploaded sha256:m\nPotluck.zip uploaded sha256:old\n"},
		{"yanked", `{"id": 7, "draft": false, "prerelease": true}`, other, nil, true, "manifest.json uploaded sha256:m\n"},
		{"a draft", `{"id": 7, "draft": true, "prerelease": false}`, other, nil, true, "manifest.json uploaded sha256:m\n"},
		{"no release", "", other, nil, true, "manifest.json uploaded sha256:m\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, fake := t.TempDir(), t.TempDir()
			write := func(p, s string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(p, []byte(s), mode); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(dir, "gh"), fakeGH, 0o755)
			write(filepath.Join(dir, "sleep"), "#!/bin/sh\n", 0o755)
			zip := filepath.Join(dir, "in.zip")
			write(zip, "the app", 0o644)
			write(filepath.Join(fake, "assets"), c.assets, 0o644)
			if c.release != "" {
				write(filepath.Join(fake, "release"), c.release, 0o644)
			}
			for n, s := range c.files {
				write(filepath.Join(fake, n), s, 0o644)
			}
			cmd := exec.Command("bash", "publish-app.sh", "potluck-main-aaaaaaaaaaaa", zip)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GH_FAKE="+fake, "GH_REPO=o/r", "TMPDIR="+dir, "GITHUB_OUTPUT="+filepath.Join(fake, "out"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.ok {
				t.Errorf("ended %v, want ok %v", err, c.ok)
			}
			said, _ := os.ReadFile(filepath.Join(fake, "out"))
			if has := strings.Contains(c.want, "Potluck.zip "); has != (string(said) == "published=true\n") {
				t.Errorf("GITHUB_OUTPUT: %q", said)
			}
			b, _ := os.ReadFile(filepath.Join(fake, "assets"))
			var got strings.Builder
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				_, rest, _ := strings.Cut(l, " ")
				got.WriteString(rest + "\n")
			}
			if got.String() != c.want {
				t.Errorf("assets:\n%swant:\n%s", got.String(), c.want)
			}
			if t.Failed() {
				calls, _ := os.ReadFile(filepath.Join(fake, "calls"))
				t.Logf("%s\n%s", out, calls)
			}
		})
	}
}
