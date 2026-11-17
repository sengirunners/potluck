package runtarget_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// A size bound earlier whose driver now describes its box without the size
// (potluck-driver-tart does when it cannot read its image: gone after a disk
// swap, or Docker unreadable) is listed with the driver's own reason, when it
// gives one, and the command that repairs and rebinds it, never only the
// shape error.
func TestBoxWithoutSizeSaysHowToFix(t *testing.T) {
	soon, later := time.Now().Add(time.Hour).Format("15:04:05"), time.Now().Add(2*time.Hour).Format("15:04:05")
	for _, c := range []struct {
		name, program string
		config        map[string]string
		why, fix      string
	}{
		{"with the driver's reason", "zm", map[string]string{"window": soon + "-" + later}, "reclaimed: outside the window " + soon + "-" + later, "run zm host share on the machine that lends it"},
		{"no reason given", "zm", nil, "", "run zm host share on the machine that lends it"},
		{"in potluck's words", "potluck", nil, "", "run potluck host on the machine that lends it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.program != "zm" {
				config.Program, config.Verbs = c.program, map[string]string{"host share": "host"}
				t.Cleanup(func() { config.Program, config.Verbs = "zm", nil })
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			fix, _ := filepath.EvalSymlinks(t.TempDir())
			state := filepath.Join(fix, "state")
			drv, err := refdriver.Install(fix, state, "box-no-size")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { refdriver.KillAll(state) })
			acme := ids.New()
			b := computeapi.RunTarget{Name: "host-a-c1m2", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, Config: c.config}
			h := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
			if err := os.WriteFile(filepath.Join(home, runtarget.HostFile), []byte(rt.EncodeHostRunTargets([]computeapi.HostRunTarget{{RunTarget: b, Share: []string{acme}}})), 0o600); err != nil {
				t.Fatal(err)
			}
			l, err := computeclient.HostRunTargets(context.Background(), h.owner)
			if err != nil || len(l.Targets) != 1 {
				t.Fatalf("host list: %+v %v", l, err)
			}
			v := l.Targets[0]
			if v.Describe != nil {
				t.Fatalf("a box without its size described: %+v", v.Describe)
			}
			for _, want := range []string{"potluck-driver-ref describes a box without its size", c.why, c.fix} {
				if !strings.Contains(v.Error, want) {
					t.Errorf("error %q does not say %q", v.Error, want)
				}
			}
		})
	}
}
