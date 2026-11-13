package cli

import (
	"runtime/debug"

	"github.com/sengirunners/potluck/kit/api"
)

// stampVersion sets api.Version and api.Commit from what go build recorded
// (the module version and vcs.revision, stamped in a git checkout) when no
// -ldflags set them, so --version and status name the build, not
// "v0.0.0-dev (unknown)".
func stampVersion() {
	if bi, ok := debug.ReadBuildInfo(); ok && api.Commit == "unknown" {
		api.Version, api.Commit = versionOf(bi, api.Version, api.Commit)
	}
}

// versionOf is the version and commit bi records, else version and commit.
// The commit stays hex (release.SameCommit); a modified tree shows in the
// version ("+dirty"), as go build writes it there.
func versionOf(bi *debug.BuildInfo, version, commit string) (string, string) {
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			commit = s.Value[:min(12, len(s.Value))]
		}
	}
	return version, commit
}
