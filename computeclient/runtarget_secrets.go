package computeclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/protocol"
)

// SetRunTargetSecrets puts values through the box-scope secrets door. The
// daemon enforces the box grant, transport policy, capability and memory rules.
func SetRunTargetSecrets(ctx context.Context, c *client.Client, target, id string, req protocol.BoxSecretsArgs) (protocol.BoxSecretsResult, error) {
	var result protocol.BoxSecretsResult
	err := c.Do(ctx, http.MethodPost, runTargetPath(target)+"/handles/"+client.Seg(id)+"/secrets", req, &result)
	return result, err
}
