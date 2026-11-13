package runtarget

import (
	"context"
	"slices"
	"strconv"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// A box's network (SPEC 20 "Boxes", Network; zm-eakh.12.5.19).

// checkNet refuses a net that is not a box's on or off.
func checkNet(b computeapi.RunTarget, net string) error {
	switch {
	case net != "" && b.Kind != computeapi.RunTargetKindBox:
		return api.Errorf(api.CodeBadRequest, "net is for a box target")
	case net != "" && net != computeapi.NetOn && net != computeapi.NetOff:
		return api.Errorf(api.CodeBadRequest, "net is on or off")
	}
	return nil
}

// netKey is net as the request fingerprint holds it: on is the default, the
// same request as unset (and as one made before net existed).
func netKey(net string) string {
	if net == computeapi.NetOn {
		return ""
	}
	return net
}

// netFor is the net zm sends the driver: a driver that honours it (net-off)
// gets on for a trusted host's box (the binding's filtered network:
// internet, never the host, its LAN, metadata or mail) and off for a public
// host's (no egress allowlist yet: SPEC 20 open question 6) or when asked.
// One that does not is sent nothing, as before, and cannot honour an off.
// The class is read at acquire only: a renew does not ask again.
func (s *Service) netFor(ctx context.Context, b computeapi.RunTarget, d *computeapi.RunTargetDescribe, asked string) (string, error) {
	if b.Kind == computeapi.RunTargetKindBox && slices.Contains(d.Capabilities, computeapi.CapNetOff) {
		if asked != computeapi.NetOff && (b.Host == "" || s.hostClass(ctx, b) == api.MachineTrusted) {
			return computeapi.NetOn, nil
		}
		return computeapi.NetOff, nil
	}
	if asked == computeapi.NetOff {
		return "", api.Errorf(api.CodeBadRequest, "run target %s cannot make a box without a network (no %s capability)", b.Name, computeapi.CapNetOff)
	}
	return "", nil
}

// netNotHeld says why a box sent net off is ended: its driver did not answer
// exactly off (a host that put an older zm-run-tart back while its advert
// still lists net-off), so it may have a network.
func netNotHeld(answered string) string {
	if answered == "" {
		return "was sent net off and did not confirm it (no net in its answer)"
	}
	return "was sent net off and answered net " + strconv.Quote(answered)
}
