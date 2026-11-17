package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The old spellings a program runs (host share, with the pool as --pool,
// and host doctor) are host and doctor underneath: each answers --json
// with the new command's shape, pinned in testdata/host_old_spellings.golden,
// stdout holds that JSON alone, host doctor --json exits 0 whatever doctor
// found (the program reads the answer), and neither shows in help.
func TestHostOldSpellings(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	shapes := map[string]string{}
	share := []string{"--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"}
	pair := func(name string, old, now []string, oldCode, nowCode int) {
		t.Helper()
		o, oe, oc := m.run(t, append([]string{"--json", "host"}, old...)...)
		n, ne, nc := m.run(t, append([]string{"--json"}, now...)...)
		if oc != oldCode || nc != nowCode {
			t.Fatalf("%s: exit %d (want %d), new exit %d (want %d)\n%s\n%s", name, oc, oldCode, nc, nowCode, oe, ne)
		}
		if !json.Valid([]byte(o)) || !json.Valid([]byte(n)) {
			t.Fatalf("%s: stdout is not one JSON answer:\n%s\n--- new\n%s", name, o, n)
		}
		if so, sn := jsonShape(t, o), jsonShape(t, n); so != sn {
			t.Errorf("host %s answers another shape than %s:\n%s\n--- vs\n%s", name, now[0], so, sn)
		}
		shapes[name] = jsonShape(t, o)
	}
	pair("share", append([]string{"share", "--pool", "trial"}, share...), append([]string{"host", "trial"}, share...), 0, 0)
	pair("share --dry-run", append([]string{"share", "--dry-run", "--layer", "oci"}, share...), append([]string{"host", "--dry-run", "--layer", "oci"}, share...), 0, 0)
	// This machine's swap is not known safe, so doctor fails; the old
	// spelling's --json answers it with exit 0.
	pair("doctor", []string{"doctor"}, []string{"doctor"}, 0, 1)
	// In the potluck program the pool is its global --pool (-P).
	if o := m.ok(t, append([]string{"--json", "host", "share", "-P", "trial", "--dry-run"}, share...)...); !json.Valid([]byte(o)) {
		t.Errorf("host share -P: %s", o)
	}
	if _, _, code := m.run(t, "host", "doctor"); code != 1 {
		t.Errorf("host doctor without --json: exit %d, want doctor's 1", code)
	}

	var got []string
	for _, k := range []string{"share", "share --dry-run", "doctor"} {
		got = append(got, "== host "+k+" --json\n"+shapes[k])
	}
	checkGolden(t, filepath.Join("testdata", "host_old_spellings.golden"), strings.Join(got, "\n")+"\n", *updateOldSpellings)

	help := m.ok(t, "host", "--help")
	for _, old := range []string{"share", "doctor"} {
		if strings.Contains(help, "\n  "+old+" ") {
			t.Errorf("host --help lists the old spelling %s:\n%s", old, help)
		}
	}
	for old, now := range map[string]string{"share": "potluck host", "doctor": "potluck doctor"} {
		if h := m.ok(t, "host", old, "--help"); !strings.Contains(h, "The old spelling of "+now+",") {
			t.Errorf("host %s --help does not name %s:\n%s", old, now, h)
		}
	}
}
