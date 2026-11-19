package pushbase_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/pushbase"
)

func TestAcquireBaseMissAndFallback(t *testing.T) {
	for _, mode := range []string{"hit", "old-daemon", "another-builder-sealed", "fill-refused", "evicted", "unrelated-missing"} {
		t.Run(mode, func(t *testing.T) {
			var asks []computeapi.AcquireRunTargetRequest
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				if r.URL.Path == "/v0/run_targets" {
					api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "box", Kind: "box"}, HostClass: api.MachineTrusted}}})
					return
				}
				if r.URL.Path != "/v0/run_targets/box/acquire" {
					t.Fatalf("unexpected request %s", r.URL.Path)
				}
				var req computeapi.AcquireRunTargetRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				asks = append(asks, req)
				if mode == "old-daemon" && req.Base != nil {
					api.WriteError(w, api.Errorf(api.CodeBadRequest, "unknown base field"))
					return
				}
				if req.Base != nil && req.Base.Fill {
					if mode == "fill-refused" {
						api.WriteError(w, api.Errorf(api.CodeBadRequest, "unsupported fill"))
					} else {
						api.WriteError(w, api.Errorf(api.CodeConflict, "sealed meanwhile"))
					}
					return
				}
				if mode == "unrelated-missing" {
					api.WriteError(w, api.Errorf(api.CodeNotFound, "binding missing"))
					return
				}
				if req.Base != nil && mode != "hit" && (len(asks) == 1 || mode == "evicted") {
					api.WriteError(w, api.Errorf(api.CodeNotFound, "missing base").WithData(computeapi.BaseRefusal{Reason: computeapi.BaseMissing}))
					return
				}
				api.WriteJSON(w, 200, computeapi.RunTargetHandle{Handle: "box:box/task", Base: &computeapi.BoxBaseAnswer{Hit: req.Base != nil}})
			}))
			req := computeapi.AcquireRunTargetRequest{RequestID: "task"}
			repo := &pushbase.Repository{Key: strings.Repeat("a", 64), Commit: strings.Repeat("b", 40)}
			var observed []time.Duration
			target, h, err := pushbase.Acquire(context.Background(), c, computeclient.BoxWant{}, &req, func(_ context.Context, _ string, r *computeapi.AcquireRunTargetRequest) error {
				r.TTLS = 77
				return nil
			}, &pushbase.Plan{Repository: repo, Dir: "unused", ObserveFill: func(d time.Duration) { observed = append(observed, d) }})
			if mode == "unrelated-missing" {
				if !api.IsCode(err, api.CodeNotFound) || len(asks) != 2 {
					t.Fatalf("unrelated error lost: %v %d", err, len(asks))
				}
				return
			}
			if err != nil || target != "box" || h.Handle != "box:box/task" {
				t.Fatalf("acquire: %s %+v %v", target, h, err)
			}
			if req.TTLS != 77 || asks[len(asks)-1].TTLS != 77 {
				t.Fatal("prepared request lost")
			}
			wantsFill := mode == "another-builder-sealed" || mode == "fill-refused" || mode == "evicted"
			if wantsFill != (len(observed) == 1) || (len(observed) > 0 && observed[0] <= 0) {
				t.Fatalf("fill observation: %v", observed)
			}
			fallback := mode == "fill-refused" || mode == "evicted" || mode == "old-daemon"
			if (req.Base == nil) != fallback || h.Base.Hit == fallback {
				t.Fatalf("fallback %v effective request %+v answer %+v", fallback, req, h)
			}
			for _, ask := range asks {
				if ask.Base != nil && ask.Base.Fill {
					if ask.RequestID == "task" || ask.Net != computeapi.NetOff {
						t.Fatalf("unsafe builder: %+v", ask)
					}
				} else if ask.RequestID != "task" {
					t.Fatal("task request identity changed")
				}
			}
		})
	}
}

func TestAcquireBaseDoesNotBypassPreparationRefusal(t *testing.T) {
	calledB := false
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.URL.Path == "/v0/run_targets" {
			var targets []computeapi.RunTargetView
			for _, name := range []string{"a", "b"} {
				targets = append(targets, computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: "box"}, HostClass: api.MachineTrusted, Describe: &computeapi.RunTargetDescribe{Kind: "box", Available: true, Size: &computeapi.RunTargetSize{Name: "small", OS: "linux", CPU: 1, MemoryMB: 1024}}})
			}
			api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: targets})
			return
		}
		if strings.Contains(r.URL.Path, "/b/") {
			calledB = true
		}
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "busy").WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedUnavailable}))
	}))
	req := computeapi.AcquireRunTargetRequest{RequestID: "task"}
	plan := &pushbase.Plan{Repository: &pushbase.Repository{Key: strings.Repeat("a", 64)}}
	_, _, err := pushbase.Acquire(context.Background(), c, computeclient.BoxWant{}, &req, func(_ context.Context, target string, r *computeapi.AcquireRunTargetRequest) error {
		if target == "b" {
			return api.Errorf(api.CodeBadRequest, "image does not fit dependencies")
		}
		r.TTLS = 77
		return nil
	}, plan)
	if !api.IsCode(err, api.CodeBadRequest) || calledB {
		t.Fatalf("preparation bypassed: B=%v error=%v", calledB, err)
	}
}
