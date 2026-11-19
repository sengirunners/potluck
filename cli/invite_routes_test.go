package cli

import (
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// An invite that left a route out says which, why, how to fix it, and what
// the code needs instead; one that used the first route says nothing.
func TestRoutesDownLines(t *testing.T) {
	if got := routesDown(api.InviteResponse{Code: "https://home.example#fp:secret"}); got != "" {
		t.Fatalf("first route used: %q, want nothing", got)
	}
	got := routesDown(api.InviteResponse{Code: "potluck-mini:fp:ABCD-EFGH", NotUsed: []api.InviteRouteDown{{
		Via: api.ViaTunnel, Reason: "the tunnel https://home.example does not reach this machine now: it did not answer", Hint: "check cloudflared is running"}}})
	for _, want := range []string{
		"not by the tunnel: the tunnel https://home.example does not reach this machine now: it did not answer\n",
		"  to fix it: check cloudflared is running\n",
		"so this code is for the tailnet instead: it works only for a machine on your tailnet",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
