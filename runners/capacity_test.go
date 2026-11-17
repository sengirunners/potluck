package runners_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

// capacityRead is GET path on the broker's public listener: status, body and
// headers.
func capacityRead(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// A host's capacity report is served on the public listener as it was
// published, until its ttl ends; nothing else answers for a host that never
// published.
func TestCapacityReportPublishedAndServed(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.svc.PublicHandler())
	defer srv.Close()
	ctx := context.Background()
	url := srv.URL + "/v0/runners/capacity/factory-1"

	if code, _, _ := capacityRead(t, url); code != http.StatusNotFound {
		t.Fatalf("before any publish: %d, want 404", code)
	}
	report := `{"schema": "example-capacity-v1", "ready": true, "free": {"oncall-pc-2": 3}, "at": "2026-11-17T19:00:00+00:00"}`
	var put struct {
		Host      string `json:"host"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := f.owner.Do(ctx, "PUT", "/v0/runners/capacity/factory-1?ttl_s=30", rawJSON(report), &put); err != nil {
		t.Fatal(err)
	}
	if put.Host != "factory-1" || put.ExpiresAt == "" {
		t.Fatalf("publish answer: %+v", put)
	}
	code, body, h := capacityRead(t, url)
	want := `{"schema":"example-capacity-v1","ready":true,"free":{"oncall-pc-2":3},"at":"2026-11-17T19:00:00+00:00"}`
	if code != http.StatusOK || body != want {
		t.Fatalf("served: %d %s, want 200 %s", code, body, want)
	}
	if h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", h)
	}
	// The daemon's own router serves the same answer without a token.
	guest := client.WithHandler(f.dm.Handler)
	var got map[string]any
	if err := guest.Do(ctx, "GET", "/v0/runners/capacity/factory-1", nil, &got); err != nil || got["ready"] != true {
		t.Fatalf("router read: %v %v", got, err)
	}
	if code, _, _ := capacityRead(t, srv.URL+"/v0/runners/capacity/factory-2"); code != http.StatusNotFound {
		t.Fatalf("another host: %d, want 404", code)
	}
	if code, _, _ := capacityRead(t, srv.URL+"/v0/runners/capacity/Bad_Name"); code != http.StatusNotFound {
		t.Fatalf("a bad host name: %d, want 404", code)
	}
	// A newer report replaces it; past its ttl it is gone.
	if err := f.owner.Do(ctx, "PUT", "/v0/runners/capacity/factory-1", rawJSON(`{"ready": false}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := capacityRead(t, url); body != `{"ready":false}` {
		t.Fatalf("replaced: %s", body)
	}
	f.clock.advance(59 * time.Second)
	if code, _, _ := capacityRead(t, url); code != http.StatusOK {
		t.Fatalf("within the default ttl: %d", code)
	}
	f.clock.advance(2 * time.Second)
	if code, _, _ := capacityRead(t, url); code != http.StatusNotFound {
		t.Fatalf("past its ttl: %d, want 404", code)
	}
}

// Only the host's own machine (or the authority's owner) publishes; bad
// reports and ttls are refused.
func TestCapacityReportRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		path, body string
	}{
		{"/v0/runners/capacity/factory-1", `[1, 2]`},
		{"/v0/runners/capacity/factory-1", `"ready"`},
		{"/v0/runners/capacity/factory-1", `null`},
		{"/v0/runners/capacity/factory-1", `{"pad": "` + strings.Repeat("x", 64<<10) + `"}`},
		{"/v0/runners/capacity/factory-1?ttl_s=5", `{}`},
		{"/v0/runners/capacity/factory-1?ttl_s=601", `{}`},
		{"/v0/runners/capacity/factory-1?ttl_s=x", `{}`},
		{"/v0/runners/capacity/Factory_1", `{}`},
	} {
		if err := f.owner.Do(ctx, "PUT", c.path, rawJSON(c.body), nil); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("PUT %s %.40s: %v, want bad_request", c.path, c.body, err)
		}
	}
	guest := client.WithHandler(f.dm.Handler)
	if err := guest.Do(ctx, "PUT", "/v0/runners/capacity/factory-1", rawJSON(`{}`), nil); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("anonymous publish: %v", err)
	}
	// A member of the pool project that is not the host's replica.
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "pool", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	bo := client.WithHandler(f.dm.Handler)
	bo.Token, bo.Project, bo.ProjectFrom = tok.Secret, "pool", "flag"
	if err := bo.Do(ctx, "PUT", "/v0/runners/capacity/factory-1", rawJSON(`{}`), nil); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member's token publishing a host's report: %v, want forbidden", err)
	}
}

type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// The publish rule: the host's own replica (node-bound token, Machine is the
// host) seeing the pool, or the owner on the socket; never another machine's
// replica, a replica that cannot see the pool, or a remote caller with an
// all scope.
func TestMayPublishCapacity(t *testing.T) {
	const pool = "01POOLPROJECT"
	sees := authz.FromTuples([]authz.Tuple{{Subject: "id", Relation: authz.Member, Object: authz.ProjectObject(pool)}}, nil)
	other := authz.FromTuples([]authz.Tuple{{Subject: "id", Relation: authz.Member, Object: authz.ProjectObject("01ELSEWHERE")}}, nil)
	for _, c := range []struct {
		name string
		sub  *authz.Subject
		want bool
	}{
		{"host's replica", &authz.Subject{Node: "n1", Machine: "factory-1", Scope: sees, Remote: true}, true},
		{"owner on the socket", &authz.Subject{Scope: authz.All()}, true},
		{"another machine's replica", &authz.Subject{Node: "n2", Machine: "factory-2", Scope: sees, Remote: true}, false},
		{"replica not in the pool", &authz.Subject{Node: "n1", Machine: "factory-1", Scope: other, Remote: true}, false},
		{"plain token named like the host", &authz.Subject{Machine: "factory-1", Scope: sees, Remote: true}, false},
		{"remote all scope", &authz.Subject{Scope: authz.All(), Remote: true}, false},
		{"nobody", nil, false},
	} {
		if got := runners.MayPublishCapacity(c.sub, "factory-1", pool); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
