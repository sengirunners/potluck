package vmhost

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeIncus is an incus CLI with no Incus: an instance is a directory under
// $FAKE_INCUS/i holding its init argv and a "running" file while started; a
// profile is $FAKE_INCUS/profiles/<name>.json, the expanded_devices object;
// "down" in $FAKE_INCUS makes every call fail as an unreachable incusd (a
// restart). exec runs on the host. It speaks just what incusCLI says.
const fakeIncus = `#!/bin/sh
d=$FAKE_INCUS; echo "$*" >> "$d/calls"
if [ -e "$d/down" ]; then echo "Error: Get \"http://unix.socket/1.0\": dial unix: connection refused" >&2; exit 1; fi
nm() { echo "${1#*:}"; }
inst() { n=$1; st=Stopped; [ -e "$d/i/$n/running" ] && st=Running
  printf '{"name":"%s","type":"virtual-machine","status":"%s","config":{%s},"expanded_config":%s,"expanded_devices":%s}' \
    "$n" "$st" "$(cat "$d/i/$n/config")" "$(cat "$d/i/$n/xconfig")" "$(cat "$d/i/$n/devices")"; }
case "$1" in
remote)
  case "$2" in get-default) cat "$d/default-remote" ;; list) cat "$d/remotes.json" ;; esac; exit 0 ;;
image) n=$(nm "$3"); [ -e "$d/images/$n" ] || { echo "Error: Image not found" >&2; exit 1; }; echo "Type: $(cat "$d/images/$n")"; exit 0 ;;
info) [ -d "$d/i/$(nm "$2")" ]; exit ;;
init) n=$(nm "$3"); prof=; cfg='"user.zm.run-target":"1"'; shift 3
  while [ $# -gt 0 ]; do case "$1" in --profile) prof=$2; shift ;; esac; shift; done
  mkdir -p "$d/i/$n"; printf '%s' "$cfg" > "$d/i/$n/config"
  cp "$d/profiles/$prof.json" "$d/i/$n/devices"; cp "$d/profiles/$prof.config" "$d/i/$n/xconfig" 2>/dev/null || echo '{}' > "$d/i/$n/xconfig"
  exit 0 ;;
query) p=${2#*:}; p=${p%%\?*}
  [ "$p" = /1.0/projects ] && { cat "$d/projects.json"; exit 0; }
  case "$p" in /1.0/projects/*) cat "$d/project.json"; exit 0 ;; esac
  case "$p" in /1.0/network-acls/*) a=$d/acls/${p#/1.0/network-acls/}.json
    [ -e "$a" ] || { echo "Error: Network ACL not found" >&2; exit 1; }; cat "$a"; exit 0 ;; esac
  n=${p#/1.0/instances/}; n=${n%/state}
  [ -d "$d/i/$n" ] || { echo "Error: Instance not found" >&2; exit 1; }
  case "$p" in */state) [ -e "$d/i/$n/running" ] && echo '{"status": "Running"}' || echo '{"status": "Stopped"}' ;; *) inst "$n" ;; esac; exit 0 ;;
start) n=$(nm "$2"); [ -d "$d/i/$n" ] || exit 1; touch "$d/i/$n/running"
  [ -e "$d/edit-on-start" ] && cp "$d/edit-on-start" "$d/i/$n/devices"; exit 0 ;;
exec) n=$(nm "$2"); shift 2; while [ "$1" != "--" ]; do shift; done; shift
  [ -e "$d/i/$n/running" ] || { echo "Error: Instance is not running" >&2; exit 1; }
  exec "$@" ;;
list) printf '['; sep=; for i in "$d"/i/*; do [ -d "$i" ] || continue; printf '%s' "$sep"; inst "$(basename "$i")"; sep=,; done; printf ']'; exit 0 ;;
delete) n=$(nm "$3"); [ -d "$d/i/$n" ] || { echo "Error: Instance not found" >&2; exit 1; }; rm -rf "$d/i/$n"; exit 0 ;;
esac
exit 2
`

const fencedNIC = `{"root":{"type":"disk","path":"/","pool":"zm"},"eth0":{"type":"nic","network":"zmbr0","security.port_isolation":"true","security.acls":"zm-egress"}}`

// mailACL is the owner's ACL: the private ranges dropped, outbound mail rejected.
const mailACL = `{"name":"zm-egress","egress":[{"action":"drop","destination":"10.0.0.0/8,172.16.0.0/12,192.168.0.0/16","state":"enabled"},
{"action":"reject","protocol":"tcp","destination_port":"25,465,587","state":"enabled"},{"action":"allow","state":"enabled"}]}`

func newFakeIncus(t *testing.T, remoteAddr string) (incusCLI, string) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"i", "images", "profiles", "acls"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("default-remote", "zmr\n")
	write("remotes.json", `{"local":{"Addr":"unix://","Protocol":"incus"},"zmr":{"Addr":"`+remoteAddr+`","Protocol":"incus"}}`)
	write("projects.json", `["/1.0/projects/zm"]`)
	write("project.json", `{"name":"zm","config":{"limits.instances":"4","limits.cpu":"16","limits.memory":"48GiB"}}`)
	write("images/ci-vm", "virtual-machine")
	write("images/ci-ct", "container")
	write("profiles/fenced.json", fencedNIC)
	write("acls/zm-egress.json", mailACL)
	bin := filepath.Join(t.TempDir(), "incus")
	if err := os.WriteFile(bin, []byte(fakeIncus), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_INCUS", dir)
	return incusCLI{bin: bin, project: "zm", profile: "fenced"}, dir
}

func TestIncusConfig(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"target": "vm", "vm": "incus", "image": "ci-vm", "remote": "https://github.com/o/r.git", "state_dir": t.TempDir(),
			"incus_profile": "fenced"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := parseConfig(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.IncusBin != "incus" || c.IncusProject != "zm" || c.IncusRemote != "" {
		t.Fatalf("defaults: %+v", c)
	}
	for _, kv := range [][]string{{"incus_profile", ""}, {"incus_project", "default"}, {"incus_project", "a b"}, {"incus_remote", "x:y"}, {"incus_profile", "../p"}} {
		if _, err := parseConfig(base(kv...)); err == nil {
			t.Errorf("%v accepted", kv)
		}
	}
	c, err = parseConfig(base("incus_remote", "zmr", "incus_project", "pool", "incus_conf", "/etc/zm/incus"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := parseConfig((&drv{cfg: c}).rawConfig())
	if err != nil || again.IncusRemote != "zmr" || again.IncusProject != "pool" || again.IncusProfile != "fenced" || again.IncusConf != "/etc/zm/incus" || again.VM != "incus" {
		t.Fatalf("rawConfig round trip: %+v %v", again, err)
	}
}

func TestIncusRefusesTheAdminSocket(t *testing.T) {
	l, _ := newFakeIncus(t, "unix:///var/lib/incus/unix.socket")
	if _, err := l.OSOf("ci-vm"); err == nil || !strings.Contains(err.Error(), "admin socket") {
		t.Fatalf("admin socket: %v", err)
	}
	l.remote = "local"
	if _, err := l.OSOf("ci-vm"); err == nil {
		t.Fatal("the local remote was accepted")
	}
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	// A full-trust certificate sees every project: incus-admin in effect.
	_ = os.WriteFile(filepath.Join(dir, "projects.json"), []byte(`["/1.0/projects/default","/1.0/projects/zm"]`), 0o600)
	if _, err := l.OSOf("ci-vm"); err == nil || !strings.Contains(err.Error(), "restricted to project zm alone") {
		t.Fatalf("an unrestricted certificate: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "projects.json"), []byte(`["/1.0/projects/zm"]`), 0o600)
	// A project with no limits leaves the owner's share unenforced.
	_ = os.WriteFile(filepath.Join(dir, "project.json"), []byte(`{"name":"zm","config":{"limits.instances":"4"}}`), 0o600)
	if _, err := l.OSOf("ci-vm"); err == nil || !strings.Contains(err.Error(), "limits.cpu, limits.memory") {
		t.Fatalf("a project without limits: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "project.json"), []byte(`{"name":"zm","config":{"limits.instances":"4","limits.cpu":"16","limits.memory":"48GiB"}}`), 0o600)
	if os, err := l.OSOf("ci-vm"); err != nil || os != "linux" {
		t.Fatalf("certificate remote: %q %v", os, err)
	}
	if _, err := l.OSOf("ci-ct"); err == nil || !strings.Contains(err.Error(), "virtual-machine") {
		t.Fatalf("a container image: %v", err)
	}
}

func TestIncusFence(t *testing.T) {
	ok := map[string]map[string]string{"root": {"type": "disk", "path": "/"},
		"eth0": {"type": "nic", "network": "zmbr0", "security.port_isolation": "true", "security.acls": "zm-egress"}}
	vm := "virtual-machine"
	if err := fenced(incusInstance{Type: vm, ExpandedDevices: ok}, true); err != nil {
		t.Fatal(err)
	}
	with := func(n string, d map[string]string) map[string]map[string]string {
		m := map[string]map[string]string{}
		for k, v := range ok {
			m[k] = v
		}
		m[n] = d
		return m
	}
	for name, in := range map[string]incusInstance{
		"host path":      {Type: vm, ExpandedDevices: with("data", map[string]string{"type": "disk", "path": "/data", "source": "/srv"})},
		"root from host": {Type: vm, ExpandedDevices: with("root", map[string]string{"type": "disk", "path": "/", "source": "/srv/img"})},
		"gpu":            {Type: vm, ExpandedDevices: with("gpu0", map[string]string{"type": "gpu"})},
		"proxy":          {Type: vm, ExpandedDevices: with("p", map[string]string{"type": "proxy"})},
		"no isolation":   {Type: vm, ExpandedDevices: with("eth0", map[string]string{"type": "nic", "network": "zmbr0", "security.acls": "a"})},
		"no acl":         {Type: vm, ExpandedDevices: with("eth0", map[string]string{"type": "nic", "network": "zmbr0", "security.port_isolation": "true"})},
		"macvlan":        {Type: vm, ExpandedDevices: with("eth0", map[string]string{"type": "nic", "nictype": "macvlan", "parent": "eno1", "security.port_isolation": "true", "security.acls": "a"})},
		"physical":       {Type: vm, ExpandedDevices: with("eth0", map[string]string{"type": "nic", "nictype": "physical", "parent": "eno2", "security.port_isolation": "true", "security.acls": "a"})},
		"host bridge":    {Type: vm, ExpandedDevices: with("eth0", map[string]string{"type": "nic", "nictype": "bridged", "parent": "br0", "security.port_isolation": "true", "security.acls": "a"})},
		"routed":         {Type: vm, ExpandedDevices: with("eth0", map[string]string{"type": "nic", "nictype": "routed", "network": "x", "security.port_isolation": "true", "security.acls": "a"})},
		"raw qemu":       {Type: vm, ExpandedDevices: ok, ExpandedConfig: map[string]string{"raw.qemu": "-device x"}},
		"container":      {Type: "container", ExpandedDevices: ok},
		"no type":        {ExpandedDevices: ok},
	} {
		if err := fenced(in, true); err == nil {
			t.Errorf("%s passed the fence", name)
		}
	}
	// network=nat (the unsafe dev setting) skips the NIC checks, never the host ones.
	if err := fenced(incusInstance{Type: vm, ExpandedDevices: with("eth0", map[string]string{"type": "nic", "network": "lxdbr0"})}, false); err != nil {
		t.Fatalf("nat: %v", err)
	}
	if err := fenced(incusInstance{Type: vm, ExpandedDevices: with("data", map[string]string{"type": "disk", "path": "/d", "source": "/srv"})}, false); err == nil {
		t.Fatal("nat let a host path through")
	}
}

// boot clones and boots vm, answering its watcher's exit channel.
func bootIncus(t *testing.T, l incusCLI, vm string) (io.WriteCloser, <-chan struct{}) {
	t.Helper()
	if err := l.Clone("ci-vm", vm, 2, 2048); err != nil {
		t.Fatal(err)
	}
	cmd, held, err := l.Boot(vm, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	if err := waitReady(l, vm, time.Now().Add(5*time.Second), exited); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close(); l.Destroy(vm, 0) })
	return held, exited
}

func TestIncusLayer(t *testing.T) {
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	l.remote = "zmr"
	held, exited := bootIncus(t, l, "zmrt-x-r1")
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, want := range []string{"init zmr:ci-vm zmr:zmrt-x-r1 --vm --project zm --profile fenced --config user.potluck.run-target=1 --config limits.cpu=2 --config limits.memory=2048MiB",
		"start zmr:zmrt-x-r1 --project zm"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("no call %q in:\n%s", want, calls)
		}
	}
	// Exits and stdin pass through; nothing goes to an unlabelled instance.
	var ee *exec.ExitError
	if err := l.Exec("zmrt-x-r1", nil, io.Discard, io.Discard, "sh", "-c", "exit 7").Run(); !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exec exit = %v", err)
	}
	var out strings.Builder
	if err := l.Exec("zmrt-x-r1", strings.NewReader("hi"), &out, io.Discard, "cat").Run(); err != nil || out.String() != "hi" {
		t.Fatalf("exec stdin = %q %v", out.String(), err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "i", "someoneElses"), 0o700); err != nil {
		t.Fatal(err)
	}
	for f, s := range map[string]string{"config": `"x":"y"`, "xconfig": "{}", "devices": "{}"} {
		_ = os.WriteFile(filepath.Join(dir, "i", "someoneElses", f), []byte(s), 0o600)
	}
	vs, err := l.List()
	if err != nil || len(vs) != 1 || vs[0].Name != "zmrt-x-r1" || !vs[0].Running {
		t.Fatalf("List = %+v %v", vs, err)
	}
	// The supervisor goes: the guard deletes the VM; the watcher sees it gone.
	held.Close()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher outlived its VM")
	}
	if _, err := os.Stat(filepath.Join(dir, "i", "zmrt-x-r1")); !os.IsNotExist(err) {
		t.Fatalf("the VM outlived its supervisor: %v", err)
	}
	l.Destroy("zmrt-x-r1", 0) // gone already: fine
}

func TestIncusRefusesAnUnfencedProfile(t *testing.T) {
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	_ = os.WriteFile(filepath.Join(dir, "profiles", "leaky.json"),
		[]byte(`{"root":{"type":"disk","path":"/"},"eth0":{"type":"nic","network":"incusbr0"},"home":{"type":"disk","path":"/mnt","source":"/home"}}`), 0o600)
	l.profile = "leaky"
	if err := l.Clone("ci-vm", "zmrt-x-r2", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Boot("zmrt-x-r2", true, io.Discard); err == nil || !strings.Contains(err.Error(), "port_isolation") || !strings.Contains(err.Error(), "home") {
		t.Fatalf("unfenced boot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "i", "zmrt-x-r2", "running")); err == nil {
		t.Fatal("an unfenced VM was started")
	}
}

// An incusd restart is not the VM going: the watcher keeps asking, and a
// guard whose delete fails while incusd is away retries until it lands.
func TestIncusSurvivesAnIncusdRestart(t *testing.T) {
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	held, exited := bootIncus(t, l, "zmrt-x-r3")
	down := filepath.Join(dir, "down")
	_ = os.WriteFile(down, nil, 0o600)
	select {
	case <-exited:
		t.Fatal("the watcher read an incusd restart as the VM gone")
	case <-time.After(3 * time.Second):
	}
	held.Close() // the supervisor goes while incusd is away
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "i", "zmrt-x-r3")); err != nil {
		t.Fatal("the VM went while incusd was away?")
	}
	_ = os.Remove(down) // incusd is back: the guard's retry lands
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "i", "zmrt-x-r3")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the guard gave up across the restart")
		}
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher outlived its VM")
	}
}

// A VM that stops on its own ends the watcher with the pipe still open.
func TestIncusWatcherEndsWithTheVM(t *testing.T) {
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	_, exited := bootIncus(t, l, "zmrt-x-r4")
	_ = os.Remove(filepath.Join(dir, "i", "zmrt-x-r4", "running"))
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher outlived its VM")
	}
}

// An edit that lands between the check and the start (the same certificate
// can change the VM) is caught by the check after the start: the VM is
// deleted and Boot fails.
func TestIncusRechecksAfterStart(t *testing.T) {
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	_ = os.WriteFile(filepath.Join(dir, "edit-on-start"),
		[]byte(`{"root":{"type":"disk","path":"/"},"eth0":{"type":"nic","nictype":"macvlan","parent":"eno1","security.port_isolation":"true","security.acls":"a"}}`), 0o600)
	if err := l.Clone("ci-vm", "zmrt-x-r5", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Boot("zmrt-x-r5", true, io.Discard); err == nil || !strings.Contains(err.Error(), "managed bridge") {
		t.Fatalf("an edit between check and start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "i", "zmrt-x-r5")); !os.IsNotExist(err) {
		t.Fatal("the edited VM was left running")
	}
}
