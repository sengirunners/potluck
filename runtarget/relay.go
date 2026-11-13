package runtarget

// Boxes on other machines (zm-eakh.12.5.10, SPEC 20 "Across machines"): a
// host machine's box bindings shared with a team project are advertised in
// its heartbeat for that project; the project's authority keeps the latest
// set per machine (memory only; a machine that stops beating drops out) and
// relays run-target driver verbs to the host as machine ops (run_target,
// on teamlink's queue): taken by the host's held-open poll or a heartbeat
// answer, recorded before they run, run at most once, the result posted at
// once or carried by the next beat. Same pattern as the
// team's session relay. The driver's arguments (an exec's argv,
// stdin and secrets) and its answer never reach a database: the authority
// holds them in memory for the one call (teamlink.OpMem), the host's report
// is blanked once an answer took it. Everything here rides teamlink (KIT),
// reached through core.Deps (teamlink.From): the op kind, the heartbeat's
// "run_targets" part, the proxy's route rows (relay_routes.go) and the
// machine classes. A daemon wired with KIT and compute only lends and uses
// boxes across machines.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Tunables (tests shorten them).
var (
	// advertStale: an advert older than this, or a host whose last beat is
	// (it stopped beating), is not listed or waited on. Presence goes stale
	// at 60 s too.
	advertStale = 60 * time.Second
	// advertMax bounds the bindings one machine advertises per project.
	advertMax = 32
)

var advertName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// runAdverts is the authority's latest advert set per project and machine.
type runAdverts struct {
	mu sync.Mutex
	by map[string]map[string]advertSet // project -> machine -> set
}

type advertSet struct {
	at  time.Time
	ads []computeapi.RunTargetAdvert
}

// set replaces machine's adverts for project, from a beat now (none: it
// lends nothing).
func (a *runAdverts) set(project, machine string, ads []computeapi.RunTargetAdvert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.by == nil {
		a.by = map[string]map[string]advertSet{}
	}
	if a.by[project] == nil {
		a.by[project] = map[string]advertSet{}
	}
	a.by[project][machine] = advertSet{at: time.Now(), ads: ads}
}

// drop forgets machine's adverts for project.
func (a *runAdverts) drop(project, machine string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.by[project], machine)
}

// dropSet forgets machine's adverts for project if they are still the set
// stored at at (a beat since then stored newer ones, checked afresh).
func (a *runAdverts) dropSet(project, machine string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if set, ok := a.by[project][machine]; ok && set.at.Equal(at) {
		delete(a.by[project], machine)
	}
}

// heard reports whether machine beat for project within advertStale
// (teamlink keeps every machine's last beat).
func (s *Service) heard(project, machine string) bool {
	return time.Since(s.link.LastBeat(project, machine)) <= advertStale
}

// cleanAdverts keeps the adverts of a heartbeat that are well formed: a box
// of the untrusted-capable class (isolation ephemeral, credentials false), a
// path-safe name once each, bounded text. The describe is data a host wrote:
// the authority reads only these fields. A host never advertises another
// class (HostAdverts), so one that does is not believed: its advert is
// dropped here, where every reader (list, describe, acquire) takes it from,
// and no binding at this authority ever reads as a credentialed or unisolated
// box on another machine, whatever its host's operator class.
func cleanAdverts(in []computeapi.RunTargetAdvert) []computeapi.RunTargetAdvert {
	var out []computeapi.RunTargetAdvert
	seen := map[string]bool{}
	for _, ad := range in {
		d := ad.Describe
		if len(out) >= advertMax || !advertName.MatchString(ad.Binding) || seen[ad.Binding] || d.Kind != computeapi.RunTargetKindBox || d.Size == nil {
			continue
		}
		if !(computeapi.RunTargetClass{Isolation: d.Isolation, Credentials: d.Credentials}).UntrustedCapable() {
			continue
		}
		seen[ad.Binding] = true
		d.Why = cutLine(d.Why, 300)
		d.Runners = protocol.CleanRunners(d.Runners, protocol.MaxRunners)
		d.Host = protocol.CleanHost(d.Host)
		d.Name = ad.Binding
		d.Capabilities = capStrings(d.Capabilities, 16, 64)
		d.Verbs = capStrings(d.Verbs, 16, 32)
		d.Images = advertImages(d.Images)
		sz := *d.Size
		d.Size = &sz
		out = append(out, computeapi.RunTargetAdvert{Binding: ad.Binding, Describe: d})
	}
	return out
}

// advertImages are an advert's images that are pins (name@sha256:<hex>),
// at most advertImagesMax, in the host's order: a dependency layer's key
// holds the digest of one (deps.go).
func advertImages(in []string) []string {
	var out []string
	for _, im := range in {
		if len(out) < advertImagesMax && rt.ImagePinned(im) {
			out = append(out, im)
		}
	}
	return out
}

// advertImagesMax bounds the images one advert names.
const advertImagesMax = 16

// enrolledHost reports whether machine may lend boxes to project: a
// trusted one (a member's machine by default, or one the owner trusted) or
// one whose class the owner chose (public: a lender the owner named, by
// zm machine-class or on its invite). A guest's or area member's machine, by
// default, is neither: its adverts are dropped.
func (s *Service) enrolledHost(ctx context.Context, project, machine string) bool {
	if s.enrolled != nil {
		return s.enrolled(ctx, project, machine)
	}
	c, err := s.link.MachineClass(ctx, project, machine)
	return err == nil && (c.Class == api.MachineTrusted || c.SetBy != "" || c.FromInvite)
}

// enrolledAdverts are the well-formed adverts of a heartbeat from an
// enrolled host, else none.
func (s *Service) enrolledAdverts(ctx context.Context, project, machine string, raw json.RawMessage) []computeapi.RunTargetAdvert {
	var in []computeapi.RunTargetAdvert
	if len(raw) > 0 && json.Unmarshal(raw, &in) != nil {
		return nil
	}
	ads := cleanAdverts(in)
	if len(ads) == 0 || !s.enrolledHost(ctx, project, machine) {
		return nil
	}
	return ads
}

func capStrings(in []string, n, each int) []string {
	var out []string
	for _, s := range in {
		if len(out) == n {
			break
		}
		out = append(out, cutLine(s, each))
	}
	return out
}

// cutLine is s on one line, at most n bytes.
func cutLine(s string, n int) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c == 0x7f {
			b[i] = ' '
		}
	}
	if len(b) > n {
		b = b[:n]
	}
	return strings.ToValidUTF8(string(b), "")
}

// HostAdvert is one binding a machine advertised to this authority.
type HostAdvert struct {
	Machine string
	computeapi.RunTargetAdvert
}

// RunTargetAdverts is project's current adverts at this authority, by
// machine then binding (none without teamlink). A host is listed only while
// it is enrolled now: a grant lowered since its last beat (a member made a
// guest) drops its adverts at once, not at its next beat.
func (s *Service) RunTargetAdverts(ctx context.Context, project string) []HostAdvert {
	if s.link == nil {
		return nil
	}
	s.adverts.mu.Lock()
	sets := map[string]advertSet{}
	for machine, set := range s.adverts.by[project] {
		if time.Since(set.at) > advertStale || machine == s.d.Config.Machine {
			delete(s.adverts.by[project], machine)
			continue
		}
		sets[machine] = set
	}
	s.adverts.mu.Unlock()
	// Enrollment reads the registry: checked outside the lock, so a slow
	// read never holds up a heartbeat storing its adverts.
	var out []HostAdvert
	for machine, set := range sets {
		if !s.enrolledHost(ctx, project, machine) {
			// Not listed either way; forgotten only when the answer was
			// not cut short by the caller going away.
			if ctx.Err() == nil {
				s.adverts.dropSet(project, machine, set.at)
			}
			continue
		}
		for _, ad := range set.ads {
			out = append(out, HostAdvert{Machine: machine, RunTargetAdvert: ad})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Machine != out[j].Machine {
			return out[i].Machine < out[j].Machine
		}
		return out[i].Binding < out[j].Binding
	})
	return out
}

// ---- the authority ----

// RelayRunTarget runs one driver verb of a binding machine advertised to
// project (the authority's ULID): it queues a run_target machine op, taken
// by the machine's held-open poll (or its next heartbeat answer) and run at
// most once there, and waits up to wait for its result. No answer is a
// CodeUnavailable error, a driver timeout on the host rt.CodeDriverTimeout;
// the driver's refusals come back as they were.
func (s *Service) RelayRunTarget(ctx context.Context, project, machine string, op computeapi.RunTargetOp, wait time.Duration) (json.RawMessage, error) {
	if s.link == nil {
		return nil, api.Errorf(api.CodeUnavailable, "this machine relays no run targets (not a team authority)")
	}
	ending := op.Verb == "status" || op.Verb == "release" || op.Verb == "list" || op.Verb == protocol.VerbProgress || op.Verb == protocol.VerbCancel
	if machine == s.d.Config.Machine || !s.d.Store.IsTeam(project) || (!ending && !s.enrolledHost(ctx, project, machine)) {
		// A host nobody enrolled (any more) gets no box, no command and no
		// renewal; its boxes can still be looked at and ended.
		return nil, api.NotFound("machine", machine)
	}
	if !s.heard(project, machine) {
		// A host that stopped beating (asleep, offline) is not waited on for
		// the verb's limit: the sweep and handle lists would stall on it.
		// An acquire answers as a driver timeout: the host may have taken it
		// before going quiet, so the authority keeps the row for a retry with
		// the same request_id instead of forgetting a box that may exist.
		if op.Verb == "acquire" {
			return nil, &api.Error{Code: rt.CodeDriverTimeout, Message: machine + " has not been heard from in " + advertStale.String()}
		}
		return nil, api.Errorf(api.CodeUnavailable, "%s has not been heard from in %s", machine, advertStale)
	}
	s.link.ExpireOps(ctx)
	mop := api.MachineOp{ID: ids.New(), Kind: opRunTarget}
	setRTOp(&mop, &computeapi.RunTargetOp{Binding: op.Binding, Verb: op.Verb, ID: op.ID}) // without the args: they stay out of the database
	e := s.rtmem.Put(mop.ID, op.Args)
	defer s.rtmem.Forget(mop.ID)
	if carriesSecret(op.Args) {
		s.secretOps.Store(mop.ID, machine)
		defer s.secretOps.Delete(mop.ID)
	}
	if err := s.link.QueueOp(ctx, teamlink.Op{MachineOp: mop, Project: project, Machine: machine, FromMachine: s.d.Config.Machine}); err != nil {
		return nil, err // QueueOp woke the host's held-open poll
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-e.Done():
		return relayAnswer(e.Result())
	case <-t.C:
	case <-ctx.Done():
	}
	// No answer. An op no heartbeat carried yet never runs; one carried may
	// still (the host's own record keeps it to once): the caller reconciles.
	_, _ = s.link.UpdateOp(context.WithoutCancel(ctx), mop.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: teamlink.Str("not taken in time"), Unsent: true})
	return nil, &api.Error{Code: rt.CodeDriverTimeout, Message: machine + " did not answer"}
}

// opArgs fills in a run_target op's arguments for the answer that carries
// it (the host's poll or heartbeat, ctx its request); nil when it may go,
// else why it never runs: the arguments are gone (the authority restarted),
// or they hold secrets or a jit_config and cannot be sealed to the host
// (opRefused answers the waiting call that refusal, once teamlink ended the
// op with it). Such arguments go sealed
// to the key the host's own request named (SPEC 17.11 "Sealed secrets"), on
// every transport. A host that names none (a Potluck or zm from before
// sealing on that leg) is refused, except over the tailnet in a pool whose
// sealing is preferred, where it gets them as before, in the clear
// (sealing.go). An op an answer carried may have run on the host: a refusal
// then only keeps it out of this answer, and the host's report answers the
// waiting call (teamlink.OpHandler.Args).
func (s *Service) opArgs(ctx context.Context, project string, op *api.MachineOp) error {
	rop := rtOp(op)
	if rop == nil {
		return errNotTaken
	}
	args, ok := s.rtmem.Args(op.ID)
	if !ok {
		return errNotTaken
	}
	if m, ok := s.secretOps.Load(op.ID); ok {
		// The class again at delivery: a host demoted since the grant check
		// gets no secret (the waiting call is answered this refusal).
		if s.classAtHandOver(ctx, project, m.(string)) != api.MachineTrusted {
			return &api.Error{Code: api.CodeForbidden, Message: m.(string) + " is a public host: it is given no secrets, only secret-free work"}
		}
	}
	rop.Sealing = 1
	if carriesSecret(args) {
		rm := core.RemoteOf(ctx)
		own, ferr := s.ownKey()
		tailnet := rm != nil && rm.Via != core.ViaTunnel
		switch {
		case ferr != nil:
			return errOwnKey(ferr)
		case own == "" && tailnet:
			// Sealing off: this daemon stands in for a build from before
			// sealing, which sent the op as it is.
			rop.Sealing, rop.Args = 0, args
			setRTOp(op, rop)
			return nil
		case tailnet && len(rm.SealKey) == 0:
			// A host over the tailnet that names no seal key: a Potluck (or
			// zm) from before sealing there. Under preferred it gets the op as
			// before, in the clear and unmarked, and is warned about (every
			// time when it sealed before: a downgrade signal); under required
			// it is refused below.
			machine := core.SubjectOf(ctx).Machine
			stopped := s.stoppedSealing(project, machine, "it was to be sent secrets or a jit_config")
			if s.sealingMode(ctx, project) == computeapi.SealingPreferred {
				if !stopped {
					s.warnUnsealed(project, machine, "was sent secrets or a jit_config")
				}
				rop.Sealing, rop.Args = 0, args
				setRTOp(op, rop)
				return nil
			}
		}
		sealed, by, err := s.sealOpArgs(rm, project, op, args)
		if err != nil {
			return err
		}
		rop.Sealed, rop.SealedBy = sealed, by
		setRTOp(op, rop)
		return nil
	}
	rop.Args = args
	setRTOp(op, rop)
	return nil
}

// errNotTaken ends an op whose arguments are gone (the authority restarted)
// or do not decode: it never runs, so its caller may ask again with the same
// request id (the code a relay that heard nothing answers).
var errNotTaken = &api.Error{Code: rt.CodeDriverTimeout, Message: "not taken in time"}

// opRefused answers op's waiting call the refusal teamlink ended op with
// (teamlink.OpHandler.Refused).
func (s *Service) opRefused(_ context.Context, _ string, op api.MachineOp, err error) {
	msg, code, hint := teamlink.ErrParts(err)
	s.rtmem.Finish(op.ID, teamlink.OpResult{Fail: true, Code: code, Msg: msg, Hint: hint})
}

// classAtHandOver is machine's class in project, asked when an op is
// handed to it and by hostClass: public unless teamlink says trusted.
func (s *Service) classAtHandOver(ctx context.Context, project, machine string) string {
	if s.opClass != nil {
		return s.opClass(ctx, project, machine)
	}
	if s.link == nil {
		return api.MachinePublic
	}
	if c, err := s.link.MachineClass(ctx, project, machine); err == nil && c.Class == api.MachineTrusted {
		return api.MachineTrusted
	}
	return api.MachinePublic
}

// rtOp is op's run_target request (its payload); nil when it has none or
// it does not decode.
func rtOp(op *api.MachineOp) *computeapi.RunTargetOp {
	if len(op.Payload) == 0 {
		return nil
	}
	var rop computeapi.RunTargetOp
	if json.Unmarshal(op.Payload, &rop) != nil {
		return nil
	}
	return &rop
}

// setRTOp stores rop as op's payload.
func setRTOp(op *api.MachineOp, rop *computeapi.RunTargetOp) {
	op.Payload, _ = json.Marshal(rop)
}

// opSealTTL bounds how long a sealed op's arguments open at the host: the
// answer carrying them is read at once, a heartbeat's resend is sealed anew.
var opSealTTL = 10 * time.Minute

// opSealPurpose binds sealed arguments to one op of one project: its id,
// binding, verb and box (the HPKE info adds both machines' key ids and the
// expiry), so they open for nothing else.
func opSealPurpose(project string, op *api.MachineOp) string {
	rop := rtOp(op)
	if rop == nil {
		rop = &computeapi.RunTargetOp{}
	}
	b, _ := json.Marshal([]string{"run_target op", project, op.ID, rop.Binding, rop.Verb, rop.ID})
	return string(b)
}

// sealOpArgs seals args to the host whose request rm is, and names the key
// id it sealed them from.
func (s *Service) sealOpArgs(rm *core.Remote, project string, op *api.MachineOp, args []byte) (string, string, *api.Error) {
	sl, ok := s.d.Transport(core.ViaTunnel).(core.Sealer)
	if !ok {
		return "", "", sealbox.ErrNoKey("this machine")
	}
	if rm == nil || rm.SealID == "" || len(rm.SealKey) == 0 {
		return "", "", sealbox.ErrNoKey("the host")
	}
	by, ferr := s.ownKey()
	if ferr != nil {
		return "", "", errOwnKey(ferr)
	}
	if by == "" {
		return "", "", sealbox.ErrNoKey("this machine")
	}
	sealed, err := sl.SealTo(rm.SealID, rm.SealKey, opSealPurpose(project, op), opSealTTL, args)
	if err != nil {
		var e *api.Error
		if errors.As(err, &e) {
			return "", "", e
		}
		return "", "", api.Errorf(api.CodeInternal, "sealing for the host failed") // never the error's text: it may echo args
	}
	return sealed, by, nil
}

// opArgsHere are a relayed op's arguments as this host's driver gets them:
// opened when its authority sealed them (via is how this machine joined the
// project, fp what it pinned), refused when they hold secrets or a
// jit_config in the clear from an authority that seals.
func (s *Service) opArgsHere(via, fp, project string, op *api.MachineOp) ([]byte, *api.Error) {
	rop := rtOp(op)
	if rop == nil {
		return nil, api.Errorf(api.CodeBadRequest, "the op carries no run-target request")
	}
	sl, ok := s.d.Transport(core.ViaTunnel).(core.Sealer)
	if rop.Sealed == "" {
		if !carriesSecret(rop.Args) {
			return rop.Args, nil
		}
		switch {
		case rop.Sealing > 0:
			// The authority seals every such op: this clear copy is a bug or
			// an envelope stripped on the way, and is never used.
			return nil, api.Errorf(api.CodeForbidden, "this op carried secrets or a jit_config in the clear from an authority that seals them: refused, not run").
				WithHint("update the pool's machine's daemon; if it is current, report this")
		case via == api.ViaTunnel:
			return nil, api.Errorf(api.CodeForbidden, "this op carried secrets or a jit_config in the clear over the tunnel: refused, not run")
		case ok && s.sealsHere():
			// What this pool's machine last reported beside its key decides:
			// required, never in the clear until this machine's owner forgets
			// it; preferred (a rollback), taken, warned every time.
			switch mode, err := sl.HomeSealed(fp); {
			case err != nil:
				var e *api.Error
				if errors.As(err, &e) {
					return nil, e
				}
				return nil, api.Errorf(api.CodeUnavailable, "%v", err)
			case mode == computeapi.SealingPreferred:
				s.d.Log.Warn("sealing: this pool's machine stopped sealing (it named a key before, in a pool that prefers sealing): an op with secrets came in the clear over the tailnet; update it", "project", project)
			case mode != "":
				return nil, api.Errorf(api.CodeForbidden, "this op carried secrets or a jit_config in the clear, and this pool's machine named a seal key to this machine before, in a pool that requires sealing: refused, not run (a downgrade, or it was put back to an older build)").
					WithHint("if its owner put it back to an older build on purpose: potluck sealing forget -P <pool>, on this machine")
			}
		}
		return rop.Args, nil // an authority from before sealing on the tailnet
	}
	if !ok {
		return nil, sealbox.ErrOpen
	}
	from := rop.SealedBy
	if via == api.ViaTunnel {
		// Over the tunnel the home's key is the one this machine pinned.
		from = "zmkey:" + fp
	}
	if from == "" || from == "zmkey:" {
		return nil, sealbox.ErrOpen
	}
	pt, err := sl.OpenFrom(from, opSealPurpose(project, op), rop.Sealed)
	if err != nil {
		var e *api.Error
		if errors.As(err, &e) {
			return nil, e
		}
		return nil, sealbox.ErrOpen
	}
	return pt, nil
}

// sealsHere reports whether this daemon seals (false while sealing is off:
// a test standing in for a build from before sealing, which checked
// nothing; a key fault counts as sealing, so the check stays).
func (s *Service) sealsHere() bool {
	sl, ok := s.d.Transport(core.ViaTunnel).(core.Sealer)
	if !ok {
		return false
	}
	id, _, err := sl.SealIdentity()
	return err != nil || id != ""
}

// carriesSecret reports whether a run-target request body or a relayed
// op's arguments hold a renter's secrets or a jit_config
// (sealbox.CarriesSecret, one test for every leg). SPEC 20 "Boxes",
// Secrets: neither goes between machines in the clear, on any leg or
// transport: a member to its authority, the authority to a host; they go
// sealed (SPEC 17.11 "Sealed secrets") or not at all.
func carriesSecret(raw []byte) bool { return sealbox.CarriesSecret(raw) }

// applyRunTargetReport takes the host's report on a run_target op.
func (s *Service) applyRunTargetReport(ctx context.Context, o teamlink.Op, rep api.MachineOpReport) {
	s.rtmem.Finish(o.ID, reportResult(rep))
	_, _ = s.link.UpdateOp(ctx, o.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone)})
}

// reportResult is a host's report as the waiting call's result (a failure
// keeps the report's payload: hostAnswer's mark).
func reportResult(rep api.MachineOpReport) teamlink.OpResult {
	r := teamlink.OpResult{Raw: rep.Payload, Code: rep.Code, Msg: rep.Error, Hint: rep.Hint, Fail: rep.Error != ""}
	if !r.Fail && len(r.Raw) == 0 {
		r = teamlink.OpResult{Fail: true, Msg: "the host answered no result"}
	}
	if r.Fail && r.Code == "" {
		// It ran and said nothing of what it did: internal, never a refusal
		// (refusal, runtarget.go), so what it made is found by list.
		r.Code = api.CodeInternal
	}
	return r
}

// noHandleMark is the payload beside a status's not_found that is about the
// handle itself (rt.NoHandle): the host's driver, or its ledger, has no such
// handle. The report's code stays not_found and a failed report's payload was
// never read, so an authority on a build without the mark does exactly what
// it did before; a host on such a build sends none, and its not_found is then
// not taken as the box being gone (the sweep is the backstop, sweep.go).
const noHandleMark = `{"no_handle":true}`

// hostAnswer is HostCall's answer to verb as this host reports it.
func hostAnswer(verb string, res json.RawMessage, err error) (json.RawMessage, error) {
	if err != nil {
		if verb == verbStatus && rt.IsNoHandle(err) && api.IsCode(err, api.CodeNotFound) {
			return json.RawMessage(noHandleMark), err
		}
		return nil, err
	}
	if len(res) == 0 {
		res = json.RawMessage("{}")
	}
	if res = capAnswer(verb, res); res == nil {
		return nil, api.Errorf(api.CodeInternal, "the host's driver answered over the relay's limit")
	}
	return res, nil
}

// relayAnswer is a host's result as the authority's call returns it: a
// not_found that carries the mark is rt.NoHandle, still a not_found.
func relayAnswer(r teamlink.OpResult) (json.RawMessage, error) {
	err := r.Err()
	if err == nil {
		return r.Raw, nil
	}
	var mark struct {
		NoHandle bool `json:"no_handle"`
	}
	if e, ok := err.(*api.Error); ok && e.Code == api.CodeNotFound && json.Unmarshal(r.Raw, &mark) == nil && mark.NoHandle {
		return nil, rt.NoHandle(e)
	}
	return nil, err
}

// opRunTarget is a run-target driver verb relayed to the host that lends
// the box (SPEC 20 "Across machines"): payload computeapi.RunTargetOp (its
// arguments filled in, or sealed, only for the answer that carries it),
// report the driver's answer verbatim, posted at once and blanked on the
// host once taken.
const opRunTarget = "run_target"

// Relayed ops' lifetimes: one no machine took by then is dropped (opTake);
// an answer nobody collected is blanked then (opKeep).
const (
	opTake = time.Minute
	opKeep = 10 * time.Minute
)

// relayOn hooks the relay into link (New, before Start): the run_target op
// kind, the heartbeat's run_targets part, the proxy's run-target rows and
// the machine-class subscriber that drops a host no longer enrolled.
func (s *Service) relayOn(link *teamlink.Service) {
	s.link = link
	link.RegisterOp(opRunTarget, teamlink.OpHandler{Run: s.runRunTargetOp, Apply: s.applyRunTargetReport, Args: s.opArgs, Refused: s.opRefused,
		Expiry: opTake, ReportNow: true, BlankOnAck: true, Keep: opKeep})
	link.RegisterBeatPart("run_targets", teamlink.BeatPart{Request: s.runTargetsRequest, Serve: s.serveRunTargets})
	link.RegisterRoutes("runtarget", RelayRoutes()...)
	link.Hooks.ClassChanged = append(link.Hooks.ClassChanged, s.classChanged)
	link.Hooks.DropForgotten = append(link.Hooks.DropForgotten, s.dropForgotten) // hostforget.go
}

// classChanged is a teamlink Hooks.ClassChanged: back at a guest's default
// (public, set by nobody) a machine is no longer a host.
func (s *Service) classChanged(_ context.Context, project, machine string, c api.MachineClass) {
	if c.Class == api.MachinePublic && c.SetBy == "" && !c.FromInvite {
		s.adverts.drop(project, machine)
	}
}

// lend notes whether this machine's last heartbeat to project carried box
// adverts: while it lends, it polls for ops (teamlink.WantsOps).
func (s *Service) lend(project string, on bool) { s.link.WantsOps(project, "lending", on) }

// ---- the host ----

// advertsFor are this machine's shared bindings for the project (its id) a
// replica heartbeats, for its beat.
func (s *Service) advertsFor(ctx context.Context, project string) json.RawMessage {
	ads := s.HostAdverts(ctx, project)
	if len(ads) == 0 {
		return nil
	}
	if len(ads) > advertMax {
		ads = ads[:advertMax]
	}
	b, _ := json.Marshal(ads)
	return b
}

// runRunTargetOp runs a relayed run_target op here: the run-target module
// checks the binding exists and is shared with this project, then calls its
// driver as a local call would.
func (s *Service) runRunTargetOp(ctx context.Context, project string, op api.MachineOp) (json.RawMessage, error) {
	rop := rtOp(&op)
	if rop == nil {
		return nil, api.Errorf(api.CodeUnavailable, "this machine shares no run targets")
	}
	in, via, fp := false, "", ""
	for _, x := range s.link.List() {
		if snap := x.Snapshot(); snap.Project.ID == project && teamlink.Ended(snap) == nil {
			in, via, fp = true, snap.Via, snap.Fingerprint
		}
	}
	if !in {
		return nil, api.Errorf(api.CodeNotFound, "this machine is not in that project")
	}
	// Sealed to this machine's key by its authority: opened here only, then
	// handed to the driver as a local call's.
	args, e := s.opArgsHere(via, fp, project, &op)
	if e != nil {
		return nil, e
	}
	res, err := s.HostCall(ctx, project, rop.Binding, rop.Verb, rop.ID, args)
	return hostAnswer(rop.Verb, res, err)
}

// opAnswerMax bounds one relayed answer as the host reports it (encoded
// JSON; a heartbeat is at most 8 MiB, and control bytes encode to 6 each).
var opAnswerMax = 3 << 20

// capAnswer is res within opAnswerMax, measured as the report encodes it
// (Go's encoder escapes < > & to 6 bytes each, also inside a raw answer): an
// exec's output is cut to fit (and marked truncated); anything else over it
// is nil (an error answer).
func capAnswer(verb string, res json.RawMessage) json.RawMessage {
	enc, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	if len(enc) <= opAnswerMax {
		return enc
	}
	var m map[string]json.RawMessage
	var out, errs string
	if verb != "exec" || json.Unmarshal(res, &m) != nil {
		return nil
	}
	_ = json.Unmarshal(m["stdout"], &out)
	_ = json.Unmarshal(m["stderr"], &errs)
	for range 64 {
		m["stdout"], _ = json.Marshal(out)
		m["stderr"], _ = json.Marshal(errs)
		m["truncated"] = json.RawMessage("true")
		if b, _ := json.Marshal(m); len(b) <= opAnswerMax {
			return b
		}
		long := &out
		if len(errs) > len(out) {
			long = &errs
		}
		if n := len(*long) * 3 / 4; string(m["encoding"]) == `"base64"` {
			*long = (*long)[:n/4*4] // whole base64 quanta: the bytes kept decode
		} else {
			*long = strings.ToValidUTF8((*long)[:n], "")
		}
	}
	return nil
}

// runTargetsRequest is the run_targets part: the boxes this machine lends
// the project; while it lends, it polls for ops.
func (s *Service) runTargetsRequest(ctx context.Context, b teamlink.Beat) (json.RawMessage, func(context.Context, json.RawMessage), error) {
	v := s.advertsFor(ctx, b.Snap.Project.ID)
	id := b.Snap.Project.ID
	return v, func(context.Context, json.RawMessage) { s.lend(id, len(v) > 0) }, nil
}

// serveRunTargets keeps the boxes the calling machine lends p (none when
// its beat had no run_targets part).
func (s *Service) serveRunTargets(ctx context.Context, p api.Project, v json.RawMessage) (json.RawMessage, error) {
	machine := core.SubjectOf(ctx).Machine
	ads := s.enrolledAdverts(ctx, p.ID, machine, v)
	s.adverts.set(p.ID, machine, ads)
	rm := core.RemoteOf(ctx)
	s.heardSeal(p.ID, machine, rm != nil && len(rm.SealKey) > 0, len(ads) > 0)
	return nil, nil
}

// SetAdvertStale sets how long a host's last beat keeps it listed and
// relayed to (tests; the returned func undoes it).
func SetAdvertStale(d time.Duration) func() {
	old := advertStale
	advertStale = d
	return func() { advertStale = old }
}
