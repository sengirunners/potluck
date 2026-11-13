package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHost makes HostName answer name (stable) for the test.
func fakeHost(t *testing.T, name *string) {
	old := HostName
	HostName = func() (string, bool) { return *name, true }
	t.Cleanup(func() { HostName = old })
}

// The first start persists the name in use; a later host rename (a
// network's DHCP renaming the Mac) changes nothing.
func TestMachinePersistedAtFirstStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvMachine, "")
	host := "Host-A-Laptop"
	fakeHost(t, &host)
	// An existing config.json with keys this zm does not know: kept.
	os.WriteFile(filepath.Join(home, FileConfig), []byte(`{"human": "judy", "future_key": {"x": 1}}`), 0o600)
	c, err := LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.Machine != "host-a-laptop" || c.MachineFrom != MachineFromHost {
		t.Fatalf("first load: %q from %s", c.Machine, c.MachineFrom)
	}
	if note, warn, err := c.PersistMachine(); err != nil || warn || !strings.Contains(note, "persisted") {
		t.Fatalf("persist: %q %v", note, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, FileConfig))
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil || doc["machine"] != "host-a-laptop" || doc["future_key"] == nil || doc["human"] != "judy" {
		t.Fatalf("config.json after persist: %s", b)
	}
	if fi, _ := os.Stat(filepath.Join(home, FileConfig)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}

	host = "unknown42a411d299bc"
	c2, _ := LoadFrom(home)
	if c2.Machine != "host-a-laptop" || c2.MachineFrom != MachineFromFile {
		t.Fatalf("after a rename: %q from %s", c2.Machine, c2.MachineFrom)
	}
	if note, warn, err := c2.PersistMachine(); err != nil || warn || note != "" {
		t.Fatalf("second persist: %q %v", note, err)
	}
}

// ZM_MACHINE wins over the persisted name, with a warning; on a fresh
// home it is what gets persisted.
func TestMachineOverride(t *testing.T) {
	home := t.TempDir()
	host := "mini"
	fakeHost(t, &host)
	t.Setenv(EnvMachine, "Box-1")
	c, _ := LoadFrom(home)
	if c.Machine != "box-1" || c.MachineFrom != MachineFromEnv {
		t.Fatalf("override: %q from %s", c.Machine, c.MachineFrom)
	}
	if _, warn, err := c.PersistMachine(); err != nil || warn {
		t.Fatalf("persist override on a fresh home: %v %v", warn, err)
	}
	t.Setenv(EnvMachine, "")
	if c, _ := LoadFrom(home); c.Machine != "box-1" {
		t.Fatalf("the override was not what got persisted: %q", c.Machine)
	}
	t.Setenv(EnvMachine, "other")
	c, _ = LoadFrom(home)
	w, warn, err := c.PersistMachine()
	if err != nil || !warn || c.Machine != "other" || !strings.Contains(w, `"box-1"`) || !strings.Contains(w, EnvMachine+"=other") {
		t.Fatalf("disagreeing override: machine %q warning %q %v", c.Machine, w, err)
	}
	if c, _ := LoadFrom(home); c.File.Machine != "box-1" {
		t.Fatal("an override replaced the persisted name")
	}
	// A blank override is no override.
	t.Setenv(EnvMachine, "  ")
	if c, _ := LoadFrom(home); c.Machine != "box-1" || c.MachineFrom != MachineFromFile {
		t.Fatalf("blank override: %q from %s", c.Machine, c.MachineFrom)
	}
}

// On macOS the first value is the stable LocalHostName, not the host name;
// without one, the host name is used but marked unstable.
func TestHostNamePrefersLocalHostName(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	oldL, oldH := localHostName, osHostname
	t.Cleanup(func() { localHostName, osHostname = oldL, oldH })
	osHostname = func() (string, error) { return "unknown42a411d299bc.attlocal.net", nil }
	localHostName = func() string { return "Host-A-Laptop" }
	if h, stable := HostName(); h != "Host-A-Laptop" || !stable {
		t.Fatalf("with LocalHostName: %q %v", h, stable)
	}
	localHostName = func() string { return "" }
	if h, stable := HostName(); h != "unknown42a411d299bc" || stable {
		t.Fatalf("without: %q %v", h, stable)
	}
}

// Unstable and fallback names are never persisted (tried again next start).
func TestUnstableNameNotPersisted(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvMachine, "")
	old := HostName
	t.Cleanup(func() { HostName = old })
	HostName = func() (string, bool) { return "unknown42a411d299bc", false }
	c, _ := LoadFrom(home)
	if c.MachineFrom != MachineFromUnstable {
		t.Fatalf("source %s", c.MachineFrom)
	}
	if note, warn, err := c.PersistMachine(); err != nil || !warn || !strings.Contains(note, "not persisted") {
		t.Fatalf("persist: %q %v %v", note, warn, err)
	}
	if _, err := os.Stat(filepath.Join(home, FileConfig)); err == nil {
		t.Fatal("an unstable name was written")
	}
	HostName = func() (string, bool) { return "", true }
	c, _ = LoadFrom(home)
	if c.Machine != "localhost" || c.MachineFrom != MachineFromFallback {
		t.Fatalf("fallback: %q %s", c.Machine, c.MachineFrom)
	}
	if _, warn, _ := c.PersistMachine(); !warn {
		t.Fatal("fallback persisted")
	}
}

// An upgraded install keeps the name its daemon ran as, unless that was a
// DHCP name; ZM_MACHINE and a persisted name always win.
func TestAdoptMachine(t *testing.T) {
	t.Setenv(EnvMachine, "")
	host := "Host-A-Laptop"
	fakeHost(t, &host)
	for _, x := range []struct {
		prev, want string
		adopted    bool
	}{
		{"", "host-a-laptop", false},
		{"studio", "studio", true},
		{"unknown42a411d299bc", "host-a-laptop", false},
		{"host-a-laptop", "host-a-laptop", false},
	} {
		c, _ := LoadFrom(t.TempDir())
		if got := c.AdoptMachine(x.prev); got != x.adopted || c.Machine != x.want {
			t.Errorf("prev %q: adopted %v machine %q, want %v %q", x.prev, got, c.Machine, x.adopted, x.want)
		}
	}
	// A Mac with no LocalHostName whose old daemon ran as its DHCP name:
	// that name is neither adopted nor promoted to a persistable source.
	HostNameOld := HostName
	HostName = func() (string, bool) { return "unknown42a411d299bc", false }
	c, _ := LoadFrom(t.TempDir())
	if c.AdoptMachine("unknown42a411d299bc") || c.MachineFrom != MachineFromUnstable {
		t.Fatalf("DHCP name promoted: %s", c.MachineFrom)
	}
	if _, warn, _ := c.PersistMachine(); !warn {
		t.Fatal("a DHCP name was persisted")
	}
	// Same, but the old name is a real one: kept.
	HostName = func() (string, bool) { return "studio", false }
	c, _ = LoadFrom(t.TempDir())
	if c.AdoptMachine("studio"); c.MachineFrom != MachineFromPrevious {
		t.Fatalf("real previous name not kept: %s", c.MachineFrom)
	}
	HostName = HostNameOld
	t.Setenv(EnvMachine, "box")
	c, _ = LoadFrom(t.TempDir())
	if c.AdoptMachine("studio") || c.Machine != "box" {
		t.Fatalf("env lost to the previous name: %q", c.Machine)
	}
}

// Save keeps keys this zm does not know and drops File keys that are now empty.
func TestSaveKeepsUnknownKeys(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, FileConfig), []byte(`{"machine": "box", "default_project": "x", "future_key": [1]}`), 0o600)
	c, _ := LoadFrom(home)
	c.File.DefaultProject = ""
	c.File.Tailnet.Enabled = true
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, FileConfig))
	var doc map[string]any
	json.Unmarshal(b, &doc)
	if doc["future_key"] == nil || doc["machine"] != "box" || doc["default_project"] != nil || doc["tailnet"] == nil {
		t.Fatalf("after Save: %s", b)
	}
}
