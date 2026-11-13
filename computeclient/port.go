package computeclient

import (
	"context"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/portstream"
	"github.com/sengirunners/potluck/protocol"
	"net"
)

// DialRunTargetPort connects only to the sandbox's own loopback TCP port.
// Its bounded channel calls use the same holder checks and relay as exec.
// Keep ctx alive until the connection is closed.
func DialRunTargetPort(ctx context.Context, c *client.Client, target, id string, port int) (net.Conn, error) {
	return portstream.Dial(ctx, port, func(ctx context.Context, req protocol.PortRequest) (protocol.PortResult, error) {
		var out protocol.PortResult
		err := c.Do(ctx, "POST", runTargetPath(target)+"/handles/"+client.Seg(id)+"/port", req, &out)
		return out, err
	})
}

func PublishRunTargetPreview(ctx context.Context, c *client.Client, target, id string, req computeapi.PreviewRequest) (computeapi.Preview, error) {
	var out computeapi.Preview
	err := c.Do(ctx, "POST", runTargetPath(target)+"/handles/"+client.Seg(id)+"/previews", req, &out)
	return out, err
}
func RunTargetPreviews(ctx context.Context, c *client.Client, target, id string) (computeapi.PreviewList, error) {
	var out computeapi.PreviewList
	err := c.Do(ctx, "GET", runTargetPath(target)+"/handles/"+client.Seg(id)+"/previews", nil, &out)
	return out, err
}
func RevokeRunTargetPreview(ctx context.Context, c *client.Client, target, id, preview string) error {
	return c.Do(ctx, "POST", runTargetPath(target)+"/handles/"+client.Seg(id)+"/previews", computeapi.PreviewRequest{Revoke: preview}, nil)
}
