package runtarget

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// FakeHost makes machine a host of project (its id) that advertises ads, as
// its heartbeat would, with relay carrying every op to it in place of the
// team link.
func (s *Service) FakeHost(project, machine string, ads []computeapi.RunTargetAdvert,
	relay func(ctx context.Context, op computeapi.RunTargetOp) (json.RawMessage, error)) {
	s.enroll(machine)
	s.adverts.set(project, machine, ads)
	s.relay = func(ctx context.Context, _, _ string, op computeapi.RunTargetOp, _ time.Duration) (json.RawMessage, error) {
		return relay(ctx, op)
	}
}

// enroll makes machine an enrolled host, as well as any enrolled before.
func (s *Service) enroll(machine string) {
	prev := s.enrolled
	s.enrolled = func(ctx context.Context, p, m string) bool { return m == machine || (prev != nil && prev(ctx, p, m)) }
}

// Beat is the run_targets part of a heartbeat that machine, an enrolled host,
// sends project's authority (this one) carrying adverts, as the authority
// serves it.
func (s *Service) Beat(ctx context.Context, project api.Project, machine string, adverts []computeapi.RunTargetAdvert) error {
	s.enroll(machine)
	raw, err := json.Marshal(adverts)
	if err != nil {
		return err
	}
	ctx = core.WithSubject(ctx, &authz.Subject{Machine: machine})
	_, err = s.serveRunTargets(ctx, project, raw)
	return err
}

// RunOpHere is this machine taking a relayed op of project as a host does
// (runRunTargetOp), its answer as the authority receives it.
func (s *Service) RunOpHere(ctx context.Context, project string, op computeapi.RunTargetOp) (json.RawMessage, error) {
	mop := api.MachineOp{ID: "op-test", Kind: opRunTarget}
	setRTOp(&mop, &op)
	return relayAnswer(reportResult(report(s.runRunTargetOp(ctx, project, mop))))
}

// report is a host's report of an op's answer, as teamlink makes it: the
// payload, and of an error its code, message and hint.
func report(res json.RawMessage, err error) api.MachineOpReport {
	rep := api.MachineOpReport{Payload: res}
	if err != nil {
		rep.Error, rep.Code, rep.Hint = teamlink.ErrParts(err)
	}
	return rep
}

// AsRelayed is HostCall's answer to verb as this build's host reports it and
// this build's authority receives it.
func AsRelayed(verb string, res json.RawMessage, err error) (json.RawMessage, error) {
	return relayAnswer(reportResult(report(hostAnswer(verb, res, err))))
}

// AsRelayedToOld is the same report as an authority built before the
// no-handle mark reads it: a failure's code, message and hint, no payload.
func AsRelayedToOld(verb string, res json.RawMessage, err error) error {
	rep := report(hostAnswer(verb, res, err))
	return teamlink.OpResult{Code: rep.Code, Msg: rep.Error, Hint: rep.Hint, Fail: rep.Error != ""}.Err()
}

// AsRelayedFromOld is the error of a host built before the mark (it reports
// no payload with a failure), as this build's authority receives it.
func AsRelayedFromOld(err error) (json.RawMessage, error) {
	return relayAnswer(reportResult(report(nil, err)))
}

// JoinForTest records this machine as joined to the team project id under
// slug, as teamlink holds a join (no authority behind it).
func (s *Service) JoinForTest(id, slug string) { s.JoinStateForTest(id, slug, api.RemoteOK) }

// JoinStateForTest is JoinForTest with the join in state (api.RemoteRevoked:
// access ended for now).
func (s *Service) JoinStateForTest(id, slug, state string) {
	s.link.Add(&teamlink.Remote{R: api.Remote{Project: api.ProjectRef{ID: id, Slug: slug}, State: state}})
}

// LeaveForTest forgets the join of project id.
func (s *Service) LeaveForTest(id string) { s.link.Forget(id) }

// LoadJoinsForTest is teamlink's load of the joins this machine records, as
// at the daemon's start.
func (s *Service) LoadJoinsForTest(ctx context.Context) error { return s.link.Load(ctx) }

// ForgetHostMarkForTest takes registry.db's mark of the host key migration
// away, as a restore of registry.db from before the upgrade does.
func (s *Service) ForgetHostMarkForTest() error {
	_, err := s.d.Store.Registry().Exec(`DELETE FROM run_host_keys`)
	return err
}

// SetAcquireLimit shortens an acquire's time limit, so a test can see one
// cut off.
func (s *Service) SetAcquireLimit(d time.Duration) { s.acquireWithin = d }

// SetDepsPoll sets the wait between asks for a dependency layer another box
// is building.
func (s *Service) SetDepsPoll(d time.Duration) { s.depsPoll = d }

// SetDepsRecipe replaces kind's recipe for the test's life: the reference
// driver's boxes run sh on this machine, so no test runs npm.
func SetDepsRecipe(t interface{ Cleanup(func()) }, kind string, recipe func(scripts bool) string) {
	k := depsKinds[kind]
	old := k.recipe
	k.recipe = recipe
	depsKinds[kind] = k
	t.Cleanup(func() { k.recipe = old; depsKinds[kind] = k })
}

// SetHostClass makes every host's class class (trusted or public).
func (s *Service) SetHostClass(class string) {
	s.opClass = func(context.Context, string, string) string { return class }
}

// DropForgottenForTest runs teamlink's DropForgotten hooks for project id,
// as a forget of its authority key that drops host state does.
func (s *Service) DropForgottenForTest(ctx context.Context, id string) ([]string, error) {
	return s.link.DropForgotten(ctx, id)
}

// CancelsUnknownMax is how many cancels of request_ids it never received
// one actor may send in 24 hours.
const CancelsUnknownMax = cancelsUnknownMax

// CancelsActorRelayedMax is how many cancels a lending host relays in 24
// hours for one actor of a project.
const CancelsActorRelayedMax = cancelsActorRelayedMax

// AddHolderCheck registers f as the session module would: whether a holder
// is live (core.Deps.HolderLive).
func (s *Service) AddHolderCheck(f core.HolderCheck) { s.d.AddHolderCheck(f) }

// SetDatabaseRecipesForTest sets project's [[databases]] recipes, as its
// owner's project.toml would.
func (s *Service) SetDatabaseRecipesForTest(ctx context.Context, project string, recipes any) error {
	_, err := s.d.Projects.SetSection(ctx, project, "databases", func(any) (any, error) { return recipes, nil })
	return err
}

// StartingOn counts an acquire on a host binding in, as HostCall does, until
// the func it returns is called (hostimages.go).
func (s *Service) StartingOn(binding string) func() { return s.images.enter(binding) }

// SetImagePins replaces the pin table the host image refresh reads.
func (s *Service) SetImagePins(f func() []imagepins.DefaultPin) { s.images.pins = f }

// SetImagePull replaces the host image refresh's pull.
func (s *Service) SetImagePull(f func(ctx context.Context, cli, image string) error) {
	s.images.pull = f
}

// AfterListed runs f in a refresh after it last listed a binding's live
// handles, before it re-checks the gate.
func (s *Service) AfterListed(f func()) { s.images.listed = f }
