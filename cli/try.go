package cli

// try: a first sandbox on this machine alone (docs/try.md). It makes the
// pool (create's makePool), shares this machine (the host command itself,
// with the image hostprobe.ChooseFirstImage picks), then makes a sandbox,
// runs uname -a in it and removes it (sandbox create's sandboxCreate and
// the client calls exec and rm make). It owns only the order and its words.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
)

// trialPool is the pool try makes when no --pool is given: the one the
// README's one-machine trial makes by hand.
const trialPool = "trial"

// tryTTL is the try sandbox's lease: it is removed at once, and the lease
// ends it if try is stopped before then.
const tryTTL = 10 * time.Minute

// tryArgv is what try runs in its sandbox.
var tryArgv = []string{"uname", "-a"}

func (a *app) tryCmd() *cobra.Command {
	var images []string
	cmd := &cobra.Command{
		Use:   "try [--image name@sha256:<digest>]...",
		Short: "A first sandbox on this machine alone: make a pool, share this machine, run uname -a in a sandbox, remove it",
		Long: `Try runs the one-machine trial in one command. It makes the pool (` + trialPool + `, or
--pool) with this machine running it and no Tailscale, as ` + a.cmd("create "+trialPool+" --via tunnel") + `
does, unless this machine is in that pool already. When no machine in the
pool shares sandboxes yet, it shares this one: it runs ` + a.cmd("host") + ` with
--reclaim-idle-s 0 (sandboxes run while you use the machine), which checks
the machine, may ask for your password once for its root step, pulls an image
and runs the security checks. On a Mac it lends Potluck's small Linux image
(about 1 GB) instead of the macOS one (about 27 GB). Then it makes a sandbox,
runs uname -a in it and removes it.

The machine stays shared with the pool afterwards, so the next
` + a.cmd("sandbox create") + ` is quick; ` + a.cmd("host stop") + ` stops sharing it. Running try
again is safe: it skips what is done.

--image lends that image instead, as ` + a.cmd("host") + ` --image does.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.try(cmd.Context(), images) },
	}
	cmd.Flags().StringArrayVar(&images, "image", nil, "an image pinned by digest for the host run to lend, name@sha256:<64 hex> (repeat; default: the layer's, on a Mac Potluck's small Linux image)")
	return cmd
}

func (a *app) try(ctx context.Context, images []string) error {
	if a.asJSON() {
		return exitWith{2, errors.New("try prints words, not JSON: for JSON run its steps (" + a.cmd("create") + ", " + a.cmd("host") + ", " + a.cmd("sandbox create") + ") with --json")}
	}
	c, _, err := a.client()
	if err != nil {
		return err
	}
	pool := cmp.Or(a.pool(), trialPool)
	l, err := c.ProjectList(ctx) // the pools this machine runs or joined
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(l.Projects, func(p api.Project) bool { return p.Slug == pool }) {
		if _, err := a.makePool(ctx, c, pool, api.ViaTunnel); err != nil {
			return err
		}
		a.say("pool %s is ready: this machine runs it, for itself alone", pool)
	}
	cp := inPool(c, pool)
	first, chose := a.firstImage(ctx, images)
	// Placement's own answer decides whether to share: no machine in the
	// pool shares sandboxes yet.
	lentSmall := false // this run shared the machine with the small Linux image
	if _, err := computeclient.PlaceBox(ctx, cp, computeclient.BoxWant{}); api.IsCode(err, api.CodeNotFound) {
		if err := a.tryHost(ctx, pool, images, first, chose); err != nil {
			return err
		}
		lentSmall = chose && first.With != ""
	} else if err != nil {
		return err
	}

	if err := a.trySandbox(ctx, cp); err != nil {
		return err
	}
	a.tryNext(pool, first, lentSmall)
	return nil
}

// trySandbox makes a sandbox, runs tryArgv in it and removes it: removed
// whenever it was made, even when an answer after that is an error.
func (a *app) trySandbox(ctx context.Context, cp *client.Client) (err error) {
	req := computeapi.AcquireRunTargetRequest{TTLS: int(tryTTL / time.Second), RequestID: "cli-" + strings.ToLower(ids.New())}
	h, _, err := a.sandboxCreate(ctx, cp, computeclient.BoxWant{}, req, pushFlags{})
	if h.Handle != "" {
		defer func() {
			if rmErr := a.tryRemove(ctx, cp, h.Handle); rmErr != nil {
				err = errors.Join(err, rmErr)
			}
		}()
	}
	if err != nil {
		return err
	}
	if err := a.printHandle(h, false); err != nil {
		return err
	}
	return a.tryRun(ctx, cp, h.Handle)
}

// firstImage is the image a first host run of try lends here (chose false:
// none to choose, as with images given, or the host run says why).
func (a *app) firstImage(ctx context.Context, images []string) (hostprobe.FirstImage, bool) {
	if len(images) > 0 {
		return hostprobe.FirstImage{}, false
	}
	dir, err := (&hostFlags{}).dir(a)
	if err != nil {
		return hostprobe.FirstImage{}, false
	}
	return hostprobe.ChooseFirstImage(hostprobe.ShareOptions{Report: detectHost(ctx, dir)})
}

// tryHost shares this machine with pool by running the host command, as a
// person would type it (printed first).
func (a *app) tryHost(ctx context.Context, pool string, images []string, first hostprobe.FirstImage, chose bool) error {
	args := []string{pool, "--reclaim-idle-s", "0"}
	for _, im := range images {
		args = append(args, "--image", im)
	}
	if chose && first.With != "" { // a Linux guest: an earlier macOS binding's root=on must not carry over
		args = append(args, "--with", first.With, "--root=false")
	}
	switch {
	case chose && first.With != "":
		a.say("sharing this machine: Potluck's small Linux image%s, not macOS%s", aboutGB(first.Lent), aboutGB(first.Default))
	case chose && first.Unpublished:
		a.say("sharing this machine: Potluck's small Linux image for it is not published yet, so this pulls macOS%s", aboutGB(first.Default))
	}
	a.say("running: %s", a.cmd("host "+strings.Join(args, " ")))
	hc := a.hostCmd()
	hc.SetContext(ctx)
	if err := hc.ParseFlags(args); err != nil {
		return err
	}
	return hc.RunE(hc, hc.Flags().Args())
}

// aboutGB is " (about N GB to pull)" for a pin that says, else "".
func aboutGB(p hostprobe.DefaultPin) string {
	if p.PullGB <= 0 {
		return ""
	}
	return fmt.Sprintf(" (about %d GB to pull)", p.PullGB)
}

// tryRun runs tryArgv in the sandbox, its output as sandbox exec prints it.
func (a *app) tryRun(ctx context.Context, c *client.Client, handle string) error {
	target, id, err := computeclient.SplitBoxHandle(handle)
	if err != nil {
		return err
	}
	a.say("$ %s", a.cmd("sandbox exec "+sandboxID(handle)+" -- "+strings.Join(tryArgv, " ")))
	r, err := computeclient.ExecRunTarget(ctx, c, target, id, computeapi.ExecRunTargetRequest{Argv: tryArgv})
	if err != nil {
		return err
	}
	fmt.Fprint(a.host.Stdout, r.Stdout)
	fmt.Fprint(a.host.Stderr, r.Stderr)
	if r.Exit != 0 {
		return fmt.Errorf("%s exited %d in the sandbox", strings.Join(tryArgv, " "), r.Exit)
	}
	return nil
}

// tryRemove ends the sandbox, even when try was stopped (its lease would
// end it later).
func (a *app) tryRemove(ctx context.Context, c *client.Client, handle string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	target, id, err := computeclient.SplitBoxHandle(handle)
	if err == nil {
		_, err = computeclient.ReleaseRunTarget(ctx, c, target, id)
	}
	if err != nil {
		return fmt.Errorf("removing the sandbox: %w (%s ends it; its lease ends it in %s)", err, a.cmd("sandbox rm "+sandboxID(handle)), tryTTL)
	}
	a.say("removed %s", sandboxID(handle))
	return nil
}

// tryNext is what to do after a try; lentSmall: this run shared the machine
// with the small Linux image in place of first.Default, a macOS one.
func (a *app) tryNext(pool string, first hostprobe.FirstImage, lentSmall bool) {
	p := ""
	if pool != trialPool {
		p = " -P " + pool
	}
	a.say("next:")
	a.say("  %s   a sandbox of your own; then %s and %s", a.cmd("sandbox create"+p), a.cmd("sandbox exec <id> -- <command>"), a.cmd("sandbox rm <id>"))
	a.say("  %s   a friend runs sandboxes here (add --host for one who shares a machine)", a.cmd("invite <name>"+p))
	if lentSmall && first.Default.Image != "" {
		a.say("  macOS guests: %s%s; it replaces the Linux image this machine lends", a.cmd("host "+pool+" --image "+first.Default.Image), aboutGB(first.Default))
	}
	a.say("  %s   stop sharing this machine", a.cmd("host stop"))
}
