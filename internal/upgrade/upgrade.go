// Package upgrade fetches and verifies Potluck's signed releases (kit/release
// format release.Potluck) for `potluck upgrade` and install.sh: from the
// GitHub releases of sengirunners/potluck, or from a directory holding one
// release's files (air-gapped hosts, tests, and while the repository is
// private). The swap itself is kit/swap's Place (cli/upgrade.go).
package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/release"
)

// KeyPlaceholder stands in for ReleaseKey until the channel's key exists. A
// build that still has it trusts only config update_keys, and a release
// build refuses it (scripts/release, TestReleaseKeyIsSet).
const KeyPlaceholder = "ed25519:PLACEHOLDER-run-go-run-./scripts/release-keygen"

// ReleaseKey is the public key of Potluck's release channel: every build
// trusts it, and config update_keys may add more. Its private half lives
// only in the potluck-release environment of sengirunners/potluck (secret
// POTLUCK_RELEASE_KEY), which deploys from main alone.
//
// PLACEHOLDER: replace KeyPlaceholder here, and RELEASE_KEY in install.sh,
// with the "public key" line `go run ./scripts/release-keygen` prints
// (TestInstallScriptCarriesTheReleaseKey holds them equal).
const ReleaseKey = "ed25519:Z9tq+TU7lP6IwxiY2qzZC+bRNEv1G1fzxlLJYHtvTMc="

// Repo publishes the releases; every release tag is TagPrefix<version>.
const (
	Repo      = "sengirunners/potluck"
	TagPrefix = "potluck-"
)

// Keys are the release keys this build trusts: ReleaseKey (unless it is
// still the placeholder) and extra (config update_keys).
func Keys(extra []string) []string {
	var keys []string
	if ReleaseKey != KeyPlaceholder {
		keys = append(keys, ReleaseKey)
	}
	for _, k := range extra {
		if k = strings.TrimSpace(k); k != "" && k != ReleaseKey {
			keys = append(keys, k)
		}
	}
	return keys
}

// Source is where one release's files come from.
type Source interface {
	// Manifest is the newest release's manifest and signature, unverified.
	Manifest() (body, sig []byte, err error)
	// File is one file of that release.
	File(name string) ([]byte, error)
	String() string
}

// Size limits on what a source hands over.
const (
	maxManifest = 1 << 20
	maxTarball  = 1 << 30
)

// Dir is a directory holding one release's files (manifest.json,
// manifest.json.sig and the tarballs).
type Dir string

func (d Dir) String() string { return string(d) }

func (d Dir) Manifest() ([]byte, []byte, error) {
	body, err := readMax(filepath.Join(string(d), release.ManifestFile), maxManifest)
	if err != nil {
		return nil, nil, err
	}
	sig, err := readMax(filepath.Join(string(d), release.SignatureFile), maxManifest)
	return body, sig, err
}

func (d Dir) File(name string) ([]byte, error) {
	if name != filepath.Base(name) {
		return nil, fmt.Errorf("release file %q", name)
	}
	return readMax(filepath.Join(string(d), name), maxTarball)
}

func readMax(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s is over %d bytes", path, limit)
	}
	return b, err
}

// GitHub reads the newest release of Repo through GitHub's API: the newest
// published release whose tag starts with TagPrefix and that carries a
// manifest. Drafts and prereleases (a yanked release) are never taken.
// Token (GH_TOKEN or GITHUB_TOKEN) is sent when set: it is what reads a
// private repository.
type GitHub struct {
	API    string // https://api.github.com
	Repo   string
	Token  string
	Client *http.Client

	tag    string
	assets map[string]string // name -> API URL
}

// NewGitHub is the GitHub source for Repo, with the token from env.
func NewGitHub(getenv func(string) string) *GitHub {
	tok := getenv("GH_TOKEN")
	if tok == "" {
		tok = getenv("GITHUB_TOKEN")
	}
	return &GitHub{API: "https://api.github.com", Repo: Repo, Token: strings.TrimSpace(tok), Client: &http.Client{Timeout: 10 * time.Minute}}
}

func (g *GitHub) String() string {
	if g.tag != "" {
		return "github.com/" + g.Repo + " release " + g.tag
	}
	return "github.com/" + g.Repo
}

// ghRelease is the part of GitHub's release listing read here.
type ghRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// newest picks the release to install from GitHub's listing (newest first).
func newest(data []byte) (ghRelease, error) {
	var rs []ghRelease
	if err := json.Unmarshal(data, &rs); err != nil {
		return ghRelease{}, fmt.Errorf("release listing: %w", err)
	}
	for _, r := range rs {
		if r.Draft || r.Prerelease || !strings.HasPrefix(r.Tag, TagPrefix) {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == release.ManifestFile {
				return r, nil
			}
		}
	}
	return ghRelease{}, errors.New("no published potluck release yet")
}

func (g *GitHub) Manifest() ([]byte, []byte, error) {
	data, err := g.get(g.API+"/repos/"+g.Repo+"/releases?per_page=30", "application/vnd.github+json", maxManifest*8)
	if err != nil {
		return nil, nil, err
	}
	r, err := newest(data)
	if err != nil {
		return nil, nil, err
	}
	g.tag, g.assets = r.Tag, map[string]string{}
	for _, a := range r.Assets {
		g.assets[a.Name] = a.URL
	}
	body, err := g.File(release.ManifestFile)
	if err != nil {
		return nil, nil, err
	}
	sig, err := g.File(release.SignatureFile)
	return body, sig, err
}

func (g *GitHub) File(name string) ([]byte, error) {
	u, ok := g.assets[name]
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", g.tag, name)
	}
	limit := int64(maxTarball)
	if strings.HasPrefix(name, release.ManifestFile) {
		limit = maxManifest
	}
	return g.get(u, "application/octet-stream", limit)
}

// get fetches u. Go's client drops the Authorization header when an asset
// download redirects to another host (GitHub's storage).
func (g *GitHub) get(u, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := Access(resp.StatusCode, g.Repo, g.Token != ""); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s is over %d bytes", u, limit)
	}
	return b, err
}

// Access turns GitHub's refusals into one plain sentence. GitHub answers a
// private repository it will not show with 404, as it does a repository with
// no release, so a 404 without a token names both.
func Access(status int, repo string, token bool) error {
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusNotFound && !token:
		return fmt.Errorf("github.com/%s answered 404 with no token: it is private (set GH_TOKEN or GITHUB_TOKEN to a token that can read it) or has no release yet. "+
			"Or install from a release directory (--release <dir>)", repo)
	case status == http.StatusNotFound:
		return fmt.Errorf("github.com/%s answered 404: the token in GH_TOKEN/GITHUB_TOKEN cannot read the repository (or it has no release yet)", repo)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("github.com/%s answered 401: the token in GH_TOKEN/GITHUB_TOKEN is not valid", repo)
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return fmt.Errorf("github.com/%s answered %d: rate limited or refused; set GH_TOKEN, or try again later", repo, status)
	}
	return fmt.Errorf("github.com/%s answered %d", repo, status)
}

// Staged is a verified release whose programs for one platform are in Dir.
type Staged struct {
	Manifest release.Manifest
	Dir      string
	Names    []string
}

// Stage fetches the newest release from src, verifies its manifest with
// keys and its tarball for goos/goarch with the manifest, and writes the
// programs into stage (which must exist and be empty). Nothing is
// installed.
func Stage(src Source, keys []string, goos, goarch, stage string) (Staged, error) {
	if len(keys) == 0 {
		return Staged{}, errors.New("this potluck trusts no release key: it is a development build, and config update_keys is empty. " +
			"Install a release (install.sh), or add the channel's key to update_keys in $POTLUCK_HOME/config.json")
	}
	body, sig, err := src.Manifest()
	if err != nil {
		return Staged{}, err
	}
	m, err := release.Potluck.Verify(body, string(sig), keys)
	if err != nil {
		return Staged{}, fmt.Errorf("refusing the release from %s: %w", src, err)
	}
	name := release.Potluck.Tarball(m.Version, goos, goarch)
	if _, ok := m.Files[name]; !ok {
		return Staged{}, fmt.Errorf("release %s has no build for %s/%s", m.Version, goos, goarch)
	}
	tb, err := src.File(name)
	if err != nil {
		return Staged{}, err
	}
	names, err := release.Potluck.Unpack(m, tb, goos, goarch, stage)
	if err != nil {
		return Staged{}, fmt.Errorf("refusing the release from %s: %w", src, err)
	}
	return Staged{Manifest: m, Dir: stage, Names: names}, nil
}

// Older reports why m must not replace the release this home installed
// last (st): a release built before it is a rollback, which a download host
// could otherwise force by serving an old signed release. "" when m may be
// installed.
func Older(m release.Manifest, st release.State, installed bool) string {
	if installed && st.Version != m.Version && st.Created.After(m.Created) {
		return fmt.Sprintf("release %s (built %s) is older than %s, which this machine installed (built %s): refusing to go back",
			m.Version, m.Created.Format(time.RFC3339), st.Version, st.Created.Format(time.RFC3339))
	}
	return ""
}
