package teamlink

// Operator class (SPEC 8 "Operator class", zm-eakh.12.3): each machine of a
// team project is trusted (whoever runs it is trusted with the project's
// secrets, private code and the parent's authority) or public (a lender's or
// a stranger's: secret-free work only). The authority decides it, never the
// machine's own report: by default a machine is trusted when its token for
// the project is a whole-project member's or more (and the authority's own
// machine always is), and public when it joined as a guest or for one area;
// an invite may carry the class instead (the owner's choice when they made
// it, zm-eakh.12.23.13: zm pool create --host), which the redeeming machine's
// token takes in redemption's transaction, never from the redeemer's request;
// the owner may set it either way later. A class the owner set holds for the
// node it was set for, and only while that node's join lasts: another node
// that later takes the machine's name, or the same node joining again after
// its join ended (Registry.OnFreshJoin), starts from the default.
//
// It is enforced where work is placed: a remote task (an agent or a script
// run, both carrying the parent's authority and the project's code) and Z3
// on a machine go only to trusted machines. SPEC 20 makes a run target on a
// public host credential-free whatever its binding says.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var machineClassMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "team/0006_machine_class", Script: `
CREATE TABLE team_machine_class (
	project TEXT NOT NULL,
	machine TEXT NOT NULL,
	class   TEXT NOT NULL, -- trusted | public
	node    TEXT NOT NULL, -- the machine's node when it was set: a later node under the name does not inherit it
	set_by  TEXT NOT NULL, -- the identity that set it
	set_at  INTEGER NOT NULL,
	PRIMARY KEY (project, machine)
);`},
}

const (
	whyAuthority = "the authority's own machine"
	whyMember    = "joined as a member of the project (or more)"
	whyGuest     = "joined as a guest or for one area: public until the owner trusts it"
	whyInvite    = "the class its invite carried (the owner's choice)"
)

func (s *Service) classRoutes(r *core.Router) {
	r.HandleFunc("POST /v0/team/machines/{machine}/class", core.Authed, "this machine's owner on its socket (sent on to a joined project's authority), or at the authority a replica's node-bound token; its identity owns or administers the team project", jsonH(s.handleSetClass))
	r.HandleFunc("GET /v0/team/machines/class", core.Authed, "as POST /v0/team/machines/{machine}/class", jsonH(s.handleListClasses))
}

// classCaller is the caller of a class route: this machine's owner on its
// socket, or (at the authority) a replica's node-bound token.
func classCaller(r *http.Request) (*authz.Subject, bool, error) {
	sub := core.SubjectOf(r.Context())
	replica := sub != nil && sub.Node != "" && sub.Machine != ""
	if !replica && (sub == nil || sub.Via != authz.ViaLocal) {
		return nil, false, api.Errorf(api.CodeForbidden, "only the project's owner, on their machine's socket, sets operator classes").
			WithHint("run it there: zm machine-class -P <project>")
	}
	return sub, replica, nil
}

// forgetClass drops what the owner set for machine in project (its ULID)
// when the machine joins with no live join by its node (Registry.OnFreshJoin):
// a host that left, by whatever way its join ended, and joins again as a
// plain guest starts at public, not at the trust it held before. Only a
// trusted row goes: a public row is the owner tightening the machine, and
// joining again never loosens what the owner chose.
func forgetClass(ctx context.Context, tx *sql.Tx, project, machine string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM team_machine_class WHERE project=? AND machine=? AND class=?`, project, machine, api.MachineTrusted)
	return err
}

// handleSetClass is POST /v0/team/machines/{machine}/class.
func (s *Service) handleSetClass(r *http.Request) (any, error) {
	ctx := r.Context()
	sub, replica, err := classCaller(r)
	if err != nil {
		return nil, err
	}
	var req api.MachineClassRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	machine := r.PathValue("machine")
	switch {
	case req.Project == "":
		return nil, api.Errorf(api.CodeBadRequest, "name the team project %s is in", machine)
	case req.Class != "" && req.Class != api.MachineTrusted && req.Class != api.MachinePublic:
		return nil, api.Errorf(api.CodeBadRequest, "class is trusted, public, or empty to go back to the default")
	}
	if !replica {
		if x := s.ByName(req.Project); x != nil {
			snap := x.Snapshot()
			if err := Ended(snap); err != nil {
				return nil, err
			}
			fwd := req
			fwd.Project = snap.Project.ID
			var out api.MachineClass
			err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/machines/"+url.PathEscape(machine)+"/class", fwd, &out)
			return out, OlderFor(err, x, "operator classes")
		}
	}
	project, err := s.OwnerProject(ctx, sub, req.Project, "sets operator classes")
	if err != nil {
		return nil, err
	}
	if !s.projectMachine(ctx, project, machine) && !s.joinedMachine(ctx, project, machine) {
		return nil, api.NotFound("machine", machine)
	}
	db := s.d.Store.Registry()
	switch {
	case req.Class == "":
		_, err = db.ExecContext(ctx, `DELETE FROM team_machine_class WHERE project=? AND machine=?`, project, machine)
	case machine == s.d.Config.Machine:
		// The authority's own machine is the owner's: always trusted.
		if req.Class != api.MachineTrusted {
			return nil, api.Errorf(api.CodeBadRequest, "%s is the authority's own machine: it is always trusted", machine)
		}
	default:
		j, jerr := s.d.Projects.MachineJoin(ctx, project, machine)
		if jerr != nil {
			return nil, jerr
		}
		if len(j.Nodes) == 0 {
			return nil, api.NotFound("machine", machine)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO team_machine_class (project, machine, class, node, set_by, set_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (project, machine) DO UPDATE SET class=excluded.class, node=excluded.node, set_by=excluded.set_by, set_at=excluded.set_at`,
			project, machine, req.Class, j.Nodes[0], sub.Identity.Name, store.Millis(time.Now()))
	}
	if err != nil {
		return nil, err
	}
	c, err := s.MachineClass(ctx, project, machine)
	if err == nil {
		for _, f := range s.Hooks.ClassChanged {
			f(ctx, project, machine, c)
		}
	}
	return c, err
}

// handleListClasses is GET /v0/team/machines/class?project=.
func (s *Service) handleListClasses(r *http.Request) (any, error) {
	ctx := r.Context()
	sub, replica, err := classCaller(r)
	if err != nil {
		return nil, err
	}
	name := r.URL.Query().Get("project")
	if name == "" {
		return nil, api.Errorf(api.CodeBadRequest, "name the team project: ?project=")
	}
	if !replica {
		if x := s.ByName(name); x != nil {
			snap := x.Snapshot()
			if err := Ended(snap); err != nil {
				return nil, err
			}
			var out api.MachineClassList
			err := s.CallRemote(ctx, x, http.MethodGet, "/v0/team/machines/class?project="+url.QueryEscape(snap.Project.ID), nil, &out)
			return out, OlderFor(err, x, "operator classes")
		}
	}
	project, err := s.OwnerProject(ctx, sub, name, "reads operator classes")
	if err != nil {
		return nil, err
	}
	out := api.MachineClassList{Project: project, Machines: []api.MachineClass{}}
	for _, m := range s.machines(ctx, project) {
		c, err := s.MachineClass(ctx, project, m)
		if err != nil {
			return nil, err
		}
		out.Machines = append(out.Machines, c)
	}
	// An area guest's machine sends no numbers (machine.go), so it is listed
	// only once the owner set its class: a pool's host (zm pool, SPEC 20).
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT machine FROM team_machine_class WHERE project=? ORDER BY machine`, project)
	if err != nil {
		return nil, err
	}
	var set []string
	for rows.Next() {
		var m string
		if rows.Scan(&m) == nil {
			set = append(set, m)
		}
	}
	rows.Close()
	invited, err := s.d.Projects.InvitedMachines(ctx, project)
	if err != nil {
		return nil, err
	}
	set = append(set, invited...)
	for _, m := range set {
		if slices.ContainsFunc(out.Machines, func(c api.MachineClass) bool { return c.Machine == m }) || !s.joinedMachine(ctx, project, m) {
			continue
		}
		c, err := s.MachineClass(ctx, project, m)
		if err != nil {
			return nil, err
		}
		out.Machines = append(out.Machines, c)
	}
	sort.Slice(out.Machines, func(i, j int) bool { return out.Machines[i].Machine < out.Machines[j].Machine })
	return out, nil
}

// joinedMachine reports whether machine joined project (an area guest's
// too, which sends no numbers): a pool's host gets its class this way.
func (s *Service) joinedMachine(ctx context.Context, project, machine string) bool {
	j, err := s.d.Projects.MachineJoin(ctx, project, machine)
	return err == nil && len(j.Nodes) > 0
}

// machines are project's machines (Hooks.Machines); none without it (a
// joined machine is found by joinedMachine).
func (s *Service) machines(ctx context.Context, project string) []string {
	if s.Hooks.Machines == nil {
		return nil
	}
	return s.Hooks.Machines(ctx, project)
}

// projectMachine reports whether machine is one of project's (this
// machine, or one with numbers for it).
func (s *Service) projectMachine(ctx context.Context, project, machine string) bool {
	return slices.Contains(s.machines(ctx, project), machine)
}

// MachineClass implements core.MachineClasses: machine's operator class in
// project, at its authority.
func (s *Service) MachineClass(ctx context.Context, project, machine string) (api.MachineClass, error) {
	c := api.MachineClass{Machine: machine}
	if machine == s.d.Config.Machine {
		c.Class, c.Why = api.MachineTrusted, whyAuthority
		return c, nil
	}
	j, err := s.d.Projects.MachineJoin(ctx, project, machine)
	if err != nil {
		return c, err
	}
	var class, node, setBy string
	var setAt int64
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT class, node, set_by, set_at FROM team_machine_class WHERE project=? AND machine=?`,
		project, machine).Scan(&class, &node, &setBy, &setAt)
	switch {
	case err == nil && slices.Contains(j.Nodes, node):
		at := time.UnixMilli(setAt).UTC()
		c.Class, c.SetBy, c.SetAt, c.Why = class, setBy, &at, "set by "+setBy
		return c, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return c, err
	}
	// No class set, or set for another node that held the name before: the
	// invite's, else the role's.
	if j.Class != "" {
		c.Class, c.Why, c.FromInvite = j.Class, whyInvite, true
		return c, nil
	}
	if j.Member {
		c.Class, c.Why = api.MachineTrusted, whyMember
	} else {
		c.Class, c.Why = api.MachinePublic, whyGuest
	}
	return c, nil
}

// TrustedMachines splits ms into the trusted ones and the names of the
// public ones, for placing work.
func (s *Service) TrustedMachines(ctx context.Context, project string, ms []api.Machine) ([]api.Machine, map[string]bool, error) {
	var out []api.Machine
	public := map[string]bool{}
	for _, m := range ms {
		c, err := s.MachineClass(ctx, project, m.Machine)
		if err != nil {
			return nil, nil, err
		}
		if c.Class == api.MachineTrusted {
			out = append(out, m)
		} else {
			public[m.Machine] = true
		}
	}
	return out, public, nil
}

// PublicRefused is the refusal for work named onto a public machine.
func PublicRefused(machine, doing string) error {
	return api.Errorf(api.CodeForbidden, "%s is a public machine in this project: it %s only on trusted machines", machine, doing).
		WithHint("if you trust whoever runs it with the project's secrets and code: zm machine-class " + machine + " trusted -P <project>")
}

// OwnerProject is project's ULID when it is a team project sub owns or
// administers; doing is what the refusal says only they do.
func (s *Service) OwnerProject(ctx context.Context, sub *authz.Subject, project, doing string) (string, error) {
	p, err := s.d.Projects.Get(sub.Scope, project)
	if err != nil {
		return "", err
	}
	if !s.d.Store.IsTeam(p.ID) {
		return "", api.NotFound("project", project)
	}
	admin, err := s.d.ProjectAdmin(ctx, sub, p.ID)
	if err != nil {
		return "", err
	}
	if !admin {
		return "", api.Errorf(api.CodeForbidden, "only the project's owner (or an admin) %s", doing)
	}
	return p.ID, nil
}

// OlderFor names an authority older than feature.
func OlderFor(err error, x *Remote, feature string) error {
	if err != nil && OlderAuthority(err) {
		var e *api.Error
		if errors.As(err, &e) && e.Code != api.CodeNotFound {
			return api.Errorf(api.CodeUnavailable, "the authority %s is older than %s", x.Snapshot().Authority, feature).
				WithHint("its owner upgrades its zm daemon")
		}
	}
	return err
}

// OlderAuthority reports an answer that says the route is not there (404,
// 405), as from an authority older than it.
func OlderAuthority(err error) bool {
	var e *api.Error
	if errors.As(err, &e) && (e.HTTPStatus == http.StatusNotFound || e.HTTPStatus == http.StatusMethodNotAllowed || e.Code == api.CodeNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "405 Method Not Allowed") || strings.Contains(err.Error(), "404 Not Found")
}
