// Package authz is zm's permission model: grant tuples, scopes, Check and
// bearer-token secrets. It is pure Go with no storage; kit/project keeps
// the tuples in registry.db and builds Subjects from them. Owned by the
// integrator. See SPEC.md "Who is calling" and "Projects and areas".
//
// A tuple is (subject identity ULID, relation, object) with relation
// owner|member|guest and object "project:<ulid>" or "area:<ulid>/<name>".
// Default deny: a Scope built from no tuples can do nothing.
package authz

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// Relation is a role in a tuple. Weakest first; "" is no relation.
type Relation string

const (
	None   Relation = ""
	Guest  Relation = api.RoleGuest
	Member Relation = api.RoleMember
	Owner  Relation = api.RoleOwner
)

func (r Relation) rank() int {
	switch r {
	case Guest:
		return 1
	case Member:
		return 2
	case Owner:
		return 3
	}
	return 0
}

// ParseRelation accepts guest, member, owner.
func ParseRelation(s string) (Relation, error) {
	r := Relation(strings.ToLower(strings.TrimSpace(s)))
	if r.rank() == 0 {
		return None, api.Errorf(api.CodeBadRequest, "role must be owner, member or guest, not %q", s)
	}
	return r, nil
}

// Min is the weaker of a and b.
func Min(a, b Relation) Relation {
	if a.rank() < b.rank() {
		return a
	}
	return b
}

// Max is the stronger of a and b.
func Max(a, b Relation) Relation {
	if a.rank() > b.rank() {
		return a
	}
	return b
}

// Action is what a caller wants to do with an object.
type Action string

const (
	Read    Action = "read"    // see it (lists, gets, events, autocomplete)
	Write   Action = "write"   // send mail, create/comment/claim work
	Operate Action = "operate" // spawn, stop, attach, type into sessions; force-release leases
	Admin   Action = "admin"   // grants, tokens, areas, project settings
)

// Allows reports whether relation r permits action a.
//
//	owner: read write operate admin
//	member: read write operate
//	guest: read write
func (r Relation) Allows(a Action) bool {
	switch a {
	case Read, Write:
		return r.rank() >= 1
	case Operate:
		return r.rank() >= 2
	case Admin:
		return r.rank() >= 3
	}
	return false
}

// Object is the thing being checked: a project, or an area inside it.
// Project "" or "none" is machine-level (only the machine's owner).
type Object struct {
	Project string
	Area    string
}

func machineLevel(p string) bool { return p == "" || p == api.ProjectNone }

// Tuple is one stored grant.
type Tuple struct {
	Subject  string   // identity ULID
	Relation Relation // owner | member | guest
	Object   string   // "project:<ulid>" or "area:<ulid>/<name>"
}

// ProjectObject is "project:<ulid>".
func ProjectObject(project string) string { return "project:" + project }

// AreaObject is "area:<ulid>/<name>"; an empty area is ProjectObject.
func AreaObject(project, area string) string {
	if area == "" {
		return ProjectObject(project)
	}
	return "area:" + project + "/" + area
}

// ParseObject splits a tuple object into project and area ("" for a project object).
func ParseObject(s string) (project, area string, err error) {
	switch {
	case strings.HasPrefix(s, "project:"):
		project = strings.TrimPrefix(s, "project:")
	case strings.HasPrefix(s, "area:"):
		var ok bool
		project, area, ok = strings.Cut(strings.TrimPrefix(s, "area:"), "/")
		if !ok || area == "" {
			return "", "", fmt.Errorf("bad area object %q", s)
		}
	default:
		return "", "", fmt.Errorf("bad object %q", s)
	}
	if project == "" {
		return "", "", fmt.Errorf("bad object %q", s)
	}
	return project, area, nil
}

// Cap restricts what a token may do: at most Role, only inside Project
// (and Area). Zero fields do not restrict.
type Cap struct {
	Project string
	Area    string
	Role    Relation
}

// Scope is everything a subject may see and do. Build it with All, FromTuples
// or Empty; the zero value denies everything.
type Scope struct {
	all     bool
	machine bool
	proj    map[string]Relation
	area    map[string]map[string]Relation
}

// SameFor reports whether two scopes grant the same on project ("" = on every project).
func (s Scope) SameFor(o Scope, project string) bool {
	if project != "" {
		s, o = s.Only(project), o.Only(project)
	}
	return s.all == o.all && s.machine == o.machine && maps.Equal(s.proj, o.proj) &&
		maps.EqualFunc(s.area, o.area, func(a, b map[string]Relation) bool { return maps.Equal(a, b) })
}

// All is the daemon itself and the local OS user: everything, including
// machine-level objects. Tests use it for "no restriction".
func All() Scope { return Scope{all: true, machine: true} }

// Empty denies everything.
func Empty() Scope { return Scope{} }

// FromTuples builds a scope from an identity's tuples, narrowed by cap (nil = no cap).
// Malformed tuples are skipped.
func FromTuples(tuples []Tuple, cap *Cap) Scope {
	s := Scope{proj: map[string]Relation{}, area: map[string]map[string]Relation{}}
	for _, t := range tuples {
		p, a, err := ParseObject(t.Object)
		if err != nil || t.Relation.rank() == 0 {
			continue
		}
		rel := t.Relation
		if cap != nil {
			if cap.Project != "" && p != cap.Project {
				continue
			}
			if cap.Area != "" {
				if a == "" {
					a = cap.Area // a project-wide grant narrowed to the token's area
				} else if a != cap.Area {
					continue
				}
			}
			if cap.Role != None {
				rel = Min(rel, cap.Role)
			}
		}
		if a == "" {
			s.proj[p] = Max(s.proj[p], rel)
			continue
		}
		if s.area[p] == nil {
			s.area[p] = map[string]Relation{}
		}
		s.area[p][a] = Max(s.area[p][a], rel)
	}
	return s
}

// IsAll reports whether the scope is unrestricted.
func (s Scope) IsAll() bool { return s.all }

// Machine reports whether the scope sees machine-level objects (project "none").
func (s Scope) Machine() bool { return s.machine }

// WithMachine adds access to the local machine log while retaining project caps.
func (s Scope) WithMachine() Scope { s.machine = true; return s }

// Relation is the subject's effective relation to o.
func (s Scope) Relation(o Object) Relation {
	if s.all {
		return Owner
	}
	if machineLevel(o.Project) {
		if s.machine {
			return Owner
		}
		return None
	}
	r := s.proj[o.Project]
	if o.Area != "" {
		r = Max(r, s.area[o.Project][o.Area])
	}
	return r
}

// Can reports whether the scope permits a on o. Default deny.
func (s Scope) Can(a Action, o Object) bool { return s.Relation(o).Allows(a) }

// Role is the strongest relation the scope has anywhere in project (a
// project-wide grant, or any area grant). "" = the project is invisible.
func (s Scope) Role(project string) Relation {
	if s.all {
		return Owner
	}
	if machineLevel(project) {
		if s.machine {
			return Owner
		}
		return None
	}
	r := s.proj[project]
	for _, ar := range s.area[project] {
		r = Max(r, ar)
	}
	return r
}

// SeesProject reports whether the project exists for this subject at all.
func (s Scope) SeesProject(project string) bool { return s.Role(project) != None }

// WholeProject reports whether the subject has a project-wide relation
// (sees every area, and objects with no area).
func (s Scope) WholeProject(project string) bool {
	return s.all || (!machineLevel(project) && s.proj[project] != None) || (machineLevel(project) && s.machine)
}

// Areas lists the areas the subject sees in project, sorted. whole=true
// means every area (and the area-less objects) is visible.
func (s Scope) Areas(project string) (whole bool, areas []string) {
	if s.WholeProject(project) {
		return true, nil
	}
	for a := range s.area[project] {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	return false, areas
}

// Projects lists the project ULIDs the subject sees (not "none"), sorted.
// all=true means every project.
func (s Scope) Projects() (all bool, ids []string) {
	if s.all {
		return true, nil
	}
	seen := map[string]bool{}
	for p, r := range s.proj {
		if r != None {
			seen[p] = true
		}
	}
	for p, m := range s.area {
		if len(m) > 0 {
			seen[p] = true
		}
	}
	for p := range seen {
		ids = append(ids, p)
	}
	sort.Strings(ids)
	return false, ids
}

// Only narrows the scope to one project (or "none"): what a list call with
// an explicit --project may return. It never widens.
func (s Scope) Only(project string) Scope {
	out := Scope{proj: map[string]Relation{}, area: map[string]map[string]Relation{}}
	if machineLevel(project) {
		out.machine = s.machine
		return out
	}
	if s.all {
		out.proj[project] = Owner
		return out
	}
	if r := s.proj[project]; r != None {
		out.proj[project] = r
	}
	if m := s.area[project]; len(m) > 0 {
		out.area[project] = map[string]Relation{}
		for a, r := range m {
			out.area[project][a] = r
		}
	}
	return out
}

// Capped is the scope in one project only, each relation at most r: a
// session started for another machine's launcher (SPEC 1.3). It never widens.
func (s Scope) Capped(project string, r Relation) Scope {
	out := s.Only(project)
	if machineLevel(project) {
		return out
	}
	for p, x := range out.proj {
		out.proj[p] = Min(x, r)
	}
	for _, m := range out.area {
		for a, x := range m {
			m[a] = Min(x, r)
		}
	}
	return out
}

// SQL returns a WHERE fragment (and its args) selecting the rows of a table
// with project and area columns that the scope may act on with a. Use it on
// every registry-table query that returns rows to a caller:
//
//	where, args := sub.Scope.SQL("project", "area", authz.Read)
//	rows, err := db.QueryContext(ctx, "SELECT ... FROM sessions WHERE "+where+" AND state != 'ended'", args...)
func (s Scope) SQL(projectCol, areaCol string, a Action) (string, []any) {
	if s.all {
		return "1=1", nil
	}
	var parts []string
	var args []any
	if s.machine && Owner.Allows(a) {
		parts = append(parts, projectCol+" = ?")
		args = append(args, api.ProjectNone)
	}
	ps := make([]string, 0, len(s.proj))
	for p := range s.proj {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	for _, p := range ps {
		if s.proj[p].Allows(a) {
			parts = append(parts, projectCol+" = ?")
			args = append(args, p)
		}
	}
	aps := make([]string, 0, len(s.area))
	for p := range s.area {
		aps = append(aps, p)
	}
	sort.Strings(aps)
	for _, p := range aps {
		if s.proj[p].Allows(a) {
			continue // already whole-project
		}
		var names []string
		for name, r := range s.area[p] {
			if r.Allows(a) {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
		parts = append(parts, "("+projectCol+" = ? AND "+areaCol+" IN ("+ph+"))")
		args = append(args, p)
		for _, n := range names {
			args = append(args, n)
		}
	}
	if len(parts) == 0 {
		return "0=1", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// AreaSQL is SQL for one project's own database (project.db tables have an
// area column but no project column).
func (s Scope) AreaSQL(project, areaCol string, a Action) (string, []any) {
	if s.Relation(Object{Project: project}).Allows(a) {
		return "1=1", nil
	}
	var names []string
	for name, r := range s.area[project] {
		if r.Allows(a) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "0=1", nil
	}
	sort.Strings(names)
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	return areaCol + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + ")", args
}

// Subject is an authenticated caller.
type Subject struct {
	// PersonalMail is negotiated for a node-bound replica, never inferred
	// from ownership or a machine/session prefix.
	PersonalMail bool
	Identity     api.Identity
	// Address is where replies to the caller go: a session address,
	// human:<name> or machine:<host>.
	Address string
	// Session is the caller's session address when the caller is a session.
	Session string
	// Via is how it was authenticated: local (Unix socket, the OS user),
	// token, tailnet (WhoIs) or system (the daemon itself).
	Via string
	// Remote is true for callers on a network listener (the hard boundary).
	Remote  bool
	TokenID string
	Scope   Scope
	// Node is set for a node-bound token (a team replica's, SPEC 17.4): the
	// stable node ID it is bound to, which the request came from. Machine
	// is the replica's machine name recorded at redemption: it may answer
	// as any session on that machine (Zm-As). ScopeVersion is the
	// identity's scope_version when the subject was built.
	Node         string
	Machine      string
	ScopeVersion int64
	// Cap is the token's cap (nil for the socket and tailnet logins).
	Cap *Cap
}

// Via values.
const (
	ViaLocal   = "local"
	ViaToken   = "token"
	ViaTailnet = "tailnet"
	ViaSystem  = "system"
)

// System is the daemon acting on its own (lease ticker, orphan sweeps).
func System(machine string) *Subject {
	return &Subject{
		Identity: api.Identity{Name: "zm", Kind: api.IdentityMachine},
		Address:  "machine:" + machine, Via: ViaSystem, Scope: All(),
	}
}

// Can reports whether the subject may do a on o.
func (s *Subject) Can(a Action, o Object) bool { return s != nil && s.Scope.Can(a, o) }

// Check returns nil when allowed. When the subject cannot even read o it
// returns api.NotFound(kind, id), identical to a missing object; when it can
// read but not do a, a forbidden error (no leak: it already sees o).
func (s *Subject) Check(a Action, o Object, kind, id string) error {
	if s.Can(a, o) {
		return nil
	}
	if !s.Can(Read, o) {
		return api.NotFound(kind, id)
	}
	return api.Errorf(api.CodeForbidden, "your role cannot %s this %s", a, kind)
}

// ForSession is the subject a session acts as: its launcher's scope, never
// more, with the session as the reply address.
func (s *Subject) ForSession(address string) *Subject {
	c := *s
	c.Address, c.Session = address, address
	return &c
}

// PersonalMailReader refuses private data to replicas that would drop its
// audience fields. Other callers read at the enforcing authority itself.
func PersonalMailReader(sub *Subject) bool { return sub == nil || sub.Node == "" || sub.PersonalMail }
