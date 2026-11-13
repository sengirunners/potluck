package vmhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOCI is a container CLI with no containers: a container is a directory
// under $FAKE_OCI/c holding its create argv and an "up" file while started,
// and exec runs on the host. It speaks just what ociCLI says.
const fakeOCI = `#!/bin/sh
d=$FAKE_OCI; echo "$*" >> "$d/calls"
case "$1" in exec|start) ;; *) echo "WARN[0000] fake: a warning on stderr, as rootless podman prints on every command" >&2 ;; esac
case "$1" in
context) printf '"%s"\n' "${FAKE_DOCKER_CONTEXT_HOST:-$DOCKER_HOST}"; exit 0;;
image) if [ "$3" = --format ]; then case "$4" in *Volumes*) cat "$d/images/$5.volumes" 2>/dev/null; exit 0 ;; esac; [ -e "$d/images/$5" ] || exit 1; cat "$d/images/$5.inspect" 2>/dev/null || echo; exit 0; fi
  [ -e "$d/images/$3" ]; exit ;;
stop) n=$4; [ -d "$d/c/$n" ] || { echo "Error: No such container: $n" >&2; exit 1; }; rm -f "$d/c/$n/up"; exit 0 ;;
volume) if [ "$*" = "volume ls -q" ]; then [ -e "$d/ls-fails" ] && { echo "Error: fake failure" >&2; exit 1; }; { grep -vx potluck-cache "$d/vols"; [ -e "$d/cache-volume" ] && echo potluck-cache; } 2>/dev/null | sort -u; exit 0; fi
  if [ "$2" = create ]; then eval v=\${$#}; echo "$v" >> "$d/vols"; exit 0; fi
  if [ "$2" = rm ]; then for v in "$@"; do :; done; [ $# = 3 ] || { echo "fake: volume rm takes one volume" >&2; exit 2; }
    [ -e "$d/inuse-$v" ] && { echo "Error: volume is in use" >&2; exit 1; }; [ -e "$d/rmfail-$v" ] && { echo "Error: fake failure" >&2; exit 1; }
    grep -qx "$v" "$d/vols" 2>/dev/null || { echo "Error: no such volume: $v" >&2; exit 1; }
    grep -vx "$v" "$d/vols" > "$d/vols.new"; mv "$d/vols.new" "$d/vols"; exit 0; fi
  if [ "$2 $3" = "inspect --format" ] && [ "$4" = "{{.Mountpoint}}" ]; then shift 4; for v; do echo "/var/lib/fake/$v/_data"; done; exit 0; fi
  if [ "$2 $3" = "inspect potluck-cache" ]; then [ -e "$d/cache-inspect-fails" ] && { echo "Error: fake failure" >&2; exit 1; }; [ -e "$d/cache-volume" ]; exit; fi; [ "$2" = inspect ] && [ -e "$d/novol-$3" ] && exit 1; exit 0 ;;
info) if [ "$3" = "{{json .Runtimes}}" ] && [ -e "$d/runtimes" ]; then cat "$d/runtimes"; exit 0; fi; exit 2 ;;
container) if [ "$2 $3" = "inspect --format" ]; then [ -d "$d/c/$5" ] || exit 1
    grep '^type=volume,source=potluck-cache,' "$d/c/$5/args" | while read -r m; do case $m in *,readonly*) echo "potluck-cache /opt/potluck-cache false" ;; *) echo "potluck-cache /opt/potluck-cache true" ;; esac; done; exit 0; fi
  [ -d "$d/c/$3" ]; exit ;;
create) n=$3; mkdir -p "$d/c/$n"; printf '%s\n' "$@" > "$d/c/$n/args"
  for a; do case "$a" in type=volume,source=*) v=${a#type=volume,source=}; echo "${v%%,*}" >> "$d/vols" ;; esac; done; exit 0 ;;
start) n=$3; [ -d "$d/c/$n" ] || exit 1; touch "$d/c/$n/up"; while [ -e "$d/c/$n/up" ]; do sleep 0.05; done; exit 0 ;;
exec) shift; i=; [ "$1" = -i ] && { i=1; shift; }; [ "$1" = -t ] && shift; n=$1; shift
  [ -e "$d/c/$n/up" ] || { echo "container $n is not running" >&2; exit 1; }
  if [ -n "$i" ]; then exec "$@"; else exec "$@" </dev/null; fi ;;
ps) if [ "$3" = -q ]; then v=${5#volume=}; [ -e "$d/inuse-$v" ] && echo c1; [ -e "$d/measuring-$v" ] && echo c2; exit 0; fi
  case "$4" in volume=*) v=${4#volume=}; [ -e "$d/inuse-$v" ] && echo box1; [ -e "$d/measuring-$v" ] && echo potluck-measure-ab12; exit 0 ;; esac; for c in "$d"/c/*; do [ -d "$c" ] || continue; n=$(basename "$c")
  if [ -e "$c/up" ]; then printf '%s\tUp 2 seconds\n' "$n"; else printf '%s\tCreated\n' "$n"; fi; done; exit 0 ;;
rm) eval n=\${$#}; [ -d "$d/c/$n" ] || { echo "Error: No such container: $n" >&2; exit 1; }; rm -rf "$d/c/$n"; exit 0 ;;
esac
exit 2
`

// Insert a fake CLI arm at the stable case boundary, independently of another
// command's implementation. Missing fixture wiring must fail visibly.
func fakeOCICase(t *testing.T, script, arm string) string {
	t.Helper()
	const marker = `case "$1" in
context)`
	if strings.Count(script, marker) != 1 {
		t.Fatal("fake OCI case boundary missing or ambiguous")
	}
	return strings.Replace(script, marker, "case \"$1\" in\n"+arm+"\ncontext)", 1)
}

// fakeRuntimes is the fake CLI's docker info runtimes: runsc, and zm's
// runsc-root as hostprobe registers it.
const fakeRuntimes = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"},"runsc-root":{"path":"/usr/local/bin/runsc","runtimeArgs":["--allow-suid"]}}`

func newFakeOCI(t *testing.T, network string) (ociCLI, string) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"c", "images"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte(fakeOCI), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OCI", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", fakeDockerEngine(t, dir))
	shim := filepath.Join(dir, "runsc")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho 'runsc version release-test'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtimes"), []byte(strings.ReplaceAll(fakeRuntimes, "/usr/local/bin/runsc", shim)), 0o600); err != nil {
		t.Fatal(err)
	}
	kata := filepath.Join(dir, "kata-shim")
	if err := os.WriteFile(kata, []byte("#!/bin/sh\necho 'Kata Containers containerd shim (Rust): id: io.containerd.kata.v2, version: 4.3.1, commit: 03cf2ec06a5d302f23af811744f6bcea49988bfb'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "runtimes"))
	var rt map[string]any
	_ = json.Unmarshal(b, &rt)
	for _, name := range []string{"kata", "kata-qemu", "io.containerd.kata.v2"} {
		rt[name] = map[string]any{"runtimeType": kata, "options": map[string]string{"ConfigPath": filepath.Join(dir, "kata.toml")}}
	}
	b, _ = json.Marshal(rt)
	if err := os.WriteFile(filepath.Join(dir, "runtimes"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return ociCLI{bin: bin, runtime: "runsc", network: network}, dir
}

func TestOCIConfig(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"target": "lx", "vm": "oci", "image": "zm/linux-ci", "remote": "https://github.com/o/r.git", "state_dir": t.TempDir(),
			"oci_runtime": "kata"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := parseConfig(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.OCICLI != "docker" || c.OCINetwork != "none" || !c.HostFetch {
		t.Fatalf("defaults: cli %q network %q host fetch %v (no network: the host must fetch)", c.OCICLI, c.OCINetwork, c.HostFetch)
	}
	if n := c.netName(); !strings.HasPrefix(n, "none") {
		t.Fatalf("network name %q", n)
	}
	for _, r := range []string{"", "runc", "crun", "youki", "runc-kata-shim", "/tmp/kata/runc", "notkata", "gvisor-runc"} {
		if _, err := parseConfig(base("oci_runtime", r)); err == nil || !strings.Contains(err.Error(), "oci_runtime") {
			t.Errorf("runtime %q: %v", r, err)
		}
	}
	for _, r := range []string{"runsc", "io.containerd.kata-fc.v2", "kata-clh"} {
		if _, err := parseConfig(base("oci_runtime", r)); err != nil {
			t.Errorf("runtime %q: %v", r, err)
		}
	}
	for _, n := range []string{"bridge", "host", "container:abc", "ns:/proc/1/ns/net", "slirp4netns:allow_host_loopback=true", "pasta:-T,80", "private", "Bridge"} {
		if _, err := parseConfig(base("oci_network", n)); err == nil || !strings.Contains(err.Error(), "oci_network") {
			t.Errorf("isolated network %q: %v", n, err)
		}
	}
	if _, err := parseConfig(base("oci_network", "bridge", "network", "nat")); err != nil {
		t.Errorf("network=nat (the unsafe dev setting) with bridge: %v", err)
	}
	if _, err := parseConfig(base("fetch", "guest")); err == nil {
		t.Fatal("fetch=guest with no network was accepted")
	}
	c, err = parseConfig(base("fetch", "guest", "oci_network", "zm-egress"))
	if err != nil || c.HostFetch {
		t.Fatalf("a filtered network with guest fetch: %+v %v", c, err)
	}
	// The supervisor rebuilds the same layer from the saved config.
	d := &drv{cfg: c}
	again, err := parseConfig(d.rawConfig())
	if err != nil || again.OCIRuntime != c.OCIRuntime || again.OCINetwork != c.OCINetwork || again.OCICLI != c.OCICLI || again.VM != "oci" ||
		strings.Join(again.OCIDNS, ",") != strings.Join(c.OCIDNS, ",") {
		t.Fatalf("rawConfig round trip: %+v %v", again, err)
	}
}

// DNS (zm-eakh.12.24.7): a named network gets public resolvers by default,
// oci_dns takes only public unicast IPs, and network none gets none.
func TestOCIDNSConfig(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"target": "lx", "vm": "oci", "image": "zm/linux-ci", "remote": "https://github.com/o/r.git", "state_dir": t.TempDir(),
			"oci_runtime": "runsc"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := parseConfig(base())
	if err != nil || c.OCIDNS != nil {
		t.Fatalf("network none: dns %v %v", c.OCIDNS, err)
	}
	if _, err := parseConfig(base("oci_dns", "1.1.1.1")); err == nil || !strings.Contains(err.Error(), "oci_dns needs a network") {
		t.Fatalf("oci_dns with network none: %v", err)
	}
	c, err = parseConfig(base("oci_network", "zm-box"))
	if err != nil || strings.Join(c.OCIDNS, ",") != "1.1.1.1,8.8.8.8" {
		t.Fatalf("a named network's default: %v %v", c.OCIDNS, err)
	}
	for _, ip := range c.OCIDNS {
		if !ociPublicIP(netip.MustParseAddr(ip)) {
			t.Errorf("default resolver %s is not public", ip)
		}
	}
	c, err = parseConfig(base("oci_network", "zm-box", "oci_dns", " 9.9.9.9, 2606:4700:4700::1111 ,::ffff:8.8.4.4"))
	if err != nil || strings.Join(c.OCIDNS, ",") != "9.9.9.9,2606:4700:4700::1111,8.8.4.4" {
		t.Fatalf("oci_dns: %v %v", c.OCIDNS, err)
	}
	// The supervisor rebuilds the owner's resolvers, not the default.
	c, err = parseConfig(base("oci_network", "zm-box", "oci_dns", "9.9.9.9"))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := parseConfig((&drv{cfg: c}).rawConfig()); err != nil || strings.Join(again.OCIDNS, ",") != "9.9.9.9" {
		t.Fatalf("rawConfig round trip of oci_dns 9.9.9.9: %v %v", again.OCIDNS, err)
	}
	for _, v := range []string{"127.0.0.11", "127.0.0.53", "::1", "10.0.0.1", "172.17.0.1", "192.168.1.1", "100.100.100.100", "169.254.169.254",
		"fd00::1", "fe80::1", "224.0.0.251", "0.0.0.0", "::", "255.255.255.255", "::ffff:192.168.1.1", "dns.google", "1.1.1", "fe80::1%eth0",
		"192.0.2.53", "198.51.100.53", "203.0.113.53", "2001:db8::53", "3fff::53", "2001:0:4136:e378:8000:63bf:3fff:fdd2", "2002:c0a8:101::1",
		"64:ff9b::c0a8:101", "64:ff9b:1::1", "100::1", "4000::1",
		",", " ", "1.1.1.1,1.0.0.1,8.8.8.8,8.8.4.4"} {
		if _, err := parseConfig(base("oci_network", "zm-box", "oci_dns", v)); err == nil || !strings.Contains(err.Error(), "oci_dns") {
			t.Errorf("oci_dns %q: %v", v, err)
		}
	}
	// A network joining another namespace (network=nat only) keeps its DNS:
	// no default, and oci_dns refused.
	for _, n := range []string{"container:abc", "ns:/proc/1/ns/net"} {
		c, err := parseConfig(base("oci_network", n, "network", "nat"))
		if err != nil || c.OCIDNS != nil {
			t.Errorf("oci_network %s: dns %v %v", n, c.OCIDNS, err)
		}
		if _, err := parseConfig(base("oci_network", n, "network", "nat", "oci_dns", "1.1.1.1")); err == nil || !strings.Contains(err.Error(), "oci_dns") {
			t.Errorf("oci_dns with oci_network %s: %v", n, err)
		}
	}
}

// create carries the resolvers on a network: --dns for each, and for docker
// and podman one read-only bind of the box's own resolv.conf naming them
// (theirs points at a resolver a boundary runtime cannot reach), removed with
// the box; nerdctl writes --dns into resolv.conf itself; network none, or one
// joining another namespace, gets neither.
func TestOCICloneDNS(t *testing.T) {
	old := ociNetReady // zm-box's filter unit, which this host has not (TestOCICloneOnANetwork checks it)
	ociNetReady = func(string) error { return nil }
	t.Cleanup(func() { ociNetReady = old })
	createArgs := func(o ociCLI, dir, vm string) []string {
		t.Helper()
		if err := o.Clone("zm-linux-ci", vm, 0, 0); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "c", vm, "args"))
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	flag := func(args []string, f string) (vs []string) {
		for i, a := range args {
			if a == f && i+1 < len(args) {
				vs = append(vs, args[i+1])
			}
		}
		return vs
	}
	for _, n := range []string{"none", "container:abc", "ns:/proc/1/ns/net"} {
		o, dir := newFakeOCI(t, n)
		o.dns, o.stateDir = []string{"1.1.1.1"}, t.TempDir()
		if args := createArgs(o, dir, "n"); len(flag(args, "--dns")) > 0 || len(flag(args, "--mount")) > 0 {
			t.Errorf("network %s got DNS: %v", n, args)
		}
	}
	for _, cli := range []string{"docker", "podman"} {
		o, dir := newFakeOCI(t, "zm-box")
		bin := filepath.Join(filepath.Dir(o.bin), cli)
		if cli != "docker" {
			if err := os.Rename(o.bin, bin); err != nil {
				t.Fatal(err)
			}
		}
		o.bin, o.dns, o.stateDir = bin, []string{"1.1.1.1", "8.8.8.8"}, t.TempDir()
		if cli != "docker" {
			if err := o.Clone("zm-linux-ci", "a", 0, 0); err == nil || !strings.Contains(err.Error(), "requires Docker") {
				t.Fatalf("unverified alternative OCI engine accepted: %v", err)
			}
			continue
		}
		args := createArgs(o, dir, "a")
		if got := strings.Join(flag(args, "--dns"), ","); got != "1.1.1.1,8.8.8.8" {
			t.Errorf("%s --dns %q: %v", cli, got, args)
		}
		ms := flag(args, "--mount")
		if len(ms) != 1 {
			t.Fatalf("%s: want one mount, the resolv.conf: %v", cli, args)
		}
		src, ok := strings.CutPrefix(ms[0], "type=bind,source=")
		src, rest, _ := strings.Cut(src, ",")
		if !ok || rest != "target=/etc/resolv.conf,readonly" || src != filepath.Join(o.stateDir, "oci-resolv", "a.conf") {
			t.Fatalf("%s mount %q: a read-only bind of the box's own resolv.conf onto /etc/resolv.conf", cli, ms[0])
		}
		b, err := os.ReadFile(src)
		if err != nil || !strings.Contains(string(b), "\nnameserver 1.1.1.1\nnameserver 8.8.8.8\n") || strings.Contains(string(b), "127.0.0.11") {
			t.Fatalf("%s resolv.conf %q %v", cli, b, err)
		}
		if st, _ := os.Stat(src); st.Mode().Perm() != 0o644 {
			t.Errorf("%s resolv.conf mode %v: the guest's user must read it", cli, st.Mode())
		}
		// Each box has its own file; the first's is untouched by the second.
		o.dns = []string{"9.9.9.9"}
		other := flag(createArgs(o, dir, "b"), "--mount")
		if len(other) != 1 || !strings.Contains(other[0], "source="+filepath.Join(o.stateDir, "oci-resolv", "b.conf")+",") {
			t.Errorf("%s: a second box's mount %v", cli, other)
		}
		if b2, _ := os.ReadFile(src); string(b2) != string(b) {
			t.Errorf("%s: a running box's resolv.conf changed under it", cli)
		}
		// Destroy removes the box and its file, and only its file.
		o.Destroy("a", 0)
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Errorf("%s: the box's resolv.conf outlived it: %v", cli, err)
		}
		if es, _ := os.ReadDir(filepath.Join(o.stateDir, "oci-resolv")); len(es) != 1 || es[0].Name() != "b.conf" {
			t.Errorf("%s: oci-resolv holds %v, want b.conf alone", cli, es)
		}
	}
	o, _ := newFakeOCI(t, "zm-box")
	nerd := filepath.Join(filepath.Dir(o.bin), "nerdctl")
	if err := os.Rename(o.bin, nerd); err != nil {
		t.Fatal(err)
	}
	o.bin, o.dns, o.stateDir = nerd, []string{"1.1.1.1"}, t.TempDir()
	if err := o.Clone("zm-linux-ci", "x", 0, 0); err == nil || !strings.Contains(err.Error(), "requires Docker") {
		t.Fatalf("unverified nerdctl accepted: %v", err)
	}
	// The layer built from a config carries the default resolvers.
	c, err := parseConfig(map[string]string{"target": "lx", "vm": "oci", "image": "zm/linux-ci", "remote": "https://github.com/o/r.git",
		"state_dir": t.TempDir(), "oci_runtime": "runsc", "oci_network": "zm-box"})
	if err != nil {
		t.Fatal(err)
	}
	if l := newLayer(c).(ociCLI); strings.Join(l.dns, ",") != "1.1.1.1,8.8.8.8" || l.stateDir != c.StateDir {
		t.Fatalf("layer from config: %+v", l)
	}
}

// An oci box that serves a GitHub runner needs a network (zm-eakh.12.24.6):
// with none the runner could never reach GitHub, so the binding is refused.
func TestOCIRunnerBoxNeedsANetwork(t *testing.T) {
	box := func(network string) map[string]string {
		m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_runtime": "runsc", "state_dir": t.TempDir(), "size": "c1m2",
			"cpu": "1", "memory": "2048", "images": "zm/box@sha256:" + strings.Repeat("ab", 32), "runner_dir": "/home/runner"}
		if network != "" {
			m["oci_network"] = network
		}
		return m
	}
	for _, n := range []string{"", "none"} {
		if _, err := parseConfig(box(n)); err == nil || !strings.Contains(err.Error(), "runner_dir needs a network") {
			t.Errorf("oci_network %q with runner_dir: %v", n, err)
		}
	}
	c, err := parseConfig(box("zm-box"))
	if err != nil || c.JITRunnerDir != "/home/runner" {
		t.Fatalf("on zm's filtered network: %+v %v", c, err)
	}
	// Unsaid net (an older daemon, a conformance run): only a runner box gets
	// the network, and every checkout comes from the host. Net on (a trusted
	// host's box, zm-eakh.12.5.19) gets it too; net off never does.
	if c.jobNet(true, "").OCINetwork != "zm-box" || c.jobNet(false, "").OCINetwork != "none" || !c.HostFetch {
		t.Errorf("runner %q, job %q, host fetch %v", c.jobNet(true, "").OCINetwork, c.jobNet(false, "").OCINetwork, c.HostFetch)
	}
	if c.jobNet(false, "on").OCINetwork != "zm-box" || c.jobNet(false, "off").OCINetwork != "none" || c.jobNet(true, "off").OCINetwork != "none" {
		t.Errorf("net on %q, net off %q", c.jobNet(false, "on").OCINetwork, c.jobNet(false, "off").OCINetwork)
	}
	if n := c.netName(); !strings.HasPrefix(n, "zm-box (filtered by its owner) for GitHub runners and boxes acquired with net on") {
		t.Errorf("describe network %q", n)
	}
	g := box("zm-box")
	g["fetch"], g["remote"] = "guest", "https://github.com/o/r.git"
	if _, err := parseConfig(g); err == nil {
		t.Error("fetch=guest on a runner binding, whose job boxes have no network")
	}
	m := box("")
	delete(m, "runner_dir")
	if _, err := parseConfig(m); err != nil {
		t.Errorf("a job box with no network: %v", err)
	}
	if c, _ := parseConfig(box("mine")); c.jobNet(false, "").OCINetwork != "none" {
		t.Error("an owner's network reached a job box on a runner binding")
	}
	m = box("mine")
	delete(m, "runner_dir")
	c, _ = parseConfig(m)
	if c.jobNet(false, "").OCINetwork != "mine" || c.jobNet(false, "on").OCINetwork != "mine" {
		t.Error("a binding without a runner lost its owner's network")
	}
	if o := c.jobNet(false, "off"); o.OCINetwork != "none" || len(o.OCIDNS) != 0 || !o.HostFetch {
		t.Errorf("net off on a binding without a runner: %q dns %v host fetch %v", o.OCINetwork, o.OCIDNS, o.HostFetch)
	}
}

// At the call site: a box acquired without a jit_config on a runner binding
// is created by its supervisor with --network none; on a binding without a
// runner it keeps the owner's network.
func TestOCIJobBoxOnARunnerBindingHasNoNetwork(t *testing.T) {
	o, dir := newFakeOCI(t, "mine")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil { // OSOf's image inspect
		t.Fatal(err)
	}
	for i, runner := range []string{"/home/runner", ""} {
		state := stateDir(t)
		m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "oci_network": "mine",
			"state_dir": state, "size": "c1m2", "cpu": "1", "memory": "512", "images": img, "runner_dir": runner, "boot_timeout_s": "20"}
		res, err := call(t, m, "acquire", "", map[string]any{"request_id": fmt.Sprint("k", i), "actor": "a", "ttl_s": 60})
		if err != nil {
			t.Fatalf("runner %q: acquire: %v", runner, err)
		}
		if h, _ := res["handle"].(string); h != "" {
			t.Cleanup(func() { _, _ = call(t, m, "release", h, map[string]any{}) })
		}
		created, _ := filepath.Glob(filepath.Join(dir, "c", "*", "args"))
		want := map[string]string{"/home/runner": "none", "": "mine"}[runner]
		found := false
		for _, f := range created {
			b, _ := os.ReadFile(f)
			a := " " + strings.Join(strings.Fields(string(b)), " ") + " "
			found = found || strings.Contains(a, " --network "+want+" ")
			// DNS (zm-eakh.12.24.7) follows the box's own network: none for the job box,
			// the default resolvers and its resolv.conf on the owner's network.
			if hasDNS := strings.Contains(a, " --dns ") || strings.Contains(a, "target=/etc/resolv.conf"); hasDNS != (want != "none") {
				t.Errorf("runner %q: box on %s has DNS %v: %s", runner, want, hasDNS, a)
			}
		}
		if !found {
			t.Errorf("runner %q: no box created with --network %s (%v)", runner, want, res)
		}
		for _, f := range created {
			os.RemoveAll(filepath.Dir(f))
		}
	}
}

// At the call site (zm-eakh.12.5.19): the daemon sends net
// on for a trusted host's box, so a sandbox on a runner binding is made on
// the binding's filtered network with NET_RAW dropped and its resolvers, not
// none; net off keeps any box on none (a host-fetched checkout), on a runner
// binding or not. Net is part of the request: a request_id reused with
// another is refused.
func TestOCISandboxNet(t *testing.T) {
	o, dir := newFakeOCI(t, "mine")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		runner, net, want string
	}{
		{"/home/runner", "on", "mine"},
		{"/home/runner", "off", "none"},
		{"", "on", "mine"},
		{"", "off", "none"},
	} {
		state := stateDir(t)
		m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "oci_network": "mine",
			"state_dir": state, "size": "c1m2", "cpu": "1", "memory": "512", "images": img, "runner_dir": c.runner, "boot_timeout_s": "20"}
		if d, _ := call(t, m, "describe", "", nil); !strings.Contains(fmt.Sprint(d["capabilities"]), "net-off") {
			t.Errorf("runner %q: capabilities %v lack net-off", c.runner, d["capabilities"])
		}
		key := fmt.Sprint("n", i)
		res, err := call(t, m, "acquire", "", map[string]any{"request_id": key, "actor": "a", "ttl_s": 60, "net": c.net})
		if err != nil {
			t.Fatalf("runner %q net %s: acquire: %v", c.runner, c.net, err)
		}
		if h, _ := res["handle"].(string); h != "" {
			t.Cleanup(func() { _, _ = call(t, m, "release", h, map[string]any{}) })
		}
		if want := map[bool]string{true: "on", false: "off"}[c.want != "none"]; res["net"] != want {
			t.Errorf("runner %q net %s: the answer says net %v, want %s", c.runner, c.net, res["net"], want)
		}
		created, _ := filepath.Glob(filepath.Join(dir, "c", "*", "args"))
		if len(created) != 1 {
			t.Fatalf("runner %q net %s: created %v (%v)", c.runner, c.net, created, res)
		}
		b, _ := os.ReadFile(created[0])
		a := " " + strings.Join(strings.Fields(string(b)), " ") + " "
		if !strings.Contains(a, " --network "+c.want+" ") {
			t.Errorf("runner %q net %s: want --network %s: %s", c.runner, c.net, c.want, a)
		}
		on := c.want != "none"
		for _, f := range []string{" --cap-drop NET_RAW ", " --dns 1.1.1.1 ", "target=/etc/resolv.conf,readonly"} {
			if strings.Contains(a, f) != on {
				t.Errorf("runner %q net %s: %q present %v, want %v: %s", c.runner, c.net, f, !on, on, a)
			}
		}
		other := "off"
		if c.net == "off" {
			other = "on"
		}
		if _, err := call(t, m, "acquire", "", map[string]any{"request_id": key, "actor": "a", "ttl_s": 60, "net": other}); err == nil || !strings.Contains(err.Error(), "different net") {
			t.Errorf("runner %q: request_id %s again with net %s: %v", c.runner, key, other, err)
		}
		os.RemoveAll(filepath.Dir(created[0]))
	}
	m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "oci_network": "mine",
		"state_dir": t.TempDir(), "size": "c1m2", "cpu": "1", "memory": "512", "images": img}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "bad", "actor": "a", "ttl_s": 60, "net": "yes"}); err == nil || !strings.Contains(err.Error(), "net is on or off") {
		t.Errorf("net yes: %v", err)
	}
}

// ... and a box that brought a jit_config is made on the binding's network,
// with NET_RAW dropped and its own resolvers (zm-eakh.12.24.7): both PRs'
// flags on one create.
func TestOCIRunnerBoxGetsTheNetworkAndDNS(t *testing.T) {
	o, dir := newFakeOCI(t, "mine")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := t.TempDir() // the "guest" is the host here: a runner that stays up
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := stateDir(t)
	m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "oci_network": "mine",
		"state_dir": state, "size": "c1m2", "cpu": "1", "memory": "512", "images": img, "runner_dir": runner, "boot_timeout_s": "20"}
	res, err := call(t, m, "acquire", "", map[string]any{"request_id": "j1", "actor": "a", "ttl_s": 60, "jit_config": "eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0="})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if h, _ := res["handle"].(string); h != "" {
		t.Cleanup(func() { _, _ = call(t, m, "release", h, map[string]any{}) })
	}
	created, _ := filepath.Glob(filepath.Join(dir, "c", "*", "args"))
	if len(created) != 1 {
		t.Fatalf("created %v (%v)", created, res)
	}
	b, _ := os.ReadFile(created[0])
	a := " " + strings.Join(strings.Fields(string(b)), " ") + " "
	for _, want := range []string{" --network mine ", " --cap-drop NET_RAW ", " --dns 1.1.1.1 ", "target=/etc/resolv.conf,readonly"} {
		if !strings.Contains(a, want) {
			t.Errorf("the runner box's create lacks %q: %s", want, a)
		}
	}
}

// A box on a network drops NET_RAW, and one on Potluck's own network, under
// its new name or the one zm gave it, starts only while its filter unit is
// active (zm-eakh.12.24.6 review).
func TestOCICloneOnANetwork(t *testing.T) {
	ready := errors.New("the filter unit is \"failed\"")
	old := ociNetReady
	t.Cleanup(func() { ociNetReady = old })
	ociNetReady = func(n string) error {
		if _, own := ociBoxNets[n]; own {
			return ready
		}
		return nil
	}
	args := func(network string) (string, error) {
		o, dir := newFakeOCI(t, network)
		err := o.Clone("img", "vm1", 1, 512)
		b, _ := os.ReadFile(filepath.Join(dir, "c", "vm1", "args"))
		return " " + strings.Join(strings.Fields(string(b)), " ") + " ", err
	}
	if a, err := args("none"); err != nil || strings.Contains(a, "NET_RAW") {
		t.Errorf("none: %v %s", err, a)
	}
	if a, err := args("mine"); err != nil || !strings.Contains(a, " --cap-drop NET_RAW ") {
		t.Errorf("an owner's network: %v %s", err, a)
	}
	for _, net := range []string{"potluck-box", "zm-box"} {
		ready = errors.New("the filter unit is \"failed\"")
		if a, err := args(net); err == nil || a != "  " {
			t.Errorf("%s with its unit failed: created %s (%v)", net, a, err)
		}
		ready = nil
		if a, err := args(net); err != nil || !strings.Contains(a, " --network "+net+" ") || !strings.Contains(a, " --cap-drop NET_RAW ") {
			t.Errorf("%s with its unit active: %v %s", net, err, a)
		}
	}
}

// Each own network is ready by its own unit: a machine with only the old unit
// active starts boxes on zm-box and not on potluck-box.
func TestOCINetReadyAsksTheNetworksUnit(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1 $2\" = \"is-active zm-box-net.service\" ] && { echo active; exit 0; }\necho inactive; exit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := ociNetReady("zm-box"); err != nil {
		t.Errorf("zm-box with zm-box-net.service active: %v", err)
	}
	if err := ociNetReady("potluck-box"); err == nil || !strings.Contains(err.Error(), "potluck-box-net.service") {
		t.Errorf("potluck-box with only the old unit active: %v", err)
	}
	if err := ociNetReady("mine"); err != nil {
		t.Errorf("an owner's network: %v", err)
	}
}

func TestOCILayer(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	if _, err := o.OSOf("zm/linux-ci"); err == nil {
		t.Fatal("an image that is not there")
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "zm-linux-ci"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if os, err := o.OSOf("zm-linux-ci"); err != nil || os != "linux" {
		t.Fatalf("OSOf = %q %v", os, err)
	}
	if err := o.Clone("zm-linux-ci", "zmrt-x-r1", 2, 2048); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "c", "zmrt-x-r1", "args"))
	args := strings.Fields(string(b))
	for _, want := range []string{"--runtime", "runsc", "--network", "none", "no-new-privileges", "--label", ociLabelNew, "--cpus", "2", "--memory", "2048m"} {
		if !strings.Contains(" "+strings.Join(args, " ")+" ", " "+want+" ") {
			t.Errorf("create lacks %q: %v", want, args)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "--entrypoint sh") || strings.Contains(strings.Join(args, " "), "infinity") {
		t.Errorf("PID 1 must be a plain sh loop: %v", args)
	}
	for _, never := range []string{"-v", "--volume", "--mount", "--privileged", "--cap-add", "--device"} {
		for _, a := range args {
			if a == never {
				t.Errorf("create has %q: %v", never, args)
			}
		}
	}

	cmd, held, err := o.Boot("zmrt-x-r1", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	if err := waitReady(o, "zmrt-x-r1", time.Now().Add(5*time.Second), exited); err != nil {
		t.Fatal(err)
	}
	vs, err := o.List()
	if err != nil || len(vs) != 1 || vs[0].Name != "zmrt-x-r1" || !vs[0].Running || !vs[0].Local {
		t.Fatalf("List = %+v %v", vs, err)
	}
	// The guest command's exit passes through; stdin reaches it.
	run := o.Exec("zmrt-x-r1", nil, io.Discard, io.Discard, "sh", "-c", "exit 7")
	var ee *exec.ExitError
	if err := run.Run(); !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exec exit = %v", err)
	}
	var out strings.Builder
	in := o.Exec("zmrt-x-r1", strings.NewReader("hello"), &out, &out, "cat")
	if err := in.Run(); err != nil || out.String() != "hello" {
		t.Fatalf("exec stdin = %q %v", out.String(), err)
	}

	// The supervisor goes: its pipe closes and the container is removed.
	held.Close()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("start -a outlived the pipe")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		vs, _ := o.List()
		if len(vs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the container outlived its supervisor: %+v", vs)
		}
	}
	o.Destroy("zmrt-x-r1", 0) // already gone: fine
	if run := o.Exec("zmrt-x-r1", nil, io.Discard, io.Discard, "true"); run.Run() == nil {
		t.Fatal("exec in a removed container succeeded")
	}
}

// A container that ends on its own (OOM, a runtime crash) ends Boot's
// process at once, with the supervisor's pipe still open: the supervisor
// then knows the VM went.
func TestOCIBootEndsWithTheContainer(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	_ = os.MkdirAll(filepath.Join(dir, "c", "y"), 0o700)
	cmd, held, err := o.Boot("y", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	if err := waitReady(o, "y", time.Now().Add(5*time.Second), exited); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "c", "y", "up")) // it dies under the run
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("Boot's process outlived its container")
	}
}

func TestOCIRefusesSharedNetworksWhenIsolated(t *testing.T) {
	for _, n := range []string{"bridge", "host", "default", "", "container:abc", "slirp4netns:allow_host_loopback=true"} {
		o, _ := newFakeOCI(t, n)
		if _, _, err := o.Boot("x", true, io.Discard); err == nil {
			t.Errorf("isolated boot on %q", n)
		}
	}
	o, dir := newFakeOCI(t, "bridge")
	_ = os.MkdirAll(filepath.Join(dir, "c", "x"), 0o700)
	cmd, held, err := o.Boot("x", false, io.Discard) // network=nat: the unsafe dev setting
	if err != nil {
		t.Fatal(err)
	}
	held.Close()
	_ = cmd.Wait()
}
