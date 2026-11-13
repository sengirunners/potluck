package owners

// Join by GitHub (SPEC 22; owners-2026-11-01 Q11): a person's machine,
// on the tailnet, asks this authority to join a team project by name. The
// authority runs the GitHub login itself for any project name, known or
// not. When the joining node collects the outcome, it takes the strongest
// GitHub grant for the approving account (its own, or an org's it is an
// active member of, asked live when GitHub answered), finds or creates the
// identity linked to the account, and admits the node exactly as a
// redeemed invite does. An unknown project, one with no GitHub grants and
// no grant for this account are one byte-identical refusal, given only
// after the flow: nobody learns which projects exist. No code.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

func (s *Service) joinRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/join/github", core.Public, "none: \"a zm authority\", its protocol and machine; no project input, no project list", jsonH(s.handleDiscover))
	r.HandleFunc("POST /v0/join/github", core.Public, "a tailnet node WhoIs identified (not the socket, not this machine, not a tagged node), or a tunnel caller's key (not the home's own); any project name starts a login; optional area and role scope it (the tunnel requires an area and is guest only); only that node or key collects; at most 4 logins in flight per node", jsonH(s.handleJoin))
}

func (s *Service) handleDiscover(r *http.Request) (any, error) {
	return api.GitHubAuthority{Authority: true, Protocol: api.WireProtocol, Machine: s.d.Config.Machine, GitHub: s.dev != nil}, nil
}

func (s *Service) handleJoin(r *http.Request) (any, error) {
	ctx := r.Context()
	rm := core.RemoteOf(ctx)
	tunnel := rm != nil && rm.Via == core.ViaTunnel
	switch {
	case core.IsLocal(ctx):
		return nil, api.Errorf(api.CodeForbidden, "a GitHub join comes from the member's own machine").WithHint("run zm join <project> on the member's machine")
	case rm == nil || rm.NodeID == "":
		return nil, api.Errorf(api.CodeUnauthenticated, "unknown tailnet node").WithHint("join through the tailnet: zm join <project>")
	case rm.Self:
		return nil, api.Errorf(api.CodeForbidden, "a GitHub join does not come from the authority's own machine")
	case rm.Tagged:
		return nil, api.Errorf(api.CodeForbidden, "a tagged node is a server, not a person's machine").
			WithHint("machines and agents join with an invite: zm invite <project> --as <name>")
	}
	var req api.GitHubJoinRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	machine := strings.TrimSpace(req.Machine)
	if !api.ValidName(machine) || machine != strings.ToLower(machine) {
		return nil, api.Errorf(api.CodeBadRequest, "bad machine name %q", req.Machine)
	}
	if machine == s.d.Config.Machine {
		return nil, api.Errorf(api.CodeConflict, "machine name %s is taken at this authority by another node", machine).
			WithHint("join with --machine <another name>")
	}
	ref := strings.TrimSpace(req.Project)
	if ref == "" || len(ref) > 200 {
		return nil, api.Errorf(api.CodeBadRequest, "name the project to join")
	}
	area, role := strings.TrimSpace(req.Area), strings.ToLower(strings.TrimSpace(req.Role))
	if area != "" && !api.ValidSlug(area) {
		return nil, api.Errorf(api.CodeBadRequest, "bad area name %q", req.Area)
	}
	if role != "" {
		if _, err := authz.ParseRelation(role); err != nil {
			return nil, err
		}
	}
	if tunnel {
		// Anyone can make a key: over https a GitHub join is one area, as
		// a guest unless more is asked (and granted), never a whole
		// project and never the strongest grant by default.
		if area == "" {
			return nil, api.Errorf(api.CodeBadRequest, "a GitHub join over https names one area").WithHint("zm join <project>/<area> --from https://<host>#<fingerprint>")
		}
		if role == "" {
			role = api.RoleGuest
		}
		// A stranger can ask for any role and phish a grant holder into
		// approving the code: over the internet a join is a guest's.
		if role != api.RoleGuest {
			return nil, api.Errorf(api.CodeBadRequest, "a GitHub join over https is a guest's, not %s", role).
				WithHint("a member or owner joins over the tailnet, or with an invite: zm invite <project> --as <name>")
		}
	}
	name := rm.Node
	if name == "" {
		name = "?"
	}
	from := fmt.Sprintf("node %s (%s, tailnet login %s) as machine %s", name, rm.NodeID, rm.Login, machine)
	if tunnel {
		from, name = fmt.Sprintf("key %s over https as machine %s", rm.NodeID, machine), "https"
	}
	if area != "" {
		from += fmt.Sprintf(", for area %s as %s", area, orGrant(role))
	} else if role != "" {
		from += ", as " + role
	}
	return s.begin(ctx, &login{purpose: PurposeJoin, node: rm.NodeID, nodeName: name, project: ref, machine: machine, area: area, role: role, from: from,
		tunnel: tunnel, addr: addrOf(tunnel, rm)})
}

func addrOf(tunnel bool, rm *core.Remote) string {
	if !tunnel {
		return ""
	}
	return core.AddrBucket(rm.Addr)
}

func orGrant(role string) string {
	if role == "" {
		return "its grant allows"
	}
	return role
}

// noGrant is the one refusal for an unknown project, a project with no
// GitHub grants, a project that is not a team project and no grant for
// this account: the same bytes but for what the joiner typed.
func noGrant(ref string) error {
	return api.Errorf(api.CodeForbidden, "no grant for you in %q on this authority", ref).
		WithHint("the project's owner runs: zm grant @<your GitHub login> member <project>")
}

// admit runs when the joining node collects the outcome: the grant, the
// identity (found by its link, or created and linked in the admission's
// own transaction: never an orphan, never a name match), the admission,
// its record (so revoking the grant revokes it), the mail.
func (s *Service) admit(ctx context.Context, l *login, acc Account) (api.RedeemResponse, error) {
	if acc.Kind != KindUser {
		return api.RedeemResponse{}, api.Errorf(api.CodeForbidden, "@%s is a GitHub %s; only a person's account joins", acc.Login, acc.Kind)
	}
	p, ok := s.d.Projects.Lookup(l.project)
	if !ok || !s.d.Projects.IsTeam(p.ID) {
		return api.RedeemResponse{}, noGrant(l.project)
	}
	scoped := l.area != "" || l.role != ""
	if l.area != "" && !s.d.Projects.HasArea(p.ID, l.area) {
		return api.RedeemResponse{}, noGrant(l.project)
	}
	role, from, err := s.grantFor(ctx, p.ID, acc, l.orgs, scoped, l.area)
	if err != nil || role == "" || role == "none" {
		return api.RedeemResponse{}, noGrant(l.project)
	}
	area := from.area
	if scoped {
		// Exactly the area asked for, at the role asked for when the grants
		// reach it: asking for more than they give is the one refusal.
		if l.role != "" {
			if authz.Max(authz.Relation(role), authz.Relation(l.role)) != authz.Relation(role) {
				return api.RedeemResponse{}, noGrant(l.project)
			}
			role = l.role
		}
		area = l.area
	}
	src := source(acc.ID, from)
	var who api.Identity
	var out project.Admitted
	for i := 1; i < 100; i++ {
		var linked string
		in := project.AdmitInput{Node: l.node, Machine: l.machine, Project: p.ID, Area: area, Role: role, By: src, Account: acc.ID,
			Note: fmt.Sprintf("joined from %s by GitHub @%s", l.machine, acc.Login)}
		// An account that owns several identities joins as the one it
		// linked first: a join picks one, the same one every time.
		if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity FROM owner_links WHERE account = ?
			ORDER BY linked, identity LIMIT 1`, acc.ID).Scan(&linked); err == nil {
			if who, err = s.d.Projects.Identity(ctx, linked); err != nil {
				return api.RedeemResponse{}, err
			}
			in.Identity = who.Name
		} else {
			// A new identity named after the login (-2, -3 when taken),
			// created and linked in one transaction.
			in.Identity, in.MustCreate = identityName(acc, i), true
			in.Within = func(ctx context.Context, tx *sql.Tx, id string) error {
				if s.withinFault != nil {
					if err := s.withinFault(); err != nil {
						return err // tests: a failure between the identity and its link
					}
				}
				who = api.Identity{ID: id, Name: in.Identity, Kind: api.IdentityHuman}
				_, _, err := s.linkTx(ctx, tx, who, acc) // the join's mail says it
				return err
			}
		}
		out, err = s.d.Projects.Admit(ctx, in)
		if project.IsIdentityTaken(err) {
			continue
		}
		break
	}
	if err != nil {
		return api.RedeemResponse{}, err
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO github_admissions (id, grant_account, grant_kind, grant_area, project,
		area, account, identity, node, token, source, created, checked) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ids.New(), from.account, from.kind, from.area, p.ID, out.Cap.Area, acc.ID, who.ID, l.node, out.TokenID, src,
		store.Millis(time.Now()), store.Millis(time.Now())); err != nil {
		s.d.Log.Warn("owners: recording an admission", "err", err)
	}
	if out.NewNode {
		s.tell(ctx, who, []string{p.ID}, fmt.Sprintf("GitHub @%s (%s) joined %s as identity %s (%s%s) from %s. If that was not them: "+
			"zm identity unlink github %s --revoke (the machine owner or a project admin), and zm revoke @%s %s.",
			acc.Login, acc.ID, p.Slug, who.Name, out.Cap.Role, areaSuffix(out.Cap.Area), l.from, who.Name, acc.Login, p.Slug))
	}
	if _, err := s.d.Events.Publish(ctx, events.Input{Type: api.EventTeamJoined, Subject: out.Machine, Data: api.TeamJoined{
		Project: api.ProjectRef{ID: out.Cap.Project, Slug: out.Cap.Slug}, Area: out.Cap.Area, Role: out.Cap.Role,
		Identity: out.Identity, Machine: out.Machine, Node: l.node}}); err != nil {
		s.d.Log.Warn("owners: publish joined", "err", err)
	}
	return out.RedeemResponse, nil
}

// identityName is the i-th name tried for a new identity of acc: the
// login, then login-2, login-3, ...
func identityName(acc Account, i int) string {
	base := strings.ToLower(acc.Login)
	if !api.ValidName(base) {
		base = "gh-" + strings.TrimPrefix(acc.ID, "github:")
	}
	if i > 1 {
		return fmt.Sprintf("%s-%d", base, i)
	}
	return base
}
