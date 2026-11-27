package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv(testHomeEnv, home)
	t.Setenv(EnvHuman, "")
	t.Setenv(EnvMachine, "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Home != home || c.Human == "" || c.Machine == "" {
		t.Fatalf("defaults: %+v", c)
	}
	c.File.Human = "Ana Lopez"
	c.File.Machine = "Studio.local"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c2.Human != "ana-lopez" || c2.Machine != "studio.local" {
		t.Fatalf("from file: human=%q machine=%q", c2.Human, c2.Machine)
	}
	t.Setenv(EnvMachine, "box1")
	c3, _ := Load()
	if c3.Machine != "box1" {
		t.Fatalf("env override: %q", c3.Machine)
	}
	if c3.SocketPath() != home+"/test.sock" {
		t.Fatalf("socket %q", c3.SocketPath())
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{"Host-A-Laptop": "host-a-laptop", " a b ": "a-b", "--x--": "x"} {
		if got := Sanitize(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestAdmissionDefaultsAndPaths(t *testing.T) {
	if got := (Admission{MaxManagedSessions: -1}).WithDefaults().MaxManagedSessions; got != 0 {
		t.Fatalf("negative limit = %d, want 0 (no limit)", got)
	}
	a := Admission{MaxManagedSessions: 2}.WithDefaults()
	if a.MaxManagedSessions != 2 || a.MinMemoryFreePercent != 20 || a.MinDiskFreeMB != 5120 || a.MaxSwapUsedMB != 4096 {
		t.Fatalf("%+v", a)
	}
	c := &Config{Home: "/h"}
	if c.RegistryPath() != "/h/registry.db" || c.ProjectDir("01X") != "/h/projects/01X" || c.LeaseTick().Seconds() != 30 {
		t.Fatal(c.RegistryPath(), c.ProjectDir("01X"), c.LeaseTick())
	}
}

// LoadClient never asks the user database: $ZM_HUMAN, the file, $USER.
func TestLoadClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv(testHomeEnv, home)
	t.Setenv(EnvHuman, "")
	t.Setenv("USER", "Zed")
	c, err := LoadClient()
	if err != nil || c.Home != home || c.Human != "zed" || c.Machine == "" {
		t.Fatalf("client: %+v %v", c, err)
	}
	t.Setenv("USER", "")
	if c, _ = LoadClient(); c.Human != "me" {
		t.Fatalf("no USER: %q", c.Human)
	}
	t.Setenv(EnvHuman, "ana")
	if c, _ = LoadClient(); c.Human != "ana" {
		t.Fatalf("env: %q", c.Human)
	}
}

// A binary with its own home (cmd/potluck) names its variable, directory and
// socket; it then reads no other program's variable or directory.
func TestOwnHome(t *testing.T) {
	user, other, own := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv(testHomeEnv, other)
	t.Setenv("OWN_HOME", "")
	if h, err := HomeDir(); err != nil || h != other {
		t.Fatalf("the test's names: %q %v", h, err)
	}
	wire(t, Names{HomeEnvs: []string{"OWN_HOME"}, HomeDirs: []string{".own"}, Sockets: []string{"own.sock"}})
	c, err := Load()
	if err != nil || c.Home != filepath.Join(user, ".own") || c.SocketPath() != filepath.Join(user, ".own", "own.sock") {
		t.Fatalf("own default with another program's variable set: %+v %v", c, err)
	}
	t.Setenv("OWN_HOME", own)
	if c, err = LoadClient(); err != nil || c.Home != own {
		t.Fatalf("own variable: %+v %v", c, err)
	}
}

// MachineSuffix ends only a name taken from the host; MachineEnv and
// config.json name the machine as they are. zm's defaults add nothing.
func TestMachineSuffix(t *testing.T) {
	old := [2]string{MachineEnv, MachineSuffix}
	t.Cleanup(func() { MachineEnv, MachineSuffix = old[0], old[1] })
	t.Setenv(EnvMachine, "")
	t.Setenv("OWN_MACHINE", "")
	host := func() (string, bool) { return "Studio", true }
	home := t.TempDir()
	if c, _ := load(home, osUser, host); c.Machine != "studio" {
		t.Fatalf("zm's default: %q", c.Machine)
	}
	MachineEnv, MachineSuffix = "OWN_MACHINE", "-own"
	if c, _ := load(home, osUser, host); c.Machine != "studio-own" || c.MachineFrom != MachineFromHost {
		t.Fatalf("suffixed: %+v", c)
	}
	long := func() (string, bool) { return strings.Repeat("a", 70), true }
	if c, _ := load(home, osUser, long); len(c.Machine) > 63 || !strings.HasSuffix(c.Machine, "-own") {
		t.Fatalf("long host: %q", c.Machine)
	}
	t.Setenv(EnvMachine, "zm-box") // zm's variable no longer names it
	t.Setenv("OWN_MACHINE", "desk")
	if c, _ := load(home, osUser, host); c.Machine != "desk" || c.MachineFrom != MachineFromEnv {
		t.Fatalf("own variable: %+v", c)
	}
	t.Setenv("OWN_MACHINE", "")
	if c, _ := load(home, osUser, host); c.Machine != "studio-own" {
		t.Fatalf("ZM_MACHINE was read: %q", c.Machine)
	}
	c := &Config{Home: home, File: File{Machine: "Kept"}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if c, _ := load(home, osUser, host); c.Machine != "kept" {
		t.Fatalf("config.json: %q", c.Machine)
	}
}

// sandbox_task_cmd must be an absolute path and sandbox_task_slots 1..16;
// slots default to 1 and mean nothing without the command. A bad value
// never fails the load: it disables the driver or clamps the slots, with a
// warning for the daemon's log, and Save keeps it as written.
func TestSandboxTaskConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv(testHomeEnv, home)
	write := func(body string) (*Config, error) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, FileConfig), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load()
	}
	for _, tc := range []struct {
		body, cmd string
		slots     int
		warn      string
	}{
		{`{"remote_tasks":true}`, "", 0, ""},
		{`{"remote_tasks":true,"sandbox_task_slots":4}`, "", 0, ""},
		{`{"remote_tasks":true,"sandbox_task_cmd":"/opt/driver"}`, "/opt/driver", 1, ""},
		{`{"sandbox_task_cmd":"/opt/driver","sandbox_task_slots":16}`, "/opt/driver", 16, ""},
		{`{"sandbox_task_cmd":"farm/zm-task.mjs"}`, "", 0, "not an absolute path"},
		{`{"sandbox_task_cmd":"~/driver","sandbox_task_slots":3}`, "", 0, "not an absolute path"},
		{`{"sandbox_task_cmd":"/opt/driver","sandbox_task_slots":17}`, "/opt/driver", 16, "using 16"},
		{`{"sandbox_task_cmd":"/opt/driver","sandbox_task_slots":-1}`, "/opt/driver", 1, "using 1"},
	} {
		c, err := write(tc.body)
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		cmd, slots := c.File.SandboxTasks()
		warn := c.File.SandboxTaskWarning()
		if cmd != tc.cmd || slots != tc.slots || (tc.warn == "") != (warn == "") || !strings.Contains(warn, tc.warn) {
			t.Errorf("%s: %q %d %q", tc.body, cmd, slots, warn)
		}
	}
	c, err := write(`{"sandbox_task_cmd":"farm/zm-task.mjs","sandbox_task_slots":40}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(); err != nil || c.File.SandboxTaskCmd != "farm/zm-task.mjs" || c.File.SandboxTaskSlots != 40 {
		t.Fatalf("Save rewrote the bad values: %+v %v", c.File, err)
	}
}

// update_repos stays raw: any JSON loads, whatever its type, and is saved
// back as it was. Unset means nothing.
func TestUpdateReposStaysRaw(t *testing.T) {
	for _, raw := range []string{`["a/b","c/d"]`, `"a/b"`, `42`, `{"x":1}`, `null`} {
		home := t.TempDir()
		path := filepath.Join(home, FileConfig)
		if err := os.WriteFile(path, []byte(`{"human":"ana","update_repos":`+raw+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadFrom(home)
		if err != nil {
			t.Fatalf("update_repos %s fails the load: %v", raw, err)
		}
		if string(c.File.UpdateRepos) != raw {
			t.Fatalf("update_repos %s read as %q", raw, c.File.UpdateRepos)
		}
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if err := json.Compact(&got, doc["update_repos"]); err != nil || got.String() != raw {
			t.Fatalf("update_repos %s saved as %s", raw, b)
		}
	}
	home := t.TempDir()
	c, err := LoadFrom(home)
	if err != nil || c.File.UpdateRepos != nil {
		t.Fatalf("unset: %q %v", c.File.UpdateRepos, err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, FileConfig)); strings.Contains(string(b), "update_repos") {
		t.Fatalf("unset key written: %s", b)
	}
}
