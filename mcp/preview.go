package mcp

import (
	"context"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
)

type previewPublishIn struct {
	Handle string `json:"handle"`
	Port   int    `json:"port"`
	Public bool   `json:"public" jsonschema:"must be true: anyone with the URL can reach this port until revoke or lease end"`
}
type previewRevokeIn struct {
	Handle  string `json:"handle"`
	Preview string `json:"preview"`
}

func addSandboxPreviewTools(tb *Toolbox) {
	add(tb, "sandbox_preview_publish", additive, false, func(ctx context.Context, c *client.Client, in previewPublishIn) (any, error) {
		target, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		return computeclient.PublishRunTargetPreview(ctx, c, target, id, computeapi.PreviewRequest{Port: in.Port, Public: in.Public})
	})
	add(tb, "sandbox_preview_list", readOnly, true, func(ctx context.Context, c *client.Client, in sandboxHandleIn) (any, error) {
		target, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		return computeclient.RunTargetPreviews(ctx, c, target, id)
	})
	add(tb, "sandbox_preview_revoke", idempotent, false, func(ctx context.Context, c *client.Client, in previewRevokeIn) (any, error) {
		target, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		err = computeclient.RevokeRunTargetPreview(ctx, c, target, id, in.Preview)
		return struct{}{}, err
	})
}
