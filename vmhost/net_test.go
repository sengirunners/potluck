package vmhost

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// With its isolated network off (network=nat), every layer describes
// isolation none, so zm refuses it as a box and treats a run as unisolated;
// with it on, ephemeral (zm-eakh.12.5.12). The layers' binaries are missing:
// describe still answers the class.
func TestNatDescribesIsolationNone(t *testing.T) {
	for _, vm := range []string{"tart", "oci", "incus", "fake"} {
		for network, want := range map[string]string{"": "ephemeral", "softnet": "ephemeral", "nat": "none"} {
			m := cfg(t)
			m["vm"], m["network"] = vm, network
			missing := filepath.Join(t.TempDir(), "missing")
			m["oci_cli"], m["oci_runtime"], m["incus"], m["incus_profile"] = missing, "runsc", missing, "fenced"
			m["kind"], m["size"], m["cpu"], m["memory"] = "box", "s", "1", "512"
			m["images"] = "r@sha256:" + strings.Repeat("a", 64)
			d, err := call(t, m, "describe", "", nil)
			if err != nil {
				t.Fatalf("%s network=%q: %v", vm, network, err)
			}
			if d["isolation"] != want {
				t.Errorf("%s network=%q: isolation %v, want %s", vm, network, d["isolation"], want)
			}
		}
	}
}

// The Tart call site: an isolated boot runs tart with Softnet blocking the
// host but DNS, multicast, every IPv4 address this Mac holds with its
// network's own and broadcast addresses (a public one too), and outbound mail
// (zm-eakh.12.5.13), in Softnet's rule syntax.
func TestTartBootBlocksMail(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tart")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1\" = run ] && printf '%s\\n' \"$@\" > "+strconv.Quote(argsFile)+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHostAddrs(t, nil, "192.168.1.20/24", "203.0.113.7/24", "203.0.113.9/24", "127.0.0.1/8", "100.101.5.5/32", "fe80::1/64", "2001:db8::5/64")
	cmd, held, err := tartCLI{bin: bin}.Boot("zmrt-x-r1", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	held.Close()
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, want := range []string{"--net-softnet",
		"--net-softnet-block=@host,224.0.0.0/4,100.101.5.5/32,192.168.1.0/32,192.168.1.20/32,192.168.1.255/32," +
			"203.0.113.0/32,203.0.113.7/32,203.0.113.9/32,203.0.113.255/32,out proto tcp port 25,out proto tcp port 465,out proto tcp port 587",
		"--net-softnet-allow=out proto udp @host port 53,out proto tcp @host port 53"} {
		if !slices.Contains(args, want) {
			t.Errorf("tart run %q lacks %q", args, want)
		}
	}
}

// An isolated Incus VM boots only when each NIC's ACLs reject or drop
// outbound tcp 25, 465 and 587 to anywhere (zm-eakh.12.5.13).
func TestIncusRefusesAnACLThatLetsMailOut(t *testing.T) {
	for name, c := range map[string]struct {
		acl, want string
	}{
		"mail rejected":   {mailACL, ""},
		"a port range":    {`{"egress":[{"action":"drop","protocol":"tcp","destination_port":"20-600","state":"logged"}]}`, ""},
		"both any ranges": {`{"egress":[{"action":"reject","protocol":"tcp","destination":"0.0.0.0/0, ::/0"}]}`, ""},
		"no mail rule":    {`{"egress":[{"action":"drop","destination":"10.0.0.0/8"},{"action":"allow"}]}`, "tcp [25 465 587]"},
		"587 left":        {`{"egress":[{"action":"reject","protocol":"tcp","destination_port":"25,465"}]}`, "tcp [587]"},
		"allow, not drop": {`{"egress":[{"action":"allow","protocol":"tcp","destination_port":"25,465,587"}]}`, "outbound mail"},
		"disabled":        {`{"egress":[{"action":"reject","protocol":"tcp","destination_port":"25,465,587","state":"disabled"}]}`, "outbound mail"},
		"one destination": {`{"egress":[{"action":"reject","protocol":"tcp","destination":"192.0.2.0/24","destination_port":"25,465,587"}]}`, "outbound mail"},
		"one source port": {`{"egress":[{"action":"reject","protocol":"tcp","source_port":"1","destination_port":"25,465,587"}]}`, "outbound mail"},
		"an unknown key":  {`{"egress":[{"action":"reject","protocol":"tcp","destination_port":"25,465,587","only_on":"tuesdays"}]}`, "outbound mail"},
		"described":       {`{"egress":[{"action":"reject","protocol":"tcp","destination_port":"25,465,587","description":"no mail","source_port":"","icmp_type":"","icmp_code":""}]}`, ""},
		"udp only":        {`{"egress":[{"action":"reject","protocol":"udp","destination_port":"25,465,587"}]}`, "outbound mail"},
		"no such ACL":     {"", "cannot read its ACL"},
	} {
		t.Run(name, func(t *testing.T) {
			l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
			p := filepath.Join(dir, "acls", "zm-egress.json")
			if c.acl == "" {
				_ = os.Remove(p)
			} else if err := os.WriteFile(p, []byte(c.acl), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := l.Clone("ci-vm", "zmrt-x-m", 0, 0); err != nil {
				t.Fatal(err)
			}
			defer l.Destroy("zmrt-x-m", 0)
			cmd, held, err := l.Boot("zmrt-x-m", true, io.Discard)
			if err == nil {
				held.Close()
				_ = cmd.Wait()
			}
			_, started := os.Stat(filepath.Join(dir, "i", "zmrt-x-m", "running"))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("boot: %v, want a refusal naming %q", err, c.want)
			case c.want != "" && started == nil:
				t.Fatal("a VM that can send mail was started")
			}
		})
	}
	// Unisolated (network=nat, describing isolation none) is not checked.
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	_ = os.Remove(filepath.Join(dir, "acls", "zm-egress.json"))
	if err := l.Clone("ci-vm", "zmrt-x-n", 0, 0); err != nil {
		t.Fatal(err)
	}
	defer l.Destroy("zmrt-x-n", 0)
	cmd, held, err := l.Boot("zmrt-x-n", false, io.Discard)
	if err != nil {
		t.Fatalf("an unisolated boot: %v", err)
	}
	held.Close()
	_ = cmd.Wait()
}

// Tart's own PATH lookup must find the helper checked by host preflight,
// even when the daemon's environment resolves a different softnet.
func TestTartBootUsesPinnedSoftnet(t *testing.T) {
	dir := t.TempDir()
	helperDir := filepath.Join(dir, "checked")
	if err := os.Mkdir(helperDir, 0700); err != nil {
		t.Fatal(err)
	}
	fallbackDir := filepath.Join(dir, "daemon-path")
	if err := os.Mkdir(fallbackDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fallbackDir, "softnet"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fallbackDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	helper := filepath.Join(helperDir, "softnet")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(dir, "resolved")
	tart := filepath.Join(dir, "tart")
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then command -v softnet > " + strconv.Quote(result) + "; fi\n"
	if err := os.WriteFile(tart, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	cmd, held, err := (tartCLI{bin: tart, softnetPath: helper}).Boot("fake-vm", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	_ = held.Close()
	b, err := os.ReadFile(result)
	if err != nil || strings.TrimSpace(string(b)) != helper {
		t.Fatalf("resolved %q: %v", b, err)
	}
	if err := os.Remove(helper); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(result); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (tartCLI{bin: tart, softnetPath: helper}).Boot("fake-vm", true, io.Discard); err == nil {
		t.Fatal("missing pinned helper fell back to daemon PATH")
	}
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatalf("Tart ran after helper vanished: %v", err)
	}
}
