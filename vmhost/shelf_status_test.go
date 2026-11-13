package vmhost

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// host shelf status says the shelf's size (no names), the fill mode, the
// limits and the queue, and reads without filling or making the volume.
func TestShelfStatus(t *testing.T) {
	o, dir, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	body := []byte("tarball")
	r.publish("left-pad", "1.3.0", sri512(body), "", body)
	lock := filepath.Join(t.TempDir(), "package-lock.json")
	if err := os.WriteFile(lock, []byte(`{"packages": {"node_modules/left-pad": {"resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz", "integrity": "`+sri512(body)+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := func(extra map[string]string) *bytes.Reader {
		m := map[string]string{
			"target": "b", "kind": "box", "vm": "oci", "image": "img@sha256:" + strings.Repeat("a", 64), "images": "img@sha256:" + strings.Repeat("a", 64),
			"size": "c1m2", "cpu": "1", "memory": "2048", "state_dir": o.stateDir, "oci_cli": o.bin, "oci_runtime": o.runtime, "shelf_max_gb": "1",
			"shelf_fill": "boxes", "volume_max_gb": "40",
		}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(map[string]any{"config": m})
		return bytes.NewReader(b)
	}
	var out, errs bytes.Buffer
	if code := hostCmd([]string{"shelf", "fill", "--npm-lock", lock}, req(map[string]string{"shelf_fill": "seed"}), &out, &errs); code != 0 {
		t.Fatalf("fill: exit %d: %s %s", code, out.String(), errs.String())
	}
	if _, _, err := queueShelfWants(o.stateDir, "r0123456789ab", []byte("npm a 1.0.0 "+sri512([]byte("x"))+"\nnot a want\n"), false); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if code := hostCmd([]string{"shelf", "status"}, req(nil), &out, &errs); code != 0 {
		t.Fatalf("status: exit %d: %s %s", code, out.String(), errs.String())
	}
	got := out.String()
	for _, re := range []string{
		`(?m)^shelf: /opt/potluck-cache, \d+ bytes, \d+ files$`,
		`(?m)^fill: boxes$`,
		`(?m)^limits: shelf_max_gb=1 shelf_keep_s=\d+ shelf_fill_mbit=16 shelf_fill_delay_s=300 persist_min_free_gb=\d+ volume_max_gb=40$`,
		`(?m)^queue: 1 box files, 1 with unread wants, [1-9]\d* unread bytes of 67108864 allowed$`,
	} {
		if !regexp.MustCompile(re).MatchString(got) {
			t.Errorf("status lacks %s:\n%s", re, got)
		}
	}
	if strings.Contains(got+errs.String(), "left-pad") || len(blobsOn(t, vol)) != 1 {
		t.Errorf("status named a package or changed the shelf:\n%s", got)
	}

	// Filling off, and no volume: said plainly.
	if err := os.Remove(filepath.Join(dir, "cache-volume")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := hostCmd([]string{"shelf", "status"}, req(map[string]string{"shelf_max_gb": "0"}), &out, &errs); code != 0 {
		t.Fatalf("status without a volume: exit %d: %s", code, errs.String())
	}
	if got := out.String(); !strings.Contains(got, "shelf: no potluck-cache volume") || !strings.Contains(got, "fill: boxes (but shelf_max_gb is 0, so nothing fills)") {
		t.Errorf("no volume:\n%s", got)
	}

	// A binding that is not oci has no shelf; an argument is a usage error.
	out.Reset()
	errs.Reset()
	if code := hostCmd([]string{"shelf", "status"}, req(map[string]string{"vm": "tart", "shelf_max_gb": "0"}), &out, &errs); code != 1 || !strings.Contains(errs.String(), "has no shelf") {
		t.Errorf("vm=tart: exit %d %s", code, errs.String())
	}
	if code := hostCmd([]string{"shelf", "status", "x"}, req(nil), &out, &errs); code != 2 {
		t.Errorf("an argument: exit %d", code)
	}
}
