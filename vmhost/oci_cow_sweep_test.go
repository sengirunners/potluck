package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cow copy whose container is gone before Destroy could delete it is
// deleted by the next sweep (zm-eakh.12.65.1); a live box's, a released one
// whose supervisor still runs, one a container still mounts, another state
// directory's and another CLI's are kept (an older driver's file, naming no
// CLI, is this CLI's when its daemon has the volumes); a second sweep does
// nothing more.
func TestSweepDropsLeakedCOW(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	state := t.TempDir()
	o.stateDir = state
	st := store{state}
	if err := os.MkdirAll(filepath.Join(state, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := map[string]string{"target": "lx", "vm": "oci", "image": "zm/linux-ci", "remote": "https://github.com/o/r.git",
		"state_dir": state, "oci_runtime": "runsc", "oci_cli": o.bin}
	// Each public list starts a fresh driver. Cleanup cannot depend on an
	// acquire in this process having pinned its metadata producer first.
	sweep := func() error {
		_, err := dispatch("list", "", nil, config)
		return err
	}

	p := st.vmPrefixNew()
	gone, leased, mounted, running := p+"gone", p+"leased", p+"mounted", p+"running"
	foreign := "potluck-00000000-gone"
	released, otherCLI, unnoted, oldLeak := p+"released", p+"othercli", p+"unnoted", p+"oldleak"
	vols := func(vm string) []string { return []string{vm + "-cow0", vm + "-cow0u", vm + "-cow0w"} }
	for _, vm := range []string{gone, leased, mounted, running, foreign, released} {
		if err := o.noteCOW(vm, vols(vm)); err != nil {
			t.Fatal(err)
		}
	}
	// othercli: another binding's box in the same state directory, on a daemon this CLI cannot see.
	if err := (ociCLI{bin: "podman", stateDir: state}).noteCOW(otherCLI, vols(otherCLI)); err != nil {
		t.Fatal(err)
	}
	// unnoted and oldleak: files that name no CLI (an older driver's). This
	// daemon has oldleak's volumes, so they are this CLI's; it has none of
	// unnoted's (another binding's).
	for _, vm := range []string{unnoted, oldLeak} {
		if err := os.WriteFile(o.cowFile(vm), []byte(strings.Join(vols(vm), "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "novol-"+unnoted+"-cow0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// released: its lease is gone but its supervisor runs (it may be making the copies still).
	if err := st.save(record{ID: "r-released", VM: released, State: stReleased, Supervisor: os.Getpid(), Start: time.Now().UTC(),
		End: time.Now().UTC(), Deadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// leased: a live record, its container not made yet (CloneVolumes notes first).
	if err := st.save(record{ID: "r-leased", VM: leased, State: stStarting, Supervisor: os.Getpid(), Start: time.Now().UTC(),
		Deadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// mounted: no container of ours, but some container still mounts its upper.
	if err := os.WriteFile(filepath.Join(dir, "inuse-"+mounted+"-cow0u"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// running: its container is there and a live record holds it.
	if err := os.MkdirAll(filepath.Join(dir, "c", running), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.save(record{ID: "r-running", VM: running, State: stReady, Supervisor: os.Getpid(), Start: time.Now().UTC(),
		Deadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	removed := func() map[string]bool {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		rm := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			if a, ok := strings.CutPrefix(line, "volume rm "); ok {
				for _, v := range strings.Fields(a) {
					rm[v] = true
				}
			}
		}
		return rm
	}
	noted := func(vm string) bool {
		_, err := os.Stat(o.cowFile(vm))
		return err == nil
	}

	endpoint := os.Getenv("DOCKER_HOST")
	for _, host := range []string{"tcp://remote.invalid:2375", "unix:///missing/potluck-docker.sock"} {
		t.Setenv("DOCKER_HOST", host)
		if err := sweep(); err != nil {
			t.Fatal(err)
		}
		if len(removed()) != 0 || !noted(gone) || !noted(oldLeak) {
			t.Fatal("unverified metadata producer mutated orphan volumes")
		}
	}
	t.Setenv("DOCKER_HOST", endpoint)
	if err := sweep(); err != nil {
		t.Fatal(err)
	}
	rm := removed()
	for _, vm := range []string{gone, oldLeak} {
		for _, v := range vols(vm) {
			if !rm[v] {
				t.Errorf("the leaked cow volume %s was not removed (calls: %v)", v, rm)
			}
		}
		if noted(vm) {
			t.Errorf("the leaked box %s's oci-cow file is still there", vm)
		}
	}
	for _, vm := range []string{leased, mounted, running, foreign, released, otherCLI, unnoted} {
		for _, v := range vols(vm) {
			if rm[v] {
				t.Errorf("%s was removed: %s is live, busy, mounted, or not this directory's or this CLI's", v, vm)
			}
		}
		if !noted(vm) {
			t.Errorf("%s's oci-cow file was removed", vm)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "c", running)); err != nil {
		t.Errorf("the running box's container went: %v", err)
	}

	before, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if err := sweep(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if extra := string(after[len(before):]); strings.Contains(extra, "volume rm") || strings.Contains(extra, "rm -f") {
		t.Errorf("a second sweep removed more:\n%s", extra)
	}
}
