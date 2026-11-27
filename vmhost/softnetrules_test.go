package vmhost

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeHostAddrs makes this machine's interface addresses as (CIDRs), or err.
func fakeHostAddrs(t *testing.T, err error, as ...string) {
	t.Helper()
	var out []net.Addr
	for _, a := range as {
		ip, n, perr := net.ParseCIDR(a)
		if perr != nil {
			t.Fatal(perr)
		}
		n.IP = ip
		out = append(out, n)
	}
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) { return out, err }
	t.Cleanup(func() { interfaceAddrs = old })
}

// A box cannot boot isolated when this Mac's addresses cannot be read: its
// rules would leave them open. Tart never runs.
func TestTartBootRefusedWithoutHostAddresses(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "tart")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1\" = run ] && touch "+strconv.Quote(ran)+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHostAddrs(t, errors.New("route socket closed"))
	if _, _, err := (tartCLI{bin: bin}).Boot("zmrt-x-r1", true, io.Discard); err == nil {
		t.Fatal("an isolated boot went ahead without this Mac's addresses")
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Fatalf("tart ran: %v", err)
	}
	if _, err := SoftnetBlockRules(); err == nil {
		t.Fatal("SoftnetBlockRules made rules without this Mac's addresses")
	}
	// A box with no network of Softnet's needs none of them.
	if _, err := TartBootArguments(false); err != nil {
		t.Fatal(err)
	}
}

// IPv4-mapped and IPv6 addresses come back as they are; a /31 or /32 has no
// network or broadcast address of its own to block.
func TestSoftnetBlockRulesShapes(t *testing.T) {
	fakeHostAddrs(t, nil, "198.51.100.4/31", "198.51.100.9/32", "2001:db8::9/128", "10.1.2.3/30")
	as, err := HostAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 4 || as[2].String() != "2001:db8::9/128" || !as[0].Addr().Is4() {
		t.Fatalf("HostAddresses = %v", as)
	}
	got := softnetBlockRulesFor(as)
	want := "@host,224.0.0.0/4,10.1.2.0/32,10.1.2.3/32,198.51.100.4/32,198.51.100.9/32," + softnetMailBlock
	if got != want {
		t.Fatalf("rules\n got %s\nwant %s", got, want)
	}
}
