package runtarget

// Run targets' rows for teamlink's proxy (SPEC 20), registered by relayOn.
// Bindings are made on the machine that serves them, so bind and unbind
// for a joined project are refused and the machine-level ?host=1 reads stay
// local; everything else is the joined project's authority's (boxes across
// machines), by -P or the cwd. Each call asks a driver, on the authority or
// relayed to a host, and may take its verb's limit (runTargetAnswerWithin);
// none is hedged, and their answers (exec output, an acquire's url) are
// never stored as a live write's record: a repeat is safe by request_id or
// holder checks. Secrets and a jit_config cross the tunnel only sealed
// (SPEC 17.11). The database recipe rows ask no driver: they read and edit
// the authority's project.toml, which is what its acquires resolve.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	tl "github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// RelayRoutes are the run-target routes' rows for teamlink's proxy.
func RelayRoutes() []tl.RouteRule {
	h := &tl.RouteHook{
		// This machine's own machine-level bindings (the owner's).
		Local: func(r *http.Request) bool { return r.URL.Query().Get("host") == "1" || hostBind(r) },
		Seal:  carriesSecret,
		// An acquire's deps carry a layer's files (deps.go): more than 1 MiB.
		BigBody: func(r *http.Request, body []byte) bool {
			n, _ := forwardedDeps(r.Method, r.URL.Path, body)
			return n > 0
		},
		AnswerWithin: func(r *http.Request, body []byte) time.Duration {
			return runTargetAnswerWithin(r.Method, r.URL.Path, body)
		},
	}
	bind := *h
	bind.Refuse = func(_ *http.Request, _ []byte, snap api.Remote) error { return runBindRefused(snap.Project.Slug) }
	once := func(p, why string, h *tl.RouteHook) tl.RouteRule {
		return tl.RouteRule{Pattern: p, Kind: tl.KindWrite, Hedge: tl.Once, Why: why, Hook: h}
	}
	return []tl.RouteRule{
		once("GET /v0/run_targets/{target}/handles/{id}/previews", "previews belong to the lease authority", h),
		once("POST /v0/run_targets/{target}/handles/{id}/previews", "publish or revoke once", h),
		once("POST /v0/run_targets/{target}/handles/{id}/port", "a bounded port channel operation, never hedged", h),
		once("GET /v0/run_targets", "?describe=1 asks every driver: a hedge doubles that work", h),
		once("GET /v0/run_targets/{target}", "describe asks the driver", h),
		once("GET /v0/run_targets/{target}/handles", "refreshes live handles from their driver", h),
		once("GET /v0/run_targets/{target}/handles/{id}", "status asks the driver (relayed to a host's: a machine op)", h),
		once("POST /v0/run_targets", "refused for a joined project (bound where served)", &bind),
		once("DELETE /v0/run_targets/{target}", "refused for a joined project (bound where served)", &bind),
		once("POST /v0/run_targets/{target}/acquire", "a driver call; a retry with its request_id answers the same handle", h),
		once("POST /v0/run_targets/{target}/handles/{id}/exec", "runs a command once; its output is never kept at the authority", h),
		once("POST /v0/run_targets/{target}/handles/{id}/output", "a driver call that waits up to wait_s; its output is never kept at the authority", h),
		once("POST /v0/run_targets/{target}/handles/{id}/release", "a driver call, once", h),
		once("POST /v0/run_targets/{target}/handles/{id}/renew", "a driver call, once", h),
		once("POST /v0/run_targets/{target}/handles/{id}/input", "writes keystrokes once; a hedged copy would type them twice", h),
		once("POST /v0/run_targets/{target}/handles/{id}/signal", "a driver call, once", h),
		once("POST /v0/run_targets/{target}/handles/{id}/spawn", "starts a process once; a retry with its request_id answers the same pid", h),
		once("POST /v0/run_targets/{target}/handles/{id}/stop", "a driver call, once; a stopped box answers as it is", h),
		once("POST /v0/run_targets/{target}/handles/{id}/start", "boots a box once; a started box answers as it is", h),
		once("GET /v0/sealing", "the pool's sealing mode, as its machine heard its machines", h),
		once("PUT /v0/sealing", "sets the pool's sealing mode once", h),
		once("POST /v0/sealing/forget", "this machine's own record of its pools' machines", &tl.RouteHook{Local: func(*http.Request) bool { return true }}),
		once("GET /v0/databases", "the project's database recipes, as its authority's project.toml says", nil),
		once("PUT /v0/databases/{name}", "an admin's edit of project.toml, sent once", nil),
		once("DELETE /v0/databases/{name}", "an admin's edit of project.toml, sent once", nil),
		once("POST /v0/run_targets/{target}/handles/{id}/secrets", "a driver call, once; its values cross the tunnel only sealed and are never kept", h),
	}
}

// runBindRefused answers a bind or unbind of a run target for a joined project.
func runBindRefused(slug string) error {
	return api.Errorf(api.CodeBadRequest, "run targets are bound on the machine that serves them, not for the joined project %s", slug).
		WithHint("on the lending machine: " + config.Command("target bind", "--host --share", slug) + " (or " + config.PotluckCommand("host", "--pool", slug) + ")")
}

// hostBind reports whether r binds a machine-level binding of this machine
// (zm target bind --host sends "host" in the body): it stays local, from a
// joined project's folder too. The body is put back as it was.
func hostBind(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v0/run_targets" {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, tl.MaxForwardBody+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
	var req struct {
		Host bool `json:"host"`
	}
	return err == nil && json.Unmarshal(b, &req) == nil && req.Host
}

// runTargetAnswerWithin is how long the authority may take to answer a
// run-target call a member's proxy forwards (0: not one): the verb's limit
// plus the relay's slack. An acquire may time out once, retry with the same
// key and reconcile from list (SPEC 20); an exec waits its timeout_s + 30 s.
// The authority writes nothing before its answer, so the proxy's
// CallTimeout of silence would cut these short while the host still works.
func runTargetAnswerWithin(method, path string, body []byte) time.Duration {
	if path != "/v0/run_targets" && !strings.HasPrefix(path, "/v0/run_targets/") {
		return 0
	}
	const margin = 10 * time.Second
	verb := rt.VerbLimit + rt.RelaySlack + margin
	switch {
	case method == http.MethodPost && strings.HasSuffix(path, "/acquire"):
		n, waitS := forwardedDeps(method, path, body)
		return 2*(rt.AcquireLimit+rt.RelaySlack) + verb + DepsAnswerWithin(n, waitS)
	case method == http.MethodPost && strings.HasSuffix(path, "/exec"):
		var req struct {
			TimeoutS int `json:"timeout_s"`
		}
		_ = json.Unmarshal(body, &req)
		if req.TimeoutS <= 0 {
			req.TimeoutS = rt.ExecDefaultS
		}
		return time.Duration(min(req.TimeoutS, rt.ExecMaxS)+30)*time.Second + rt.RelaySlack + margin
	}
	return verb
}
