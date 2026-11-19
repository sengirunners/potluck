package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/pushbase"
)

func (a *app) sandboxCreateCmd() *cobra.Command {
	var req computeapi.AcquireRunTargetRequest
	var want computeclient.BoxWant
	var ttl time.Duration
	var push pushFlags
	var netOpt string
	var vols []string
	var df depscli.Flags
	cmd := &cobra.Command{Use: "create [--size c2m4] [--os linux] [--on <machine-size>] [--ttl 1h] [--ref HEAD [--dirty]] [--template agent|browser] [--net off] [--persist] [--volume name:path[:rw|ro|cow]]... [--deps npm[:path]]... [--database <recipe>]",
		Short: "Start a sandbox on the smallest shared machine size that fits (or --on); prints its id",
		Long: `With --ref (or --dirty) the sandbox gets that commit of this checkout, sent
as a git bundle, checked out in ~/checkout (--dir); no credential enters it.

--template agent boots the coding-agent image (git, gh, Node, Go, Python, the
Claude Code and Codex CLIs). --template browser adds sandboxed Chromium,
Playwright and PNG screenshots. The machine must share the selected image.

On a trusted machine a Linux sandbox reaches the internet through the
machine's filtered network (never the machine, its LAN, cloud metadata or
outbound mail); --net off makes it with no network at all. A public
machine's sandbox has none.

--persist keeps the sandbox's disk when it stops (` + a.cmd("sandbox stop") + `, or
its lease running out): ` + a.cmd("sandbox start") + ` boots the same disk on the
same machine, for you only, until the machine's keep runs out (30 days by
default; ` + a.cmd("sandbox ls") + ` shows when). ` + a.cmd("sandbox rm") + ` deletes it.
Trusted machines whose driver keeps disks only.

--volume name:path[:mode] mounts a named volume of yours at path (relative
to the sandbox's HOME) that outlives the sandbox: the next one that names it
on the same machine sees what this one left. rw (the default) is one
sandbox's at a time; ro and cow are shared by any number of yours. Trusted
machines only; repeat for more.

--deps npm[:path] mounts a dependency layer of yours at path (default: the
checkout, --dir, plus this directory's place in its repository): node_modules, built once from the package-lock.json and
package.json files (and each workspace's package.json) of this directory,
and its .npmrc's resolution keys (legacy-peer-deps and the like; an .npmrc
with any other key, such as a registry or a token, is refused),
installed by the machine in a sandbox of yours with the network and no
secrets, and reused by your later sandboxes on that machine. ` + a.name() + ` prints
whether it was a hit (already built) or built. The sandbox sees it
copy-on-write: what it writes never reaches the layer or another sandbox.
Trusted machines only. --deps-wait S waits at most S seconds while another
sandbox builds the same layer (default 600; 0 takes the sandbox at once
without it); --deps-scripts off builds without running dependencies'
install scripts. The files come from this directory, not from --ref.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			if ttl < 0 {
				return fmt.Errorf("--ttl must not be negative")
			}
			req.TTLS = int(ttl / time.Second)
			if req.Net, err = netFlag(netOpt); err != nil {
				return err
			}
			want.Net = req.Net
			if req.Volumes, err = volumeFlags(vols); err != nil {
				return err
			}
			if err := df.Request(cmd.Context(), cmd.Flags(), push.repo, push.dir, &req); err != nil {
				return err
			}
			want.Volumes = len(req.Volumes) > 0 || len(req.Deps) > 0
			if req.RequestID == "" {
				req.RequestID = "cli-" + strings.ToLower(ids.New())
			}
			h, res, err := a.sandboxCreate(cmd.Context(), c, want, req, push)
			switch {
			case err != nil:
				return err
			case a.asJSON() && res != nil:
				return a.printJSON(struct {
					computeapi.RunTargetHandle
					Push boxpush.Result `json:"push"`
				}{h, *res})
			case res != nil:
				return nil // printed before the push
			}
			return a.printHandle(h, false)
		}}
	f := cmd.Flags()
	f.StringVar(&want.On, "on", "", a.onUsage())
	f.StringVar(&want.Size, "size", "", "a size one of the pool's machines states (c<vCPU>m<GiB>, e.g. c2m4); with --on, a machine name")
	f.StringVar(&want.OS, "os", "", "linux or macos")
	f.DurationVar(&ttl, "ttl", 0, "lease length (default: the machine's)")
	f.StringVar(&req.Ref, "ref", "", "a commit of this checkout to send in (HEAD)")
	f.StringVar(&req.Image, "image", "", "one of the machine's images, by name (default the first)")
	f.StringVar(&want.Template, "template", "", "an image template: "+strings.Join(computeapi.BoxTemplates(), ", ")+" (default: the machine's first image)")
	f.StringVar(&req.RequestID, "request-id", "", "idempotency key: the same key answers the same sandbox")
	f.StringVar(&netOpt, "net", "on", netUsage)
	f.BoolVar(&req.Persist, "persist", false, "keep the sandbox's disk when it stops, for "+a.cmd("sandbox start")+" (a trusted machine whose driver keeps disks)")
	f.StringArrayVar(&vols, "volume", nil, "name:path[:rw|ro|cow]: a named volume of yours that outlives the sandbox, mounted at path (repeatable)")
	push.add(f)
	df.Add(f)
	databaseFlags(cmd, &req.Database)
	return cmd
}

// volumeFlags reads --volume name:path[:mode].
func volumeFlags(vs []string) ([]computeapi.BoxVolume, error) {
	var out []computeapi.BoxVolume
	for _, v := range vs {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("--volume %q: name:path[:rw|ro|cow]", v)
		}
		bv := computeapi.BoxVolume{Name: parts[0], Path: parts[1], Mode: computeapi.VolumeRW}
		if len(parts) == 3 {
			bv.Mode = parts[2]
		}
		switch bv.Mode {
		case computeapi.VolumeRW, computeapi.VolumeRO, computeapi.VolumeCOW:
		default:
			return nil, fmt.Errorf("--volume %q: the mode is rw, ro or cow", v)
		}
		out = append(out, bv)
	}
	return out, nil
}

const netUsage = "on: the machine's filtered network on a trusted machine (internet, never the machine or its LAN); off: no network at all"

// netFlag is --net as the request's net: on is the default, sent as nothing.
func netFlag(v string) (string, error) {
	switch v {
	case "", computeapi.NetOn:
		return "", nil
	case computeapi.NetOff:
		return computeapi.NetOff, nil
	}
	return "", fmt.Errorf("--net is on or off, not %q", v)
}

// sandboxCreate places and starts a sandbox; with a ref (or --dirty) on a
// machine that does not check out refs itself, it pushes this checkout's
// commit into it, printing the id first (not with --json).
func (a *app) sandboxCreate(ctx context.Context, c *client.Client, want computeclient.BoxWant, req computeapi.AcquireRunTargetRequest, push pushFlags) (computeapi.RunTargetHandle, *boxpush.Result, error) {
	var h computeapi.RunTargetHandle
	req.Template = want.Template
	var src *boxpush.Source
	plan := &pushbase.Plan{}
	ref := req.Ref // the push below clears the ref of a binding that does not check it out
	if len(req.Deps) > 0 && (ref != "" || push.dirty) {
		depscli.WarnMismatch(ctx, a.host.Stderr, a.name(), push.repo, ref, push.dirty, push.untracked, req.Deps)
	}
	pushing := false  // the binding asked checks out no refs: this checkout's commit is pushed in
	var prepErr error // a refusal before the acquire: nothing was asked of that binding
	prep := func(ctx context.Context, target string, r *computeapi.AcquireRunTargetRequest) error {
		pushing, prepErr = false, nil
		plan.Repository = nil
		if prepErr = depscli.ImageOK(ctx, c, target, r.Image, r.Deps); prepErr != nil {
			return prepErr
		}
		if ref == "" && !push.dirty {
			return nil
		}
		self, err := boxpush.ChecksOutItself(ctx, c, target)
		switch {
		case err != nil:
			prepErr = err
		case self:
			if prepErr = boxpush.SelfMisfit(target, push.dirty, len(r.Deps) > 0); prepErr != nil {
				return prepErr
			}
		case !self && src == nil: // resolved before the sandbox, so a bad ref costs nothing
			src, prepErr = push.source(ctx, ref, a.host.Stderr, a.name())
		}
		if prepErr == nil && !self {
			if p := push.hostBase(ctx, src); p != nil {
				*plan = *p
			}
			r.Ref, pushing = "", true
		}
		return prepErr
	}
	target, h, err := pushbase.Acquire(ctx, c, want, &req, prep, plan)
	switch {
	case err != nil && (target == "" || err == prepErr): // placement, or before the acquire: nothing was asked
		return h, nil, err
	case err != nil && computeclient.MadeNothing(err): // nothing was made: a retry needs no key
		return h, nil, err
	case err != nil:
		return h, nil, retryWithKey(err, req.RequestID)
	}
	depscli.Print(a.host.Stderr, a.name(), req.Deps, h.Deps)
	if !pushing {
		return h, nil, nil
	}
	if !a.asJSON() {
		_ = a.printHandle(h, false)
	}
	_, id, _ := computeclient.SplitBoxHandle(h.Handle)
	res, err := push.push(ctx, c, target, id, src, a.host.Stderr, a.name())
	if err != nil {
		s := sandboxID(h.Handle)
		return h, nil, fmt.Errorf("%w (the sandbox is kept: %s to resume, %s to end it)", err, a.cmd("sandbox push "+s), a.cmd("sandbox rm "+s))
	}
	return h, &res, nil
}
