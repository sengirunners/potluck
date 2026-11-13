package runtarget

// Named volumes (SPEC 20 "Boxes", Named volumes; zm-eakh.12.32.1):
// a box acquire may name volumes, directories that outlive
// the box on its host and mount in every later box of the holder's that names
// them. The driver keeps them per owner and binding, and decides the mode's
// lock (rw one box at a time; ro and cow shared). The owner is zm's word, sent
// as volume_owner: the caller's identity (every session of one identity shares
// its volumes), or, for an acquire made for another actor by the machine
// owner, that actor. A renter never names an owner, so never reaches another
// holder's volume. Only a trusted host keeps one: a public host could read it.

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

var volumeName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// checkVolumes refuses an acquire's volumes where none may be kept. The
// driver checks each path itself (it knows the box's HOME).
func (s *Service) checkVolumes(ctx context.Context, b computeapi.RunTarget, d *computeapi.RunTargetDescribe, req computeapi.AcquireRunTargetRequest) error {
	if len(req.Volumes) == 0 {
		return nil
	}
	switch {
	case b.Kind != computeapi.RunTargetKindBox:
		return api.Errorf(api.CodeBadRequest, "volumes are for a box target")
	case len(req.Volumes) > computeapi.VolumesMax:
		return api.Errorf(api.CodeBadRequest, "at most %d volumes", computeapi.VolumesMax)
	case req.Persist || req.JITConfig != "":
		return api.Errorf(api.CodeBadRequest, "a box with volumes is neither persistent nor a GitHub runner's")
	case !slices.Contains(d.Capabilities, computeapi.CapVolumes):
		return api.Errorf(api.CodeBadRequest, "run target %s mounts no volumes (potluck-driver-%s lists no volumes capability)", b.Name, b.Driver)
	}
	for _, v := range req.Volumes {
		switch {
		case !volumeName.MatchString(v.Name):
			return api.Errorf(api.CodeBadRequest, "volume name %q: lowercase letters, digits, . _ -, at most 63", v.Name)
		case v.Mode != "" && v.Mode != computeapi.VolumeRW && v.Mode != computeapi.VolumeRO && v.Mode != computeapi.VolumeCOW:
			return api.Errorf(api.CodeBadRequest, "volume %s: mode is rw, ro or cow", v.Name)
		case v.Path == "" || len(v.Path) > 256 || strings.ContainsAny(v.Path, "\x00\n\r") || strings.HasPrefix(v.Path, "/"):
			return api.Errorf(api.CodeBadRequest, "volume %s: a path under the box user's HOME (relative; never absolute)", v.Name)
		case v.New:
			return api.Errorf(api.CodeBadRequest, "volume %s: new is the answer's, not the request's", v.Name)
		case strings.HasPrefix(v.Name, depsPrefix): // deps.go: a layer's name is the daemon's, derived from its inputs
			return api.Errorf(api.CodeBadRequest, "volume %s: names starting %s are dependency layers, named by the daemon (ask for one with deps)", v.Name, depsPrefix)
		case v.Sealed:
			return api.Errorf(api.CodeBadRequest, "volume %s: sealed is the daemon's, for a dependency layer (ask for one with deps)", v.Name)
		case v.Checkout:
			return api.Errorf(api.CodeBadRequest, "volume %s: checkout is the daemon's, for a dependency layer (ask for one with deps and a commit)", v.Name)
		}
	}
	return s.hostTrusted(ctx, b, "named volume")
}

// volumeOwner is whose an acquire's volumes are: the caller's identity, or
// the actor the machine owner acquired for.
func volumeOwner(sub *authz.Subject, actor string) string {
	if sub == nil || sub.Identity.ID == "" || actor != sub.Address {
		return "actor:" + actor
	}
	return "identity:" + sub.Identity.ID
}
