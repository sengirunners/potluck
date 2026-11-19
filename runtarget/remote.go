package runtarget

// Boxes on other machines, the authority's side (SPEC 20 "Across machines"):
// a team project's authority lists the box bindings its machines advertised
// as <binding>@<machine> and, for such a name, behaves exactly as for a local
// box (run_handles rows, areas, holder checks, the sweep, the end hook)
// except that the driver call is relayed to the host (relay.go). The host's
// operator class (teamlink's machine classes) decides secrets and
// jit_config. Owned by run targets (zm-compute-runs).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// relaySlack is what a relayed call may take over the driver's own limit:
// the host takes the op at once over its held-open poll, or (the fallback)
// with its next heartbeat answer, 15 s apart.
const relaySlack = rt.RelaySlack

// remoteDriver names a remote binding's driver in messages: the real one is
// the host's own business.
const remoteDriver = "remote"

// machineName is the <machine> of a remote binding's name: path-safe, so it
// is no more than a handle's or a URL's segment.
var machineName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// RemoteName is the name a host's binding has at an authority.
func RemoteName(binding, machine string) string { return binding + "@" + machine }

// SplitRemoteName takes <binding>@<machine> apart; ok is false for any other
// name (a local binding's name never holds an @).
func SplitRemoteName(name string) (binding, machine string, ok bool) {
	binding, machine, ok = strings.Cut(name, "@")
	if !ok || !rt.ValidName(binding) || !machineName.MatchString(machine) {
		return "", "", false
	}
	return binding, machine, true
}

// remoteStub is the binding for name with no advert behind it (a handle's
// verbs still go to its host).
func remoteStub(projectID, name string) (computeapi.RunTarget, bool) {
	binding, machine, ok := SplitRemoteName(name)
	if !ok {
		return computeapi.RunTarget{}, false
	}
	return computeapi.RunTarget{Name: name, Driver: remoteDriver, Kind: computeapi.RunTargetKindBox, Isolation: computeapi.RunTargetIsolationNone,
		Host: machine, HostBinding: binding, HostProject: projectID}, true
}

// remoteFromAdvert is the binding an advert makes at the authority: the
// class and lifetime cap are the advert's; it carries no config, images, repo
// or budget (those are the host's own).
func remoteFromAdvert(projectID string, ra HostAdvert) (computeapi.RunTarget, bool) {
	b, ok := remoteStub(projectID, RemoteName(ra.Binding, ra.Machine))
	if !ok {
		return b, false
	}
	b.Isolation, b.Credentials, b.TTLMaxS = ra.Describe.Isolation, ra.Describe.Credentials, ra.Describe.TTLMaxS
	return b, true
}

// advertOf is the advert behind a remote binding.
func (s *Service) advertOf(ctx context.Context, b computeapi.RunTarget) (HostAdvert, bool) {
	for _, ra := range s.RunTargetAdverts(ctx, b.HostProject) {
		if ra.Machine == b.Host && ra.Binding == b.HostBinding {
			return ra, true
		}
	}
	return HostAdvert{}, false
}

// remoteByName is the remote binding name has at project: one a machine
// advertises now, else one a handle of this project was made on (so its
// handles can still be asked about and released while the host is silent).
func (s *Service) remoteByName(ctx context.Context, projectID, name string, db *sql.DB) (computeapi.RunTarget, bool) {
	b, ok := remoteStub(projectID, name)
	if !ok {
		return b, false
	}
	if ra, ok := s.advertOf(ctx, b); ok {
		return remoteFromAdvert(projectID, ra)
	}
	if db != nil {
		if rows, err := listRows(ctx, db, name, false, "1=1", nil); err == nil && len(rows) > 0 {
			return b, true
		}
	}
	return computeapi.RunTarget{}, false
}

// remoteDescribe is a remote binding's describe: the host's last advert.
func (s *Service) remoteDescribe(ctx context.Context, b computeapi.RunTarget) (*computeapi.RunTargetDescribe, error) {
	ra, ok := s.advertOf(ctx, b)
	if !ok {
		return nil, api.Errorf(api.CodeUnavailable, "%s advertises no run target %s now (is it running and sharing it with this project?)", b.Host, b.HostBinding)
	}
	d := ra.Describe
	d.Name = b.Name
	return &d, nil
}

// hostClass is the operator class of b's host in b's project: public unless
// the authority says trusted (it fails closed).
func (s *Service) hostClass(ctx context.Context, b computeapi.RunTarget) string {
	return s.classAtHandOver(ctx, b.HostProject, b.Host)
}

// hostTrusted refuses what only a trusted host may be given. The refusal
// names nothing the caller sent.
func (s *Service) hostTrusted(ctx context.Context, b computeapi.RunTarget, what string) error {
	if b.Host == "" || s.hostClass(ctx, b) == api.MachineTrusted {
		return nil
	}
	return api.Errorf(api.CodeForbidden, "%s is a public host: it is given no %s, only secret-free work", b.Host, what).
		WithHint("if you trust whoever runs it: " + config.Command("machine-class", b.Host, "trusted -P <project>"))
}

// remoteViews are the bindings project's machines advertise, as list shows them.
func (s *Service) remoteViews(ctx context.Context, projectID string, describe bool) []computeapi.RunTargetView {
	var out []computeapi.RunTargetView
	for _, ra := range s.RunTargetAdverts(ctx, projectID) {
		b, ok := remoteFromAdvert(projectID, ra)
		if !ok {
			continue
		}
		d := ra.Describe
		d.Name = b.Name
		v := computeapi.RunTargetView{RunTarget: b, Effective: stricter(b, &d), Host: b.Host, HostClass: s.hostClass(ctx, b)}
		if describe {
			v.Describe = &d
		}
		out = append(out, v)
	}
	return out
}

// remoteID turns a handle of the authority (box:<binding>@<machine>/<id>)
// into the host's own (box:<binding>/<id>), or back.
func remoteID(id, from, to string) string {
	if rest, ok := strings.CutPrefix(id, computeapi.RunTargetKindBox+":"+from+"/"); ok {
		return computeapi.RunTargetKindBox + ":" + to + "/" + rest
	}
	return id
}

// callRemote relays one verb of b to its host and decodes the answer.
func (s *Service) callRemote(ctx context.Context, b computeapi.RunTarget, verb, id string, args any, out any, limit time.Duration) error {
	op := computeapi.RunTargetOp{Binding: b.HostBinding, Verb: verb, ID: remoteID(id, b.Name, b.HostBinding)}
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return err
		}
		op.Args = raw
	}
	relay := s.RelayRunTarget
	if s.relay != nil {
		relay = s.relay
	}
	res, err := relay(ctx, b.HostProject, b.Host, op, limit+relaySlack)
	if err != nil {
		if e, ok := err.(*api.Error); ok && e.Code == rt.CodeDriverTimeout {
			return fmt.Errorf("%s's driver %s: %w after %s", b.Host, verb, errTimeout, limit)
		}
		return err
	}
	if out == nil {
		return nil
	}
	if res, err = remoteHandles(res, verb, b.HostBinding, b.Name); err != nil {
		return err
	}
	if err := protocol.DecodeResult(res, out); err != nil {
		return api.Errorf(api.CodeInternal, "%s's driver %s: bad result: %v", b.Host, verb, err)
	}
	return nil
}

// remoteHandles renames the handles in an acquire or list answer from the
// host's binding name to the authority's.
func remoteHandles(res json.RawMessage, verb, from, to string) (json.RawMessage, error) {
	if verb != verbAcquire && verb != verbList {
		return res, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(res, &m); err != nil {
		return nil, api.Errorf(api.CodeInternal, "a relayed %s answered no object", verb)
	}
	rename := func(raw json.RawMessage) json.RawMessage {
		var o map[string]json.RawMessage
		if json.Unmarshal(raw, &o) != nil {
			return raw
		}
		var h string
		if json.Unmarshal(o["handle"], &h) == nil && h != "" {
			o["handle"], _ = json.Marshal(remoteID(h, from, to))
		}
		b, _ := json.Marshal(o)
		return b
	}
	if verb == verbAcquire {
		return json.Marshal(json.RawMessage(rename(res)))
	}
	var hs []json.RawMessage
	if json.Unmarshal(m["handles"], &hs) == nil {
		for i := range hs {
			hs[i] = rename(hs[i])
		}
		m["handles"], _ = json.Marshal(hs)
	}
	return json.Marshal(m)
}
