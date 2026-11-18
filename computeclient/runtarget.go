package computeclient

// One method per /v0/run_targets route in API.md (SPEC 20). The project is
// the resolved one (Project, Cwd), as for trackers.

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
)

func runTargetPath(target string) string { return "/v0/run_targets/" + client.Seg(target) }

// RunTargets is GET /v0/run_targets (describe asks each driver).
func RunTargets(ctx context.Context, c *client.Client, describe bool) (computeapi.RunTargetList, error) {
	var l computeapi.RunTargetList
	p := "/v0/run_targets"
	if describe {
		p += "?describe=1"
	}
	return l, c.Do(ctx, http.MethodGet, p, nil, &l)
}

// RunTargetsLive is GET /v0/run_targets?describe=1&live=1 (the machine's
// owner only): RunTargets described, each of this machine's bindings with
// its live handles (Live, and Stopped among them) as its driver lists them.
func RunTargetsLive(ctx context.Context, c *client.Client) (computeapi.RunTargetList, error) {
	var l computeapi.RunTargetList
	return l, c.Do(ctx, http.MethodGet, "/v0/run_targets?describe=1&live=1", nil, &l)
}

// BindRunTarget is POST /v0/run_targets (the machine's owner only).
func BindRunTarget(ctx context.Context, c *client.Client, req computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
	var v computeapi.RunTargetView
	return v, c.Do(ctx, http.MethodPost, "/v0/run_targets", req, &v)
}

// HostRunTargets is GET /v0/run_targets?host=1: this machine's machine-level
// bindings (the machine's owner only).
func HostRunTargets(ctx context.Context, c *client.Client) (computeapi.RunTargetList, error) {
	var l computeapi.RunTargetList
	return l, c.Do(ctx, http.MethodGet, "/v0/run_targets?host=1", nil, &l)
}

// HostRunTargetsLive is GET /v0/run_targets?host=1&live=1: HostRunTargets
// with each binding's live handles (Live, and Stopped among them) as its
// driver lists them; a binding whose driver did not answer has Error set.
func HostRunTargetsLive(ctx context.Context, c *client.Client) (computeapi.RunTargetList, error) {
	var l computeapi.RunTargetList
	return l, c.Do(ctx, http.MethodGet, "/v0/run_targets?host=1&live=1", nil, &l)
}

// UnbindHostRunTarget is DELETE /v0/run_targets/{target}?host=1.
func UnbindHostRunTarget(ctx context.Context, c *client.Client, target string) error {
	return c.Do(ctx, http.MethodDelete, runTargetPath(target)+"?host=1", nil, nil)
}

// UnbindRunTarget is DELETE /v0/run_targets/{target}.
func UnbindRunTarget(ctx context.Context, c *client.Client, target string) error {
	return c.Do(ctx, http.MethodDelete, runTargetPath(target), nil, nil)
}

// DescribeRunTarget is GET /v0/run_targets/{target}.
func DescribeRunTarget(ctx context.Context, c *client.Client, target string) (computeapi.RunTargetView, error) {
	var v computeapi.RunTargetView
	return v, c.Do(ctx, http.MethodGet, runTargetPath(target), nil, &v)
}

// AcquireRunTarget is POST /v0/run_targets/{target}/acquire.
func AcquireRunTarget(ctx context.Context, c *client.Client, target string, req computeapi.AcquireRunTargetRequest) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/acquire", req, &h)
}

// HandleFilter narrows a list of handles: Live keeps those not yet ended,
// Mine those the caller made.
type HandleFilter struct {
	Live, Mine bool
}

// RunTargetHandles is GET /v0/run_targets/{target}/handles.
func RunTargetHandles(ctx context.Context, c *client.Client, target string, live bool) ([]computeapi.RunTargetHandle, error) {
	return ListHandles(ctx, c, target, HandleFilter{Live: live})
}

// ListHandles is GET /v0/run_targets/{target}/handles with filter f.
func ListHandles(ctx context.Context, c *client.Client, target string, f HandleFilter) ([]computeapi.RunTargetHandle, error) {
	var l computeapi.RunTargetHandles
	q := url.Values{}
	if f.Live {
		q.Set("live", "1")
	}
	if f.Mine {
		q.Set("mine", "1")
	}
	p := runTargetPath(target) + "/handles"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return l.Handles, c.Do(ctx, http.MethodGet, p, nil, &l)
}

// RunTargetStatus is GET /v0/run_targets/{target}/handles/{id} (id: the
// part of the handle after the binding's name).
func RunTargetStatus(ctx context.Context, c *client.Client, target, id string) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, c.Do(ctx, http.MethodGet, runTargetPath(target)+"/handles/"+client.Seg(id), nil, &h)
}

// ReleaseRunTarget is POST /v0/run_targets/{target}/handles/{id}/release.
func ReleaseRunTarget(ctx context.Context, c *client.Client, target, id string) (computeapi.RunTargetRelease, error) {
	var r computeapi.RunTargetRelease
	return r, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/release", struct{}{}, &r)
}

// ExecRunTarget is POST /v0/run_targets/{target}/handles/{id}/exec (a box's holder).
func ExecRunTarget(ctx context.Context, c *client.Client, target, id string, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
	var r computeapi.RunTargetExec
	return r, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/exec", req, &r)
}

// RenewRunTarget is POST /v0/run_targets/{target}/handles/{id}/renew.
func RenewRunTarget(ctx context.Context, c *client.Client, target, id string, ttlS int) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/renew", computeapi.RenewRunTargetRequest{TTLS: ttlS}, &h)
}

// SpawnRunTarget is POST .../handles/{id}/spawn: a background process in a
// box you hold (SPEC 20 "Boxes", background processes).
func SpawnRunTarget(ctx context.Context, c *client.Client, target, id string, req computeapi.SpawnRunTargetRequest) (computeapi.RunTargetSpawn, error) {
	var r computeapi.RunTargetSpawn
	return r, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/spawn", req, &r)
}

// RunTargetOutput is POST .../handles/{id}/output: a page of a background
// process's output, waiting up to wait_s for more.
func RunTargetOutput(ctx context.Context, c *client.Client, target, id string, req computeapi.OutputRunTargetRequest) (computeapi.RunTargetOutput, error) {
	var r computeapi.RunTargetOutput
	return r, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/output", req, &r)
}

// SignalRunTarget is POST .../handles/{id}/signal: TERM, INT or KILL to a
// background process's group.
func SignalRunTarget(ctx context.Context, c *client.Client, target, id string, req computeapi.SignalRunTargetRequest) (computeapi.RunTargetSignal, error) {
	var r computeapi.RunTargetSignal
	return r, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/signal", req, &r)
}

// StopRunTarget is POST .../handles/{id}/stop: a persistent box stops, its
// disk kept (SPEC 20 "Boxes", Kept disks).
func StopRunTarget(ctx context.Context, c *client.Client, target, id string) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/stop", struct{}{}, &h)
}

// StartRunTarget is POST .../handles/{id}/start: a stopped box boots its kept
// disk again with a new lease (ttlS 0: the driver's default).
func StartRunTarget(ctx context.Context, c *client.Client, target, id string, ttlS int) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/start", computeapi.StartRunTargetRequest{TTLS: ttlS}, &h)
}

// Owner is c's machine-owner binding calls as methods: hostprobe's Binder
// (with its optional lister and unbinder).
type Owner struct{ C *client.Client }

// RunTargets is RunTargets on o.C.
func (o Owner) RunTargets(ctx context.Context, describe bool) (computeapi.RunTargetList, error) {
	return RunTargets(ctx, o.C, describe)
}

// BindRunTarget is BindRunTarget on o.C.
func (o Owner) BindRunTarget(ctx context.Context, req computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
	return BindRunTarget(ctx, o.C, req)
}

// UnbindRunTarget is UnbindRunTarget on o.C.
func (o Owner) UnbindRunTarget(ctx context.Context, target string) error {
	return UnbindRunTarget(ctx, o.C, target)
}

// UnbindHostRunTarget is UnbindHostRunTarget on o.C.
func (o Owner) UnbindHostRunTarget(ctx context.Context, target string) error {
	return UnbindHostRunTarget(ctx, o.C, target)
}

// Sealing is GET /v0/sealing: the pool's sealing mode and its machines.
func Sealing(ctx context.Context, c *client.Client) (computeapi.Sealing, error) {
	var v computeapi.Sealing
	return v, c.Do(ctx, http.MethodGet, "/v0/sealing", nil, &v)
}

// SetSealing is PUT /v0/sealing (admin on the pool).
func SetSealing(ctx context.Context, c *client.Client, req computeapi.SetSealingRequest) (computeapi.Sealing, error) {
	var v computeapi.Sealing
	return v, c.Do(ctx, http.MethodPut, "/v0/sealing", req, &v)
}

// ForgetSealedHome is POST /v0/sealing/forget (this machine's owner).
func ForgetSealedHome(ctx context.Context, c *client.Client, pool string) (computeapi.ForgetSealedHome, error) {
	var v computeapi.ForgetSealedHome
	return v, c.Do(ctx, http.MethodPost, "/v0/sealing/forget", computeapi.ForgetSealedHomeRequest{Pool: pool}, &v)
}

// RunTargetAcquireProgress is GET /v0/run_targets/{target}/acquires/{request_id}.
// actor "" is the caller; another actor is the machine owner's to name, as
// in an acquire's Actor.
func RunTargetAcquireProgress(ctx context.Context, c *client.Client, target, requestID, actor string) (computeapi.RunTargetAcquireProgress, error) {
	var p computeapi.RunTargetAcquireProgress
	return p, c.Do(ctx, http.MethodGet, acquirePath(target, requestID, actor, ""), nil, &p)
}

// CancelRunTargetAcquire is POST /v0/run_targets/{target}/acquires/{request_id}/cancel
// (actor as in RunTargetAcquireProgress).
func CancelRunTargetAcquire(ctx context.Context, c *client.Client, target, requestID, actor string) (computeapi.RunTargetAcquireCancel, error) {
	var a computeapi.RunTargetAcquireCancel
	return a, c.Do(ctx, http.MethodPost, acquirePath(target, requestID, actor, "/cancel"), struct{}{}, &a)
}

func acquirePath(target, requestID, actor, verb string) string {
	p := runTargetPath(target) + "/acquires/" + client.Seg(requestID) + verb
	if actor != "" {
		p += "?actor=" + url.QueryEscape(actor)
	}
	return p
}
