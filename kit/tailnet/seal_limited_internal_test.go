package tailnet

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// A limited token opens nothing sealed: authSealed (the real sealAuth) refuses it
// with the token-limit answer even from its own device, where it does
// authenticate.
func TestSealAuthRefusesALimitedToken(t *testing.T) {
	ctx := context.Background()
	d, _ := sealDeps(t)
	st, err := store.Open(ctx, d.Config.Home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := project.Open(ctx, st, d.Config)
	if err != nil {
		t.Fatal(err)
	}
	d.Projects = reg
	local := reg.Local("")
	if _, err := reg.Create(ctx, local, api.NewProjectRequest{Slug: "proj"}); err != nil {
		t.Fatal(err)
	}
	pin := project.Pin{NodeID: "nDEV", NodeKey: "nodekey:dev", Name: "dev.example.ts.net"}
	out, err := reg.MintLimited(ctx, local, api.MintTokenRequest{Limit: "answer", Project: "proj", TTLSeconds: 600},
		project.LimitedMint{Pin: pin, MaxTTL: time.Hour, Admitted: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	rm := core.Remote{NodeID: pin.NodeID, NodeKey: pin.NodeKey}
	if _, err := reg.FromTokenCaller(ctx, out.Secret, project.Caller{Remote: true, NodeID: rm.NodeID, NodeKey: rm.NodeKey}); err != nil {
		t.Fatalf("from its device: %v", err)
	}
	r := httptest.NewRequest("POST", "/v0/run-targets/x", nil)
	r.Header.Set(api.HeaderAuthorization, "Bearer "+out.Secret)
	_, err = authSealed(&Service{d: d}, r, rm)
	if e, ok := err.(*api.Error); !ok || e.Code != api.CodeForbidden || string(e.Data) != `{"reason":"`+core.LimitReason+`"}` {
		t.Fatalf("sealAuth: %v", err)
	}
}
