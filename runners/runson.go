package runners

import (
	"net/http"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/placement"
)

// RunsOn returns labels that place a job on v, or the broker's reason why
// it cannot. setup comes from GET /v0/runners/pool on the broker (also
// available to remote pool readers). v must be the project-scoped view from
// GET /v0/run_targets?describe=1 with the client project set to setup.Pool
// (the Zm-Project header), not the machine's host=1 inventory: trust is
// relative to the pool project.
//
// Explicit size aliases select one binding, just as in placement, while all
// fitness checks still apply. This tests the configured fallback runtime
// plus startup allowance; a workflow may require a different budget.
// This is a snapshot: recent refusals and competition from other boxes are
// not represented by the view.
func RunsOn(v computeapi.RunTargetView, setup Setup) ([]string, bool, string) {
	if setup.Label == "" || setup.JobTTLS <= 0 {
		return nil, false, "pool label and effective job_ttl_s must be set"
	}
	size, os := "", "linux"
	if d := v.Describe; d != nil && d.Size != nil {
		size = d.Size.Name
		for _, candidate := range osLabels {
			if placement.OSFits(candidate, d.Size.OS) {
				os = candidate
				break
			}
		}
	}
	// Prefer an explicit alias for this binding; sorting makes multiple aliases
	// deterministic without assigning significance to map iteration order.
	var aliases []string
	for alias, target := range setup.Sizes {
		if target == v.Name && alias != "" {
			aliases = append(aliases, alias)
		}
	}
	slices.Sort(aliases)
	var why string
	// A plain pool label can place automatically even when the driver's
	// named size is overridden to another binding and no default is set.
	for _, candidate := range append(aliases, size, "") {
		label := strings.ToLower(setup.Label)
		if candidate != "" {
			label += ":" + candidate
		}
		labels := []string{label, os}
		w, ours, parseWhy := parseLabels(labels, setup.Label, setup.DefaultSize)
		if !ours || parseWhy != "" {
			// Invalid driver sizes must still report automatic placement's reason.
			_, why = fits([]computeapi.RunTargetView{v}, want{os: os}, nil, nil, setup.JobTTLS+startupAllowance)
			if why == "" {
				why = parseWhy
			}
			continue
		}
		names, reason := bindingFits([]computeapi.RunTargetView{v}, setup.Sizes, w, nil, nil, setup.JobTTLS+startupAllowance)
		if slices.Contains(names, v.Name) {
			return labels, true, ""
		}
		if why == "" {
			why = reason
			if why == "" {
				why = "size " + w.size + " is assigned to another binding"
			}
		}
	}
	return nil, false, why
}

// poolStatus exposes only placement inputs, never the local setup's App or
// listener details. Readers use project-scoped run-target views alongside it.
func (s *Service) poolStatus(r *http.Request) (any, error) {
	_, err := s.readablePool(r)
	if err != nil {
		return nil, err
	}
	c := s.conf()
	return Setup{Pool: c.Pool.Project, Label: c.Label, JobTTLS: s.jobTTL(), Sizes: c.Pool.Sizes, DefaultSize: c.Pool.DefaultSize}, nil
}

// readablePool hides broker configuration and lookup errors from callers
// that cannot see its project. The machine owner can still diagnose setup.
func (s *Service) readablePool(r *http.Request) (api.ProjectRef, error) {
	p, err := s.pool()
	scope := core.SubjectOf(r.Context()).Scope
	if !scope.IsAll() && (err != nil || !scope.SeesProject(p.ID)) {
		return api.ProjectRef{}, api.Errorf(api.CodeNotFound, "runner pool not found")
	}
	return p, err
}
