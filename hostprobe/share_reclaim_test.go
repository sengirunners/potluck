package hostprobe

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/vmhost"
)

// The binding `zm host share` makes on a Mac carries a reclaim policy the vmhost
// driver reads (zm-eakh.12.23.4): owner input (HID idle under the default) says
// no, so a running box ends as reclaimed and no box starts. Moved here from
// vmhost when vmhost went to sengirunners/potluck (zm-eakh.12.34).
func TestShareBindingReclaimsOnOwnerInput(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("reclaim_idle_s is macOS-only")
	}
	rep := Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Cores: 16, RAMMB: 48 << 10, FreeDiskGB: 500,
		Layers: []Layer{{Name: "tart", Available: true, Supported: true}}}
	pins := []string{"ghcr.io/x/ubuntu@sha256:" + strings.Repeat("a", 64)}
	withTartImages(t, pins...)
	res, err := shareOn(context.Background(), nil, nil, ShareOptions{Report: rep, DryRun: true, NamePrefix: "m",
		Images: pins, Sizes: []string{"c2m4"}, ReclaimIdleS: DefaultReclaimIdleS})
	if err != nil {
		t.Fatal(err)
	}
	// What the daemon sends the driver (runtarget/driver.go): the binding's
	// config, its target and kind, and its pins as config images; the driver
	// reads its reclaim policy from that (vmhost.ReclaimPolicy is its parse).
	m := map[string]string{"target": "m-c2m4", "kind": "box", "state_dir": t.TempDir(), "images": strings.Join(pins, ",")}
	for k, v := range res.Sizes[0].Config {
		m[k] = v
	}
	p, err := vmhost.ReclaimPolicy(m)
	if err != nil {
		t.Fatal(err)
	}
	if p.IdleS != DefaultReclaimIdleS {
		t.Fatalf("reclaim idle %d, want %d", p.IdleS, DefaultReclaimIdleS)
	}
	if ok, why := p.Allows(time.Now(), 0); ok || !strings.Contains(why, "owner is active") {
		t.Errorf("owner input: allows %v (%s)", ok, why)
	}
	if ok, _ := p.Allows(time.Now(), 301*time.Second); !ok {
		t.Error("an idle Mac is not lent")
	}
}
