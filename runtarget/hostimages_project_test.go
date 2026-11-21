package runtarget_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// projectImages is imagesHost with a project "acme" whose owner binds boxes
// in its project.toml, as a host with no pool does.
type projectImages struct {
	*imagesHost
	pid string
	in  *client.Client // the owner, in the project
}

func newProjectImages(t *testing.T) *projectImages {
	t.Helper()
	h := newImagesHost(t, nil)
	in := *h.owner
	in.Project, in.ProjectFrom = "acme", "flag"
	p, err := in.CreateProject(context.Background(), api.NewProjectRequest{Slug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	// The refresh waits for the host file's migration, which Start runs in a
	// daemon; a host call runs it here.
	if _, err := computeclient.HostRunTargets(context.Background(), h.owner); err != nil {
		t.Fatal(err)
	}
	return &projectImages{imagesHost: h, pid: p.ID, in: &in}
}

// bindIn binds (or rebinds) a box in the project, lending images.
func (p *projectImages) bindIn(t *testing.T, name string, images []string, c map[string]string) {
	t.Helper()
	if _, err := computeclient.BindRunTarget(context.Background(), p.in, computeapi.BindRunTargetRequest{Replace: true,
		Target: computeapi.RunTarget{Name: name, Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: images, Config: c}}); err != nil {
		t.Fatal(err)
	}
}

// viewIn is the project's binding name as its owner's list shows it.
func (p *projectImages) viewIn(t *testing.T, name string) computeapi.RunTargetView {
	t.Helper()
	l, err := computeclient.RunTargets(context.Background(), p.in, false)
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

// A project-level binding of Potluck's own images moves to the build's pins
// as a machine-level one does: pulled first, its config kept, its view
// stops saying it is behind.
func TestProjectBindingMovesToTheBuildsPins(t *testing.T) {
	p := newProjectImages(t)
	p.bindIn(t, "mini-c1m2", []string{refImage("11"), refAgent("22")}, oci)
	if b := p.viewIn(t, "mini-c1m2").ImagesBehind; b == nil || !slices.Equal(b.Current, []string{refImage("aa"), refAgent("bb")}) || b.Why == "" {
		t.Fatalf("before the refresh: %+v", b)
	}
	p.svc.RefreshHostImages(context.Background())
	if want := []string{"docker " + refImage("aa"), "docker " + refAgent("bb")}; !slices.Equal(p.pulled, want) {
		t.Errorf("pulled %v, want %v", p.pulled, want)
	}
	v := p.viewIn(t, "mini-c1m2")
	if v.ImagesBehind != nil || !slices.Equal(v.Images, []string{refImage("aa"), refAgent("bb")}) || v.Config["oci_cli"] != "docker" || v.Isolation != "ephemeral" {
		t.Errorf("after the refresh: images %v behind %+v config %v isolation %q", v.Images, v.ImagesBehind, v.Config, v.Isolation)
	}
}

// A project-level binding with a live box waits for it, as a machine-level
// one does, and moves on the next pass after the box ends.
func TestProjectBindingWaitsForItsBoxes(t *testing.T) {
	p := newProjectImages(t)
	ctx := context.Background()
	p.bindIn(t, "mini-c1m2", []string{refPin}, oci) // the ref driver boots refPin
	box, err := computeclient.AcquireRunTarget(ctx, p.in, "mini-c1m2", computeapi.AcquireRunTargetRequest{RequestID: "r1", TTLS: 60})
	if err != nil {
		t.Fatal(err)
	}
	p.svc.RefreshHostImages(ctx)
	v := p.viewIn(t, "mini-c1m2")
	if v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "waiting for its box to end") || v.Images[0] != refPin {
		t.Fatalf("with a live box: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, p.in, "mini-c1m2", strings.TrimPrefix(box.Handle, "box:mini-c1m2/")); err != nil {
		t.Fatal(err)
	}
	p.svc.RefreshHostImages(ctx)
	if v := p.viewIn(t, "mini-c1m2"); v.ImagesBehind != nil || v.Images[0] != refImage("aa") {
		t.Errorf("after the box ended: images %v behind %+v", v.Images, v.ImagesBehind)
	}
}

// An acquire in flight holds a project binding's rebind off, and only that
// binding's: a machine-level binding of the same name, and the same name in
// another project, are other bindings.
func TestProjectBindingWaitsForABoxStarting(t *testing.T) {
	p := newProjectImages(t)
	ctx := context.Background()
	p.bindIn(t, "mini-c1m2", []string{refImage("11"), refAgent("22")}, oci)
	p.bind(t, "mini-c1m2", []string{refImage("11"), refAgent("22")}, oci) // machine-level, same name
	leaveHost := p.svc.StartingOn("mini-c1m2")
	leaveOther := p.svc.StartingOnProject("another-project-id", "mini-c1m2")
	p.svc.RefreshHostImages(ctx)
	if v := p.viewIn(t, "mini-c1m2"); v.ImagesBehind != nil || v.Images[0] != refImage("aa") {
		t.Errorf("held by another binding's acquire: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	if v := p.view(t, "mini-c1m2"); v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "a box is starting") {
		t.Errorf("the machine-level binding's own acquire did not hold it: %+v", v.ImagesBehind)
	}
	leaveHost()
	leaveOther()

	p.bindIn(t, "next-c1m2", []string{refImage("11"), refAgent("22")}, oci)
	leave := p.svc.StartingOnProject(p.pid, "next-c1m2")
	p.svc.RefreshHostImages(ctx)
	if v := p.viewIn(t, "next-c1m2"); v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "a box is starting") || v.Images[0] != refImage("11") {
		t.Fatalf("with an acquire in flight: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	leave()
	p.svc.RefreshHostImages(ctx)
	if v := p.viewIn(t, "next-c1m2"); v.ImagesBehind != nil || v.Images[0] != refImage("aa") {
		t.Errorf("after it answered: images %v behind %+v", v.Images, v.ImagesBehind)
	}
}

// A project acquire counts itself in: one that comes in and finishes while
// the refresh lists the live handles holds the rebind off, since the list
// may not have seen its box.
func TestProjectBindingWaitsForAnAcquireDuringTheList(t *testing.T) {
	p := newProjectImages(t)
	ctx := context.Background()
	p.bindIn(t, "mini-c1m2", []string{refPin}, oci) // the ref driver boots refPin
	p.svc.AfterListed(func() {
		box, err := computeclient.AcquireRunTarget(ctx, p.in, "mini-c1m2", computeapi.AcquireRunTargetRequest{RequestID: "r1", TTLS: 60})
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := computeclient.ReleaseRunTarget(ctx, p.in, "mini-c1m2", strings.TrimPrefix(box.Handle, "box:mini-c1m2/")); err != nil {
			t.Error(err)
		}
	})
	p.svc.RefreshHostImages(ctx)
	if v := p.viewIn(t, "mini-c1m2"); v.Images[0] != refPin || v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "starting") {
		t.Errorf("rebound past an acquire: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	p.svc.AfterListed(nil)
	p.svc.RefreshHostImages(ctx)
	if v := p.viewIn(t, "mini-c1m2"); v.Images[0] != refImage("aa") {
		t.Errorf("not rebound once idle: %v", v.Images)
	}
}

// An owner's own --image, and a Mac's Tart images, are never moved at
// project level either; the Mac's view says what to run.
func TestProjectBindingRefreshLeavesTheOwnersChoice(t *testing.T) {
	p := newProjectImages(t)
	p.bindIn(t, "mine-c1m2", []string{refImage("11"), refAgent("22")}, map[string]string{"vm": "oci", "oci_cli": "docker", imagepins.ImageDefaultKey: "false"})
	p.bindIn(t, "mac-c1m2", []string{refImage("11"), refAgent("22")}, map[string]string{"vm": "tart", imagepins.ImageDefaultKey: "true"})
	p.svc.RefreshHostImages(context.Background())
	if len(p.pulled) != 0 {
		t.Errorf("pulled %v", p.pulled)
	}
	if v := p.viewIn(t, "mine-c1m2"); v.ImagesBehind != nil || v.Images[0] != refImage("11") {
		t.Errorf("the owner's --image: images %v behind %+v", v.Images, v.ImagesBehind)
	}
	v := p.viewIn(t, "mac-c1m2")
	if v.Images[0] != refImage("11") || v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "tens of GB") {
		t.Errorf("the Mac's binding: images %v behind %+v", v.Images, v.ImagesBehind)
	}
}

// An owner's rebind of a project binding while the refresh pulls stands.
func TestProjectBindingRefreshLeavesARebindMadeMeanwhile(t *testing.T) {
	p := newProjectImages(t)
	p.bindIn(t, "mini-c1m2", []string{refImage("11"), refAgent("22")}, oci)
	mine := map[string]string{"vm": "oci", "oci_cli": "docker", imagepins.ImageDefaultKey: "true", "warm": "0"}
	once := sync.Once{}
	p.during = func() { once.Do(func() { p.bindIn(t, "mini-c1m2", []string{refImage("11"), refAgent("22")}, mine) }) }
	p.svc.RefreshHostImages(context.Background())
	v := p.viewIn(t, "mini-c1m2")
	if v.Config["warm"] != "0" || v.Images[0] != refImage("11") {
		t.Errorf("the owner's rebind was overwritten: images %v config %v", v.Images, v.Config)
	}
	if v.ImagesBehind == nil || !strings.Contains(v.ImagesBehind.Why, "changed meanwhile") {
		t.Errorf("view: %+v", v.ImagesBehind)
	}
}
