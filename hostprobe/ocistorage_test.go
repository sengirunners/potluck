package hostprobe

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

func ptr(n int) *int { return &n }

func storageOptions() ShareOptions {
	return ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c2m4", "c4m16"}, Driver: "ref"}
}

// ocibinding is an earlier oci size binding of the engine docker.
func ocibinding(name string, keys map[string]string) computeapi.RunTarget {
	cfg := map[string]string{"vm": "oci", "oci_cli": "docker"}
	for k, v := range keys {
		cfg[k] = v
	}
	return computeapi.RunTarget{Name: name, Kind: "box", Config: cfg}
}

// --shelf-gb, --shelf-fill and --volume-max-gb bind the same values on every
// size, and go to every size's conformance run too (it tests what is bound).
func TestShareStorageFlagsBindEverySize(t *testing.T) {
	o := storageOptions()
	o.ShelfGB, o.ShelfFill, o.VolumeMaxGB = ptr(5), "boxes", ptr(40)
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if line == "docker volume inspect potluck-cache" {
			return Result{Exit: 1, Stderr: "no such volume"}, true
		}
		return Result{Stdout: "PASS"}, true
	}}
	var bound []computeapi.RunTarget
	b := binderFunc(func(_ context.Context, r computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		bound = append(bound, r.Target)
		return computeapi.RunTargetView{}, nil
	})
	res, err := shareOn(context.Background(), run, b, o)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"shelf_max_gb": "5", "shelf_fill": "boxes", "volume_max_gb": "40"}
	if len(bound) != 2 {
		t.Fatalf("bound %d sizes, want 2", len(bound))
	}
	for _, tg := range bound {
		for k, v := range want {
			if tg.Config[k] != v {
				t.Errorf("%s: %s=%q, want %q", tg.Name, k, tg.Config[k], v)
			}
		}
	}
	for _, s := range res.Sizes {
		args := strings.Join(s.Conformance, " ")
		for k, v := range want {
			if !strings.Contains(args, "--config "+k+"="+v) {
				t.Errorf("%s conformance has no %s: %s", s.Binding, k, args)
			}
		}
	}
	// The volume is made once, before conformance, when the engine has none.
	var made, inspected, firstConformance = -1, -1, -1
	for i, c := range run.calls {
		switch {
		case c == "docker volume create potluck-cache":
			made = i
		case c == "docker volume inspect potluck-cache":
			inspected = i
		case strings.HasPrefix(c, "zm-run-conformance ") && firstConformance < 0:
			firstConformance = i
		}
	}
	if inspected < 0 || made < inspected || firstConformance < made {
		t.Errorf("calls (inspect %d, create %d, first conformance %d): %v", inspected, made, firstConformance, run.calls)
	}
}

// An existing volume is left as it is; no shelf (0) makes none.
func TestShareStorageVolume(t *testing.T) {
	for _, c := range []struct {
		name     string
		gb       int
		exists   bool
		wantMake bool
	}{{"missing", 5, false, true}, {"there", 5, true, false}, {"filling off", 0, false, false}} {
		o := storageOptions()
		o.ShelfGB = ptr(c.gb)
		run := &fakeRunner{on: func(line string) (Result, bool) {
			if line == "docker volume inspect potluck-cache" && !c.exists {
				return Result{Exit: 1, Stderr: "no such volume"}, true
			}
			return Result{Stdout: "PASS"}, true
		}}
		if _, err := checkOn(context.Background(), run, o); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if made := slices.Contains(run.calls, "docker volume create potluck-cache"); made != c.wantMake {
			t.Errorf("%s: volume made %t, want %t (%v)", c.name, made, c.wantMake, run.calls)
		}
	}
	o := storageOptions()
	o.ShelfGB = ptr(5)
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if strings.HasPrefix(line, "docker volume") {
			return Result{Exit: 1, Stderr: "daemon down"}, true
		}
		return Result{Stdout: "PASS"}, true
	}}
	if _, err := checkOn(context.Background(), run, o); err == nil || !strings.Contains(err.Error(), "make the shelf volume") {
		t.Errorf("a volume that cannot be made: %v", err)
	}
}

// A dry run plans and makes nothing.
func TestShareStorageDryRun(t *testing.T) {
	o := storageOptions()
	o.DryRun, o.ShelfGB = true, ptr(5)
	run := &fakeRunner{}
	res, err := shareOn(context.Background(), run, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Storage == nil || !slices.Equal(res.Storage.Volume, []string{"docker", "volume", "create", "potluck-cache"}) || len(run.calls) != 0 {
		t.Errorf("%+v %v", res.Storage, run.calls)
	}
}

func TestOCIStorage(t *testing.T) {
	sizes := []Size{{Name: "c2m4"}, {Name: "c4m16"}}
	eachSize := func(keys map[string]string) []computeapi.RunTarget {
		return []computeapi.RunTarget{ocibinding("h-c2m4", keys), ocibinding("h-c4m16", keys)}
	}
	cases := []struct {
		name    string
		mut     func(*ShareOptions)
		earlier []computeapi.RunTarget
		layer   string
		want    map[string]string // nil: no storage
		from    string
		err     string
	}{
		{name: "nothing asked, nothing earlier"},
		{name: "default shelf fill is not boxes", mut: func(o *ShareOptions) { o.ShelfGB = ptr(5) }, want: map[string]string{"shelf_max_gb": "5"}},
		{name: "off", mut: func(o *ShareOptions) { o.ShelfGB = ptr(0) }, want: map[string]string{"shelf_max_gb": "0"}},
		{name: "volume cap alone", mut: func(o *ShareOptions) { o.VolumeMaxGB = ptr(40) }, want: map[string]string{"volume_max_gb": "40"}},
		{name: "boxes needs a shelf", mut: func(o *ShareOptions) { o.ShelfFill = "boxes" }, err: "--shelf-fill boxes: the shelf is off"},
		{name: "boxes with a shelf off", mut: func(o *ShareOptions) { o.ShelfFill, o.ShelfGB = "boxes", ptr(0) }, err: "the shelf is off"},
		{name: "off needs no shelf", mut: func(o *ShareOptions) { o.ShelfFill = "off" }, want: map[string]string{"shelf_fill": "off"}},
		{name: "boxes over a kept shelf", mut: func(o *ShareOptions) { o.ShelfFill = "boxes" }, earlier: eachSize(map[string]string{"shelf_max_gb": "5"}),
			want: map[string]string{"shelf_max_gb": "5", "shelf_fill": "boxes"}, from: "h-c2m4"},
		{name: "kept on a re-run", earlier: eachSize(map[string]string{"shelf_max_gb": "5", "shelf_fill": "seed"}),
			want: map[string]string{"shelf_max_gb": "5", "shelf_fill": "seed"}, from: "h-c2m4"},
		{name: "a flag replaces its own key only", mut: func(o *ShareOptions) { o.ShelfGB = ptr(9) }, earlier: eachSize(map[string]string{"shelf_max_gb": "5", "shelf_fill": "boxes"}),
			want: map[string]string{"shelf_max_gb": "9", "shelf_fill": "boxes"}, from: "h-c2m4"},
		{name: "earlier sizes differ", earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"shelf_max_gb": "5"}), ocibinding("h-c4m16", map[string]string{"shelf_max_gb": "9"})},
			err: "differ in shelf_max_gb (5 and 9): pass --shelf-gb"},
		{name: "one size has none, one has a shelf", earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"shelf_max_gb": "5"}), ocibinding("h-c4m16", nil)},
			err: "differ in shelf_max_gb (5 and 0)"},
		{name: "a size left out of this run differs", mut: func(o *ShareOptions) { o.ShelfGB = ptr(9) },
			earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"shelf_max_gb": "9"}), ocibinding("h-c4m16", map[string]string{"shelf_max_gb": "9"}), ocibinding("h-c8m32", map[string]string{"shelf_max_gb": "5"})},
			err:     "binding h-c8m32 is not in this run and has shelf_max_gb=5"},
		{name: "a size left out that already agrees", mut: func(o *ShareOptions) { o.ShelfGB = ptr(9) },
			earlier: []computeapi.RunTarget{ocibinding("h-c8m32", map[string]string{"shelf_max_gb": "9"})}, want: map[string]string{"shelf_max_gb": "9"}},
		{name: "a size left out, default against default", mut: func(o *ShareOptions) { o.ShelfGB = ptr(0) },
			earlier: []computeapi.RunTarget{ocibinding("h-c8m32", nil)}, want: map[string]string{"shelf_max_gb": "0"}},
		{name: "box fill is not enabled on a size left out", mut: func(o *ShareOptions) { o.ShelfFill = "boxes" },
			earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"shelf_max_gb": "5"}), ocibinding("h-c4m16", map[string]string{"shelf_max_gb": "5"}), ocibinding("h-c8m32", map[string]string{"shelf_max_gb": "5"})},
			err:     "binding h-c8m32 is not in this run and has shelf_fill=seed"},
		{name: "another engine is not this engine's", mut: func(o *ShareOptions) { o.ShelfGB = ptr(9) },
			earlier: []computeapi.RunTarget{{Name: "h-c8m32", Kind: "box", Config: map[string]string{"vm": "oci", "oci_cli": "podman", "shelf_max_gb": "5"}}},
			want:    map[string]string{"shelf_max_gb": "9"}},
		{name: "another machine's binding is not seen", mut: func(o *ShareOptions) { o.ShelfGB = ptr(9) },
			earlier: []computeapi.RunTarget{{Name: "h-c8m32@far", Kind: "box", Host: "far", Config: map[string]string{"vm": "oci", "oci_cli": "docker", "shelf_max_gb": "5"}}},
			want:    map[string]string{"shelf_max_gb": "9"}},
		{name: "volume cap: another prefix on the engine keeps its own", mut: func(o *ShareOptions) { o.VolumeMaxGB = ptr(40) },
			earlier: []computeapi.RunTarget{ocibinding("other-c2m4", map[string]string{"volume_max_gb": "20"})}, want: map[string]string{"volume_max_gb": "40"}},
		{name: "volume cap: kept per prefix", earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"volume_max_gb": "60"}), ocibinding("other-c2m4", map[string]string{"volume_max_gb": "20"})},
			want: map[string]string{"volume_max_gb": "60"}, from: "h-c2m4"},
		{name: "volume cap: this share's sizes differ", earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"volume_max_gb": "60"}), ocibinding("h-c4m16", nil)},
			err: "differ in volume_max_gb (60 and 20)"},
		{name: "bindings unread", earlier: nil, mut: func(o *ShareOptions) { o.EarlierUnread = "daemon down"; o.ShelfGB = ptr(5) }, err: "could not read this machine's bindings (daemon down)"},
		{name: "bindings unread, every key given", mut: func(o *ShareOptions) {
			o.EarlierUnread, o.ShelfGB, o.ShelfFill, o.VolumeMaxGB = "daemon down", ptr(5), "seed", ptr(20)
		}, want: map[string]string{"shelf_max_gb": "5", "shelf_fill": "seed", "volume_max_gb": "20"}},
		{name: "volume cap: a size left out keeps its own", mut: func(o *ShareOptions) { o.VolumeMaxGB = ptr(60) },
			earlier: []computeapi.RunTarget{ocibinding("h-c8m32", map[string]string{"volume_max_gb": "20"})}, want: map[string]string{"volume_max_gb": "60"}},
		{name: "volume cap: a size left out is not compared on a re-run", earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"volume_max_gb": "60"}), ocibinding("h-c8m32", nil)},
			want: map[string]string{"volume_max_gb": "60"}, from: "h-c2m4"},
		{name: "a binding without oci_cli runs on docker", mut: func(o *ShareOptions) { o.ShelfGB = ptr(9) },
			earlier: []computeapi.RunTarget{{Name: "h-c8m32", Kind: "box", Config: map[string]string{"vm": "oci", "shelf_max_gb": "5"}}}, err: "binding h-c8m32 is not in this run"},
		{name: "tuning keys kept", mut: func(o *ShareOptions) { o.ShelfGB = ptr(9) },
			earlier: []computeapi.RunTarget{ocibinding("h-c2m4", map[string]string{"shelf_max_gb": "9", "shelf_fill_mbit": "4", "shelf_keep_s": "3600"})},
			want:    map[string]string{"shelf_max_gb": "9", "shelf_fill_mbit": "4", "shelf_keep_s": "3600"}, from: "h-c2m4"},
		{name: "unread, a dry run binds nothing", mut: func(o *ShareOptions) { o.DryRun, o.EarlierUnread = true, "daemon down" }},
		{name: "bad fill", mut: func(o *ShareOptions) { o.ShelfFill = "always" }, err: "want off, seed or boxes"},
		{name: "negative shelf", mut: func(o *ShareOptions) { o.ShelfGB = ptr(-1) }, err: "--shelf-gb"},
		{name: "huge volume cap", mut: func(o *ShareOptions) { o.VolumeMaxGB = ptr(1 << 30) }, err: "--volume-max-gb"},
		{name: "tart", layer: "tart", mut: func(o *ShareOptions) { o.ShelfGB = ptr(5) }, err: "are for the oci layer"},
		{name: "tart keeps nothing", layer: "tart", earlier: eachSize(map[string]string{"shelf_max_gb": "5"})},
	}
	for _, c := range cases {
		o := storageOptions()
		o.NamePrefix, o.Earlier = "h", c.earlier
		if c.mut != nil {
			c.mut(&o)
		}
		layer := cmpOr(c.layer, "oci")
		st, err := ociStorage(o, layer, "docker", sizes)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want == nil:
			if st != nil {
				t.Errorf("%s: %+v, want none", c.name, st)
			}
		case st == nil || len(st.Keys) != len(c.want) || st.From != c.from:
			t.Errorf("%s: %+v, want %v from %q", c.name, st, c.want, c.from)
		default:
			for k, v := range c.want {
				if st.Keys[k] != v {
					t.Errorf("%s: %s=%q, want %q", c.name, k, st.Keys[k], v)
				}
			}
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Status warns where sizes of one engine carry different limits.
func TestStorageDisagreements(t *testing.T) {
	alike := []computeapi.RunTarget{ocibinding("h-a", map[string]string{"shelf_max_gb": "5"}), ocibinding("h-b", map[string]string{"shelf_max_gb": "5"})}
	if got := StorageDisagreements(alike); len(got) != 0 {
		t.Errorf("alike: %v", got)
	}
	differ := []computeapi.RunTarget{ocibinding("h-a", map[string]string{"shelf_max_gb": "5", "shelf_fill": "boxes"}), ocibinding("h-b", map[string]string{"shelf_max_gb": "9"}),
		{Name: "t-a", Kind: "box", Config: map[string]string{"vm": "tart"}}}
	differ = append(differ, ocibinding("h-c", map[string]string{"shelf_max_gb": "5", "shelf_fill": "boxes", "volume_max_gb": "99", "shelf_keep_s": "60"}))
	got := StorageDisagreements(differ)
	if len(got) != 3 || !strings.Contains(got[0], "shelf_keep_s=2592000 on h-a but 60 on h-c") || !strings.Contains(got[1], "shelf_max_gb=5 on h-a but 9 on h-b") || // volume_max_gb is each size's own
		!strings.Contains(got[2], "shelf_fill=boxes on h-a but seed on h-b") {
		t.Errorf("differ: %v", got)
	}
	got = StorageDisagreements(differ[:3])
	if len(got) != 2 || !strings.Contains(got[0], "shelf_max_gb=5 on h-a but 9 on h-b") || !strings.Contains(got[1], "shelf_fill=boxes on h-a but seed on h-b") {
		t.Errorf("differ: %v", got)
	}
}

// A size that fails conformance stays bound with the limits it had; the
// result says so rather than claiming every size carries the new ones.
func TestShareFailedSizeKeepsOldStorageSaid(t *testing.T) {
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if strings.HasPrefix(line, "zm-run-conformance ") && strings.Contains(line, "size=c4m16") {
			return Result{Exit: 1}, true
		}
		return Result{Stdout: "PASS"}, true
	}}
	o := storageOptions()
	o.ShelfGB = ptr(5)
	b := listBinder{binderFunc: func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		return computeapi.RunTargetView{}, nil
	}, names: []string{"h-c2m4", "h-c4m16"}}
	res, err := shareOn(context.Background(), run, b, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed() || !strings.Contains(res.Sizes[1].Error, "still bound from before, with the shelf and volume limits it had") {
		t.Errorf("%+v", res.Sizes)
	}
}
