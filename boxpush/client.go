package boxpush

import (
	"context"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Exec is ExecFunc for one box handle through c.
func Exec(c *client.Client, target, id string) ExecFunc {
	return func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		return computeclient.ExecRunTarget(ctx, c, target, id, req)
	}
}

// ChecksOutItself says whether box binding target checks out a ref on its
// own (a binding with a repo: zm resolves the ref on the remote and the
// driver fetches it). Every other box, a remote one included, gets the
// renter's code by a push.
func ChecksOutItself(ctx context.Context, c *client.Client, target string) (bool, error) {
	l, err := computeclient.RunTargets(ctx, c, false)
	if err != nil {
		return false, err
	}
	for _, t := range l.Targets {
		if t.Name == target {
			return t.Repo != "", nil
		}
	}
	return false, api.Errorf(api.CodeNotFound, "no run target %s", target)
}

// SelfMisfit is the Misfit (computeclient.Misfit: nothing asked of it, the
// next binding may fit) of box binding target, which checks out a ref
// itself, for a sandbox of uncommitted changes (dirty) or with dependency
// layers (deps); nil when it fits.
func SelfMisfit(target string, dirty, deps bool) error {
	switch {
	case dirty:
		return &computeclient.Misfit{Err: api.Errorf(api.CodeBadRequest, "%s checks out pushed refs from its repo: --dirty is for a machine without one", target)}
	case deps:
		return &computeclient.Misfit{Err: api.Errorf(api.CodeBadRequest, "%s checks out --ref itself, into a directory a dependency layer is not mounted in: ask for the layer without --ref and push", target)}
	}
	return nil
}

// Into bundles mo and pushes it into box handle (box:<target>/<id>).
func Into(ctx context.Context, c *client.Client, handle string, mo MakeOptions, po PushOptions) (Result, error) {
	target, id, err := computeclient.BoxHandle(ctx, c, handle)
	if err != nil {
		return Result{}, err
	}
	b, err := Make(ctx, mo)
	if err != nil {
		return Result{}, err
	}
	defer b.Close()
	return Push(ctx, Exec(c, target, id), b, po)
}
