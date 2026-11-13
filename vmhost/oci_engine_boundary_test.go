package vmhost

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The boundary receives a genuine Docker /info JSON shape over a local Unix
// socket. Only the OS peer identity is faked; no real daemon is contacted.
func fakeDockerEngine(t *testing.T, dir string) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pl-engine-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "docker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			w.WriteHeader(404)
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, "runtimes"))
		if err != nil {
			w.WriteHeader(500)
			return
		}
		var rt any
		if json.Unmarshal(b, &rt) != nil {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ID": "test-engine", "CgroupDriver": "systemd", "CgroupVersion": "2", "Runtimes": rt})
	})}
	go srv.Serve(l)
	t.Cleanup(func() { _ = srv.Close(); _ = os.RemoveAll(root) })
	return "unix://" + socket
}

func TestAcquireVerifiesLiveDockerIdentity(t *testing.T) {
	for _, mode := range []string{"remote-env", "remote-context", "live-disk-drift", "live-options-drift", "live-options-hidden", "kata-as-runsc", "kata-as-runsc-shim", "kata-as-runsc-path"} {
		t.Run(mode, func(t *testing.T) {
			o, dir := newFakeOCI(t, "none")
			t.Setenv("PATH", filepath.Dir(o.bin)+":"+os.Getenv("PATH"))
			for _, name := range []string{"sudo", "busctl"} {
				if err := os.WriteFile(filepath.Join(filepath.Dir(o.bin), name), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.Replace(fakeOCI, "info) if", `info) if [ "$3" = "{{.CgroupDriver}} {{.CgroupVersion}}" ]; then echo 'systemd 2'; exit 0; fi; if`, 1)
			if err := os.WriteFile(o.bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			endpoint := os.Getenv("DOCKER_HOST")
			profile, p := testHostProfile(t)
			os.Setenv("DOCKER_HOST", endpoint)
			t.Setenv("FAKE_OCI_DAEMON_CONFIG", filepath.Join(filepath.Dir(p.ConfigPath), "daemon.json"))
			runtime := "kata-qemu"
			rt := map[string]any{runtime: map[string]any{"runtimeType": filepath.Join(filepath.Dir(p.ConfigPath), "shim"), "options": map[string]string{"ConfigPath": p.ConfigPath}}}
			switch mode {
			case "live-disk-drift":
				rt[runtime] = map[string]any{"runtimeType": "/other/runtime", "options": map[string]string{"ConfigPath": p.ConfigPath}}
			case "live-options-drift":
				rt[runtime] = map[string]any{"runtimeType": filepath.Join(filepath.Dir(p.ConfigPath), "shim"), "options": map[string]string{"ConfigPath": "/other/kata.toml"}}
			case "remote-env":
				t.Setenv("DOCKER_HOST", "tcp://remote.invalid:2375")
			case "remote-context":
				t.Setenv("DOCKER_HOST", "")
				t.Setenv("DOCKER_CONTEXT", "remote")
				t.Setenv("FAKE_DOCKER_CONTEXT_HOST", "ssh://remote.invalid")
			case "live-options-hidden":
				rt[runtime] = map[string]any{}
			case "kata-as-runsc", "kata-as-runsc-shim", "kata-as-runsc-path":
				runtime = "runsc"
				if mode == "kata-as-runsc-shim" {
					runtime = "io.containerd.runsc.v1"
				}
				profile = ""
				rt = map[string]any{runtime: map[string]any{"runtimeType": filepath.Join(filepath.Dir(p.ConfigPath), "shim")}}
			}
			if mode == "kata-as-runsc-path" {
				rt = map[string]any{runtime: map[string]any{"path": filepath.Join(filepath.Dir(p.ConfigPath), "shim")}}
			}
			b, _ := json.Marshal(rt)
			if err := os.WriteFile(filepath.Join(dir, "runtimes"), b, 0600); err != nil {
				t.Fatal(err)
			}
			image := "img@sha256:" + strings.Repeat("ab", 32)
			_ = os.WriteFile(filepath.Join(dir, "images", image), nil, 0600)
			ctl, srv := newController(4)
			defer srv.Close()
			m := map[string]string{"target": "live-identity", "kind": "box", "vm": "oci", "oci_cli": "docker", "oci_runtime": runtime, "state_dir": stateDir(t), "size": "c1m2", "cpu": "1", "memory": "2048", "images": image, "admission_url": srv.URL, "boot_timeout_s": "1", "admission_ttl_s": "120", "oci_host_profile": profile}
			a, err := call(t, m, "acquire", "", map[string]any{"request_id": mode, "ttl_s": 60})
			if a != nil {
				if h, ok := a["handle"].(string); ok {
					_, _ = call(t, m, "release", h, nil)
				}
			}
			if code(err) != "unavailable" {
				t.Errorf("unverified live identity accepted: %v", err)
			}
			ctl.mu.Lock()
			defer ctl.mu.Unlock()
			for _, c := range ctl.calls {
				if c == "POST /leases" {
					t.Errorf("unverified identity reached admission: %v", ctl.calls)
				}
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(calls), "create --name") {
				t.Errorf("unverified identity created workload: %s", calls)
			}
		})
	}
}
