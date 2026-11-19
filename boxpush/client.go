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
	t, err := computeclient.FindRunTarget(ctx, c, "", target)
	if err != nil {
		return false, err
	}
	return t.Repo != "", nil
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

// Into bundles mo against what box handle (box:<target>/<id>) holds and
// pushes it there (MakeAndPush).
func Into(ctx context.Context, c *client.Client, handle string, mo MakeOptions, po PushOptions) (Result, error) {
	target, id, err := computeclient.BoxHandle(ctx, c, handle)
	if err != nil {
		return Result{}, err
	}
	return MakeAndPush(ctx, Exec(c, target, id), mo, po, nil)
}
