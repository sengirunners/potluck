package runtarget_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/pushbase"
)

func TestPushBaseClientFillsSealsAndReuses(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	fixture, err := refdriver.NewRepo(t.TempDir(), "echo fixture\n")
	if err != nil {
		t.Fatal(err)
	}
	src, err := boxpush.Resolve(ctx, boxpush.MakeOptions{Dir: fixture.Worktree})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := pushbase.Identify(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	plan := &pushbase.Plan{Repository: repo, Dir: src.Repo}
	for _, key := range []string{"first-client", "repeat-client"} {
		req := computeapi.AcquireRunTargetRequest{RequestID: key, TTLS: 120}
		target, h, err := pushbase.Acquire(ctx, f.ana, computeclient.BoxWant{On: "deps"}, &req, nil, plan)
		if err != nil || h.Base == nil || !h.Base.Hit {
			t.Fatalf("%s acquired %+v %v", key, h, err)
		}
		result, err := boxpush.PushSource(ctx, boxpush.Exec(f.ana, target, idOf(h.Handle)), src, boxpush.PushOptions{}, nil)
		if err != nil || len(result.Base) != 1 {
			t.Fatalf("%s push %+v %v", key, result, err)
		}
		if got := boxExec(t, f.ana, h, `cd checkout && git rev-parse HEAD`); got.Exit != 0 || got.Stdout != src.Head+"\n" {
			t.Fatalf("checkout %+v", got)
		}
	}
	builders := 0
	for _, a := range f.requests(t, "acquire", true) {
		if string(a["net"]) == `"off"` {
			builders++
		}
	}
	if builders != 1 {
		t.Fatalf("filled %d builders for two tasks", builders)
	}
}

func TestPushBaseClientReconcilesLostBuilderResponse(t *testing.T) {
	f := newBaseFix(t)
	ctx := context.Background()
	dropped := false
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/acquire") {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			var req computeapi.AcquireRunTargetRequest
			_ = json.Unmarshal(b, &req)
			if req.Base != nil && req.Base.Fill && !dropped {
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, r)
				if rec.Code != http.StatusOK {
					t.Fatalf("builder setup: %d %s", rec.Code, rec.Body.String())
				}
				dropped = true
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				api.WriteError(w, api.Errorf(api.CodeInternal, "response lost after acquire"))
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	c.Token, c.Project, c.ProjectFrom = f.ana.Token, "acme", "flag"
	req := computeapi.AcquireRunTargetRequest{RequestID: "lost-builder-client", TTLS: 120}
	plan := &pushbase.Plan{Repository: &pushbase.Repository{Key: testBaseRepo, Commit: strings.Repeat("a", 40)}}
	_, h, err := pushbase.Acquire(ctx, c, computeclient.BoxWant{On: "deps"}, &req, nil, plan)
	if err != nil || !dropped || h.Base != nil {
		t.Fatalf("fallback %+v %v dropped=%v", h, err, dropped)
	}
	handles, err := computeclient.RunTargetHandles(ctx, f.ana, "deps", true)
	if err != nil || len(handles) != 1 || handles[0].Handle != h.Handle {
		t.Fatalf("builder still occupies capacity: %+v %v", handles, err)
	}
}
