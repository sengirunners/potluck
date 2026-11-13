package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
)

type sandboxCreateIn struct {
	On              string `json:"on,omitempty" jsonschema:"host; default: smallest that fits"`
	Size            string `json:"size,omitempty" jsonschema:"a size the host offers"`
	OS              string `json:"os,omitempty" jsonschema:"linux or macos"`
	TTLS            int    `json:"ttl_s,omitempty" jsonschema:"lease (s)"`
	Ref             string `json:"ref,omitempty" jsonschema:"ref to check out (a sandbox without a repo gets it from your checkout, in ~/checkout)"`
	Dirty           bool   `json:"dirty,omitempty" jsonschema:"also uncommitted changes to tracked files (ref HEAD)"`
	Dir             string `json:"dir,omitempty" jsonschema:"absolute path of the checkout to send from (default: this server's cwd)"`
	Image           string `json:"image,omitempty" jsonschema:"one of the host’s images"`
	Template        string `json:"template,omitempty" jsonschema:"agent: git, gh, Node, Python, claude and codex CLIs; browser: sandboxed Chromium, Playwright and PNG screenshots"`
	Net             string `json:"net,omitempty" jsonschema:"on (default): internet through a trusted host's filtered network; off: no network at all"`
	Persist         bool   `json:"persist,omitempty" jsonschema:"keep its disk when it stops; sandbox_stop needs it (trusted hosts)"`
	ClientRequestID string `json:"client_request_id,omitempty" jsonschema:"idempotency key: reuse on retry"`
	// Volumes: named volumes of yours that outlive the box (trusted hosts).
	Volumes []computeapi.BoxVolume `json:"volumes,omitempty" jsonschema:"named volumes of yours kept on the host: name, path (relative to HOME), mode rw (one sandbox at a time; default), ro or cow (shared)"`
}

type sandboxExecIn struct {
	Handle   string   `json:"handle" jsonschema:"sandbox identifier returned by sandbox_create"`
	Argv     []string `json:"argv" jsonschema:"literal argv"`
	Stdin    string   `json:"stdin,omitempty"`
	TimeoutS int      `json:"timeout_s,omitempty" jsonschema:"default 30, max 600"`
	Cwd      string   `json:"cwd,omitempty"`
}

type sandboxSpawnIn struct {
	Handle          string   `json:"handle" jsonschema:"sandbox identifier returned by sandbox_create"`
	Argv            []string `json:"argv" jsonschema:"literal argv"`
	Stdin           string   `json:"stdin,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	TimeoutS        int      `json:"timeout_s,omitempty" jsonschema:"default: until the sandbox ends"`
	ClientRequestID string   `json:"client_request_id,omitempty" jsonschema:"idempotency key: reuse on retry"`
}

type sandboxOutputIn struct {
	Handle     string `json:"handle" jsonschema:"sandbox identifier returned by sandbox_create"`
	PID        string `json:"pid"`
	StdoutFrom int64  `json:"stdout_from,omitempty" jsonschema:"last stdout_next"`
	StderrFrom int64  `json:"stderr_from,omitempty" jsonschema:"last stderr_next"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"default 32768"`
	WaitS      int    `json:"wait_s,omitempty"`
}

type sandboxSignalIn struct {
	Handle string `json:"handle" jsonschema:"sandbox identifier returned by sandbox_create"`
	PID    string `json:"pid"`
	Signal string `json:"signal,omitempty" jsonschema:"TERM (default), INT or KILL"`
}

type sandboxListIn struct {
	Live bool `json:"live,omitempty" jsonschema:"only sandboxes not yet ended"`
}

// sandboxList is sandbox_list's answer: Skipped names bindings that did not answer.
type sandboxList struct {
	Handles []computeapi.RunTargetHandle `json:"handles"`
	Skipped []string                     `json:"skipped,omitempty"`
}

type sandboxHandleIn struct {
	Handle string `json:"handle" jsonschema:"sandbox identifier returned by sandbox_create"`
	TTLS   int    `json:"ttl_s,omitempty" jsonschema:"renew, start: lease from now (s); renew's default 600"`
}

// SandboxTools adds Potluck's sandbox tools to tb: NewServer's, or another
// program's MCP server that mounts them (NewToolbox).
func SandboxTools(tb *Toolbox) {
	addSandboxFileTools(tb)
	addSandboxPreviewTools(tb)
	add(tb, "sandbox_create", additive, true, func(ctx context.Context, c *client.Client, in sandboxCreateIn) (any, error) {
		if in.TTLS < 0 {
			return nil, api.Errorf(api.CodeBadRequest, "ttl_s must not be negative")
		}
		if in.Net != "" && in.Net != computeapi.NetOn && in.Net != computeapi.NetOff {
			return nil, api.Errorf(api.CodeBadRequest, "net is on or off")
		}
		want := computeclient.BoxWant{On: in.On, Size: in.Size, OS: in.OS, Template: in.Template, Net: in.Net, Volumes: len(in.Volumes) > 0}
		req := computeapi.AcquireRunTargetRequest{RequestID: cmp.Or(in.ClientRequestID, "mcp-"+strings.ToLower(ids.New())), TTLS: in.TTLS, Ref: in.Ref, Image: in.Image, Template: in.Template, Net: in.Net, Persist: in.Persist,
			Volumes: in.Volumes}
		if in.Dir != "" && !filepath.IsAbs(in.Dir) {
			return nil, api.Errorf(api.CodeBadRequest, "dir must be an absolute path")
		}
		var b *boxpush.Bundle
		defer func() {
			if b != nil {
				b.Close()
			}
		}()
		pushing := false // the binding asked checks out no refs: this checkout's commit is pushed in
		prep := func(ctx context.Context, target string, r *computeapi.AcquireRunTargetRequest) error {
			pushing = false
			if in.Ref == "" && !in.Dirty { // a box without a repo gets this checkout's commit (zm-eakh.12.5.16)
				return nil
			}
			self, err := boxpush.ChecksOutItself(ctx, c, target)
			switch {
			case err != nil:
				return err
			case self:
				return boxpush.SelfMisfit(target, in.Dirty, false) // the tool takes no dependency layers
			}
			if b == nil {
				if b, err = boxpush.Make(ctx, boxpush.MakeOptions{Dir: in.Dir, Ref: in.Ref, Dirty: in.Dirty}); err != nil { // tracked files only
					return err
				}
			}
			r.Ref, pushing = "", true
			return nil
		}
		target, h, err := computeclient.AcquireBox(ctx, c, want, req, prep)
		if err != nil || !pushing {
			return h, err
		}
		_, id, _ := computeclient.SplitBoxHandle(h.Handle)
		res, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{})
		if err != nil {
			return nil, fmt.Errorf("%w (sandbox %s is kept; stop it or create again)", err, h.Handle)
		}
		return struct {
			computeapi.RunTargetHandle
			Push boxpush.Result `json:"push"`
		}{h, res}, nil
	})
	add(tb, "sandbox_exec", additive, true, func(ctx context.Context, c *client.Client, in sandboxExecIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		return computeclient.ExecRunTarget(ctx, c, t, id, computeapi.ExecRunTargetRequest{Argv: in.Argv, Stdin: in.Stdin, TimeoutS: in.TimeoutS, Cwd: in.Cwd})
	})
	add(tb, "sandbox_list", readOnly, true, func(ctx context.Context, c *client.Client, in sandboxListIn) (any, error) {
		hs, skipped, err := computeclient.Sandboxes(ctx, c, in.Live)
		return sandboxList{Handles: nonNil(hs), Skipped: skipped}, err
	})
	add(tb, "sandbox_renew", idempotent, true, func(ctx context.Context, c *client.Client, in sandboxHandleIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		switch {
		case in.TTLS < 0:
			return nil, api.Errorf(api.CodeBadRequest, "ttl_s must not be negative")
		case in.TTLS == 0:
			in.TTLS = 600
		}
		return computeclient.RenewRunTarget(ctx, c, t, id, in.TTLS)
	})
	add(tb, "sandbox_stop", idempotent, false, func(ctx context.Context, c *client.Client, in sandboxHandleIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		// The daemon stops a persistent box (keeping its disk) and refuses any
		// other, which stop would destroy. Its refusal's hint names the CLI,
		// so an agent gets the tools' names instead.
		ans, err := computeclient.StopRunTarget(ctx, c, t, id)
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.CodeBadRequest {
			return nil, ae.WithHint("sandbox_delete ends it; sandbox_create with persist: true makes a sandbox that stops and starts")
		}
		return ans, err
	})
	add(tb, "sandbox_start", idempotent, false, func(ctx context.Context, c *client.Client, in sandboxHandleIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		if in.TTLS < 0 {
			return nil, api.Errorf(api.CodeBadRequest, "ttl_s must not be negative")
		}
		return computeclient.StartRunTarget(ctx, c, t, id, in.TTLS)
	})
	add(tb, "sandbox_delete", idempotent, false, func(ctx context.Context, c *client.Client, in sandboxHandleIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		return computeclient.ReleaseRunTarget(ctx, c, t, id)
	})
	add(tb, "sandbox_spawn", additive, false, func(ctx context.Context, c *client.Client, in sandboxSpawnIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		return computeclient.SpawnRunTarget(ctx, c, t, id, computeapi.SpawnRunTargetRequest{Argv: in.Argv, Stdin: in.Stdin, Cwd: in.Cwd, TimeoutS: in.TimeoutS,
			RequestID: cmp.Or(in.ClientRequestID, "mcp-"+strings.ToLower(ids.New()))})
	})
	add(tb, "sandbox_output", readOnly, true, func(ctx context.Context, c *client.Client, in sandboxOutputIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		// An agent's page defaults to 32 KiB: what fits its context, not exec's MiB.
		return computeclient.RunTargetOutput(ctx, c, t, id, computeapi.OutputRunTargetRequest{PID: in.PID, StdoutFrom: in.StdoutFrom, StderrFrom: in.StderrFrom,
			MaxBytes: cmp.Or(in.MaxBytes, 32<<10), WaitS: in.WaitS})
	})
	add(tb, "sandbox_signal", idempotent, false, func(ctx context.Context, c *client.Client, in sandboxSignalIn) (any, error) {
		t, id, err := computeclient.BoxHandle(ctx, c, in.Handle)
		if err != nil {
			return nil, err
		}
		return computeclient.SignalRunTarget(ctx, c, t, id, computeapi.SignalRunTargetRequest{PID: in.PID, Signal: cmp.Or(in.Signal, "TERM")})
	})
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}
