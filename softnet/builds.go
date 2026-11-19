// Package softnet is Potluck's one record of which Softnet a Mac can use
// for Tart's isolated network (For), the Softnet builds Potluck itself
// pins (Builds), and the managed install of one of them (Store): root
// owned, setuid, kept beside the build it replaced so it can roll back.
//
// Softnet is the helper Tart starts for each VM on its isolated network. It
// runs as root (setuid), so a build here is identified by the SHA-256 of the
// exact file, pinned in this source: a potluck from a signed release carries
// the pins, and the install refuses any other file. Standard library only.
package softnet

import (
	_ "embed"
	"fmt"
	"strings"
)

// Build is one Softnet build Potluck pins.
type Build struct {
	// ID names the build: the version its --version prints, without the
	// program name and the source commit. It is the install directory's name.
	ID string
	// Version is exactly what `softnet --version` prints (trimmed).
	Version string
	// SHA256 and Bytes identify the executable file.
	SHA256 string
	Bytes  int64
	// Arch is the hardware it runs on ("arm64").
	Arch string
	// MinMacOS and MaxMacOS are the macOS major versions For offers it on.
	MinMacOS, MaxMacOS int
	// Validated is true once the build has passed Potluck's isolation
	// checks on a real Mac of each offered macOS version. Until then For
	// does not recommend it and an install needs --candidate.
	Validated bool
	// Source is where the build came from.
	Source Source
	// LicenseSHA256 is the SHA-256 of the upstream license it is under
	// (License), installed beside it.
	LicenseSHA256 string
	// URL is where an install downloads it. The file there is trusted
	// only for its SHA256.
	URL string
	// Withdrawn, when set, is why the build must no longer run: For never
	// offers it, an install refuses it, and an installed copy fails its
	// check, so preflight stops a Mac lending on it. A build is never
	// removed from Builds once a potluck has pinned it (a test holds the
	// list); it is withdrawn instead, so the reason reaches every Mac.
	Withdrawn string
}

// Source is a build's provenance: the upstream repository, the upstream
// commit it starts from, the upstream change carried onto it, and the
// commit and tree it was built from.
type Source struct {
	Upstream string `json:"upstream"`
	Base     string `json:"base"`
	Patch    string `json:"patch"`
	Commit   string `json:"commit"`
	Tree     string `json:"tree"`
	Recipe   string `json:"recipe"`
}

// License is the upstream Softnet license (FSL-1.1-ALv2, OpenAI), not
// Potluck's Apache-2.0. Every build here is under it; it ships beside the
// installed binary and in every published copy.
//
//go:embed UPSTREAM-LICENSE
var License string

// licenseSHA256 is License's SHA-256 (a test holds them equal).
const licenseSHA256 = "27f1c29e91560c0d39ec9c8826734f8c6553b01c144635041fe5427faf45c9e2"

// ReleaseRepo is the repository whose releases carry the published builds,
// each under the tag ReleaseTag(id).
const ReleaseRepo = "sengirunners/potluck"

// ReleaseTag is the GitHub release tag of build id. It never starts with
// "potluck-", so install.sh and potluck upgrade never take it for a
// Potluck release.
func ReleaseTag(id string) string { return "softnet-" + id }

// AssetName is the published executable's file name for build id.
func AssetName(id, arch string) string { return "softnet-" + id + "-darwin-" + arch }

func releaseURL(id, arch string) string {
	return "https://github.com/" + ReleaseRepo + "/releases/download/" + ReleaseTag(id) + "/" + AssetName(id, arch)
}

// Builds are the Softnet builds Potluck pins, newest first.
var Builds = []Build{{
	ID:       "0.23.0-potluck-pr227-candidate.1",
	Version:  "softnet 0.23.0-potluck-pr227-candidate.1-1f987609106e827034b2d1d059523c74d4486dc0",
	SHA256:   "a85239c278844ad7eb381bacdba2fb709157044f13ab499feba003a59acf3ce4",
	Bytes:    6232608,
	Arch:     "arm64",
	MinMacOS: 15, MaxMacOS: 15,
	Source: Source{
		Upstream: "https://github.com/openai/softnet",
		Base:     "e5fd48cf033ed0ec376710607187d761e60c2374 (0.23.0)",
		Patch:    "https://github.com/openai/softnet/pull/227 (merge 07d0a36b1a56b828e5c18d79ee1deaddca6b48b0): protocol and port rules",
		Commit:   "1f987609106e827034b2d1d059523c74d4486dc0",
		Tree:     "049741e977e969e2e27dfe74c12d24d5474dda14",
		Recipe:   "cargo build --release --frozen --target aarch64-apple-darwin, toolchain nightly-2026-11-18, MACOSX_DEPLOYMENT_TARGET=15.0, CARGO_PROFILE_RELEASE_STRIP=debuginfo; two clean checkouts build byte-identical files",
	},
	LicenseSHA256: licenseSHA256,
	URL:           releaseURL("0.23.0-potluck-pr227-candidate.1", "arm64"),
}}

// BySHA256 is the pinned build whose executable has this SHA-256.
func BySHA256(sum string) (Build, bool) {
	for _, b := range Builds {
		if b.SHA256 == sum {
			return b, true
		}
	}
	return Build{}, false
}

// ByID is the pinned build named id.
func ByID(id string) (Build, bool) {
	for _, b := range Builds {
		if b.ID == id {
			return b, true
		}
	}
	return Build{}, false
}

// Command is how preflight and boot words name Potluck's softnet command
// (the potluck program's; a program that mounts Potluck's commands has
// the same words under its own prefix).
const Command = "potluck softnet"

// Upstream Softnet: the first release that accepts the protocol and port
// rules Potluck isolates a box with, and the macOS it needs (its newer
// vmnet calls).
const (
	UpstreamMinVersion = "0.25.0"
	UpstreamMinMacOS   = 26
	UpstreamReleases   = "https://github.com/openai/softnet/releases/latest"
)

// Kind is where a Mac gets its Softnet.
type Kind int

const (
	// None: no Softnet that enforces Potluck's rules runs here.
	None Kind = iota
	// Upstream: the latest upstream release.
	Upstream
	// Managed: Build, through the managed install.
	Managed
)

// Advice is which Softnet a Mac should use.
type Advice struct {
	Kind  Kind
	Build Build // Kind Managed
	// Candidate is true for a Managed build that is not Validated yet.
	Candidate bool
}

// For is which Softnet a Mac of this macOS major version and hardware
// architecture ("arm64", "amd64") uses: the version matrix's one home.
// major 0 is an unknown version.
func For(major int, arch string) Advice {
	if arch != "arm64" || major == 0 {
		return Advice{Kind: None}
	}
	if major >= UpstreamMinMacOS {
		return Advice{Kind: Upstream}
	}
	for _, b := range Builds {
		if b.Withdrawn == "" && b.Covers(major, arch) {
			return Advice{Kind: Managed, Build: b, Candidate: !b.Validated}
		}
	}
	return Advice{Kind: None}
}

// Covers is true when b is built for this macOS major version and
// architecture.
func (b Build) Covers(major int, arch string) bool {
	return b.Arch == arch && major >= b.MinMacOS && major <= b.MaxMacOS
}

// Text says what a Mac of this major version should do to get a Softnet
// that enforces Potluck's rules. command is how a person types Potluck's
// softnet command ("potluck softnet").
func (a Advice) Text(major int, command string) string {
	install := command + " install"
	switch {
	case a.Kind == Upstream:
		return fmt.Sprintf("install the latest upstream Softnet (%s or newer, %s)", UpstreamMinVersion, UpstreamReleases)
	case a.Kind == Managed && !a.Candidate:
		return fmt.Sprintf("install Potluck's Softnet build for macOS %d with `%s` (upstream %s and newer need macOS %d)", major, install, UpstreamMinVersion, UpstreamMinMacOS)
	case a.Kind == Managed:
		return fmt.Sprintf("upstream Softnet %s and newer need macOS %d, and older releases cannot enforce Potluck's protocol and port rules; Potluck's macOS %d build %s is a candidate not yet validated on a real macOS %d Mac (to help validate it: `%s --candidate`)", UpstreamMinVersion, UpstreamMinMacOS, major, a.Build.ID, major, install)
	case major == 0:
		return "no Softnet known to enforce Potluck's protocol and port rules runs on this Mac (its macOS version is unknown)"
	default:
		return fmt.Sprintf("no Softnet known to enforce Potluck's protocol and port rules runs on macOS %d: upstream %s and newer need macOS %d%s", major, UpstreamMinVersion, UpstreamMinMacOS, managedRange())
	}
}

// managedRange names the macOS versions Potluck's own builds cover.
func managedRange() string {
	var vs []string
	for _, b := range Builds {
		if b.Withdrawn != "" {
			continue
		}
		if b.MinMacOS == b.MaxMacOS {
			vs = append(vs, fmt.Sprintf("%d", b.MinMacOS))
		} else {
			vs = append(vs, fmt.Sprintf("%d-%d", b.MinMacOS, b.MaxMacOS))
		}
	}
	if len(vs) == 0 {
		return ""
	}
	return ", and Potluck's own builds cover macOS " + strings.Join(vs, ", ")
}

// BootHint is what to do when a boot shows this Mac's Softnet refusing
// Potluck's rules, for any macOS version: never the latest upstream release
// on a Mac too old for it.
// command is how a person types Potluck's softnet command.
func BootHint(command string) string {
	return fmt.Sprintf("on macOS %d or newer install the latest upstream Softnet (%s or newer, %s); on an older macOS run `%s status` for the build that covers it, if any",
		UpstreamMinMacOS, UpstreamMinVersion, UpstreamReleases, command)
}
