package teamlink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// A machine's class is the owner's choice for the node that held the name.
// However its join ends (token revoked by the owner, the host leaving, token
// expired, grant revoked), a later join of the same node as a plain guest
// starts at public, not at the trusted class it was set to. A join that
// replaces a live one keeps it.
func TestClassForgottenWhenJoinEnds(t *testing.T) {
	ends := map[string]func(t *testing.T, ctx context.Context, d *core.Deps, orchidID string){
		"owner revokes the token": func(t *testing.T, ctx context.Context, d *core.Deps, _ string) {
			if _, err := d.Projects.RevokeToken(ctx, liveToken(t, ctx, d)); err != nil {
				t.Fatal(err)
			}
		},
		"the host leaves": func(t *testing.T, ctx context.Context, d *core.Deps, _ string) {
			sub := &authz.Subject{Node: "nBOB", TokenID: liveToken(t, ctx, d)}
			if err := d.Projects.RevokeOwn(ctx, sub); err != nil {
				t.Fatal(err)
			}
		},
		"the token expires": func(t *testing.T, ctx context.Context, d *core.Deps, _ string) {
			if _, err := d.Store.Registry().ExecContext(ctx, `UPDATE tokens SET expires = 1 WHERE node = 'nBOB'`); err != nil {
				t.Fatal(err)
			}
		},
		"the grant is revoked": func(t *testing.T, ctx context.Context, d *core.Deps, orchidID string) {
			if _, err := d.Store.Registry().ExecContext(ctx, `DELETE FROM grants WHERE project = ? AND subject = (SELECT id FROM identities WHERE name = 'bob')`, orchidID); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, end := range ends {
		for _, via := range []string{"github", "invite"} {
			t.Run(name+", rejoins by "+via, func(t *testing.T) {
				s, d, orchidID := trustedBob(t)
				ctx := core.WithSubject(context.Background(), d.Projects.Local(""))
				end(t, ctx, d, orchidID)
				joinBob(t, ctx, d, orchidID, via, "")
				if c := bobClass(t, ctx, s, orchidID); c.Class != api.MachinePublic {
					t.Fatalf("rejoined after the join ended: %+v", c)
				}
			})
		}
	}
	// An invite's class dies with the token: an expired token's trusted
	// invite class does not carry onto a later plain guest join.
	t.Run("an expired invite class does not carry", func(t *testing.T) {
		_, d := hooksService(t)
		s := New(d)
		ctx := core.WithSubject(context.Background(), d.Projects.Local(""))
		orchid, err := d.Projects.Get(d.Projects.Local("").Scope, "orchid")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Projects.SetTeam(ctx, orchid.ID, true); err != nil {
			t.Fatal(err)
		}
		joinBob(t, ctx, d, orchid.ID, "invite", api.MachineTrusted)
		if c := bobClass(t, ctx, s, orchid.ID); c.Class != api.MachineTrusted {
			t.Fatalf("a trusted invite: %+v", c)
		}
		if _, err := d.Store.Registry().ExecContext(ctx, `UPDATE tokens SET expires = 1 WHERE node = 'nBOB'`); err != nil {
			t.Fatal(err)
		}
		joinBob(t, ctx, d, orchid.ID, "github", "")
		if c := bobClass(t, ctx, s, orchid.ID); c.Class != api.MachinePublic {
			t.Fatalf("a plain join after the token expired: %+v", c)
		}
	})
	t.Run("a join that replaces a live one keeps it", func(t *testing.T) {
		s, d, orchidID := trustedBob(t)
		ctx := core.WithSubject(context.Background(), d.Projects.Local(""))
		joinBob(t, ctx, d, orchidID, "invite", "")
		if c := bobClass(t, ctx, s, orchidID); c.Class != api.MachineTrusted {
			t.Fatalf("joined again while joined: %+v", c)
		}
	})
}

// A member's machine is trusted by default. When the owner sets it public,
// leaving and joining again by GitHub must not hand the default back.
func TestPublicClassSurvivesRejoin(t *testing.T) {
	_, d := hooksService(t)
	svc := New(d)
	ctx := core.WithSubject(context.Background(), d.Projects.Local(""))
	orchid, err := d.Projects.Get(d.Projects.Local("").Scope, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Projects.SetTeam(ctx, orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	in := project.AdmitInput{Node: "nBOB", Machine: "bob-mac", Identity: "bob", Project: orchid.ID, Role: "member", By: "test"}
	if _, err := d.Projects.Admit(ctx, in); err != nil {
		t.Fatal(err)
	}
	if c := bobClass(t, ctx, svc, orchid.ID); c.Class != api.MachineTrusted {
		t.Fatalf("a member's machine starts trusted: %+v", c)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/team/machines/bob-mac/class", strings.NewReader(`{"project":"orchid","class":"public"}`)).WithContext(ctx)
	req.SetPathValue("machine", "bob-mac")
	if _, err := svc.handleSetClass(req); err != nil {
		t.Fatal(err)
	}
	if c := bobClass(t, ctx, svc, orchid.ID); c.Class != api.MachinePublic {
		t.Fatalf("the owner made it public: %+v", c)
	}
	if err := d.Projects.RevokeOwn(ctx, &authz.Subject{Node: "nBOB", TokenID: liveToken(t, ctx, d)}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Projects.Admit(ctx, in); err != nil {
		t.Fatal(err)
	}
	if c := bobClass(t, ctx, svc, orchid.ID); c.Class != api.MachinePublic {
		t.Fatalf("rejoining loosened what the owner chose: %+v", c)
	}
}

// trustedBob is a team project orchid with bob's machine joined as a guest
// and set trusted by the owner, on a Service wired by New.
func trustedBob(t *testing.T) (*Service, *core.Deps, string) {
	t.Helper()
	_, d := hooksService(t)
	s := New(d) // New wires OnFreshJoin; hooksService builds a Service by hand
	ctx := core.WithSubject(context.Background(), d.Projects.Local(""))
	orchid, err := d.Projects.Get(d.Projects.Local("").Scope, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Projects.SetTeam(ctx, orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	joinBob(t, ctx, d, orchid.ID, "github", "")
	if c := bobClass(t, ctx, s, orchid.ID); c.Class != api.MachinePublic {
		t.Fatalf("a guest starts public: %+v", c)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/team/machines/bob-mac/class", strings.NewReader(`{"project":"orchid","class":"trusted"}`)).WithContext(ctx)
	req.SetPathValue("machine", "bob-mac")
	if _, err := s.handleSetClass(req); err != nil {
		t.Fatal(err)
	}
	if c := bobClass(t, ctx, s, orchid.ID); c.Class != api.MachineTrusted {
		t.Fatalf("the owner trusted it: %+v", c)
	}
	return s, d, orchid.ID
}

// joinBob joins bob's machine to orchid as a guest of area sprout: via
// "github" is Registry.Admit's path, via "invite" a redeemed invite (carrying
// class when not empty).
func joinBob(t *testing.T, ctx context.Context, d *core.Deps, orchidID, via, class string) {
	t.Helper()
	if via == "github" {
		if _, err := d.Projects.Admit(ctx, project.AdmitInput{Node: "nBOB", Machine: "bob-mac", Identity: "bob", Project: orchidID, Area: "sprout", Role: "guest", By: "test"}); err != nil {
			t.Fatal(err)
		}
		return
	}
	inv, err := d.Projects.CreateInvite(ctx, d.Projects.Local(""), project.InviteInput{
		InviteRequest: api.InviteRequest{Project: "orchid", Area: "sprout", As: "bob", Role: "guest", Class: class},
		Authority:     "mini", Fingerprint: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	jc, err := project.ParseJoinCode(inv.Code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Projects.Redeem(ctx, project.RedeemInput{Secret: jc.Secret, Node: "nBOB", Machine: "bob-mac"}); err != nil {
		t.Fatal(err)
	}
}

func bobClass(t *testing.T, ctx context.Context, s *Service, orchidID string) api.MachineClass {
	t.Helper()
	c, err := s.MachineClass(ctx, orchidID, "bob-mac")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func liveToken(t *testing.T, ctx context.Context, d *core.Deps) string {
	t.Helper()
	var id string
	if err := d.Store.Registry().QueryRowContext(ctx, `SELECT id FROM tokens WHERE node = 'nBOB' AND revoked IS NULL`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
