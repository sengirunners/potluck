package hostprobe

import (
	"context"
	"strconv"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// A re-run that does not choose when to lend keeps what the earlier share
// of the same names bound: away (reclaim_idle_s) or all the time (none);
// a choice for this run wins, and a binding of other names is no guide.
func TestCheckKeepsEarlierReclaim(t *testing.T) {
	def, _ := DefaultImage("tart")
	away := computeapi.RunTarget{Name: "mac-c2m4", Kind: "box", Images: []string{def.Image}, Config: map[string]string{"vm": "tart", "reclaim_idle_s": "600"}}
	always := computeapi.RunTarget{Name: "mac-c2m4", Kind: "box", Images: []string{def.Image}, Config: map[string]string{"vm": "tart"}}
	other := computeapi.RunTarget{Name: "other-c2m4", Kind: "box", Images: []string{def.Image}, Config: map[string]string{"vm": "tart", "reclaim_idle_s": "900"}}
	for _, c := range []struct {
		name    string
		earlier []computeapi.RunTarget
		idle    int
		set     bool
		want    int
		from    string
	}{
		{"earlier away", []computeapi.RunTarget{away}, DefaultReclaimIdleS, false, 600, "mac-c2m4"},
		{"earlier all the time", []computeapi.RunTarget{always}, DefaultReclaimIdleS, false, 0, "mac-c2m4"},
		{"chosen now", []computeapi.RunTarget{away}, 0, true, 0, ""},
		{"no earlier", nil, DefaultReclaimIdleS, false, DefaultReclaimIdleS, ""},
		{"other names", []computeapi.RunTarget{other}, DefaultReclaimIdleS, false, DefaultReclaimIdleS, ""},
	} {
		o := ShareOptions{Report: macReport(), Earlier: c.earlier, NamePrefix: "mac", DryRun: true, Sizes: []string{"c2m4"},
			ReclaimIdleS: c.idle, ReclaimSet: c.set}
		res, err := checkOn(context.Background(), nil, o)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.ReclaimIdleS != c.want || res.ReclaimFrom != c.from {
			t.Errorf("%s: reclaim %d from %q, want %d from %q", c.name, res.ReclaimIdleS, res.ReclaimFrom, c.want, c.from)
		}
		bound := ""
		if len(res.Sizes) > 0 {
			bound = res.Sizes[0].Config["reclaim_idle_s"]
		}
		if want := map[bool]string{true: "", false: strconv.Itoa(c.want)}[c.want == 0]; bound != want {
			t.Errorf("%s: bound reclaim_idle_s %q, want %q", c.name, bound, want)
		}
	}
}
