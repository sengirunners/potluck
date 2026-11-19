package runtarget_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

func refImage(hex string) string { return "ref-image@sha256:" + strings.Repeat(hex, 32) }
func refAgent(hex string) string { return "ref-agent@sha256:" + strings.Repeat(hex, 32) }

// imagesHost is a daemon with one machine-level box binding per config
// given (named by its key), over the reference driver, whose build pins
// ref-image and ref-agent at aa and bb; each binding lends them at 11 and 22.
type imagesHost struct {
	svc    *runtarget.Service
	owner  *client.Client
	team   string
	mu     sync.Mutex
	pulled []string
	fail   error  // the next pulls' answer
	during func() // run in each pull, before it answers
	home   string
}

func newImagesHost(t *testing.T, bindings map[string]map[string]string) *imagesHost {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	h := &imagesHost{owner: client.WithHandler(dm.SocketHandler()), team: ids.New(), home: home}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			h.svc = s
		}
	}
	h.svc.SetLookPath(func(string) (string, error) { return drv, nil })
	pins, err := imagepins.ParsePins("oci linux " + refImage("aa") + "\noci linux " + refAgent("bb") + " template=agent\ntart linux " + refImage("aa") + "\ntart linux " + refAgent("bb") + " template=agent")
	if err != nil {
		t.Fatal(err)
	}
	h.svc.SetImagePins(func() []imagepins.DefaultPin { return pins })
	h.svc.SetImagePull(func(_ context.Context, cli, image string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.during != nil {
			h.during()
		}
		if h.fail != nil {
			return h.fail
		}
		h.pulled = append(h.pulled, cli+" "+image)
		return nil
	})
	for name, c := range bindings {
		h.bind(t, name, []string{refImage("11"), refAgent("22")}, c)
	}
	return h
}

// bind binds (or rebinds) a machine-level box binding shared with the team, as its owner.
func (h *imagesHost) bind(t *testing.T, name string, images []string, c map[string]string) {
	t.Helper()
	if _, err := computeclient.BindRunTarget(context.Background(), h.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{h.team}, Replace: true,
		Target: computeapi.RunTarget{Name: name, Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: images, Config: c}}); err != nil {
		t.Fatal(err)
	}
}

// view is binding name as the owner's host list shows it.
func (h *imagesHost) view(t *testing.T, name string) computeapi.RunTargetView {
	t.Helper()
	l, err := computeclient.HostRunTargets(context.Background(), h.owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range l.Targets {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no binding %s in %+v", name, l)
	return computeapi.RunTargetView{}
}

var oci = map[string]string{"vm": "oci", "oci_cli": "docker", imagepins.ImageDefaultKey: "true"}

// After an update, a host's binding of Potluck's own images moves to the
// build's pins with no one running anything: the new images are pulled
// first, the binding keeps its share and config, and its view stops saying
// it is behind.
func TestHostBindingMovesToTheBuildsPins(t *testing.T) {
	h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
	before := h.view(t, "pool-c1m2")
	if b := before.ImagesBehind; b == nil || !slices.Equal(b.Lends, []string{refImage("11"), refAgent("22")}) ||
		!slices.Equal(b.Current, []string{refImage("aa"), refAgent("bb")}) || b.Why == "" {
		t.Fatalf("before the refresh: %+v", before.ImagesBehind)
	}
	h.svc.RefreshHostImages(context.Background())
	if want := []string{"docker " + refImage("aa"), "docker " + refAgent("bb")}; !slices.Equal(h.pulled, want) {
		t.Errorf("pulled %v, want %v", h.pulled, want)
	}
	after := h.view(t, "pool-c1m2")
	if after.ImagesBehind != nil || !slices.Equal(after.Images, []string{refImage("aa"), refAgent("bb")}) ||
		after.Config["oci_cli"] != "docker" || !slices.Equal(after.Shared, []string{h.team}) {
		t.Errorf("after the refresh: images %v behind %+v config %v share %v", after.Images, after.ImagesBehind, after.Config, after.Shared)
	}
}

// A binding with live boxes is never rebound under them: its view says it
// waits, and the next pass after the box ends moves it.
func TestHostBindingWaitsForItsBoxes(t *testing.T) {
	h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{"request_id": "r1", "actor": "x", "ttl_s": 60})
	res, err := h.svc.HostCall(ctx, h.team, "pool-c1m2", "acquire", "", args)
	if err != nil {
		t.Fatal(err)
	}
	var box struct {
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(res, &box); err != nil || box.Handle == "" {
		t.Fatalf("acquire: %s %v", res, err)
	}
	h.svc.RefreshHostImages(ctx)
	v := h.view(t, "pool-c1m2")
	if v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "waiting for its box to end") ||
		!slices.Equal(v.Images, []string{refImage("11"), refAgent("22")}) {
		t.Fatalf("with a live box: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	if _, err := h.svc.HostCall(ctx, h.team, "pool-c1m2", "release", box.Handle, nil); err != nil {
		t.Fatal(err)
	}
	h.svc.RefreshHostImages(ctx)
	if v := h.view(t, "pool-c1m2"); v.ImagesBehind != nil || v.Images[0] != refImage("aa") {
		t.Errorf("after the box ended: images %v behind %+v", v.Images, v.ImagesBehind)
	}
}

// A pull that fails leaves the binding lending its old pins, and says so.
func TestHostBindingKeepsItsPinsWhenThePullFails(t *testing.T) {
	h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
	h.fail = errors.New("docker pull: no space left on device")
	h.svc.RefreshHostImages(context.Background())
	v := h.view(t, "pool-c1m2")
	if v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "no space left on device") || v.Images[0] != refImage("11") {
		t.Errorf("after a failed pull: images %v behind %+v", v.Images, v.ImagesBehind)
	}
}

// The owner's own --image, and a Mac's Tart images, are never moved; only
// the Mac's view says it is behind, with what to run.
func TestHostBindingRefreshLeavesTheOwnersChoice(t *testing.T) {
	owners := map[string]string{"vm": "oci", "oci_cli": "docker", imagepins.ImageDefaultKey: "false"}
	mac := map[string]string{"vm": "tart"}
	h := newImagesHost(t, map[string]map[string]string{"mine-c1m2": owners, "mac-c1m2": mac})
	h.svc.RefreshHostImages(context.Background())
	if len(h.pulled) != 0 {
		t.Errorf("pulled %v", h.pulled)
	}
	if v := h.view(t, "mine-c1m2"); v.ImagesBehind != nil || v.Images[0] != refImage("11") {
		t.Errorf("the owner's --image: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	v := h.view(t, "mac-c1m2")
	if v.Images[0] != refImage("11") {
		t.Errorf("the Mac's binding moved: %v", v.Images)
	}
	if v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, config.PotluckCommand("host", "--yes")) {
		t.Errorf("the Mac's view: %+v", v.ImagesBehind)
	}
}

// A box starting on the binding (an acquire in flight, before the driver has
// a handle to list) holds the rebind off as a live box does.
func TestHostBindingWaitsForABoxStarting(t *testing.T) {
	h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
	leave := h.svc.StartingOn("pool-c1m2")
	h.svc.RefreshHostImages(context.Background())
	if v := h.view(t, "pool-c1m2"); v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "a box is starting") || v.Images[0] != refImage("11") {
		t.Fatalf("with an acquire in flight: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	leave()
	h.svc.RefreshHostImages(context.Background())
	if v := h.view(t, "pool-c1m2"); v.ImagesBehind != nil || v.Images[0] != refImage("aa") {
		t.Errorf("after it answered: images %v behind %+v", v.Images, v.ImagesBehind)
	}
}

// A binding from before share recorded who chose its images, lending one
// image of Potluck's repository, may be an owner's --image: it is not moved,
// and its view names the command that moves it.
func TestHostBindingFromBeforeTheKeyWithOneImageIsTheOwners(t *testing.T) {
	h := newImagesHost(t, nil)
	h.bind(t, "old-c1m2", []string{refImage("11")}, map[string]string{"vm": "oci", "oci_cli": "docker"})
	h.svc.RefreshHostImages(context.Background())
	v := h.view(t, "old-c1m2")
	if len(h.pulled) != 0 || v.Images[0] != refImage("11") {
		t.Errorf("moved: pulled %v, images %v", h.pulled, v.Images)
	}
	if v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, config.PotluckCommand("host", "--yes")) || !strings.Contains(v.ImagesBehind.Why, "image you named") {
		t.Errorf("view: %+v", v.ImagesBehind)
	}
}

// An owner's rebind while the refresh pulls stands: the refresh writes
// nothing over it.
func TestHostBindingRefreshLeavesARebindMadeMeanwhile(t *testing.T) {
	h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
	mine := map[string]string{"vm": "oci", "oci_cli": "docker", imagepins.ImageDefaultKey: "true", "warm": "0"}
	once := sync.Once{}
	h.during = func() { once.Do(func() { h.bind(t, "pool-c1m2", []string{refImage("11"), refAgent("22")}, mine) }) }
	h.svc.RefreshHostImages(context.Background())
	v := h.view(t, "pool-c1m2")
	if v.Config["warm"] != "0" || v.Images[0] != refImage("11") {
		t.Errorf("the owner's rebind was overwritten: images %v config %v", v.Images, v.Config)
	}
	if v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "changed meanwhile") {
		t.Errorf("view: %+v", v.ImagesBehind)
	}
}

// swapDriver points the binding's driver at a refdriver with bug, as an
// update of the driver would.
func (h *imagesHost) swapDriver(t *testing.T, bug string) {
	t.Helper()
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	broken, err := refdriver.Install(fix, state, bug)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	h.svc.SetLookPath(func(string) (string, error) { return broken, nil })
	if _, err := runtarget.PinDriver(h.home, []string{fix}, "ref"); err != nil {
		t.Fatal(err)
	}
}

// A driver that cannot describe the box with the new images, or describes
// a class that no longer isolates, keeps the old pins: the binding is never
// written into a state no bind would accept (isolation fails closed).
func TestHostBindingKeepsItsPinsWhenTheDriverRefuses(t *testing.T) {
	for bug, why := range map[string]string{"box-no-size": "driver did not take the new images", "box-credentialed": "with the new images"} {
		t.Run(bug, func(t *testing.T) {
			h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
			h.swapDriver(t, bug)
			h.svc.RefreshHostImages(context.Background())
			l, err := computeclient.HostRunTargets(context.Background(), h.owner)
			if err != nil || len(l.Targets) != 1 {
				t.Fatalf("list: %+v %v", l, err)
			}
			if v := l.Targets[0]; v.Images[0] != refImage("11") || v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, why) {
				t.Errorf("after a refused describe: images %v behind %+v", v.Images, v.ImagesBehind)
			}
		})
	}
}

// An acquire that comes in and finishes while the refresh lists the live
// handles holds the rebind off: the list may not have seen its box.
func TestHostBindingWaitsForAnAcquireDuringTheList(t *testing.T) {
	h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
	h.svc.AfterListed(func() { h.svc.StartingOn("pool-c1m2")() })
	h.svc.RefreshHostImages(context.Background())
	if v := h.view(t, "pool-c1m2"); v.Images[0] != refImage("11") || v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "starting") {
		t.Errorf("rebound past an acquire: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	h.svc.AfterListed(nil)
	h.svc.RefreshHostImages(context.Background())
	if v := h.view(t, "pool-c1m2"); v.Images[0] != refImage("aa") {
		t.Errorf("not rebound once idle: %v", v.Images)
	}
}

// The heartbeat right after a rebind advertises the new pins: the size is
// not left out of a beat, and the pool keys new boxes by the new digest.
func TestHostBindingAdvertsTheNewPinsAtOnce(t *testing.T) {
	h := newImagesHost(t, map[string]map[string]string{"pool-c1m2": oci})
	ctx := context.Background()
	h.svc.RefreshHostImages(ctx)
	ads := h.svc.HostAdverts(ctx, h.team)
	if len(ads) != 1 || !slices.Equal(ads[0].Describe.Images, []string{refImage("aa"), refAgent("bb")}) {
		t.Errorf("adverts after the rebind: %+v", ads)
	}
}
