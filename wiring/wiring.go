// Package wiring is the daemon wirings below zm's (internal/wiring, ORCH):
// Kit, the shared base (identity, projects, transports, teamlink), and
// Compute, Kit plus run targets, runners, usage and secret grants. It
// imports KIT and COMPUTE packages only (zm-eakh.12.35 G1), so a binary or
// test wired from it links no orchestration. Owned by the integrator.
package wiring

import (
	"context"

	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
	ghaccounts "github.com/sengirunners/potluck/kit/owners/github"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/usage"
)

// KitMods are the shared base's modules.
type KitMods struct {
	Net  *tailnet.Service
	Tun  *tunnel.Service
	Own  *owners.Service
	Link *teamlink.Service
}

// NewKit constructs the base modules (teamlink: join, leave, invites, the
// proxy to joined projects, the machine-op queue and heartbeat), provides
// teamlink on d (teamlink.From), sets d.Net, GitHubLogin, LinkedAccount and
// d.Team (teamlink; zm's Full replaces it with team, which embeds it), and
// registers the team transports (core.TeamTransport, SPEC 17.11) in the
// order an invite without --via tries them. A new transport (the host's own
// Tailscale) is one line here. Construct it before any module that reaches
// teamlink (run targets do in New).
func NewKit(d *core.Deps) KitMods {
	k := KitMods{Net: tailnet.New(d), Tun: tunnel.New(d), Own: ownersModule(d), Link: teamlink.New(d)}
	d.Provide(teamlink.ServiceName, k.Link)
	d.Net = k.Net.Net()
	d.AddTransport(k.Tun.TeamTransport()) // first: its code needs no Tailscale, and it is offered only while it answers
	d.AddTransport(tailnet.TeamTransport(d, d.Net))
	d.GitHubLogin, d.LinkedAccount = k.Own.GitHubLogin, k.Own.LinkedAccount
	d.Team = k.Link
	return k
}

// Kit is the shared base alone: tailnet, tunnel, owners and teamlink.
func Kit(d *core.Deps) ([]core.Module, error) {
	k := NewKit(d)
	return []core.Module{k.Net, k.Link, k.Own, k.Tun}, nil
}

// Compute is Kit plus run targets (with their relay across machines on
// teamlink), the secrets issuer (when config.json names one), runners,
// usage and the secret grants' routes.
func Compute(d *core.Deps) ([]core.Module, error) {
	k := NewKit(d)
	// The issuer after run targets (it hooks their ends) and before the
	// broker (it issues): modules close in reverse, so the broker stops first.
	rts := runtarget.New(d)
	return []core.Module{k.Net, k.Link, rts, issuers.NewModule(d), runners.New(d), usage.New(d), grants.New(d), k.Own, k.Tun}, nil
}

// ownersModule is the owners brick over its GitHub driver (SPEC 22),
// configured by config.json "github"; no client_id, no logins.
func ownersModule(d *core.Deps) *owners.Service {
	gh := d.Config.File.GitHub
	var dev owners.Device
	if gh.ClientID != "" {
		dev = ghaccounts.NewDevice(gh.WebURL, gh.ClientID, nil)
	}
	drv := ghaccounts.New(gh.APIURL, nil)
	if gh.LookupToken != "" {
		ref, err := secrets.Parse(gh.LookupToken)
		if err != nil || ref.Name == "" {
			// The error would quote the value: a token pasted in config.json.
			d.Log.Warn("owners: github.lookup_token is not a secret reference (file:, keychain: or env:); looking up without it")
		} else {
			rd := secrets.Default()
			drv.LookupToken = func(ctx context.Context) (string, error) {
				v, err := rd.Get(ctx, ref)
				return v.Reveal(), err
			}
		}
	}
	return owners.New(d, drv, dev)
}

// ComputeModules are the modules Compute adds to Kit, by the name their
// routes carry (core.Route.Module): a daemon whose routes come from all of
// them serves what potluck's commands call (daemonrecord.Record.Serves),
// whichever program runs it.
var ComputeModules = []string{"grants", "runners", "runtarget", "usage"}
