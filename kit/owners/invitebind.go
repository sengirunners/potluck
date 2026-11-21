package owners

// Optional GitHub sign-in when joining with an invite (SPEC 17.11 and 22;
// zm-5c36.5). A person who joins a team project with an invite, over the
// tunnel or the tailnet, may add --github: once the join is done their
// daemon starts a login with purpose "invite", authenticated by the
// node-bound (over the tunnel, key-bound) token it holds. The authority runs
// GitHub's device flow as for every login; when the same node collects the
// outcome it links the token's identity to the approving account and, when
// the account holds a GitHub grant in the token's project that gives more
// than the token while keeping what it gave (widens), admits the node at it
// exactly as a GitHub join from that node would, recorded as an admission so
// revoking the grant takes back what it gave. The starting token stays live
// for the member to give back once it holds the new one. Otherwise it only
// links, and the invite's access stands. An account already linked to
// another identity is refused (linkInviteTx). No code of our own runs the
// sign-in: GitHub's device flow, through the same start and collect as every
// login.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// startInvite starts an invite sign-in for sub: only a replica's node-bound
// token (never the socket, a minted token or a tailnet login), only a
// person's identity, only in the team project the token is capped to, and
// only from the node the token is bound to.
func (s *Service) startInvite(ctx context.Context, sub *authz.Subject) (api.GitHubLoginStart, error) {
	if sub == nil || sub.Via != authz.ViaToken || sub.Node == "" || sub.Cap == nil || sub.Cap.Project == "" {
		return api.GitHubLoginStart{}, api.Errorf(api.CodeForbidden, "GitHub sign-in after an invite comes from the machine that just joined, with its own token").
			WithHint(config.Command("join", "'<invite link>'", "--github") + " on that machine")
	}
	rm := core.RemoteOf(ctx)
	if rm == nil || rm.NodeID != sub.Node || core.IsLocal(ctx) {
		return api.GitHubLoginStart{}, api.Errorf(api.CodeForbidden, "this token is bound to another node")
	}
	if sub.Identity.Kind != api.IdentityHuman {
		return api.GitHubLoginStart{}, api.Errorf(api.CodeForbidden, "only a person signs in with GitHub; identity %s is a %s", sub.Identity.Name, sub.Identity.Kind).
			WithHint("machines and agents keep " + config.Program + "'s invites and tokens")
	}
	pid := sub.Cap.Project
	if !s.d.Projects.IsTeam(pid) {
		return api.GitHubLoginStart{}, api.NotFound("project", pid)
	}
	slug := s.d.Projects.Slug(pid)
	via := "tailnet node " + sub.Node
	if rm.Via == core.ViaTunnel {
		via = "key " + sub.Node + " over https"
	}
	from := fmt.Sprintf("machine %s (%s), identity %s, after an invite to %s", sub.Machine, via, sub.Identity.Name, slug)
	return s.begin(ctx, &login{purpose: PurposeInvite, identity: sub.Identity, node: sub.Node, nodeName: sub.Machine,
		machine: sub.Machine, project: pid, from: from, keep: sub.TokenID, capArea: sub.Cap.Area, capRole: string(sub.Cap.Role)})
}

// bindInvite runs when the node that started an invite sign-in collects
// it: the link, and the admission when acc holds a GitHub grant in the
// project.
func (s *Service) bindInvite(ctx context.Context, l *login, acc Account) (api.GitHubLoginOutcome, error) {
	if acc.Kind != KindUser {
		return api.GitHubLoginOutcome{}, api.Errorf(api.CodeForbidden, "@%s is a GitHub %s; only a person's account signs in", acc.Login, acc.Kind)
	}
	p, ok := s.d.Projects.Lookup(l.project)
	if !ok || !s.d.Projects.IsTeam(p.ID) {
		return api.GitHubLoginOutcome{}, api.NotFound("project", l.project)
	}
	// The identity as it is now (renamed or removed since the start).
	who, err := s.d.Projects.Identity(ctx, l.identity.ID)
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	role, from, err := s.grantFor(ctx, p.ID, acc, l.orgs, false, "")
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	held, err := s.holdsGrant(ctx, who.ID, p.ID, from.area)
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	// held: the identity already holds a grant where this one would go (the
	// invite's own area, or the whole project): admitting would raise that
	// grant in place, and revoking the GitHub grant could not take the
	// raise back. Linked only; the owner raises it by identity instead.
	if held || !widens(role, from.area, l.capRole, l.capArea) {
		// No grant (yet), or none that gives more than the invite's token
		// (a weaker role, or another area, which would cost the invite's
		// area): the link alone; the invite's access stands.
		var link api.GitHubIdentity
		var first bool
		if err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
			var err error
			link, first, err = s.linkInviteTx(ctx, tx, who, acc)
			return err
		}); err != nil {
			return api.GitHubLoginOutcome{}, err
		}
		if first {
			s.tellLinked(ctx, who, acc, l.from)
		}
		return api.GitHubLoginOutcome{Link: &link}, nil
	}
	src := source(acc.ID, from)
	var link api.GitHubIdentity
	var first bool
	out, err := s.d.Projects.Admit(ctx, project.AdmitInput{Node: l.node, Machine: l.machine, Identity: who.Name, Project: p.ID,
		Area: from.area, Role: role, By: src, Account: acc.ID, Keep: l.keep,
		Note: fmt.Sprintf("signed in from %s with GitHub @%s after an invite", l.machine, acc.Login),
		Within: func(ctx context.Context, tx *sql.Tx, id string) error {
			if id != who.ID {
				return api.Errorf(api.CodeConflict, "identity %s changed while the sign-in waited", who.Name)
			}
			link, first, err = s.linkInviteTx(ctx, tx, who, acc)
			return err
		}})
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO github_admissions (id, grant_account, grant_kind, grant_area, project,
		area, account, identity, node, token, source, created, checked) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ids.New(), from.account, from.kind, from.area, p.ID, out.Cap.Area, acc.ID, who.ID, l.node, out.TokenID, src,
		store.Millis(time.Now()), store.Millis(time.Now())); err != nil {
		s.d.Log.Warn("owners: recording an admission", "err", err)
	}
	if first {
		s.tellLinked(ctx, who, acc, l.from)
	}
	s.tell(ctx, who, []string{p.ID}, fmt.Sprintf("GitHub @%s (%s) signed in as identity %s in %s from %s; its GitHub grant gives %s%s. If that was not them: "+
		"%s (the machine owner or a project admin), and %s.",
		acc.Login, acc.ID, who.Name, p.Slug, l.from, out.Cap.Role, areaSuffix(out.Cap.Area),
		config.Command("identity unlink", "github", who.Name, "--revoke"), config.Command("revoke", "@"+acc.Login, p.Slug)))
	if _, err := s.d.Events.Publish(ctx, events.Input{Type: api.EventTeamJoined, Subject: out.Machine, Data: api.TeamJoined{
		Project: api.ProjectRef{ID: out.Cap.Project, Slug: out.Cap.Slug}, Area: out.Cap.Area, Role: out.Cap.Role,
		Identity: out.Identity, Machine: out.Machine, Node: l.node}}); err != nil {
		s.d.Log.Warn("owners: publish joined", "err", err)
	}
	return api.GitHubLoginOutcome{Link: &link, Join: &out.RedeemResponse}, nil
}

// holdsGrant says whether identity holds a grant on project's area (""
// for the whole project) already.
func (s *Service) holdsGrant(ctx context.Context, identity, project, area string) (bool, error) {
	var n int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM grants WHERE subject = ? AND object = ?`,
		identity, authz.AreaObject(project, area)).Scan(&n)
	return n > 0, err
}

// widens says whether a grant of role on area gives more than a token
// capped to capRole on capArea while keeping all it gave: it covers the
// token's area (the whole project covers any) and is a stronger role, or
// the same role over the whole project instead of one area.
func widens(role, area, capRole, capArea string) bool {
	r, err := authz.ParseRelation(role)
	if err != nil || r == authz.None || (area != "" && area != capArea) {
		return false
	}
	c, err := authz.ParseRelation(capRole)
	if err != nil {
		return true // a token with no known role counts for nothing
	}
	return authz.Max(r, c) != c || (r == c && area == "" && capArea != "")
}

// linkInviteTx is linkTx for an invite sign-in, which also refuses an
// account already linked to another identity here. A link login lets one
// account own several identities; this one may not: whoever holds an
// invite (a guest's, a stranger machine's) could otherwise get an existing
// member to approve a device code and take that member's GitHub grants and
// account for an identity of their own. The person joins as the identity
// their account already is (an invite --as it), or unlinks it first.
func (s *Service) linkInviteTx(ctx context.Context, tx *sql.Tx, who api.Identity, acc Account) (api.GitHubIdentity, bool, error) {
	var other string
	err := tx.QueryRowContext(ctx, `SELECT i.name FROM owner_links l JOIN identities i ON i.id = l.identity
		WHERE l.account = ? AND l.identity <> ? ORDER BY l.linked, i.name LIMIT 1`, acc.ID, who.ID).Scan(&other)
	switch {
	case err == nil:
		return api.GitHubIdentity{}, false, api.Errorf(api.CodeConflict, "GitHub @%s is already identity %s on this machine, not %s", acc.Login, other, who.Name).
			WithHint("ask the project's owner for an invite as " + other + ": " + config.Command("invite", "<project>", "--as", other))
	case !errors.Is(err, sql.ErrNoRows):
		return api.GitHubIdentity{}, false, err
	}
	return s.linkTx(ctx, tx, who, acc)
}
