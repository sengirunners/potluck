package vmhost

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// ReclaimPolicy reads what parseConfig reads and refuses what it refuses: the
// owner-input rule on a Mac, a window anywhere, and a bad value.
func TestReclaimPolicyFromBindingConfig(t *testing.T) {
	pin := "ghcr.io/x/ubuntu@sha256:" + strings.Repeat("a", 64)
	box := func(k, v string) map[string]string {
		return map[string]string{"target": "m-c2m4", "kind": "box", "state_dir": t.TempDir(), "images": pin,
			"size": "c2m4", "cpu": "2", "memory": "4096", k: v}
	}
	p, err := ReclaimPolicy(box("reclaim_idle_s", "300"))
	if runtime.GOOS != "darwin" {
		if err == nil || !strings.Contains(err.Error(), "use window") {
			t.Errorf("reclaim_idle_s off a Mac: %+v %v", p, err)
		}
	} else if err != nil || p.IdleS != 300 {
		t.Errorf("reclaim_idle_s: %+v %v", p, err)
	} else if ok, why := p.Allows(time.Now(), 0); ok || !strings.Contains(why, "owner is active") {
		t.Errorf("owner input: allows %v (%s)", ok, why)
	}
	if p, err = ReclaimPolicy(box("window", "09:00-17:00")); err != nil || p.Window != "09:00-17:00" {
		t.Errorf("window: %+v %v", p, err)
	}
	for _, kv := range [][2]string{{"reclaim_idle_s", "soon"}, {"window", "9-5"}} {
		if _, err := ReclaimPolicy(box(kv[0], kv[1])); err == nil {
			t.Errorf("%s=%s is accepted", kv[0], kv[1])
		}
	}
}
