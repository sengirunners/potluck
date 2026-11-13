package computeclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
)

// InputRunTarget is POST .../handles/{id}/input: bytes to a background
// process's terminal, or a resize (SPEC 20 "Boxes", terminals).
func InputRunTarget(ctx context.Context, c *client.Client, target, id string, req computeapi.InputRunTargetRequest) (computeapi.RunTargetInput, error) {
	var r computeapi.RunTargetInput
	return r, c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/input", req, &r)
}
