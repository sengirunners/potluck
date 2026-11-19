package hostprobe

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// dropUnfit unbinds this machine's earlier box bindings (<prefix>-<size> on
// the layer this run lends) of a size that no box fits on this machine at
// all, one box over every core and all memory: after a hardware or disk
// swap, such a size stays bound, fails every acquire, and no run rebinds it.
// A size that still fits is the owner's to keep (an earlier --share or
// --size) and is never unbound here. It returns a note for each it unbound,
// or could not, with the exact command that does. A report whose machine
// fits no size at all is a misread, never a reason to unbind.
func dropUnfit(ctx context.Context, b Binder, o ShareOptions, layer string) []string {
	fit := fitting(o.Report, Limits{Share: 1}, 1)
	if len(fit) == 0 || o.NamePrefix == "" {
		return nil
	}
	flag := ""
	if o.Pool != "" {
		flag = "--host "
	}
	u, _ := b.(unbinder)
	var notes []string
	for _, t := range o.Earlier {
		name, ok := strings.CutPrefix(t.Name, o.NamePrefix+"-")
		if !ok || t.Kind != "box" || t.Host != "" || cmp.Or(t.Config["vm"], "tart") != layer {
			continue
		}
		s, err := SizeByName(name)
		if err != nil || slices.Contains(fit, s) {
			continue
		}
		why := fmt.Sprintf("%s, bound by an earlier share, is %d vCPU and %d MB, more than this machine has (%d cores, %d MB)", t.Name, s.CPU, s.MemoryMB, o.Report.Cores, o.Report.RAMMB)
		if u == nil {
			notes = append(notes, fmt.Sprintf("%s: %s removes it", why, config.Command("target unbind", flag+t.Name)))
			continue
		}
		if o.Pool != "" {
			err = u.UnbindHostRunTarget(ctx, t.Name)
		} else {
			err = u.UnbindRunTarget(ctx, t.Name)
		}
		var ae *api.Error
		switch {
		case err == nil:
			notes = append(notes, why+": it was unbound")
		case errors.As(err, &ae) && ae.Code == api.CodeNotFound:
		default:
			notes = append(notes, fmt.Sprintf("%s, and unbinding it failed (%v): %s removes it", why, err, config.Command("target unbind", flag+t.Name)))
		}
	}
	return notes
}
