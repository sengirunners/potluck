package vmhost

import (
	"strings"
	"testing"
)

// keyLayer is a dirLayer that checks its own key, as a layerConfig does.
type keyLayer struct{ dirLayer }

func (keyLayer) ParseConfig(m map[string]string, c *config) (bad []string) {
	if v := m["keylayer_mode"]; v != "" && v != "fast" && v != "slow" {
		bad = append(bad, "keylayer_mode is fast or slow, not "+v)
	}
	if m["keylayer_mode"] == "slow" && c.Kind == "box" {
		bad = append(bad, "keylayer_mode slow is a run binding's")
	}
	return bad
}

func init() {
	layers["keylayer"] = func(c config) vmLayer { return keyLayer{dirLayer{c.StateDir}} }
}

func TestLayerValidatesItsOwnKeys(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"target": "k", "vm": "keylayer", "image": "img", "remote": "https://github.com/o/r.git", "state_dir": t.TempDir()}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	if _, err := parseConfig(base("keylayer_mode", "fast")); err != nil {
		t.Fatal(err)
	}
	_, err := parseConfig(base("keylayer_mode", "turbo", "boot_timeout_s", "0"))
	if err == nil || !strings.Contains(err.Error(), "keylayer_mode is fast or slow, not turbo") {
		t.Fatalf("a bad key: %v", err)
	}
	if !strings.Contains(err.Error(), "boot_timeout_s") { // its message joins the others
		t.Fatalf("the layer's error replaced the others: %v", err)
	}
	if _, err := parseConfig(base("keylayer_mode", "slow", "kind", "box")); err == nil || !strings.Contains(err.Error(), "slow is a run binding's") {
		t.Fatalf("the layer sees the config read so far: %v", err)
	}
	// Another layer's binding never meets keylayer's check.
	m := base("keylayer_mode", "turbo", "vm", "fake")
	if _, err := parseConfig(m); err != nil {
		t.Fatalf("a layer without the hook was checked: %v", err)
	}
}

func TestIncusKeysRefusedEarly(t *testing.T) {
	m := map[string]string{"target": "vm", "vm": "incus", "image": "ci-vm", "remote": "https://github.com/o/r.git", "state_dir": t.TempDir(),
		"incus_project": "default"}
	_, err := parseConfig(m)
	if err == nil || !strings.Contains(err.Error(), "incus_profile is required") || !strings.Contains(err.Error(), "not default") {
		t.Fatalf("incus keys: %v", err)
	}
}
