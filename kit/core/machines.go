package core

import (
	"context"
	"sort"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// VisibleMachines are the joined machines in rows (MachinePresence's, or
// teamlink's in memory) that sub may see, each with the projects sub reads
// whole that it still heartbeats for (a machine that left a project is not
// listed for it), carrying the newest numbers of any of them. self (this
// machine) is left out.
func (d *Deps) VisibleMachines(ctx context.Context, self string, sub *authz.Subject, rows []MachineSeen) []api.Machine {
	by := map[string]*api.Machine{}
	joined := map[string]map[string]bool{}
	for _, r := range rows {
		if r.Machine == self || sub == nil || !sub.Scope.WholeProject(r.Project) {
			continue
		}
		ps, ok := joined[r.Machine]
		if !ok {
			ps = map[string]bool{}
			if d.Projects != nil {
				for _, p := range d.Projects.MachineProjects(ctx, r.Machine) {
					ps[p] = true
				}
			}
			joined[r.Machine] = ps
		}
		if !ps[r.Project] {
			continue // left the project (zm leave, a revoked token)
		}
		m := by[r.Machine]
		if m == nil {
			m = &api.Machine{Machine: r.Machine}
			by[r.Machine] = m
		}
		m.Projects = append(m.Projects, r.Project)
		if m.Seen == nil || r.Seen.After(*m.Seen) { // the newest numbers of any of its projects
			t := r.Seen
			m.Stats, m.Seen = r.Stats, &t
		}
	}
	out := make([]api.Machine, 0, len(by))
	for _, m := range by {
		sort.Strings(m.Projects)
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out
}
