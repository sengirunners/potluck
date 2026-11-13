package runtarget

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// A box on another machine whose class the authority cannot read as
// trusted (here: no teamlink at all) gets no secret, and the grants are
// never read (this Service has no store: reading them would fail): it fails
// closed before anything is weighed. A box not ready is refused first.
func TestBoxSecretsGateFailsClosed(t *testing.T) {
	s := &Service{now: time.Now}
	a := protocol.BoxSecretsArgs{Secrets: map[string]string{"API_KEY_ZZQ": "zm-gate-canary-3b7d"}}
	rw := row{RunTargetHandle: computeapi.RunTargetHandle{Handle: "box:pub@lender/r1", State: stateReady,
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}}
	pub := computeapi.RunTarget{Name: "pub@lender", Kind: computeapi.RunTargetKindBox, Host: "lender", HostBinding: "pub", HostProject: "p"}
	_, err := s.boxSecretsGate(context.Background(), "p", pub, rw, a)
	if e, ok := err.(*api.Error); !ok || e.Code != api.CodeForbidden || !strings.Contains(e.Message, "public host") || strings.Contains(e.Error(), "zm-gate-canary") {
		t.Errorf("a public host: %v, want forbidden", err)
	}
	gone := rw
	gone.State = "released"
	if _, err := s.boxSecretsGate(context.Background(), "p", pub, gone, a); err == nil || err.(*api.Error).Code != api.CodeConflict {
		t.Errorf("a released box: %v, want conflict", err)
	}
}

// TestNotWritten: only a refusal says a secrets call wrote nothing; a
// failure inside (the driver crashed, answered no JSON, a scrubbed
// timeout), a timeout, a lost host or an error with no code may have.
func TestNotWritten(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{api.Errorf(api.CodeBadRequest, "no"), true},
		{&driverWords{api.Errorf(api.CodeConflict, "no")}, true},
		{api.Errorf(api.CodeInternal, "no JSON answer"), false},
		{&driverWords{api.Errorf(api.CodeInternal, "crashed")}, false},
		{api.Errorf(api.CodeUnavailable, "host not heard from"), false},
		{&api.Error{Code: rt.CodeDriverTimeout, Message: "did not answer"}, false},
		{errTimeout, false},
	} {
		if got := notWritten(tc.err); got != tc.want {
			t.Errorf("notWritten(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
