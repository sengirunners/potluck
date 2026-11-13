package vmhost

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

type tunFileInfo struct {
	os.FileInfo
	mode os.FileMode
	stat *syscall.Stat_t
}

func (f tunFileInfo) Mode() os.FileMode { return f.mode }
func (f tunFileInfo) Sys() any          { return f.stat }

func fakeKataTun(t *testing.T, info os.FileInfo, err error) *int {
	t.Helper()
	old := ociTunLstat
	t.Cleanup(func() { ociTunLstat = old })
	calls := new(int)
	ociTunLstat = func(path string) (os.FileInfo, error) {
		if path != "/dev/net/tun" {
			t.Fatalf("inspected device %q, want only /dev/net/tun", path)
		}
		*calls++
		return info, err
	}
	return calls
}

func validTunInfo() os.FileInfo {
	st := &syscall.Stat_t{Rdev: 10<<8 | 200}
	if runtime.GOOS == "darwin" {
		st.Rdev = 10<<24 | 200
	}
	return tunFileInfo{mode: os.ModeDevice | os.ModeCharDevice | 0666, stat: st}
}

// fakeGuard answers ociGuardReady with err, recording the networks asked.
func fakeGuard(t *testing.T, err error) *[]string {
	t.Helper()
	old := ociGuardReady
	t.Cleanup(func() { ociGuardReady = old })
	asked := new([]string)
	ociGuardReady = func(network string) error {
		*asked = append(*asked, network)
		return err
	}
	return asked
}

// A Kata box with docker (its guest /proc unmasked) is made only where the
// guard allows: never while the guard refuses, and nothing is created then.
// gVisor's docker box never asks for the guard.
func TestOCIKataDockerGuard(t *testing.T) {
	for _, rt := range []string{"kata-qemu", "runsc-root"} {
		t.Run(rt, func(t *testing.T) {
			fakeKataTun(t, validTunInfo(), nil)
			asked := fakeGuard(t, errors.New("zm-box-guard.service is \"failed\""))
			o, dir := newFakeOCI(t, "mine")
			o.runtime, o.root, o.docker = rt, true, true
			if ociKataRuntime(rt) {
				attachFakeHostParent(t, &o)
			}
			err := o.Clone("img", "guarded", 1, 512)
			_, created := os.Stat(filepath.Join(dir, "c", "guarded", "args"))
			if ociKataRuntime(rt) {
				if err == nil || !strings.Contains(err.Error(), "zm-box-guard") || created == nil || len(*asked) != 1 || (*asked)[0] != "mine" {
					t.Fatalf("kata docker with the guard refusing: err %v, created %v, asked %v", err, created == nil, *asked)
				}
				return
			}
			if err != nil || len(*asked) != 0 {
				t.Fatalf("gVisor docker box: err %v, guard asked %v", err, *asked)
			}
		})
	}
	// The real check: no network needs no guard; an owner's network never
	// has zm's guard.
	if err := ociGuardReady("none"); err != nil {
		t.Errorf("network none: %v", err)
	}
	if err := ociGuardReady("mine"); err == nil || !strings.Contains(err.Error(), "no such guard") {
		t.Errorf("an owner's network: %v", err)
	}
	// Every Potluck network has its guard, named after its filter unit.
	if len(ociBoxGuards) != len(ociBoxNets) {
		t.Fatalf("guards %v for networks %v", ociBoxGuards, ociBoxNets)
	}
	for n, unit := range ociBoxNets {
		if g := ociBoxGuards[n]; g != strings.TrimSuffix(unit, "-net.service")+"-guard.service" {
			t.Errorf("network %s: guard %q beside filter %q", n, g, unit)
		}
	}
}

// A box made while the guard ran starts again only while it runs: every
// boot of a Kata box reads the container's own options and networks, so a
// kept box is held whatever a later request's network says.
func TestOCIKataDockerGuardBoot(t *testing.T) {
	fakeKataTun(t, validTunInfo(), nil)
	oldNet := ociNetReady
	ociNetReady = func(string) error { return nil }
	t.Cleanup(func() { ociNetReady = oldNet })
	var guard error
	var asked []string
	oldGuard := ociGuardReady
	ociGuardReady = func(n string) error { asked = append(asked, n); return guard }
	t.Cleanup(func() { ociGuardReady = oldGuard })
	mk := func(name string, docker bool) ociCLI {
		o, _ := newFakeOCI(t, "potluck-box")
		o.runtime, o.root, o.docker = "kata-qemu", true, docker
		attachFakeHostParent(t, &o)
		if err := o.Clone("img", name, 1, 512); err != nil {
			t.Fatal(err)
		}
		return o
	}
	kept := mk("kept", true)
	asked = nil
	if err := kept.guardBoot("kept"); err != nil || len(asked) != 1 || asked[0] != "potluck-box" {
		t.Fatalf("guard running: %v, asked %v", err, asked)
	}
	guard = errors.New(`potluck-box-guard.service is "failed"`)
	if cmd, _, err := kept.Boot("kept", false, io.Discard); err == nil || !strings.Contains(err.Error(), "guard.service") {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Errorf("Boot of a Kata docker box with its guard failed: %v", err)
	}
	kept.network = "none" // a later request does not move the container
	if err := kept.guardBoot("kept"); err == nil {
		t.Error("a box made on potluck-box boots under a net-off request with its guard failed")
	}
	plain := mk("plain", false)
	asked = nil
	if err := plain.guardBoot("plain"); err != nil || len(asked) != 0 {
		t.Errorf("a Kata box without docker (its /proc masked) asks for the guard: %v %v", err, asked)
	}
}

// Exercise the actual create command: bridge support must never change the
// outer box's capability boundary or turn on raw networking for gVisor.
func TestOCIDockerNetwork(t *testing.T) {
	for _, rt := range []string{"kata", "io.containerd.kata.v2", "runsc-root"} {
		t.Run(rt, func(t *testing.T) {
			fakeKataTun(t, validTunInfo(), nil)
			fakeGuard(t, nil)
			o, dir := newFakeOCI(t, "mine")
			o.runtime, o.root, o.docker = rt, true, true
			if ociKataRuntime(rt) {
				attachFakeHostParent(t, &o)
			}
			if err := o.Clone("img", "docker-network", 1, 512); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(dir, "c", "docker-network", "args"))
			if err != nil {
				t.Fatal(err)
			}
			a := "\n" + string(b)
			mode := "bridge"
			if rt == "runsc-root" {
				mode = "none"
			}
			for _, want := range []string{"\n--env\nZM_DOCKER_NETWORK=" + mode + "\n", "\n--cap-drop\nNET_RAW\n", "\n--cap-drop\nMKNOD\n"} {
				if !strings.Contains(a, want) {
					t.Errorf("create lacks %q: %s", want, a)
				}
			}
			const tun = "\n--device\n/dev/net/tun:/dev/net/tun:rw\n"
			if present := strings.Contains(a, tun); present != (mode == "bridge") {
				t.Errorf("mode %s guest TUN mapping %v: %s", mode, present, a)
			}
			const unmask = "\n--security-opt\nsystempaths=unconfined\n"
			if present := strings.Contains(a, unmask); present != (mode == "bridge") {
				t.Errorf("mode %s guest /proc unmasked %v: %s", mode, present, a)
			}
			// No device other than this exact guest-kernel TUN request, and
			// no other loosening than the Kata guest's own /proc.
			withoutTun := strings.Replace(strings.Replace(a, tun, "\n", 1), unmask, "\n", 1)
			for _, bad := range []string{"\n--cap-add\n", "\n--privileged\n", "\n--device\n", "--device=", "--net-raw", "--allow-packet-socket-write", "systempaths=", "seccomp=unconfined", "apparmor=unconfined", "--sysctl"} {
				if strings.Contains(withoutTun, bad) {
					t.Errorf("create grants %q: %s", bad, a)
				}
			}
		})
	}
}

func TestOCIDockerTunIdentity(t *testing.T) {
	for _, name := range []string{"native-alias", "net-off", "no-docker", "runsc", "runsc-root", "kata-name-runsc", "runc", "missing-profile", "changed-profile", "unbounded-parent"} {
		t.Run(name, func(t *testing.T) {
			fakeGuard(t, nil)
			o, dir := newFakeOCI(t, "mine")
			o.runtime, o.root, o.docker = "kata-qemu", true, true
			native := name != "runsc" && name != "runsc-root" && name != "kata-name-runsc" && name != "runc"
			switch name {
			case "native-alias", "runsc":
				o.runtime = "runsc"
			case "runsc-root":
				o.runtime = "runsc-root"
			case "runc":
				o.runtime = "runc"
			case "net-off":
				o.network = "none"
			case "no-docker":
				o.docker = false
			}
			if native && name != "missing-profile" {
				attachFakeHostParent(t, &o)
			}
			switch name {
			case "kata-name-runsc":
				b, err := json.Marshal(map[string]any{o.runtime: map[string]string{"path": filepath.Join(dir, "runsc")}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "runtimes"), b, 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-profile":
				o.host.profile.ConfigSHA256 = strings.Repeat("0", 64)
			case "unbounded-parent":
				if err := os.WriteFile(filepath.Join(o.host.parent.path, "memory.max"), []byte("max"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			wantError := name == "runc" || name == "missing-profile" || name == "changed-profile" || name == "unbounded-parent"
			wantTun := native && o.docker && !wantError
			calls := fakeKataTun(t, validTunInfo(), nil)
			err := o.Clone("img", "tun-identity", 1, 512)
			if (err != nil) != wantError {
				t.Fatalf("Clone: %v, want error %v", err, wantError)
			}
			if (*calls > 0) != wantTun {
				t.Errorf("device metadata read %d times, want TUN %v", *calls, wantTun)
			}
			b, readErr := os.ReadFile(filepath.Join(dir, "c", "tun-identity", "args"))
			if wantError {
				if !os.IsNotExist(readErr) {
					t.Fatalf("refused identity reached create: %s, %v", b, readErr)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := strings.Contains("\n"+string(b), "\n--device\n/dev/net/tun:/dev/net/tun:rw\n"); got != wantTun {
				t.Errorf("TUN %v, want %v: %s", got, wantTun, b)
			}
		})
	}
}

func TestOCIDockerTunMalformedSource(t *testing.T) {
	for _, name := range []string{"missing", "symlink", "file", "directory", "block", "wrong-major", "wrong-minor", "no-stat"} {
		t.Run(name, func(t *testing.T) {
			fakeGuard(t, nil)
			o, dir := newFakeOCI(t, "mine")
			o.runtime, o.root, o.docker = "kata-qemu", true, true
			attachFakeHostParent(t, &o)
			info := validTunInfo().(tunFileInfo)
			var statErr error
			switch name {
			case "missing":
				statErr = os.ErrNotExist
			case "symlink":
				info.mode = os.ModeSymlink | 0777
			case "file":
				info.mode = 0666
			case "directory":
				info.mode = os.ModeDir | 0755
			case "block":
				info.mode = os.ModeDevice | 0666
			case "wrong-major":
				info.stat.Rdev = 11<<8 | 200
				if runtime.GOOS == "darwin" {
					info.stat.Rdev = 11<<24 | 200
				}
			case "wrong-minor":
				info.stat.Rdev = 10<<8 | 201
				if runtime.GOOS == "darwin" {
					info.stat.Rdev = 10<<24 | 201
				}
			case "no-stat":
				info.stat = nil
			}
			calls := fakeKataTun(t, info, statErr)
			if err := o.Clone("img", "bad-tun", 1, 512); err == nil || !strings.Contains(err.Error(), "character device 10:200") {
				t.Fatalf("malformed TUN accepted or wrong refusal: %v", err)
			}
			if *calls != 1 {
				t.Fatalf("metadata calls %d, want 1", *calls)
			}
			if _, err := os.Stat(filepath.Join(dir, "c", "bad-tun")); !os.IsNotExist(err) {
				t.Fatalf("malformed TUN reached create: %v", err)
			}
		})
	}
}

// Run the shipped helper with a stand-in daemon, rather than checking its
// source: network flags and invalid-mode refusal must reach the executable.
func TestDockerImageHelper(t *testing.T) {
	for _, mode := range []string{"none", "bridge", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			daemon := filepath.Join(dir, "dockerd-rootless.sh")
			if err := os.WriteFile(daemon, []byte(`#!/bin/sh
set -eu
if [ "$ZM_DOCKER_NETWORK" = bridge ]; then
  test -d "$XDG_RUNTIME_DIR/docker/libnetwork"
  test -x "$XDG_RUNTIME_DIR/docker/libnetwork"
  touch "$XDG_RUNTIME_DIR/docker/libnetwork/daemon-reached"
fi
printf 'IDMAP_PATH=%s\n' "$PATH"
printf '%s\n' "$@"
`), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "../cmd/potluck-driver-tart/images/linux/zm-dockerd")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
				"XDG_RUNTIME_DIR="+filepath.Join(dir, "runtime"), "ZM_DOCKER_NETWORK="+mode)
			out, err := cmd.CombinedOutput()
			if mode == "invalid" {
				if err == nil || !strings.Contains(string(out), "must be none or bridge") {
					t.Fatalf("invalid mode ran daemon: %s, %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("helper: %s, %v", out, err)
			}
			libnetwork := filepath.Join(dir, "runtime", "docker", "libnetwork")
			if mode == "bridge" {
				info, err := os.Stat(libnetwork)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
					t.Fatalf("libnetwork must be private and owned by daemon user: %v", info)
				}
				if _, err := os.Stat(filepath.Join(libnetwork, "daemon-reached")); err != nil {
					t.Fatalf("daemon could not traverse/write libnetwork: %v", err)
				}
				if !strings.Contains(string(out), "--exec-root="+filepath.Join(dir, "runtime", "docker")+"\n") {
					t.Fatalf("daemon exec root differs from prepared directory: %s", out)
				}
			} else if _, err := os.Stat(libnetwork); !os.IsNotExist(err) {
				t.Fatalf("non-bridge helper prepared libnetwork: %v", err)
			}
			// Native Linux needs capability-aware map helpers. The gVisor
			// boot must retain its existing helper lookup and restrictions.
			if selected := strings.Contains(string(out), "IDMAP_PATH=/usr/local/libexec/zm-rootless-idmap:"); selected != (mode == "bridge") {
				t.Errorf("mode %s selected native map helpers %v: %s", mode, selected, out)
			}
			for _, flag := range []string{"--iptables=false", "--ip6tables=false", "--bridge=none"} {
				if strings.Contains(string(out), flag) != (mode == "none") {
					t.Errorf("mode %s flags: %s", mode, out)
				}
			}
		})
	}
}
