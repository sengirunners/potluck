package teamlink

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// GET /v0/people (SPEC 17.3 "People"): the people the caller shares
// projects with, the caller included, for a cockpit's Team tab. For a reader
// of a whole project, everyone holding a grant in it; for an area guest,
// only those holding a grant on an area it reads (a guest replica learns
// nothing outside its area: not even the whole-project members' names).
// Roles and areas are not shown, save Answers to a whole-project reader. On
// a member's socket the proxy merges every authority's answer (MergePeople).
//
// Identity (bead zm-rpjh): a person's key is its stable person id
// (api.Person.ID): the verified account its identity is linked to here
// (core.Deps.LinkedAccount, "github:<numeric id>"), else the identity's
// ULID. Names are display and per-project addressing (human:<name>) only:
// they never join two authorities' identities (SPEC 16 "Identity hygiene").
// One account may own several identities here (zm-mpb3r.10): they fold
// into one row at the authority itself, not only in a member's merge.

func (s *Service) peopleRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/people", core.Authed, "read: ReadScope(?project=); every grant holder of a project read whole, else only grants on areas the caller reads; the caller's own person is flagged you; machines and seen from the sessions the caller sees", jsonH(s.handlePeople))
}

func (s *Service) handlePeople(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		return nil, err
	}
	return s.people(ctx, sub, sc)
}

func (s *Service) people(ctx context.Context, sub *authz.Subject, sc authz.Scope) (api.PeopleList, error) {
	out := api.PeopleList{People: []api.Person{}}
	var pids []string
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		pids = append(pids, p.ID)
	}
	members, err := s.d.Projects.Members(ctx, pids)
	if err != nil {
		return out, err
	}
	you := ""
	if sub != nil && sub.Identity.ID != "" {
		you = s.person(ctx, sub.Identity).ID
	}
	rows := map[string]api.Person{} // by identity ID
	at := map[string]int{}          // by person ID
	byName := map[string]int{}      // every identity name of a row
	for _, m := range members {
		if m.Identity.Kind == api.IdentityMachine {
			continue
		}
		whole := sc.WholeProject(m.Project)
		if !whole && (m.Area == "" || !sc.Can(authz.Read, authz.Object{Project: m.Project, Area: m.Area})) {
			continue // an area guest learns no one beyond its areas (SPEC 17.3: nothing outside its area, ever)
		}
		row, ok := rows[m.Identity.ID]
		if !ok {
			row = s.person(ctx, m.Identity)
			rows[m.Identity.ID] = row
		}
		i, ok := at[row.ID]
		if !ok {
			i = len(out.People)
			at[row.ID] = i
			row.You = row.ID == you
			out.People = append(out.People, row)
		}
		p := &out.People[i]
		if _, ok := byName[m.Identity.Name]; !ok {
			byName[m.Identity.Name] = i
			p.Names = append(p.Names, m.Identity.Name)
			p.Name = min(p.Name, m.Identity.Name)
			if p.GitHub == "" {
				p.GitHub = row.GitHub
			}
			p.Aliases = unionSorted(p.Aliases, row.Aliases)
		}
		answers := whole && m.Area == "" && m.Relation == authz.Owner
		j := slices.IndexFunc(p.Projects, func(x api.PersonProject) bool { return x.Project == m.Project && x.Identity == m.Identity.ID })
		if j < 0 {
			p.Projects = append(p.Projects, api.PersonProject{Project: m.Project, Slug: s.d.Projects.Slug(m.Project), Identity: m.Identity.ID, Name: m.Identity.Name, Answers: answers})
		} else if answers {
			p.Projects[j].Answers = true
		}
	}
	if len(out.People) == 0 {
		return out, nil
	}
	if h := s.d.SessionHooks(); h != nil && h.Seen != nil {
		ss, err := h.Seen(ctx, sc)
		if err != nil {
			return out, err
		}
		for _, x := range ss {
			i, ok := byName[x.Person]
			if !ok {
				continue
			}
			p := &out.People[i]
			if x.Machine != "" && !slices.Contains(p.Machines, x.Machine) {
				p.Machines = append(p.Machines, x.Machine)
			}
			seen := x.Seen
			if p.Seen == nil || seen.After(*p.Seen) {
				t := seen
				p.Seen = &t
			}
		}
	}
	for i := range out.People {
		slices.Sort(out.People[i].Machines)
		slices.Sort(out.People[i].Names)
	}
	sortPeople(out.People)
	return out, nil
}

// person is id's row before projects, names and sessions: its stable person
// id, linked login and aliases at this authority.
func (s *Service) person(ctx context.Context, id api.Identity) api.Person {
	p := api.Person{ID: id.ID, Name: id.Name, Kind: id.Kind, Projects: []api.PersonProject{}}
	if s.d.LinkedAccount != nil {
		if acc, login := s.d.LinkedAccount(ctx, id.ID); acc != "" {
			p.ID, p.GitHub = acc, login
		}
	}
	if s.d.Aliases != nil {
		p.Aliases = s.d.Aliases(ctx, id.ID)
	}
	return p
}

// mergePeopleJSON is mergePeople over the union's raw array.
func MergePeopleJSON(raw json.RawMessage) json.RawMessage {
	var rows []api.Person
	if json.Unmarshal(raw, &rows) != nil {
		return raw
	}
	b, _ := json.Marshal(MergePeople(rows))
	return b
}

// mergePeople merges the people lists of several authorities by person id
// (api.Person.ID): one row per person, with every shared project, alias and
// machine and the latest seen (internal/team's proxy union). Rows of the same name and
// different ids stay apart (two people, or one not yet linked everywhere);
// rows of different names and one id are one person, named by the least
// name, with every name in Names (each project keeps its own); You and a
// project's Answers carry over from any row. A row with no id (an authority from
// before ids) keys by its name, as it did.
func MergePeople(rows []api.Person) []api.Person {
	out := []api.Person{}
	at := map[string]int{}
	for _, p := range rows {
		key := "id:" + p.ID
		if p.ID == "" {
			key = "name:" + p.Name
		}
		i, ok := at[key]
		if !ok {
			at[key] = len(out)
			p.Projects = append([]api.PersonProject{}, p.Projects...)
			p.Aliases = slices.Clone(p.Aliases)
			p.Names = unionSorted(p.Names, []string{p.Name})
			out = append(out, p)
			continue
		}
		cur := &out[i]
		if p.Name < cur.Name {
			cur.Name = p.Name
		}
		if cur.GitHub == "" {
			cur.GitHub = p.GitHub
		}
		cur.You = cur.You || p.You
		for _, pp := range p.Projects {
			j := slices.IndexFunc(cur.Projects, func(x api.PersonProject) bool { return x.Project == pp.Project && x.Identity == pp.Identity })
			if j < 0 {
				cur.Projects = append(cur.Projects, pp)
			} else if pp.Answers {
				cur.Projects[j].Answers = true
			}
		}
		cur.Names = unionSorted(cur.Names, append([]string{p.Name}, p.Names...))
		cur.Aliases = unionSorted(cur.Aliases, p.Aliases)
		cur.Machines = unionSorted(cur.Machines, p.Machines)
		if p.Seen != nil && (cur.Seen == nil || p.Seen.After(*cur.Seen)) {
			cur.Seen = p.Seen
		}
	}
	sortPeople(out)
	return out
}

func sortPeople(out []api.Person) {
	slices.SortFunc(out, func(a, b api.Person) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func unionSorted(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range append(append([]string(nil), a...), b...) {
		set[x] = true
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
