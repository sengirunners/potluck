// Package tartpin is Potluck's record of the upstream Tart releases it
// installs for a lender (Builds) and the install itself (Place): the
// release's tart.tar.gz, checked for its pinned size and SHA-256 before
// anything is unpacked, and its tart.app moved into the user's own
// Applications folder, where vmhost.FindTart looks. No root is needed.
//
// Tart is OpenAI's, under FSL-1.1-ALv2 (License), not Potluck's
// Apache-2.0. Potluck downloads the unchanged release from its maker on the
// lender's behalf; it publishes no copy of its own. Beyond the
// standard library it uses golang.org/x/sys (the Mac's atomic rename) and
// kit/atomicfile.
package tartpin

import (
	_ "embed"
	"slices"
)

// Build is one upstream Tart release Potluck pins.
type Build struct {
	// Version is the release's tag and what `tart --version` prints.
	Version string
	// URL is the release's tart.tar.gz. The file there is trusted only
	// for its Bytes and SHA256.
	URL string
	// Bytes and SHA256 identify tart.tar.gz. They are measured from the
	// download and match the digest GitHub shows for the asset and the
	// release's own tart_<version>_checksums.txt.
	Bytes  int64
	SHA256 string
	// ProgramSHA256 is the SHA-256 of tart.app/Contents/MacOS/tart in it.
	ProgramSHA256 string
	// MinMacOS is the oldest macOS major version the program runs on (its
	// Mach-O minimum).
	MinMacOS int
	// Arches are the hardware the program is built for (a universal
	// binary has both), as Go names them.
	Arches []string
	// LicenseSHA256 is the SHA-256 of the LICENSE the release carries
	// (License).
	LicenseSHA256 string
	// Withdrawn, when set, is why the release must no longer be
	// installed. A build never leaves Builds once a potluck has pinned it
	// (a test holds the list); it is withdrawn instead.
	Withdrawn string
}

// License is the upstream Tart licence (FSL-1.1-ALv2, OpenAI), the
// LICENSE file in every pinned release, not Potluck's Apache-2.0.
//
//go:embed UPSTREAM-LICENSE
var License string

// licenseSHA256 is License's SHA-256 (a test holds them equal).
const licenseSHA256 = "27f1c29e91560c0d39ec9c8826734f8c6553b01c144635041fe5427faf45c9e2"

// Upstream is Tart's repository; its releases page is where a lender gets
// Tart by hand.
const Upstream = "https://github.com/openai/tart"

// releaseURL is release version's tart.tar.gz.
func releaseURL(version string) string {
	return Upstream + "/releases/download/" + version + "/tart.tar.gz"
}

// Builds are the Tart releases Potluck pins, newest first.
var Builds = []Build{{
	Version:       "2.42.0",
	URL:           releaseURL("2.42.0"),
	Bytes:         22983178,
	SHA256:        "a95dda214b5f676298430d665a8174992e6b300dd20c653385b60481ae819ddf",
	ProgramSHA256: "5e079375399baeee828b053bc64fac0238a801cc0b5254344b4bc0c8a0626e69",
	MinMacOS:      13,
	Arches:        []string{"arm64", "amd64"},
	LicenseSHA256: licenseSHA256,
}}

// Latest is the newest pinned build that is not withdrawn.
func Latest() (Build, bool) {
	for _, b := range Builds {
		if b.Withdrawn == "" {
			return b, true
		}
	}
	return Build{}, false
}

// ByVersion is the pinned build of this version.
func ByVersion(v string) (Build, bool) {
	for _, b := range Builds {
		if b.Version == v {
			return b, true
		}
	}
	return Build{}, false
}

// Runs is true when b's program runs on a Mac of this macOS major version
// and hardware architecture. major 0 is an unknown version.
func (b Build) Runs(major int, arch string) bool {
	return major >= b.MinMacOS && slices.Contains(b.Arches, arch)
}
