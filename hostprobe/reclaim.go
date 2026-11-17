package hostprobe

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
)

// reclaim is the reclaim policy a share binds: ReclaimIdleS when the owner
// chose it for this run (ReclaimSet), else what the first earlier box
// binding of these names (<prefix>-<size>) bound, so a re-run lends at the
// same times as before (no reclaim_idle_s: all the time); from names that
// binding. With neither it is ReclaimIdleS, the caller's default.
func (o ShareOptions) reclaim() (idleS int, from string) {
	if o.ReclaimSet {
		return o.ReclaimIdleS, ""
	}
	ts := slices.SortedFunc(slices.Values(o.Earlier), func(a, b computeapi.RunTarget) int { return cmp.Compare(a.Name, b.Name) })
	for _, t := range ts {
		size, ok := strings.CutPrefix(t.Name, o.NamePrefix+"-")
		if !ok || t.Kind != "box" || t.Host != "" {
			continue
		}
		if _, err := SizeByName(size); err != nil {
			continue
		}
		v := t.Config["reclaim_idle_s"]
		if v == "" {
			return 0, t.Name
		}
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n, t.Name
		}
	}
	return o.ReclaimIdleS, ""
}
