package vmhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testHostProfile(t *testing.T) (string, *ociHostProfile) {
	t.Helper()
	return testHostProfileRuntime(t, "kata-qemu")
}
func testHostProfileRuntime(t *testing.T, runtime string) (string, *ociHostProfile) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "kata.toml")
	shim := filepath.Join(dir, "shim")
	config := []byte("[hypervisor.qemu]\noverhead_vcpus = 0.2\noverhead_memory = 32\n[runtime]\nstatic_sandbox_resource_mgmt = true\nsandbox_cgroup_only = true\nenable_vcpus_pinning = false\n")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	binary := []byte("#!/bin/sh\necho 'Kata Containers containerd shim (Rust): id: io.containerd.kata.v2, version: 4.3.1, commit: 03cf2ec06a5d302f23af811744f6bcea49988bfb'\n")
	if err := os.WriteFile(shim, binary, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(config)
	bs := sha256.Sum256(binary)
	p := &ociHostProfile{Runtime: runtime, ConfigPath: configPath, ConfigSHA256: hex.EncodeToString(sum[:]), BinarySHA256: hex.EncodeToString(bs[:]), CPUOverhead: 1, MemoryOverheadMiB: 512, CleanupReserveS: 10}
	daemon := filepath.Join(dir, "daemon.json")
	body, _ := json.Marshal(map[string]any{"runtimes": map[string]any{p.Runtime: map[string]any{"runtimeType": shim, "options": map[string]string{"ConfigPath": configPath}}}})
	if err := os.WriteFile(daemon, body, 0600); err != nil {
		t.Fatal(err)
	}
	liveDir := t.TempDir()
	live, _ := json.Marshal(map[string]any{p.Runtime: map[string]any{"runtimeType": shim, "options": map[string]string{"ConfigPath": configPath}}})
	if err := os.WriteFile(filepath.Join(liveDir, "runtimes"), live, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", fakeDockerEngine(t, liveDir))
	path := filepath.Join(dir, "profile.json")
	writeHostProfile(t, path, p)
	return path, p
}
func writeHostProfile(t *testing.T, path string, p *ociHostProfile) {
	t.Helper()
	b, _ := json.Marshal(p)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOCIHostProfileIdentityAndCeiling(t *testing.T) {
	path, p := testHostProfile(t)
	loaded, err := LoadOCIHostProfile(path, "kata-qemu")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := loaded.HostBudget(1, 2048); err != nil || b != (hostBudget{2, 2560}) {
		t.Fatalf("one total allowance: %+v %v", b, err)
	}
	for _, mutation := range []func(*ociHostProfile){func(p *ociHostProfile) { p.CPUOverhead = 0 }, func(p *ociHostProfile) { p.MemoryOverheadMiB = 32 }, func(p *ociHostProfile) { p.CleanupReserveS = 1 }, func(p *ociHostProfile) { p.ConfigSHA256 = strings.Repeat("0", 64) }, func(p *ociHostProfile) { p.BinarySHA256 = strings.Repeat("0", 64) }} {
		bad := *p
		mutation(&bad)
		writeHostProfile(t, path, &bad)
		if _, err := LoadOCIHostProfile(path, "kata-qemu"); err == nil {
			t.Fatal("unverified/insufficient owner profile accepted")
		}
	}
	writeHostProfile(t, path, p)
	if err := os.WriteFile(p.ConfigPath, []byte("changed after selection"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.verifyRuntime(); err == nil {
		t.Fatal("native config drift was not refused before clone/boot")
	}
}

func TestHostBudgetDescriptorUsesStoredAllocation(t *testing.T) {
	path, p := testHostProfile(t)
	o, dir := newFakeOCI(t, "none")
	installProfileOnFakeEngine(t, p, dir)
	image := "img@sha256:" + strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(dir, "images", image), nil, 0600); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{"target": "bounded", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "kata-qemu", "state_dir": t.TempDir(), "size": "c2m4", "cpu": "2", "memory": "4096", "images": image, "oci_host_profile": path, "host_cpu_limit": "4", "host_memory_limit_mb": "20000"}
	r := put(t, m, record{ID: "r000000000001", State: stReady, OS: "linux", VM: "existing", Supervisor: os.Getpid(), Start: time.Now(), Deadline: time.Now().Add(time.Hour), Config: map[string]string{"cpu": "2", "memory": "4096"}, HostBudget: &hostBudget{3, 4608}})
	desc, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if desc["slots"].(map[string]any)["free"] != float64(0) {
		t.Fatalf("two workload2CPU boxes plus host overhead exceeded4CPU lending cap: %v", desc)
	}
	m["host_cpu_limit"] = "20"
	m["host_memory_limit_mb"] = "8000"
	desc, err = call(t, m, "describe", "", nil)
	if err != nil || desc["slots"].(map[string]any)["free"] != float64(0) {
		t.Fatalf("host memory cap ignored overhead: %v %v", desc, err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "local-cap", "ttl_s": 60}); code(err) != "unavailable" {
		t.Fatalf("local host cap did not refuse acquire: %v", err)
	}
	m["host_memory_limit_mb"] = "20000"
	p.CPUOverhead = 5
	writeHostProfile(t, path, p)
	m["host_cpu_limit"] = "10"
	desc, err = call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if desc["slots"].(map[string]any)["free"] != float64(1) {
		t.Fatalf("existing3CPU allocation was recomputed from changed7CPU profile: %v", desc)
	}
	st := store{m["state_dir"]}
	got, err := st.load(r.ID)
	if err != nil || *got.HostBudget != (hostBudget{3, 4608}) {
		t.Fatalf("stored host allocation changed: %+v %v", got.HostBudget, err)
	}
}

// A native parent is represented by files only; no test calls systemd.
func attachFakeHostParent(t *testing.T, o *ociCLI) {
	t.Helper()
	endpoint := os.Getenv("DOCKER_HOST")
	_, p := testHostProfileRuntime(t, o.runtime)
	if dir := os.Getenv("FAKE_OCI"); dir != "" {
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(p.ConfigPath), "daemon.json"))
		var cfg struct{ Runtimes map[string]any }
		_ = json.Unmarshal(b, &cfg)
		live, _ := json.Marshal(cfg.Runtimes)
		_ = os.WriteFile(filepath.Join(dir, "runtimes"), live, 0600)
	}
	os.Setenv("DOCKER_HOST", endpoint)
	b, err := p.budget(1, 512)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(fmt.Sprint(int64(b.MemoryMiB)*1024*1024)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(fmt.Sprintf("%d 100000", b.CPU*100000)), 0600); err != nil {
		t.Fatal(err)
	}
	o.host = &ociHostLease{profile: p, budget: b, parent: &ownedHostParent{name: hostParentName(path, "test", 0), path: path, budget: b}}
}

func installProfileOnFakeEngine(t *testing.T, p *ociHostProfile, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(p.ConfigPath), "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Runtimes map[string]any }
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	live, _ := json.Marshal(cfg.Runtimes)
	if err := os.WriteFile(filepath.Join(dir, "runtimes"), live, 0600); err != nil {
		t.Fatal(err)
	}
}
