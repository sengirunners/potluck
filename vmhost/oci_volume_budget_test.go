package vmhost

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boundedVolumeFixture(t *testing.T, runtime string, profileOn bool) (map[string]string, ociCLI, string) {
	t.Helper()
	profile, p := testHostProfileRuntime(t, runtime)
	o, dir := newFakeOCI(t, "none")
	o.runtime = runtime
	installProfileOnFakeEngine(t, p, dir)
	t.Setenv("PATH", filepath.Dir(o.bin)+":"+os.Getenv("PATH"))
	for _, name := range []string{"sudo", "busctl"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(o.bin), name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	image := "img@sha256:" + strings.Repeat("ab", 32)
	_ = os.WriteFile(filepath.Join(dir, "images", image), nil, 0600)
	_ = os.WriteFile(filepath.Join(dir, "images", image+".inspect"), []byte("agent\nHOME=/home/agent\n"), 0600)
	// The helper would succeed if reached. Its CLI is still only a private fake.
	script := fakeOCICase(t, fakeOCI, "run) printf '123\\t/potluck-measure/0\\n'; exit 0;;")
	if err := os.WriteFile(o.bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{"target": "bounded-volumes", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": runtime, "state_dir": stateDir(t), "size": "c1m2", "cpu": "1", "memory": "2048", "images": image, "boot_timeout_s": "1", "admission_ttl_s": "120", "persist_min_free_gb": "0"}
	if profileOn {
		m["oci_host_profile"] = profile
		o.nativeBound = true
	}
	return m, o, dir
}
func noOuterVolumeCommands(t *testing.T, dir string) {
	t.Helper()
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, bad := range []string{"run --rm", "volume create", "create --name", "start -a"} {
		if strings.Contains(string(calls), bad) {
			t.Errorf("unsupported native volume producer issued %q: %s", bad, calls)
		}
	}
}

func TestBoundedVolumesRefuseBeforeAcquisition(t *testing.T) {
	for _, runtime := range []string{"kata-qemu", "runsc", "io.containerd.runsc.v1"} {
		for _, profile := range []bool{true, false} {
			t.Run(runtime+"/"+map[bool]string{true: "profile", false: "no-profile"}[profile], func(t *testing.T) {
				m, _, dir := boundedVolumeFixture(t, runtime, profile)
				ctl, srv := newController(4)
				defer srv.Close()
				m["admission_url"] = srv.URL
				desc, err := call(t, m, "describe", "", nil)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(","+strings.Join(anyStrings(desc["capabilities"]), ",")+",", ",volumes,") {
					t.Error("native backend advertises unsupported outer volumes")
				}
				_, err = call(t, m, "acquire", "", map[string]any{"request_id": "unsupported-volume", "actor": "owner", "ttl_s": 60, "volumes": []map[string]string{{"name": "data", "path": "data", "mode": "rw"}}})
				if err == nil {
					t.Fatal("native outer volume acquired")
				}
				if profile && code(err) != "bad_request" {
					t.Errorf("outer volume was not refused at capability boundary: %v", err)
				}
				if vs := (store{m["state_dir"]}).allVols(); len(vs) != 0 {
					t.Errorf("unsupported volume metadata acquired: %+v", vs)
				}
				ctl.mu.Lock()
				for _, c := range ctl.calls {
					if c == "POST /leases" {
						t.Errorf("unsupported outer volume reached admission: %v", ctl.calls)
					}
				}
				ctl.mu.Unlock()
				noOuterVolumeCommands(t, dir)
			})
		}
	}
}

func TestNativeVolumeAdapterEntryPointsRefuse(t *testing.T) {
	for _, runtime := range []string{"kata-qemu", "runsc", "io.containerd.runsc.v1"} {
		t.Run(runtime, func(t *testing.T) {
			m, o, dir := boundedVolumeFixture(t, runtime, false)
			if err := o.CloneVolumes(m["images"], "native", 1, 2048, []mount{{Vol: "owned", Path: "data", Mode: volRW}}); err == nil {
				t.Error("unleased native mount accepted")
			}
			if err := o.CloneKept(m["images"], "native-kept", 1, 2048); err == nil {
				t.Error("native kept volume accepted")
			}
			if cmd, hold, err := o.BootKept("native-kept", true, io.Discard); err == nil {
				if hold != nil {
					_ = hold.Close()
				}
				if cmd != nil {
					_ = cmd.Wait()
				}
				t.Error("native kept start bypassed the owned lifetime")
			}
			if got := o.VolumeBytes(m["images"], []string{"owned"}); len(got) != 0 {
				t.Errorf("unleased native helper reported measured bytes: %v", got)
			}
			noOuterVolumeCommands(t, dir)
		})
	}
}

// Historical records can still reach active quota, post-release final size,
// and detached dead-supervisor measurement. Unknown must stay unknown without
// starting another VM, including after its external admission has ended.
func TestBoundedVolumeLifecycleNeverStartsHelper(t *testing.T) {
	for _, mode := range []string{"active-quota", "after-release", "dead-supervisor"} {
		t.Run(mode, func(t *testing.T) {
			m, o, dir := boundedVolumeFixture(t, "kata-qemu", true)
			ctl, srv := newController(4)
			defer srv.Close()
			m["admission_url"] = srv.URL
			c, err := parseConfig(m)
			if err != nil {
				t.Fatal(err)
			}
			r := put(t, m, record{ID: "r000000000001", Handle: "box:bounded-volumes/r000000000001", Kind: "box", Target: c.Target, VM: "historical", OS: "linux", Image: m["images"], State: stReady, Supervisor: os.Getpid(), Deadline: time.Now().Add(time.Minute), Config: m, HostBudget: &hostBudget{2, 2560}, Volumes: []volUse{{ID: "v000000000001", Layer: "owned", Mode: volRW}}, VolOwner: "owner"})
			st := store{m["state_dir"]}
			r.Handle = "box:" + c.Target + "/" + r.ID
			if err := st.save(r); err != nil {
				t.Fatal(err)
			}
			if err := st.saveVol(volMeta{ID: r.Volumes[0].ID, Layer: "owned", Target: c.Target, Owner: "owner", Bytes: 12, Used: time.Now()}); err != nil {
				t.Fatal(err)
			}
			o.nativeBound = true
			s := sup{st: st, id: r.ID, cfg: c, vm: o, log: io.Discard}
			switch mode {
			case "active-quota":
				_ = s.quotaCheck()
			case "after-release":
				a, err := newAdmission(c)
				if err != nil {
					t.Fatal(err)
				}
				lease, err := a.take(context.Background(), 2, 2560, "zm:bounded-volumes/historical")
				if err != nil {
					t.Fatal(err)
				}
				s.adm = &held{a: a, lease: lease, done: make(chan struct{})}
				s.stop = func() { close(s.adm.done) }
				if _, err := call(t, m, "release", r.Handle, nil); err != nil {
					t.Fatal(err)
				}
				s.releaseRoom()
				ctl.mu.Lock()
				remaining := len(ctl.leases)
				ctl.mu.Unlock()
				if remaining != 0 {
					t.Fatal("test prerequisite: admission did not release")
				}
				s.measureVolumes(r)
			case "dead-supervisor":
				_, err := st.update(r.ID, func(r *record) error { r.State = stLost; r.Supervisor = 0; return nil })
				if err != nil {
					t.Fatal(err)
				}
				if code := Main([]string{flagMeasure, st.dir, r.ID}); code != 1 {
					t.Errorf("unknown detached measurement result %d", code)
				}
			}
			meta, ok := st.loadVol(r.Volumes[0].ID)
			if !ok || !meta.MeasureFailed {
				t.Errorf("refused native measurement was not recorded unknown: %+v", meta)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(calls), "run --rm") {
				t.Errorf("%s started unleased native helper: %s", mode, calls)
			}
		})
	}
}

func TestKnownVolumeHelperUsesPinnedBoundedCommands(t *testing.T) {
	for _, mode := range []string{"success", "cleanup-hangs", "name-collision", "helper-error"} {
		t.Run(mode, func(t *testing.T) {
			o, dir := newFakeOCI(t, "none")
			endpoint := os.Getenv("DOCKER_HOST")
			id, err := inspectDockerRuntime(o.bin, o.runtime)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := id.allocation()
			o.engineHost = endpoint
			o.engine = &snapshot
			script := strings.Replace(fakeOCI, "d=$FAKE_OCI; echo", `d=$FAKE_OCI; if [ "$DOCKER_HOST" != "$FAKE_EXPECT_ENGINE" ];then echo wrong-engine >> "$d/calls";exit 9;fi;echo`, 1)
			cid := strings.Repeat("a", 64)
			writeCID := `previous=;for arg;do if [ "$previous" = --cidfile ];then printf '%s\n' "` + cid + `" > "$arg";fi;previous=$arg;done;`
			helper := `run) ` + writeCID + ` printf '123\t/potluck-measure/0\n';exit 0;;`
			if mode == "cleanup-hangs" {
				helper = "run) " + writeCID + "sleep 30;exit 1;;\nrm) sleep 30;exit 0;;"
			}
			if mode == "helper-error" {
				helper = strings.Replace(helper, "exit 0;;", "exit 1;;", 1)
			}
			if mode == "name-collision" {
				helper = "run) echo 'Conflict: name already in use' >&2;exit 125;;"
			}
			script = fakeOCICase(t, script, helper)
			if err := os.WriteFile(o.bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_EXPECT_ENGINE", endpoint)
			t.Setenv("DOCKER_HOST", "tcp://remote.invalid:2375")
			old := ociMeasureFor
			ociMeasureFor = time.Second
			t.Cleanup(func() { ociMeasureFor = old })
			started := time.Now()
			got := o.VolumeBytes("pinned-image", []string{"owned"})
			if time.Since(started) > 5*time.Second {
				t.Fatal("helper or cleanup CLI escaped timeout")
			}
			if mode == "success" && got["owned"] != 123 {
				t.Fatalf("known-runtime measurement changed: %v", got)
			}
			if (mode == "cleanup-hangs" || mode == "helper-error") && len(got) != 0 {
				t.Fatalf("uncertain helper cleanup reported bytes: %v", got)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if mode == "name-collision" {
				if len(got) != 0 || strings.Contains(string(calls), "rm -f") {
					t.Fatalf("unowned name collision cleanup: %v %s", got, calls)
				}
				return
			}
			if strings.Contains(string(calls), "wrong-engine") || !strings.Contains(string(calls), "run --rm") || !strings.Contains(string(calls), "rm -f "+cid) {
				t.Fatalf("helper did not use the pinned producer/cleanup: %s", calls)
			}
		})
	}
}

func TestNativeShelfBoundariesNeverStartHelper(t *testing.T) {
	for _, runtime := range []string{"kata-qemu", "runsc", "io.containerd.runsc.v1"} {
		for _, profile := range []bool{false, true} {
			t.Run(runtime+"/"+map[bool]string{true: "profile", false: "missing-profile"}[profile], func(t *testing.T) {
				m, o, dir := boundedVolumeFixture(t, runtime, profile)
				script := fakeOCICase(t, fakeOCI, "run) printf 'bytes 123 files 1\\n'; exit 0;;")
				if err := os.WriteFile(o.bin, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "cache-volume"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				ctl, srv := newController(4)
				defer srv.Close()
				m["admission_url"] = srv.URL
				desc, err := call(t, m, "describe", "", nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := desc["shelf"]; ok {
					t.Error("native describe advertises shelf")
				}
				if o.HasShelf() {
					t.Error("native shelf available")
				}
				if _, err := o.MeasureShelf(m["images"]); err == nil {
					t.Error("native shelf measured")
				}
				if _, err := o.ShelfHas(m["images"], []string{strings.Repeat("ab", 64)}); err == nil {
					t.Error("native shelf host-fill acquired helper")
				}
				cfg, err := parseConfig(m)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Shelf.MaxGB = 1
				if _, err := shelfFill(context.Background(), cfg, o, []shelfWant{{Name: "pkg", Version: "1.0.0", Hex: strings.Repeat("ab", 64)}}); err == nil {
					t.Error("native host fill accepted the shelf")
				}
				if _, err := o.shelfRun(m["images"], false, nil, "true", nil, time.Second); err == nil {
					t.Error("direct native shelf helper ran")
				}
				noOuterVolumeCommands(t, dir)
				ctl.mu.Lock()
				defer ctl.mu.Unlock()
				for _, c := range ctl.calls {
					if c == "POST /leases" {
						t.Errorf("shelf reached admission: %v", ctl.calls)
					}
				}
			})
		}
	}
}
