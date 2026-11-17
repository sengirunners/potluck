package depscli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runtarget"
)

// ImageOK refuses an image name with deps on a binding another machine
// serves, before a command builds its bundle: the daemon keys such a layer
// by the pin the host advertises for a template, so it takes a template,
// never an image name. The daemon refuses it too; this says so before any
// work. A binding another machine serves is one with a host binding.
func ImageOK(ctx context.Context, c *client.Client, target, image string, deps []computeapi.BoxDeps) error {
	if len(deps) == 0 || image == "" {
		return nil
	}
	v, err := computeclient.DescribeRunTarget(ctx, c, target)
	if err != nil || v.HostBinding == "" {
		return nil // the acquire says what is wrong
	}
	return fmt.Errorf("--deps on %s, which %s serves, takes a template (--template %s), not --image %s: the layer is keyed by the image the host advertises for it", v.Name, v.Host, strings.Join(computeapi.BoxTemplates(), "|"), image)
}

// Under is dir's place in its repository ("" its top) when a layer of deps
// is mounted at DefaultPath there: a command run beside its layer runs in
// that directory of the checkout dest, not at its top.
func Under(ctx context.Context, dir, dest string, deps []computeapi.BoxDeps) string {
	if len(deps) == 0 || strings.HasPrefix(dest, "/") {
		return ""
	}
	def := DefaultPath(ctx, dir, dest)
	top := strings.TrimSuffix(dest, "/") + "/"
	if !strings.HasPrefix(def, top) {
		return ""
	}
	for _, d := range deps {
		if d.Path == def {
			return strings.TrimPrefix(def, top)
		}
	}
	return ""
}

// Within is how much longer than a plain acquire one with deps may take to
// answer: waitS (nil: the default deps wait), then the layers' builds.
func Within(deps []computeapi.BoxDeps, waitS *int) time.Duration {
	wait := computeapi.DepsWaitDefaultS
	if waitS != nil {
		wait = *waitS
	}
	return runtarget.DepsAnswerWithin(len(deps), wait)
}
