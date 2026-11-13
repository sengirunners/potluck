package mcp

// sandbox_write_file and sandbox_read_file: a file into or out of a box the
// caller holds, over exec (potluck/boxcp, zm-eakh.12.5.22).
// Directories and big files are `potluck sandbox cp`'s.

import (
	"cmp"
	"context"
	"encoding/base64"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Limits: what an agent writes in one call, and its default read page.
const (
	mcpWriteMax = 1 << 20
	mcpPage     = 32 << 10
)

type sandboxWriteFileIn struct {
	Handle   string `json:"handle" jsonschema:"sandbox identifier returned by sandbox_create"`
	Path     string `json:"path" jsonschema:"file; relative: under HOME"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty" jsonschema:"text (default) or base64"`
}

type sandboxReadFileIn struct {
	Handle   string `json:"handle" jsonschema:"sandbox identifier returned by sandbox_create"`
	Path     string `json:"path" jsonschema:"file; relative: under HOME"`
	Offset   int64  `json:"offset,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"default 32768, max 524288"`
	Encoding string `json:"encoding,omitempty" jsonschema:"text (default; base64 if not UTF-8) or base64"`
}

type sandboxFilePage struct {
	boxcp.Page
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

func encodingIn(e string) (bool, error) {
	switch e {
	case "", "text":
		return false, nil
	case computeapi.EncodingBase64:
		return true, nil
	}
	return false, api.Errorf(api.CodeBadRequest, "encoding is text or base64")
}

func boxExec(ctx context.Context, c *client.Client, handle string) (boxpush.ExecFunc, error) {
	t, id, err := computeclient.BoxHandle(ctx, c, handle)
	if err != nil {
		return nil, err
	}
	return boxpush.Exec(c, t, id), nil
}

func addSandboxFileTools(tb *Toolbox) {
	// It replaces a file there; its answer's path is the box's (untrusted).
	overwrites := &sdk.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}
	add(tb, "sandbox_write_file", overwrites, true, func(ctx context.Context, c *client.Client, in sandboxWriteFileIn) (any, error) {
		b64, err := encodingIn(in.Encoding)
		if err != nil {
			return nil, err
		}
		data := []byte(in.Content)
		if b64 {
			if data, err = base64.StdEncoding.DecodeString(in.Content); err != nil {
				return nil, api.Errorf(api.CodeBadRequest, "content is not base64")
			}
		}
		if len(data) > mcpWriteMax {
			return nil, api.Errorf(api.CodeTooLarge, "content is over 1 MiB: use potluck sandbox cp")
		}
		x, err := boxExec(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		return boxcp.WriteFile(ctx, x, in.Path, data, boxcp.Options{})
	})
	add(tb, "sandbox_read_file", readOnly, true, func(ctx context.Context, c *client.Client, in sandboxReadFileIn) (any, error) {
		b64, err := encodingIn(in.Encoding)
		if err != nil {
			return nil, err
		}
		x, err := boxExec(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		pg, err := boxcp.ReadPage(ctx, x, in.Path, in.Offset, cmp.Or(in.MaxBytes, mcpPage))
		if err != nil {
			return nil, err
		}
		if !b64 {
			if text, ok := pg.Text(); ok {
				return sandboxFilePage{pg, "text", text}, nil
			}
		}
		return sandboxFilePage{pg, computeapi.EncodingBase64, base64.StdEncoding.EncodeToString(pg.Data)}, nil
	})
}
