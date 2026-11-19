package cli

import (
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/depscli"
	"github.com/spf13/cobra"
)

// requestDeps reads the job's dependency recipe once, before placement.
func (o *jobOptions) requestDeps(cmd *cobra.Command) error {
	var req computeapi.AcquireRunTargetRequest
	if err := o.df.Request(cmd.Context(), cmd.Flags(), o.repo, o.push.dir, &req); err != nil {
		return err
	}
	o.deps, o.depsWaitS = req.Deps, req.DepsWaitS
	o.want.Volumes = len(o.deps) > 0
	o.under = depscli.Under(cmd.Context(), o.repo, o.push.dir, o.deps)
	if len(o.deps) > 0 {
		depscli.WarnMismatch(cmd.Context(), o.errw, o.p.name(), o.repo, o.ref, o.push.dirty, o.push.untracked, o.deps)
	}
	return nil
}
