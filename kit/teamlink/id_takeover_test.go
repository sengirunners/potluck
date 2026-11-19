package teamlink_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// A project's id is the authority's to choose, so a second authority can
// present the id of a project this machine joined before. The machine keeps
// the id bound to the key it first joined it at: a join that presents a
// known id at another key is refused, whether the old join was left or
// ended by the authority, and the authority it first joined can still join
// again at its own key.
func TestJoinRefusesAKnownProjectIDAtAnotherKey(t *testing.T) {
	old := teamlink.PurgeAfter
	teamlink.PurgeAfter = 0 // a revoked token purges at the third refusal
	t.Cleanup(func() { teamlink.PurgeAfter = old })
	for _, ended := range []string{"left", "purged"} {
		t.Run(ended, func(t *testing.T) {
			ctx := context.Background()
			tn := tailnettest.New()
			mini := tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}
			a := kitDaemon(t, tn, "mini", mini)
			m := kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})
			p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
				t.Fatal(err)
			}
			inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
				t.Fatal(err)
			}
			switch ended {
			case "left":
				if _, err := m.Leave(ctx, "orchid"); err != nil {
					t.Fatal(err)
				}
			case "purged":
				toks, err := a.Tokens(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, tok := range toks {
					if tok.Machine == "laptop" && tok.Revoked == nil {
						if _, err := a.RevokeToken(ctx, tok.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
				waitRemoteState(t, m, p.ID, api.RemotePurged)
			}
			// Another authority (another node key) presenting the same id:
			// here the same home behind a new node, which is what the
			// replica sees of any authority that chose this id.
			tn.SetIdentity("nMINI", tailnettest.Identity{NodeID: "nEVIL", NodeKey: "nodekey:evil", Login: "mallory@example.com", Host: "mini"})
			inv2, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana2", Role: api.RoleMember})
			if err != nil {
				t.Fatal(err)
			}
			if inv2.Code == inv.Code || !strings.Contains(inv2.Code, ":") {
				t.Fatalf("second code %q", inv2.Code)
			}
			_, err = m.Join(ctx, api.JoinRequest{Code: inv2.Code})
			if err == nil || !strings.Contains(err.Error(), "another authority") {
				t.Fatalf("join of a known project id at another key: %v, want a refusal", err)
			}
			rs, err := m.Remotes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rs {
				if r.Project.ID == p.ID && r.State == api.RemoteOK {
					t.Fatalf("the refused join left a live record: %+v", r)
				}
			}
			// The authority it first joined, at its own key, joins again.
			tn.SetIdentity("nEVIL", mini)
			inv3, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana3", Role: api.RoleMember})
			if err != nil {
				t.Fatal(err)
			}
			if j, err := m.Join(ctx, api.JoinRequest{Code: inv3.Code}); err != nil || j.Remote == nil || j.Remote.Project.ID != p.ID {
				t.Fatalf("join again at the first key: %+v %v", j, err)
			}
		})
	}
}

func waitRemoteState(t *testing.T, m *client.Client, id, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _ = m.GetProject(context.Background(), id) // a call to the authority
		rs, err := m.Remotes(context.Background())
		if err == nil {
			for _, r := range rs {
				if r.Project.ID == id && r.State == state {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote %s never %s: %+v %v", id, state, rs, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// When the authority itself got a new key, the refusal names the forget;
// this machine's owner forgets the old key (refused while a join of the id
// is recorded, live or ended, without a choice for host state, without the
// key, or at a key other than the one shown), and the
// next join binds the id at the new key, which then holds as the old one
// did: the old key is refused.
func TestForgetLetsAReKeyedAuthorityJoin(t *testing.T) {
	old := teamlink.PurgeAfter
	teamlink.PurgeAfter = 0 // a revoked token purges at the third refusal
	t.Cleanup(func() { teamlink.PurgeAfter = old })
	ctx := context.Background()
	tn := tailnettest.New()
	mini := tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}
	rekeyed := tailnettest.Identity{NodeID: "nMINI2", NodeKey: "nodekey:mini2", Login: "judy@example.com", Host: "mini"}
	a := kitDaemon(t, tn, "mini", mini)
	m := kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})
	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	join := func(as string) error {
		t.Helper()
		inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: as, Role: api.RoleMember})
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Join(ctx, api.JoinRequest{Code: inv.Code})
		return err
	}
	if err := join("ana"); err != nil {
		t.Fatal(err)
	}
	first, err := m.ProjectKey(ctx, p.ID)
	if err != nil || first.Fingerprint == "" {
		t.Fatalf("the bound key: %+v %v", first, err)
	}
	keep := api.ForgetProjectKeyRequest{HostState: api.ForgetKeepHostState, Fingerprint: first.Fingerprint}
	if _, err := m.ForgetProjectKey(ctx, p.ID, keep); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "while the join is recorded") {
		t.Fatalf("forget while joined: %v", err)
	}
	// An ended join's record refuses it too: a revoked one turns live again
	// by itself.
	toks, err := a.Tokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range toks {
		if tok.Machine == "laptop" && tok.Revoked == nil {
			if _, err := a.RevokeToken(ctx, tok.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitRemoteState(t, m, p.ID, api.RemotePurged)
	if _, err := m.ForgetProjectKey(ctx, p.ID, keep); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), api.RemotePurged) {
		t.Fatalf("forget with a purged join recorded: %v", err)
	}
	if _, err := m.Leave(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	tn.SetIdentity("nMINI", rekeyed)
	err = join("ana2")
	var ae *api.Error
	if !errors.As(err, &ae) || !strings.Contains(ae.Hint, "potluck forget "+p.ID) {
		t.Fatalf("join at the new key: %v, want a refusal whose hint names the forget", err)
	}
	if _, err := m.ForgetProjectKey(ctx, p.ID, api.ForgetProjectKeyRequest{Fingerprint: first.Fingerprint}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("forget without a host state: %v", err)
	}
	if _, err := m.ForgetProjectKey(ctx, p.ID, api.ForgetProjectKeyRequest{HostState: api.ForgetKeepHostState}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("forget that names no key: %v", err)
	}
	if _, err := m.ForgetProjectKey(ctx, p.ID, api.ForgetProjectKeyRequest{HostState: api.ForgetKeepHostState, Fingerprint: "sha256:other"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("forget at another key than the bound one: %v", err)
	}
	if _, err := m.ProjectKey(ctx, p.ID); err != nil {
		t.Fatalf("a refused forget removed the key: %v", err)
	}
	if out, err := m.ForgetProjectKey(ctx, p.ID, keep); err != nil || out.Key.Fingerprint != first.Fingerprint || len(out.Dropped) != 0 {
		t.Fatalf("forget: %+v %v", out, err)
	}
	if err := join("ana3"); err != nil {
		t.Fatalf("join at the new key after the forget: %v", err)
	}
	now, err := m.ProjectKey(ctx, p.ID)
	if err != nil || now.Fingerprint == first.Fingerprint {
		t.Fatalf("the key after the forget: %+v %v (first %s)", now, err, first.Fingerprint)
	}
	if _, err := m.Leave(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	tn.SetIdentity("nMINI2", mini)
	if err := join("ana4"); err == nil || !strings.Contains(err.Error(), "another authority") {
		t.Fatalf("join at the old key once the new one is bound: %v, want a refusal", err)
	}
}
