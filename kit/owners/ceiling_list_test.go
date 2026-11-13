package owners_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// An owner sees a removal in the grants list's ceilings and on the GitHub
// grant it holds back (zm-eakh.12.23.28).
func TestCeilingsShownOnGitHubGrants(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	carl, _ := x.gh.AddUser("carlexample", "User")
	org, _ := x.gh.AddUser("acme-org", "Organization")
	x.gh.AddMember(org.ID, x.viewer, "active")
	x.gh.AddMember(org.ID, carl.ID, "active")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	n := carlNode(x)
	join := func(machine string) (*api.RedeemResponse, error) {
		t.Helper()
		x.gh.Approver(carl.ID, 0)
		out, err := n.join(t, "orchid", machine)
		return out.Join, err
	}
	if _, err := join("host-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.owner.Revoke(ctx, api.GrantRequest{Identity: "carlexample", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if j, err := join("host-b"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("removed carl joined: %+v, %v; want refused", j, err)
	}

	l, err := x.owner.GrantList(ctx, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Ceilings) != 1 || l.Ceilings[0].Identity != "carlexample" || l.Ceilings[0].Role != "" ||
		!slices.Equal(l.Ceilings[0].Accounts, []string{fmt.Sprintf("github:%d", carl.ID)}) {
		t.Fatalf("grants list ceilings: %+v; want carl's removal", l.Ceilings)
	}
	gl, err := x.owner.GitHubGrants(ctx, "orchid")
	if err != nil || len(gl.Grants) != 1 || len(gl.Grants[0].Ceilings) != 1 || gl.Grants[0].Ceilings[0].Identity != "carlexample" {
		t.Fatalf("GitHub grants: %+v, %v; want carl's grant showing his removal", gl, err)
	}
}
