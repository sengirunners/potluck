package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An unverified Kata budget must be refused at the protocol boundary before
// the external controller is asked for a smaller workload-only reservation.
func TestKataMissingHostProfileBeforeAdmission(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	image := "img@sha256:" + strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(dir, "images", image), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctl, srv := newController(4)
	defer srv.Close()
	m := map[string]string{"target": "bounded", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "kata-qemu", "state_dir": stateDir(t), "size": "c1m2", "cpu": "1", "memory": "2048", "images": image, "admission_url": srv.URL, "boot_timeout_s": "1", "admission_ttl_s": "120"}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "no-profile", "ttl_s": 60})
	if a != nil {
		if h, ok := a["handle"].(string); ok {
			_, _ = call(t, m, "release", h, nil)
		}
	}
	if code(err) != "unavailable" || !strings.Contains(err.Error(), "oci_host_profile") {
		t.Errorf("unverified Kata acquire: %v, want profile refusal", err)
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	for _, c := range ctl.calls {
		if c == "POST /leases" {
			t.Errorf("unverified workload was sent to admission: %v", ctl.calls)
			break
		}
	}
	if calls, _ := os.ReadFile(filepath.Join(dir, "calls")); strings.Contains(string(calls), "create --name") {
		t.Errorf("unverified profile created a container: %s", calls)
	}
}
