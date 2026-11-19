package server

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// limitGate is the one place a request with a limited token (authz.Cap.Limit)
// goes on or stops, right after Authenticate and before the request gates,
// the team proxy and the router. A route that admits the limit gets the
// request as it is; a public route gets it with no subject and no token; any
// other route, no route at all and the fallback answer ErrTokenLimit before
// anything looks an object up.
func (dm *Daemon) limitGate(w http.ResponseWriter, r *http.Request, limit string) (*http.Request, bool) {
	if dm.Router != nil {
		switch public, admitted := dm.Router.LimitRoute(r, limit); {
		case public:
			return core.WithoutLimited(r), true
		case admitted:
			return r, true
		}
	}
	api.WriteError(w, core.ErrTokenLimit())
	return nil, false
}

// mintLimited mints a limited token (POST /v0/tokens with a limit): it
// resolves the named device on this daemon's tailnet, then the registry
// checks every other rule before writing anything.
func (dm *Daemon) mintLimited(ctx context.Context, req api.MintTokenRequest) (api.MintTokenResponse, error) {
	if req.Limit == "" || strings.TrimSpace(req.Node) == "" {
		return api.MintTokenResponse{}, api.Errorf(api.CodeBadRequest, "a limited token needs both a limit and a device (node)")
	}
	pin, dev, err := dm.resolveDevice(ctx, req.Node)
	if err != nil {
		return api.MintTokenResponse{}, err
	}
	limits := dm.Router.Limits()
	out, err := dm.Deps.Projects.MintLimited(ctx, core.SubjectOf(ctx), req, project.LimitedMint{
		Pin:      pin,
		MaxTTL:   dm.Deps.Config.LimitedTokenMaxTTL(),
		Admitted: func(l string) bool { return slices.Contains(limits, l) },
	})
	if err != nil {
		return out, err
	}
	out.Device = &dev
	return out, nil
}

// resolveDevice finds the tailnet peer a limited token is to be pinned to,
// by its MagicDNS name or the first label of it, and
// refuses one that is not a single person's own device: this daemon's own
// node, a tagged node, a node named as a daemon, a node that relays other
// hosts' traffic, a node shared in from another tailnet, or one whose node
// key the tailnet does not report.
func (dm *Daemon) resolveDevice(ctx context.Context, name string) (project.Pin, api.TokenDevice, error) {
	refuse := func(format string, a ...any) (project.Pin, api.TokenDevice, error) {
		return project.Pin{}, api.TokenDevice{}, api.Errorf(api.CodeBadRequest, format, a...)
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	net := dm.Deps.Net
	if net == nil {
		return project.Pin{}, api.TokenDevice{}, api.Errorf(api.CodeUnavailable, "this daemon has no tailnet: a limited token works only from a tailnet device")
	}
	self, err := net.Self(ctx)
	if err != nil || !self.Running {
		return project.Pin{}, api.TokenDevice{}, api.Errorf(api.CodeUnavailable, "this daemon's tailnet node is not running").WithHint(config.Command("tailnet up"))
	}
	var found []core.NetPeer
	for _, p := range self.PeerList {
		dns := strings.ToLower(p.DNSName)
		label, _, _ := strings.Cut(dns, ".")
		// The tailnet's own name for the node, never its self-reported
		// host name, which any device may set to anything.
		if name != "" && (dns == name || label == name) {
			found = append(found, p)
		}
	}
	switch {
	case len(found) == 0:
		return project.Pin{}, api.TokenDevice{}, api.Errorf(api.CodeNotFound, "no device named %q on this daemon's tailnet", name)
	case len(found) > 1:
		return project.Pin{}, api.TokenDevice{}, api.Errorf(api.CodeConflict, "%d devices answer to %q", len(found), name).
			WithHint("name the device by its full MagicDNS name")
	}
	p := found[0]
	label, _, _ := strings.Cut(p.DNSName, ".")
	switch {
	case p.NodeID == "" || p.NodeKey == "":
		return refuse("the tailnet reports no node ID or key for %s", p.DNSName)
	case p.NodeID == self.NodeID:
		return refuse("%s is this daemon's own node", p.DNSName)
	case p.Tagged:
		return refuse("%s is a tagged node, not a person's device", p.DNSName)
	case api.IsDaemonNode(p.Host) || api.IsDaemonNode(label):
		return refuse("%s is named as a daemon's node", p.DNSName)
	case p.Relays:
		return refuse("%s carries other hosts' traffic (subnet routes or exit node)", p.DNSName)
	case p.Shared:
		return refuse("%s is shared in from another tailnet", p.DNSName)
	}
	return project.Pin{NodeID: p.NodeID, NodeKey: p.NodeKey, Name: p.DNSName},
		api.TokenDevice{Name: p.DNSName, NodeID: p.NodeID, Login: p.Login, OS: p.OS}, nil
}
