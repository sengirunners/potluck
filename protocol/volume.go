package protocol

import (
	"fmt"
	"regexp"
	"strings"
)

// Named volumes (SPEC 20 "Boxes", Named volumes): the one rule for a
// volume's path, which every driver keeps (vmhost and conformance's
// reference driver use this function): relative to the box user's HOME,
// never absolute, every segment a plain name (no empty segment, "." or
// ".."), and none of the characters a mount option list splits on (, =).
// A driver that knows HOME also refuses a path HOME makes a system directory.

var volumePathRe = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)

// VolumePathOK says whether p is a volume path under that rule.
func VolumePathOK(p string) bool {
	if p == "" || len(p) > 256 || strings.HasPrefix(p, "/") || !volumePathRe.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// VolumePathsOverlap says whether two volume paths are one, or one holds the other.
func VolumePathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// Volume modes. VolumeBuild (capability volumes-seal) makes a volume that
// does not exist yet, one box's like rw; that box's release with seal makes
// it sealed: never written again, readable ro or cow with sealed. Any other
// end of the builder deletes the volume.
const (
	VolumeRW    = "rw"
	VolumeRO    = "ro"
	VolumeCOW   = "cow"
	VolumeBuild = "build"
)

// ReasonVolumeMissing identifies an acquire refused because a requested
// sealed volume is absent or unsealed. Older drivers may omit this data.
const ReasonVolumeMissing = "volume_missing"

// VolumeMissing is optional not_found data naming the requested sealed volume.
// It names no other holder's state.
type VolumeMissing struct {
	Reason string `json:"reason"`
	Name   string `json:"name"`
}

// ReasonBuilding is the reason in an unavailable answer's data when another
// live box is building the volume.
const ReasonBuilding = "building"

// Busy is an unavailable answer's data: why, and the seconds to wait before
// asking again.
type Busy struct {
	Reason string `json:"reason"`
	RetryS int    `json:"retry_s"`
}

// KeepMax is how many paths a release's keep may name.
const KeepMax = 4096

// KeepPathsOK checks a release's keep (capability volumes-keep): at least
// one path, at most KeepMax, each a volume path (relative to the volume's
// root here, plain segments).
func KeepPathsOK(keep []string) error {
	if len(keep) == 0 || len(keep) > KeepMax {
		return fmt.Errorf("keep names 1 to %d paths", KeepMax)
	}
	for _, k := range keep {
		if !VolumePathOK(k) {
			return fmt.Errorf("keep path %q is not relative with plain segments", k)
		}
	}
	return nil
}

// KeepDirs is the directories on the way to keep's paths (every proper
// prefix of one), which a sealing driver keeps as directories while it
// deletes their other entries.
func KeepDirs(keep []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keep {
		segs := strings.Split(k, "/")
		for i := 1; i < len(segs); i++ {
			if d := strings.Join(segs[:i], "/"); !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}
