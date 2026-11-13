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
// host but DNS and outbound mail (zm-eakh.12.5.13), in Softnet's rule syntax.
func TestTartBootBlocksMail(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tart")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1\" = run ] && printf '%s\\n' \"$@\" > "+strconv.Quote(argsFile)+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, held, err := tartCLI{bin}.Boot("zmrt-x-r1", true, io.Discard)
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
		"--net-softnet-block=@host,out proto tcp port 25,out proto tcp port 465,out proto tcp port 587",
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
