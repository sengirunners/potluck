package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// told is what the tests' hypervisor of vm was last told for flag (--disk,
// --net).
func (c chTest) told(vm, flag string) string {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.l.dir(vm), fakeRan))
	if err != nil {
		c.t.Fatal(err)
	}
	args := strings.Split(string(b), "\n")
	for i, a := range args[:len(args)-1] {
		if a == flag {
			return args[i+1]
		}
	}
	c.t.Fatalf("the hypervisor of %s was told no %s: %q", vm, flag, args)
	return ""
}

// A binding's rates are its VMs' hypervisors': the disk's bytes and requests
// a second, the link's bytes a second, each a bucket that holds one second's
// worth and fills in a second. A binding with none tells its hypervisors of none. A
// kept VM boots with the rates its binding has then.
func TestCloudHVLimitsAreTheHypervisors(t *testing.T) {
	c := cloudhvFixture(t, 0)
	c.boot("vm1", false)
	if d, n := c.told("vm1", "--disk"), c.told("vm1", "--net"); strings.Contains(d+n, "_size=") || strings.Contains(d+n, "refill") {
		t.Errorf("a binding with no rates told its hypervisor --disk %s --net %s", d, n)
	}

	c.l.c.Limits = cloudhvLimits{DiskMBs: 20, DiskOps: 2000, NetMbit: 10}
	cmd, _ := c.boot("vm2", true)
	rates := func(told string) string { // what follows the rest of the argument
		_, r, _ := strings.Cut(told, ",bw_size=")
		return r
	}
	if d := rates(c.told("vm2", "--disk")); d != "20971520,bw_refill_time=1000,ops_size=2000,ops_refill_time=1000" {
		t.Errorf("20 MiB and 2000 requests a second: the disk's rates are %q", d)
	}
	if n := rates(c.told("vm2", "--net")); n != "1250000,bw_refill_time=1000" {
		t.Errorf("10 megabits a second: the link's rate is %q", n)
	}
	if n := c.told("vm2", "--net"); !strings.Contains(n, "host_mac=") || strings.Contains(n, "ops_") {
		t.Errorf("the link with a rate: %q, want its addresses still, and no limit on requests", n)
	}
	if got := c.run("vm2", "echo", "runs"); got != "runs" {
		t.Errorf("a command in a VM with rates answered %q", got)
	}

	c.l.PowerOff("vm2", cmd.Process.Pid)
	c.l.c.Limits = cloudhvLimits{DiskOps: 300}
	c.start("vm2", c.l.BootKept)
	if d, n := c.told("vm2", "--disk"), c.told("vm2", "--net"); !strings.HasSuffix(d, ",ops_size=300,ops_refill_time=1000") || strings.Contains(d+n, "bw_") {
		t.Errorf("a kept VM booted under 300 requests a second and no other rate: --disk %s --net %s", d, n)
	}
	if v, err := cloudhvLoad(c.l.dir("vm2")); err != nil || v.Limits != c.l.c.Limits {
		t.Errorf("the kept VM's record has the rates %+v, %v, want its binding's now", v.Limits, err)
	}
}

// The largest rates a binding can have are told to the hypervisor whole: in
// bytes they are past what 32 bits hold.
func TestCloudHVLargestRates(t *testing.T) {
	l := cloudhvLimits{DiskMBs: 100000, DiskOps: 10000000, NetMbit: 100000}
	if d, n := l.disk(), l.net(); d != ",bw_size=104857600000,bw_refill_time=1000,ops_size=10000000,ops_refill_time=1000" || n != ",bw_size=12500000000,bw_refill_time=1000" {
		t.Errorf("the largest rates: disk %q, link %q", d, n)
	}
}

// A warm box booted under other rates than its binding has now is not a box
// the binding makes: it is ended, not handed out.
func TestCloudHVWarmBoxOfOtherRatesIsEnded(t *testing.T) {
	rates := cloudhvLimits{DiskMBs: 20, DiskOps: 2000, NetMbit: 10}
	made := func(l cloudhvLimits) record { // a warm box as its supervisor recorded it, still starting
		m := map[string]string{}
		cloudhvConfig{Limits: l}.raw(m)
		return record{Handle: "box:a/r1", OS: "linux", VM: "vm1", State: stStarting, Config: m}
	}
	now := func(l cloudhvLimits) *drv {
		return &drv{cfg: config{VM: "cloudhv", Kind: "box", CloudHV: cloudhvConfig{Limits: l}}}
	}
	for _, same := range []cloudhvLimits{{}, rates} {
		if why := now(same).warmOffBinding(made(same)); why != "" {
			t.Errorf("a warm box with its binding's rates %+v is ended: %s", same, why)
		}
	}
	for what, other := range map[string]cloudhvLimits{"no rates": {}, "another disk rate": {DiskMBs: 21, DiskOps: 2000, NetMbit: 10},
		"another requests rate": {DiskMBs: 20, DiskOps: 2001, NetMbit: 10}, "another link rate": {DiskMBs: 20, DiskOps: 2000, NetMbit: 11}} {
		if why := now(rates).warmOffBinding(made(other)); why == "" {
			t.Errorf("a warm box booted with %s would be handed out by a binding with the rates %+v", what, rates)
		}
	}
	if why := now(cloudhvLimits{}).warmOffBinding(made(rates)); why == "" {
		t.Error("a warm box booted with rates would be handed out by a binding with none")
	}
}

// A warm box whose supervisor holds another ttl_max_s than the binding now
// sends (a rebind raised or lowered it) is not handed out: it would end at
// the old cap.
func TestWarmBoxOfOtherLifetimeCapIsEnded(t *testing.T) {
	made := func(ttl string) record {
		return record{Handle: "box:a/r1", OS: "linux", VM: "vm1", State: stStarting, Config: map[string]string{"ttl_max_s": ttl}}
	}
	d := &drv{cfg: config{VM: "tart", Kind: "box", TTLMax: 432300}}
	if why := d.warmOffBinding(made("432300")); why != "" {
		t.Errorf("a warm box with its binding's cap is ended: %s", why)
	}
	for _, other := range []string{"14400", "500000"} {
		if why := d.warmOffBinding(made(other)); why != warmOffTTL {
			t.Errorf("a warm box booted with ttl_max_s %s would be handed out under 432300: %q", other, why)
		}
	}
}
