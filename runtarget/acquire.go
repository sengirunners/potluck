package runtarget

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// jitRe is an encoded JIT runner config (GitHub's encoded_jit_config: base64).
var jitRe = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{16,}$`)

// jitRepoRe is a GitHub owner/name.
var jitRepoRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}/[A-Za-z0-9._-]{1,100}$`)

// jitEventRe is a GitHub Actions event name.
var jitEventRe = regexp.MustCompile(`^[a-z_]{1,64}$`)

func (s *Service) acquire(r *http.Request) (any, error) {
	t, b, db, err := s.target(r, authz.Operate) // before the body: only a caller who may acquire sends a large one
	if err != nil {
		return nil, err
	}
	var req computeapi.AcquireRunTargetRequest
	if err := api.DecodeJSONWithin(r, &req, depsBodyMax); err != nil { // deps.go: a layer's files ride in the body
		return nil, err
	}
	if err := checkDatabaseRequest(b, req); err != nil {
		return nil, err
	}
	if builderRequest(req.RequestID) {
		return nil, api.Errorf(api.CodeBadRequest, "request_id %s...: that prefix is the daemon's, for dependency layer builders", depsBuildPrefix)
	}
	if err := checkBase(req); err != nil {
		return nil, err
	}
	if req.Base != nil && req.Base.Fill {
		return s.acquireBox(r.Context(), t, b, db, req, daemonVols{})
	}
	if len(req.Deps) > 0 || req.DepsWaitS != nil {
		return s.acquireDeps(r.Context(), t, b, db, req) // deps.go
	}
	return s.acquireBox(r.Context(), t, b, db, req, daemonVols{})
}

// daemonVols are volumes the daemon adds to an acquire itself, never the
// caller's word (deps.go): sent to the driver after the caller's, and in the
// request's fingerprint as sum, what the caller asked that they meet.
type daemonVols struct {
	vols []computeapi.BoxVolume
	sum  string
	// digest is the image the box must boot (the one its layers are keyed
	// by); a box of another is released, as one off its binding's pin is.
	digest string
	image  string // daemon-selected builder image; never caller-controlled
	base   *baseMount
}

// acquireBoxWithVolumes is one acquire after daemon-owned volumes are prepared.
func (s *Service) acquireBoxWithVolumes(ctx context.Context, t core.Target, b computeapi.RunTarget, db *sql.DB, req computeapi.AcquireRunTargetRequest, x daemonVols) (any, error) {
	if err := checkDatabaseRequest(b, req); err != nil {
		return nil, err
	}
	sub := core.SubjectOf(ctx)
	local := owner(ctx)
	if err := requestIDOK(req.RequestID); err != nil { // acquires.go
		return nil, err
	}
	actor, err := actingFor(ctx, req.Actor)
	if err != nil {
		return nil, err
	}
	if req.TTLS < 0 {
		return nil, api.Errorf(api.CodeBadRequest, "ttl_s must not be negative")
	}
	if len(req.Args) > 64 {
		return nil, api.Errorf(api.CodeBadRequest, "at most 64 args")
	}
	for _, a := range req.Args {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return nil, api.Errorf(api.CodeBadRequest, "an arg is over 4096 bytes or holds a NUL")
		}
	}
	switch b.Kind {
	case computeapi.RunTargetKindRun:
		if err := rt.CheckRunScript(req.Script); err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "%v", err)
		}
		if req.Name != "" {
			return nil, api.Errorf(api.CodeBadRequest, "name is for a db target")
		}
	case computeapi.RunTargetKindDB:
		if req.Name == "" || !api.ValidSlug(req.Name) {
			return nil, api.Errorf(api.CodeBadRequest, "a db acquire needs a name (lowercase letters, digits, . _ -)")
		}
		if req.Script != "" || len(req.Args) > 0 {
			return nil, api.Errorf(api.CodeBadRequest, "script and args are for a run target")
		}
		if !local {
			return nil, api.Errorf(api.CodeForbidden, "a db acquire is local-only: its url never crosses the network")
		}
	case computeapi.RunTargetKindBox:
		if req.Script != "" || len(req.Args) > 0 || req.Name != "" {
			return nil, api.Errorf(api.CodeBadRequest, "a box takes image, ref and commit; script, args and name are not a box's")
		}
		if req.Ref != "" && b.Repo == "" {
			return nil, api.Errorf(api.CodeBadRequest, "run target %s has no repo: a box there takes no ref", b.Name)
		}
	}
	if b.Kind != computeapi.RunTargetKindBox && req.Image != "" {
		return nil, api.Errorf(api.CodeBadRequest, "image is for a box target")
	}
	if b.Kind != computeapi.RunTargetKindBox && req.JITConfig != "" {
		return nil, api.Errorf(api.CodeBadRequest, "jit_config is for a box target")
	}
	if err := checkNet(b, req.Net); err != nil {
		return nil, err
	}
	if req.JITConfig != "" && (len(req.JITConfig) > 64<<10 || !jitRe.MatchString(req.JITConfig)) {
		return nil, api.Errorf(api.CodeBadRequest, "jit_config is not an encoded runner config (base64, at most 64 KiB)")
	}
	if g := req.JITGuard; g != nil {
		ok := req.JITConfig != "" && jitRepoRe.MatchString(g.Repo) && len(g.Events) <= 32
		for _, e := range g.Events {
			ok = ok && jitEventRe.MatchString(e)
		}
		if !ok {
			return nil, api.Errorf(api.CodeBadRequest, "jit_guard names a repo (owner/name) and at most 32 event names, for a jit_config")
		}
		if g.OpenIfPrivate && len(g.Events) == 0 {
			return nil, api.Errorf(api.CodeBadRequest, "jit_guard.open_if_private widens its events: it needs events")
		}
		if g.Run != nil {
			if err := g.Run.Check(g.Repo); err != nil {
				return nil, api.Errorf(api.CodeBadRequest, "%v", err)
			}
		}
	}
	var jitDec *grants.Decision
	if req.JITConfig != "" {
		// A runner config registers a runner for the project's repo: only on
		// a trusted host, and only under a grant (grants.go).
		if jitDec, err = s.jitGranted(ctx, t.Project.ID, t.Area, b); err != nil {
			return nil, err
		}
		if err := s.hostTrusted(ctx, b, "jit_config"); err != nil {
			return nil, err
		}
		if err := s.callerSealed(ctx, "a jit_config"); err != nil {
			return nil, err
		}
	}
	jitSum := "" // the fingerprint holds a hash of jit_config, never the value
	if req.JITConfig != "" {
		h := sha256.Sum256([]byte(req.JITConfig))
		jitSum = hex.EncodeToString(h[:])
	}
	pin, tmplImage, err := boxPin(b, req)
	if err != nil {
		return nil, err
	}
	// Held from before the binding's lock: progress says queued, and a
	// cancel stops it before its driver call (acquires.go).
	fk := flightKey(t.Project.ID, b.Name, actorKey(actor, req.RequestID))
	s.inflight.enter(fk, req.JITConfig != "")
	defer s.inflight.leave(fk)
	if b.Kind == computeapi.RunTargetKindBox {
		defer s.requestMu.lock(t.Project.ID + "/" + actorKey(actor, req.RequestID))()
	}
	unlock := s.lock(t.Project.ID, b.Name)
	defer unlock()

	// Keys are per actor: zm sends the driver <actor tag>-<request_id>, so
	// one actor's key neither blocks nor reveals another's.
	req.RequestID = actorKey(actor, req.RequestID)
	prev, seen, err := byRequest(ctx, db, b.Name, req.RequestID)
	if err != nil {
		return nil, err
	}
	// hidden: a row of another actor's, or in an area the caller may not
	// operate in, answers as a missing one would.
	hidden := func(o row) bool { return rowHidden(ctx, t.Project.ID, o) }
	if seen && hidden(prev) {
		// Another actor's key answers like a free one would not: hidden, as missing.
		return nil, api.NotFound("handle", req.RequestID)
	}
	if seen {
		x.replayBase(prev.request)
	}

	dbRecipe, err := s.databaseFor(ctx, t.Project.ID, req, prev.request, seen)
	if err != nil {
		return nil, err
	}
	if x.image != "" {
		pin, tmplImage = x.image, ""
	}
	if dbRecipe != nil {
		if err := s.hostTrusted(ctx, b, "database snapshot"); err != nil {
			return nil, err
		}
		pin, tmplImage = dbRecipe.Image, ""
		if x.digest != "" && x.digest != pinDigest(pin) {
			return nil, api.Errorf(api.CodeConflict, "database image differs from dependency layer image")
		}
	}
	reqJSON, _ := json.Marshal(struct {
		Database                                string             `json:",omitempty"`
		BuilderImage                            string             `json:",omitempty"`
		DatabaseRecipe                          *protocol.Database `json:",omitempty"`
		Actor, Ref, Commit, Script, Name, Image string
		Args                                    []string
		TTLS                                    int
		JIT                                     string                 `json:",omitempty"`
		JITGuard                                *computeapi.JITGuard   `json:",omitempty"`
		Template                                string                 `json:",omitempty"`
		Net                                     string                 `json:",omitempty"`
		Persist                                 bool                   `json:",omitempty"`
		Volumes                                 []computeapi.BoxVolume `json:",omitempty"`
		Deps                                    string                 `json:",omitempty"`
		SelfRenew                               bool                   `json:",omitempty"`
		Base                                    *baseMount             `json:",omitempty"`
	}{req.Database, x.image, dbRecipe, actor, req.Ref, req.Commit, req.Script, req.Name, req.Image, req.Args, req.TTLS, jitSum, req.JITGuard, req.Template, netKey(req.Net), req.Persist, req.Volumes, x.sum, req.SelfRenew, x.base})
	if seen && prev.request != string(reqJSON) {
		return nil, api.Errorf(api.CodeConflict, "request_id %s was used for a different acquire", req.RequestID)
	}
	if !seen && b.Kind == computeapi.RunTargetKindBox {
		if err := s.boxRequestFree(ctx, t.Project.ID, db, b.Name, req.RequestID, hidden); err != nil { // requests.go
			return nil, err
		}
	}

	d, err := s.describeOf(ctx, b)
	if err != nil {
		return nil, err
	}
	if err := checkDatabaseDriver(dbRecipe, d.Capabilities); err != nil {
		return nil, err
	}
	if req.JITConfig != "" && !slices.Contains(d.Capabilities, protocol.CapGitHubJIT) {
		// A driver that does not list it would take the config and start no runner, or keep it where nothing scrubs it.
		return nil, api.Errorf(api.CodeBadRequest, "run target %s starts no runner (no %s capability)", b.Name, protocol.CapGitHubJIT)
	}
	if req.JITGuard != nil && !slices.Contains(d.Capabilities, protocol.CapGitHubJITGuard) {
		// Sent to a driver that ignored it, the runner would serve any job of the repo.
		return nil, api.Errorf(api.CodeBadRequest, "run target %s cannot guard its runner (no %s capability)", b.Name, protocol.CapGitHubJITGuard)
	}
	if req.JITGuard != nil && req.JITGuard.OpenIfPrivate && !slices.Contains(d.Capabilities, protocol.CapJITGuardPrivate) {
		// Sent to a driver that ignored it, the runner would refuse what the caller means to serve: refused here so
		// the caller knows, not left to fail on GitHub.
		return nil, api.Errorf(api.CodeBadRequest, "run target %s cannot check that a job's repository is private (no %s capability)", b.Name, protocol.CapJITGuardPrivate)
	}
	if req.JITGuard != nil && req.JITGuard.Run != nil &&
		(!slices.Contains(d.Capabilities, protocol.CapJITGuardRun) || !slices.Contains(d.Capabilities, protocol.CapBoxSecrets)) {
		// Sent to a driver that ignored it, the runner would serve any run, unheld, in a box the door mints into.
		return nil, api.Errorf(api.CodeBadRequest, "run target %s cannot hold a job for its secrets (its driver lists no %s and %s)", b.Name, protocol.CapJITGuardRun, protocol.CapBoxSecrets)
	}
	net, err := s.netFor(ctx, b, d, req.Net)
	if err != nil {
		return nil, err
	}
	if err := s.checkPersist(ctx, b, d, req); err != nil { // persist.go
		return nil, err
	}
	if err := s.checkVolumes(ctx, b, d, req); err != nil { // volumes.go
		return nil, err
	}
	eff := stricter(b, d)
	if eff.Guarded() && !local { // on every request: a repeat is an acquire too
		return nil, api.Errorf(api.CodeForbidden, "%s, so only this machine's owner may use it, never the pool", eff.CannotIsolate("run target "+b.Name))
	}
	if b.Kind == computeapi.RunTargetKindBox && !eff.UntrustedCapable() {
		return nil, api.Errorf(api.CodeForbidden, "%s now, so it makes no box", eff.CannotIsolate("potluck-driver-"+b.Driver))
	}
	if !d.Available && !seen {
		return nil, notAvailable(b, d) // refused.go
	}
	if !d.Available && prev.Handle == "" { // a request still being made here: retried here, never elsewhere
		return nil, api.Errorf(api.CodeUnavailable, "run target %s is not available: %s", b.Name, oneLine(d.Why))
	}
	if !seen {
		if dbRecipe != nil && !s.lendsDatabaseImage(ctx, b, dbRecipe.Image) {
			return nil, markRefused(api.Errorf(api.CodeBadRequest, "run target %s does not lend database %s's image %s", b.Name, dbRecipe.Name, dbRecipe.Image).
				WithHint("its owner pins that image on the binding"), computeapi.RefusedImage)
		}
		if c := lifetimeCap(b, d); c > 0 && req.TTLS > c {
			return nil, markRefused(api.Errorf(api.CodeBadRequest, "ttl_s %d is past the lifetime cap %d", req.TTLS, c), computeapi.RefusedLifetime)
		}
		if b.HandlesMax > 0 {
			n, err := liveCount(ctx, db, b.Name, actor)
			if err != nil {
				return nil, err
			}
			if n >= b.HandlesMax {
				return nil, markRefused(api.Errorf(api.CodeForbidden, "%s already holds %d live handles of %s (handles_max)", actor, n, b.Name), computeapi.RefusedHandlesMax)
			}
		}
		if err := s.budgetAllowed(ctx, t.Project.ID, b, db); err != nil {
			return nil, err
		}
		if b.Kind == computeapi.RunTargetKindBox {
			if err := s.boxAllowed(ctx, t.Project.ID, t.Area, db); err != nil { // ends.go
				return nil, err
			}
		}
	}
	commit := prev.Commit
	if !seen {
		// A repeat sends what the first ask resolved, though the ref moved since.
		if commit, err = s.route(ctx, b, eff, req); err != nil {
			return nil, err
		}
	}
	now := s.now().UTC().Format(tsLayout)
	if !seen {
		if err := insertAcquiring(ctx, db, b.Name, req.RequestID, string(reqJSON), b.Kind, actor, req.Ref, commit, req.Script, t.Area, now, req.SelfRenew); err != nil {
			return nil, err
		}
	}
	args := map[string]any{"request_id": req.RequestID, "actor": actor}
	if dbRecipe != nil {
		args["database"] = dbRecipe
	}
	if req.TTLS > 0 {
		args["ttl_s"] = req.TTLS
	}
	switch b.Kind {
	case computeapi.RunTargetKindRun:
		args["ref"], args["commit"], args["script"] = req.Ref, commit, req.Script
		if len(req.Args) > 0 {
			args["args"] = req.Args
		}
	case computeapi.RunTargetKindDB:
		args["name"] = req.Name
		if req.Ref != "" {
			args["ref"] = req.Ref
		}
		if commit != "" {
			args["commit"] = commit
		}
	case computeapi.RunTargetKindBox:
		if pin != "" {
			args["image"] = rt.ImageName(pin)
		} else if tmplImage != "" { // a remote binding: its host's driver holds the images and refuses one it lacks
			args["image"] = tmplImage
		}
		if req.Ref != "" {
			args["ref"], args["commit"] = req.Ref, commit
		}
		if req.JITConfig != "" {
			args["jit_config"] = req.JITConfig
		}
		if req.JITGuard != nil {
			args["jit_guard"] = req.JITGuard
		}
		if net != "" {
			args["net"] = net
		}
		if req.Persist {
			args["persist"] = true
		}
		if vols := append(slices.Clone(req.Volumes), x.vols...); len(vols) > 0 { // volumes.go: the owner is zm's word, never the caller's
			args["volumes"], args["volume_owner"], args["volume_project"] = vols, volumeOwner(sub, actor), t.Project.ID
		}
	}
	if !s.inflight.ask(fk) { // cancelled while queued: the driver is never asked
		if !seen {
			_ = deleteAcquiring(context.WithoutCancel(ctx), db, b.Name, req.RequestID)
		}
		return nil, errCancelled()
	}
	var ans acquireAnswer
	// The acquire outlives a caller that goes away: the driver may make the
	// resource anyway, and its row must learn the handle.
	actx := context.WithoutCancel(ctx)
	timedOut := false
	err = s.secretsSent(actx, jitDec, func() error { // the ledger, around a jit_config's call
		err := s.call(actx, b, verbAcquire, "", args, &ans)
		if errors.Is(err, errTimeout) {
			// SPEC 20: retry with the same key, never acquire afresh.
			timedOut = true
			err = s.call(actx, b, verbAcquire, "", args, &ans)
		}
		return err
	})
	err = scrubJIT(err, req.JITConfig) // a driver echoing it in a refusal never reaches the caller
	if err != nil && (timedOut || s.inflight.cancelSent(fk) || !refusal(err)) {
		// Cut off, failed in any way after a cancel was sent, or failed
		// without refusing (a driver that crashed, answered nothing or too
		// much may have made the box): what it made is found by list.
		return s.cutOff(actx, t.Project.ID, db, b, req.RequestID, fk, seen, err) // acquires.go
	}
	if err != nil {
		if !seen {
			// A refusal made nothing: forget the request so the key is free.
			if derr := deleteAcquiring(actx, db, b.Name, req.RequestID); derr == nil {
				err = driverRefused(b.Kind, b.Name, err) // refused.go: nothing kept, so the caller may ask another binding
			}
		}
		return nil, err
	}
	if !strings.HasPrefix(ans.Handle, b.Kind+":"+b.Name+"/") {
		return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s answered handle %q, not one of %s's", b.Driver, ans.Handle, b.Name)
	}
	if b.Kind != computeapi.RunTargetKindDB && commit != "" && ans.Commit != "" && ans.Commit != commit {
		return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s echoed commit %s, not %s", b.Driver, ans.Commit, commit)
	}
	state := stateRunning
	if b.Kind != computeapi.RunTargetKindRun {
		state = stateReady
	}
	if b.Kind == computeapi.RunTargetKindBox {
		size := sizeName(ans.Size)
		if err := setBox(actx, db, b.Name, req.RequestID, size, ans.ImageDigest, req.Persist); err != nil {
			return nil, err
		}
		why := databaseAnswer(dbRecipe, ans.Database)
		if want := cmp.Or(pinDigest(pin), x.digest); want != "" && ans.ImageDigest != want {
			why = fmt.Sprintf("booted image %s, not the pinned %s", ans.ImageDigest, want) // something other than the owner's pin
		} else if net == computeapi.NetOff && ans.Net != computeapi.NetOff {
			why = netNotHeld(ans.Net) // the renter turned the network off: unconfirmed is not off
		}
		if why == "" && x.base != nil {
			why = x.base.confirm(ans.Volumes)
		}
		if why != "" { // end it
			_ = s.record(actx, t.Project.ID, db, b.Name, req.RequestID, upd{handle: ans.Handle, state: stateReady, acquired: now,
				expiresAt: ans.ExpiresAt, ttl: ans.TTLS})
			if err := s.releaseRow(actx, t.Project.ID, db, b, ans.Handle, req.RequestID, nil); err != nil {
				// Still live as far as zm knows: the holder, the owner or the sweep ends it.
				return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s %s, and did not release it (%s): release %s", b.Driver, why, errText(err), ans.Handle)
			}
			return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s %s; released", b.Driver, why)
		}
	}
	if err := s.record(actx, t.Project.ID, db, b.Name, req.RequestID, upd{handle: ans.Handle, state: state, commit: ans.Commit,
		expiresAt: ans.ExpiresAt, ttl: ans.TTLS, log: ans.Log, spend: s.unitSpend(d, ans.Spend), acquired: now}); err != nil {
		return nil, err
	}
	row, _, err := byRequest(actx, db, b.Name, req.RequestID) // actx: a cancel that stood is acted on though the caller left
	if err != nil {
		return nil, err
	}
	if err := s.cancelStood(actx, t.Project.ID, db, b, fk, row); err != nil { // acquires.go
		return nil, err
	}
	h := row.RunTargetHandle
	h.URL = ans.URL // only here, never stored
	if b.Kind == computeapi.RunTargetKindBox && (ans.Net == computeapi.NetOn || ans.Net == computeapi.NetOff) {
		h.Net = ans.Net // only here too: what the driver made, not what zm asked
	}
	if b.Kind == computeapi.RunTargetKindBox {
		h.Volumes = ans.Volumes // only here: which are new
	}
	return h, nil
}
