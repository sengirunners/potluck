package cli

import (
	"context"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func assertJobTiming(t *testing.T, total float64, setup *float64, phases map[string]float64) {
	t.Helper()
	sum := 0.0
	for _, phase := range []string{"prepare", "acquire", "push", "start", "run", "collect", "cleanup"} {
		d, ok := phases[phase]
		if !ok || d < 0 {
			t.Fatalf("missing/negative %s in %v", phase, phases)
		}
		sum += d
	}
	if math.Abs(sum-total) > 0.01 {
		t.Fatalf("phase sum %.6f differs from total %.6f", sum, total)
	}
	if setup == nil || *setup <= 0 || *setup > total {
		t.Fatalf("invalid setup %v total %v", setup, total)
	}
	wantSetup := phases["prepare"] + phases["acquire"] + phases["push"] + phases["start"]
	if math.Abs(*setup-wantSetup) > 0.01 {
		t.Fatalf("setup %f differs from phases %f", *setup, wantSetup)
	}
}

func TestJobTypedFailuresAndLostState(t *testing.T) {
	for _, mode := range []string{"no_target", "forbidden", "lost", "output_forbidden"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakePool{bodies: map[string][]string{}}
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				if r.URL.Path == "/v0/run_targets" {
					if mode == "no_target" {
						api.WriteJSON(w, 200, computeapi.RunTargetList{})
						return
					}
					if mode == "forbidden" {
						api.WriteError(w, api.Errorf(api.CodeForbidden, "no access"))
						return
					}
				}
				if r.URL.Path == "/v0/run_targets/t/handles/h1/output" {
					if mode == "output_forbidden" {
						api.WriteError(w, api.Errorf(api.CodeForbidden, "gone"))
					} else {
						api.WriteJSON(w, 200, computeapi.RunTargetOutput{})
					}
					return
				}
				f.ServeHTTP(w, r)
			}))
			result, err := boxJob(context.Background(), c, boxJobOptions{timeout: time.Minute, out: io.Discard, errw: io.Discard}, jobResult{State: "failed"}, func(context.Context, acquiredBox) (string, error) { return "p1", nil })
			want := mode
			if mode == "lost" {
				want = ""
			}
			if mode == "output_forbidden" {
				want = "forbidden"
			}
			if err == nil || result.ErrorCode != want {
				t.Fatalf("result %+v err %v", result, err)
			}
			if mode == "lost" || mode == "output_forbidden" {
				if result.State != "lost" || !result.Released {
					t.Fatalf("lost output treated as success: %+v", result)
				}
				assertJobTiming(t, result.Seconds, result.SetupS, result.Phases)
			} else if result.SetupS != nil {
				t.Fatal("setup reported despite no spawn")
			}
		})
	}
}

func TestJobCancelledAcquireChargesRecoveryToCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &prepDaemon{fail: map[int]*api.Error{1: api.Errorf(api.CodeUnavailable, "answer lost")}, hook: map[int]func(*http.Request){
		1: func(*http.Request) { cancel() },
		2: func(*http.Request) { time.Sleep(20 * time.Millisecond) },
	}}
	r, err := boxJob(ctx, client.WithHandler(d), boxJobOptions{timeout: time.Minute, out: io.Discard, errw: io.Discard}, jobResult{State: "failed"}, func(context.Context, acquiredBox) (string, error) {
		t.Fatal("cancelled acquire started a job")
		return "", nil
	})
	if err == nil || r.State != "interrupted" || !r.Released || r.SetupS != nil {
		t.Fatalf("cancelled acquire: %+v %v", r, err)
	}
	if r.Phases["cleanup"] < 0.020 || r.Phases["acquire"]+r.Phases["cleanup"] > r.Seconds+0.001 {
		t.Fatalf("recovery not accounted as cleanup: %+v", r)
	}
}
