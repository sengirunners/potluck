package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// tartOnly answers tart list with images and deletes them, failing for
// refuse.
type tartOnly struct {
	images  []string
	refuse  string
	deleted []string
}

func (f *tartOnly) Run(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
	switch strings.Join(args, " ") {
	case "list --format json":
		var es []string
		for _, im := range f.images {
			es = append(es, fmt.Sprintf(`{"Source":"OCI","Name":%q,"Size":31,"Running":false}`, im))
		}
		return hostprobe.Result{Stdout: "[" + strings.Join(es, ",") + "]"}, nil
	}
	if len(args) == 2 && args[0] == "delete" {
		f.deleted = append(f.deleted, args[1])
		if args[1] == f.refuse {
			return hostprobe.Result{Exit: 1, Stderr: "Error: permission denied"}, nil
		}
		for i, im := range f.images {
			if im == args[1] {
				f.images = append(f.images[:i], f.images[i+1:]...)
			}
		}
		return hostprobe.Result{}, nil
	}
	return hostprobe.Result{}, errors.New("unexpected " + name + " " + strings.Join(args, " "))
}

// imagesDaemon is a daemon on home's socket answering the binding lists
// host images reads: machine-level bindings, the pools this machine runs,
// and each pool's list (a pool in fail answers 503).
type imagesDaemon struct {
	host  []computeapi.RunTargetView
	pools map[string][]computeapi.RunTargetView
	fail  map[string]bool
}

func (d *imagesDaemon) serve(t *testing.T, sock string) {
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.WireProtocol)
		w.Header().Set("Content-Type", "application/json")
		var v any
		switch pool := api.GetHeader(r.Header, api.HeaderProject); {
		case r.URL.Path == "/v0/health":
			v = api.Health{OK: true, Protocol: api.WireProtocol, Machine: "mini", PID: os.Getpid()}
		case r.URL.Path == "/v0/projects" && r.URL.Query().Get("local") == "1":
			var ps []api.Project
			for _, p := range slices.Sorted(maps.Keys(d.pools)) {
				ps = append(ps, api.Project{Slug: p})
			}
			v = api.ProjectList{Projects: ps}
		case r.URL.Path == "/v0/run_targets" && r.URL.Query().Get("host") == "1":
			v = computeapi.RunTargetList{Targets: d.host}
		case r.URL.Path == "/v0/run_targets" && d.fail[pool]:
			w.WriteHeader(http.StatusServiceUnavailable)
			v = api.Error{Code: api.CodeUnavailable, Message: "pool db locked"}
		case r.URL.Path == "/v0/run_targets" && d.pools[pool] != nil:
			v = computeapi.RunTargetList{Project: pool, Targets: d.pools[pool]}
		default:
			t.Errorf("unexpected call %s %s (pool %q)", r.Method, r.URL, pool)
			w.WriteHeader(http.StatusNotFound)
			v = api.Error{Code: api.CodeNotFound, Message: "no"}
		}
		json.NewEncoder(w).Encode(v)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// host images reads what this machine shares in every pool it runs and at
// machine level, without starting the daemon, and keeps every image when
// any of those lists cannot be read (the daemon down, or one pool's list
// failing). A run binding's image, in a pool other than the first, is in
// use. Another program's daemon built on Potluck (its machine record) is
// read too: its sizes' images are in use, and an image a share on its home
// used is kept while that daemon is not running. It lists by state and names the removal command through the
// program's prefix; --remove deletes only the removable image, says so with
// the free disk before and after, and a refused delete exits non-zero
// naming it; a second removal removes nothing.
func TestHostImages(t *testing.T) {
	tahoe, err := hostprobe.DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	older := "ghcr.io/cirruslabs/macos-sequoia-base@sha256:" + strings.Repeat("0", 64)
	home, err := os.MkdirTemp("", "pl") // a short path: the socket's name has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv("HOME", home)
	tartDir := filepath.Join(home, ".tart")
	if err := os.Mkdir(tartDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An earlier share here pulled both; the latest uses neither.
	at := time.Now().Add(-time.Hour)
	share := func(home string, start time.Time, images []string, pulled bool) {
		t.Helper()
		if err := hostprobe.StartTartShare(tartDir, home, images, start); err != nil {
			t.Fatal(err)
		}
		for _, im := range images {
			if pulled {
				if err := hostprobe.RecordTartPull(tartDir, im, home, start, start); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	share(home, at, []string{tahoe.Image, older}, true)
	share(home, at.Add(time.Minute), nil, false)
	// zm's daemon, on its own home, also shared tahoe.
	zmHome := filepath.Join(home, "zm")
	records := filepath.Join(home, "daemons")
	t.Setenv(daemonrecord.EnvDir, records)
	if err := os.Mkdir(zmHome, 0o700); err != nil {
		t.Fatal(err)
	}
	share(zmHome, at, []string{tahoe.Image}, false)
	share(zmHome, at.Add(time.Minute), nil, false)
	f := &tartOnly{images: []string{tahoe.Image, older}}
	free := 40.0
	oldDetect, oldRunner, oldFree := detectHost, hostImagesRunner, hostImagesFreeGB
	t.Cleanup(func() { detectHost, hostImagesRunner, hostImagesFreeGB = oldDetect, oldRunner, oldFree })
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", Arch: "arm64", MacOS: "15.6", TartDir: tartDir, TartFreeGB: free,
			Layers: []hostprobe.Layer{{Name: "tart", Available: true, CLI: "/fake/tart"}}}
	}
	hostImagesRunner = f
	hostImagesFreeGB = func(string) (float64, error) { return free, nil }
	run := func(args ...string) (string, int) {
		t.Helper()
		var out bytes.Buffer
		code := Run(context.Background(), append([]string{"host", "images"}, args...), strings.NewReader(""), &out, &out)
		return out.String(), code
	}
	has := func(out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("no %q in:\n%s", w, out)
			}
		}
	}

	// No daemon: everything kept, and none started.
	out, code := run()
	has(out, "is "+programName+" daemon running?", "Nothing is removable.")
	if code != 0 || strings.Contains(out, "removable:") {
		t.Fatalf("no daemon: exit %d:\n%s", code, out)
	}
	if out, code = run("--remove"); code != 0 || len(f.deleted) != 0 {
		t.Fatalf("no daemon: --remove exit %d, deleted %v:\n%s", code, f.deleted, out)
	}
	if _, err := os.Stat(filepath.Join(home, SocketName)); err == nil {
		t.Fatal("host images started the daemon")
	}

	d := &imagesDaemon{
		host: []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "mac-c2m4", Kind: "box", Images: []string{"ghcr.io/cirruslabs/macos-sequoia-base@sha256:" + strings.Repeat("9", 64)}}}},
		pools: map[string][]computeapi.RunTargetView{
			"p": {{RunTarget: computeapi.RunTarget{Name: "far@m2", Kind: "run", HostBinding: "far", Config: map[string]string{"image": tahoe.Image}}, Host: "m2"}}, // another machine's
			"q": {{RunTarget: computeapi.RunTarget{Name: "q-runner", Kind: "run", Config: map[string]string{"image": older}}}},
		},
		fail: map[string]bool{"q": true},
	}
	d.serve(t, filepath.Join(home, SocketName))

	// One pool's list fails: everything kept.
	out, code = run()
	has(out, "pool q", "Nothing is removable.")
	if code != 0 || strings.Contains(out, "removable:") {
		t.Fatalf("pool q unread: exit %d:\n%s", code, out)
	}

	// zm's record is there but its daemon does not answer: tahoe, which a
	// share on zm's home used, is kept.
	zmSock := filepath.Join(zmHome, "zm.sock")
	if _, err := daemonrecord.Write(context.Background(), records, daemonrecord.Record{Program: "zm", Home: zmHome, Socket: zmSock,
		PID: os.Getpid(), Protocol: api.WireProtocol, Modules: []string{rt.Name}}); err != nil { // bindings, and none of compute's other modules
		t.Fatal(err)
	}
	d.fail = nil
	out, code = run()
	has(out, "retained:\n  "+tahoe.Image, zmHome+", whose Potluck daemon is not running", "Nothing is removable.")
	if code != 0 {
		t.Fatalf("zm down: exit %d:\n%s", code, out)
	}

	// zm's daemon answers, and one of its sizes lends tahoe: in use.
	zm := &imagesDaemon{pools: map[string][]computeapi.RunTargetView{
		"z": {{RunTarget: computeapi.RunTarget{Name: "z-mac", Kind: "box", Images: []string{tahoe.Image}}}},
	}}
	zm.serve(t, zmSock)
	out, code = run()
	has(out, "\n  "+tahoe.Image+", 31 GB on disk: lent by z-mac", "Nothing is removable.")
	if code != 0 {
		t.Fatalf("zm lends tahoe: exit %d:\n%s", code, out)
	}

	zm.pools["z"] = []computeapi.RunTargetView{}
	out, code = run()
	has(out, "removable:\n  "+tahoe.Image+", 31 GB on disk: ", "in use:\n  "+older+", 31 GB on disk: lent by q-runner", "frees up to 31 GB on disk", programName+" host images --remove")
	if code != 0 || len(f.deleted) != 0 {
		t.Fatalf("listing: exit %d, deleted %v:\n%s", code, f.deleted, out)
	}

	f.refuse = tahoe.Image
	out, code = run("--remove")
	has(out, "NOT removed "+tahoe.Image+": tart delete failed: exit 1: Error: permission denied")
	if code == 0 {
		t.Fatalf("refused delete: exit 0:\n%s", out)
	}

	f.refuse, f.deleted = "", nil
	hostImagesFreeGB = func(string) (float64, error) { free += 31; return free - 31, nil }
	out, code = run("--remove")
	has(out, "removed "+tahoe.Image+" (31 GB on disk)", "GB before")
	if code != 0 || !slices.Equal(f.deleted, []string{tahoe.Image}) {
		t.Fatalf("removal: exit %d, deleted %v:\n%s", code, f.deleted, out)
	}
	out, code = run("--remove")
	has(out, "Nothing to remove")
	if code != 0 || len(f.deleted) != 1 {
		t.Fatalf("second removal: exit %d, deleted %v:\n%s", code, f.deleted, out)
	}

	// An image Potluck has no record of pulling (one pulled before the
	// record existed) that nothing needs: listed as unclaimed with the exact
	// command; --remove alone keeps it, naming it removes it, and naming an
	// image a size lends, or naming without --remove, deletes nothing.
	unclaimed := "ghcr.io/cirruslabs/macos-sequoia-base@sha256:" + strings.Repeat("7", 64)
	f.images, f.deleted = append(f.images, unclaimed), nil
	out, code = run()
	has(out, "retained:\n  "+unclaimed, "Nothing is removable.", "Unclaimed (31 GB on disk)", programName+" host images --remove "+unclaimed+"\n")
	if code != 0 {
		t.Fatalf("unclaimed: exit %d:\n%s", code, out)
	}
	for _, args := range [][]string{{"--remove"}, {"--remove", older}, {"--remove", unclaimed, older}, {unclaimed}} {
		out, code = run(args...)
		if len(f.deleted) != 0 || (len(args) > 1 || args[0] != "--remove") == (code == 0) {
			t.Fatalf("%v: exit %d, deleted %v:\n%s", args, code, f.deleted, out)
		}
	}
	has(out, "image names go with --remove")
	out, code = run("--remove", unclaimed)
	has(out, "removed "+unclaimed+" (31 GB on disk)")
	if code != 0 || !slices.Equal(f.deleted, []string{unclaimed}) {
		t.Fatalf("naming the unclaimed image: exit %d, deleted %v:\n%s", code, f.deleted, out)
	}
}

// A share of this Mac (potluck host, through the command tree) writes
// Potluck's record of Tart's images under its daemon's home: the images it
// uses from its start, and a claim on the one it pulled.
func TestHostRecordsItsTartPulls(t *testing.T) {
	m := soloMachine(t, "")
	m.ok(t, "create", "friends")
	tartDir := t.TempDir()
	var pulled []string
	shareRunner = runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		switch {
		case name == "/fake/tart" && strings.Join(args, " ") == "list --format json":
			return hostprobe.Result{Stdout: "[]"}, nil
		case name == "/fake/tart" && len(args) > 0 && args[0] == "pull":
			pulled = append(pulled, args[len(args)-1])
			return hostprobe.Result{}, nil
		}
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 500, TartDir: tartDir, TartFreeGB: 500,
			Layers: []hostprobe.Layer{{Name: "tart", Available: true, Supported: true, CLI: "/fake/tart"}}}
	}
	// The fixture's driver lends no box of the image, so the share may fail
	// after its pull: only the record is checked.
	m.run(t, "host", "friends", "--with", hostprobe.SmallLinux, "--root=false", "--sizes", "c1m2", "--reclaim-idle-s", "0", "--no-sudo")
	if len(pulled) != 1 || !strings.Contains(pulled[0], "@sha256:") {
		t.Fatalf("the share pulled %v, not one pinned image", pulled)
	}
	b, err := os.ReadFile(filepath.Join(tartDir, hostprobe.TartPullsFile))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Images map[string]hostprobe.TartUse `json:"images"`
		Shares map[string]time.Time         `json:"shares"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	use := rec.Images[pulled[0]]
	if use.Pulled.IsZero() || use.Homes[m.home].IsZero() || !rec.Shares[m.home].Equal(use.Homes[m.home]) {
		t.Fatalf("record after a share on %s:\n%s", m.home, b)
	}
}

// A home the record names that is gone for good keeps every image until
// host images --forget-home takes it out, which the listing names. It
// forgets only a home that does not exist, that no running daemon uses and
// that the record names, refusing anything else with the record unchanged;
// it deletes nothing, says what it forgot, and the image only that home
// used is then judged by the homes still here.
func TestHostImagesForgetHome(t *testing.T) {
	tahoe, err := hostprobe.DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("", "pl") // a short path: the socket's name has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv("HOME", home)
	records := filepath.Join(home, "daemons")
	t.Setenv(daemonrecord.EnvDir, records)
	tartDir := filepath.Join(home, ".tart")
	gone := filepath.Join(home, "gone")
	for _, d := range []string{tartDir, gone} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-time.Hour)
	if err := hostprobe.StartTartShare(tartDir, home, nil, at); err != nil {
		t.Fatal(err)
	}
	if err := hostprobe.StartTartShare(tartDir, gone, []string{tahoe.Image}, at); err != nil {
		t.Fatal(err)
	}
	if err := hostprobe.RecordTartPull(tartDir, tahoe.Image, gone, at, at); err != nil {
		t.Fatal(err)
	}
	if err := hostprobe.StartTartShare(tartDir, gone, nil, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	f := &tartOnly{images: []string{tahoe.Image}}
	oldDetect, oldRunner := detectHost, hostImagesRunner
	t.Cleanup(func() { detectHost, hostImagesRunner = oldDetect, oldRunner })
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", Arch: "arm64", MacOS: "15.6", TartDir: tartDir, TartFreeGB: 40,
			Layers: []hostprobe.Layer{{Name: "tart", Available: true, CLI: "/fake/tart"}}}
	}
	hostImagesRunner = f
	(&imagesDaemon{}).serve(t, filepath.Join(home, SocketName))
	run := func(args ...string) (string, int) {
		t.Helper()
		var out bytes.Buffer
		code := Run(context.Background(), append([]string{"host", "images"}, args...), strings.NewReader(""), &out, &out)
		return out.String(), code
	}
	record := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(tartDir, hostprobe.TartPullsFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	refused := func(why string, args ...string) {
		t.Helper()
		before := record()
		out, code := run(args...)
		if code == 0 || !strings.Contains(out, why) || record() != before || len(f.deleted) != 0 {
			t.Fatalf("%v: exit %d, deleted %v, want refused with %q and the record unchanged:\n%s", args, code, f.deleted, why, out)
		}
	}

	// Still there: never forgotten.
	refused("exists: a home that is still here is never forgotten", "--forget-home", gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	out, code := run()
	if code != 0 || !strings.Contains(out, "which no longer exists here") || !strings.Contains(out, programName+" host images --forget-home "+gone) {
		t.Fatalf("gone home: exit %d, want the forget command named:\n%s", code, out)
	}
	refused("does not name", "--forget-home", filepath.Join(home, "never"))
	refused("goes alone", "--forget-home", gone, "--remove")

	// A running daemon uses it (its record answers): not gone.
	sock := filepath.Join(home, "other.sock")
	(&imagesDaemon{}).serve(t, sock)
	if _, err := daemonrecord.Write(context.Background(), records, daemonrecord.Record{Program: "zm", Home: gone, Socket: sock,
		PID: os.Getpid(), Protocol: api.WireProtocol}); err != nil {
		t.Fatal(err)
	}
	refused("a daemon runs on", "--forget-home", gone)
	if err := os.RemoveAll(records); err != nil {
		t.Fatal(err)
	}

	out, code = run("--forget-home", gone)
	for _, want := range []string{"forgot " + gone + ", whose latest share started", "its mark is off: " + tahoe.Image, "Nothing was deleted."} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if code != 0 || len(f.deleted) != 0 || strings.Contains(record(), gone) {
		t.Fatalf("forget: exit %d, deleted %v, record:\n%s\n%s", code, f.deleted, record(), out)
	}
	if out, _ := run(); !strings.Contains(out, "removable:\n  "+tahoe.Image) {
		t.Fatalf("after forgetting, the image only the gone home used is removable:\n%s", out)
	}
	refused("does not name", "--forget-home", gone)
}
