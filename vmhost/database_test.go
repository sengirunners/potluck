package vmhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

func databaseFixture(t *testing.T) *protocol.Database {
	t.Helper()
	b, err := os.ReadFile("../database/testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Database{Project: "pool", Name: "example-db", Image: "fake/linux@sha256:" + strings.Repeat("ab", 32), Manifest: string(b)}
}

func TestDatabaseBootstrapOracles(t *testing.T) {
	// The bootstrap runs in the guest, which supplies Python 3.12 or newer
	// (docs/database-snapshots.md); a host without one cannot run its oracles.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on this host")
	}
	if exec.Command(python, "-c", "import sys; sys.exit(sys.version_info < (3, 11))").Run() != nil {
		t.Skip("python3 on this host is older than 3.11 (no hashlib.file_digest)")
	}
	cmd := exec.Command(python, "database_start_test.py")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}
}

func TestDatabaseRefusesUnsupportedDriver(t *testing.T) {
	_, _, m := fakeBinding(t, "linux-img")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	db := databaseFixture(t)
	m["images"] = db.Image
	desc, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(anyStrings(desc["capabilities"]), ","), protocol.CapDatabase) {
		t.Fatal("fake VM advertised database support")
	}
	_, err = call(t, m, "acquire", "", map[string]any{"request_id": "database", "database": db})
	if code(err) != "bad_request" {
		t.Fatalf("unsupported driver: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(m["state_dir"], "runs"))
	if len(entries) != 0 {
		t.Fatal("refusal left a resource")
	}
}

// Existing driver state supplies a ready box, as after bootstrap. Exercise
// each public command path rather than inspecting generated shell text.
func TestDatabaseEnvironmentAndReadiness(t *testing.T) {
	m, h := fakeBox(t)
	st := store{m["state_dir"]}
	records := st.all()
	if len(records) != 1 {
		t.Fatalf("records: %d", len(records))
	}
	r := records[0]
	_, err := st.update(r.ID, func(r *record) error { r.Database = databaseFixture(t); r.State = stStarting; return nil })
	if err != nil {
		t.Fatal(err)
	}
	status, err := call(t, m, "status", h, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A box is never running (docs/protocol.md "States"): a starting database
	// box answers ready like any box, and exec waits for the database.
	if status["state"] != "ready" {
		t.Fatalf("starting database box answered %v, want ready", status["state"])
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"true"}}); code(err) != "conflict" {
		t.Fatalf("exec before ready: %v", err)
	}
	// fakeBox commands run in its own guest HOME, obtained without assuming a
	// host path. It is the same private user that would own the bootstrap file.
	_, err = st.update(r.ID, func(r *record) error { r.Database = nil; r.State = stReady; return nil })
	if err != nil {
		t.Fatal(err)
	}
	const url = "postgresql://postgres:private-test@127.0.0.1:5432/postgres"
	_, err = call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", "mkdir -m 700 \"$HOME/.potluck-database\"; printf '%s\\n' '" + url + "' >\"$HOME/.potluck-database/url\""}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.update(r.ID, func(r *record) error { r.Database = databaseFixture(t); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"present", "unset"} {
		if mode == "unset" {
			// Model the guest's passwd home without consulting or writing the host's
			// real home. Only the outer guest shell loses HOME; its cd fallback is
			// supplied by the fake, then nested command shells inherit the result.
			shim := `#!/bin/sh
if [ "${POTLUCK_TEST_HOME_REMOVED:-}" != 1 ]; then
 POTLUCK_TEST_HOME=$HOME; export POTLUCK_TEST_HOME
 POTLUCK_TEST_HOME_REMOVED=1; export POTLUCK_TEST_HOME_REMOVED
 unset HOME
 script=$2; shift 2; shift
 cd() { command cd "$POTLUCK_TEST_HOME"; }
 eval "$script"
else
 exec /bin/sh "$@"
fi
`
			path := filepath.Join(m["state_dir"], "fakevms", r.VM, "bin", "sh")
			if err := os.WriteFile(path, []byte(shim), 0700); err != nil {
				t.Fatal(err)
			}
		}
		args := []string{"sh", "-c", "[ \"$DB_URL\" = \"$DATABASE_URL\" ] || exit 7; printf '%s' \"$DATABASE_URL\""}
		result, err := call(t, m, "exec", h, map[string]any{"argv": args})
		if err != nil || result["stdout"] != url {
			t.Fatalf("exec env: %v %v", result, err)
		}
		for _, pty := range []bool{false, true} {
			request := map[string]any{"request_id": "database-plain" + mode, "argv": args}
			if pty {
				request["request_id"] = "database-pty" + mode
				request["pty"] = map[string]any{"rows": 24, "cols": 80}
			}
			spawned, err := call(t, m, "spawn", h, request)
			if err != nil {
				t.Fatal(err)
			}
			text := ""
			cursor := float64(0)
			for deadline := time.Now().Add(10 * time.Second); ; {
				out, err := call(t, m, "output", h, map[string]any{"pid": spawned["pid"], "stdout_from": cursor, "wait_s": 1})
				if err != nil {
					t.Fatal(err)
				}
				text += out["stdout"].(string)
				cursor = out["stdout_next"].(float64)
				if out["running"] == false {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("spawn did not finish")
				}
			}
			if text != url {
				t.Fatalf("pty=%v env=%q", pty, text)
			}
		}
	}
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseOCIStartupLifecycle(t *testing.T) {
	m, dir := shelfWarmBox(t)
	// This fake replaces only the bootstrap command's guest execution. The
	// bootstrap itself has independent executable PostgreSQL oracle tests.
	needle := `  if [ -n "$i" ]; then exec "$@"; else exec "$@" </dev/null; fi ;;`
	replacement := `  if [ "$1" = python3 ] && [ "$2" = - ]; then
    cat >/dev/null
    touch "$d/database-starting"
    while [ -e "$d/database-gate" ] && [ -e "$d/c/$n/up" ]; do sleep 0.05; done
    [ -e "$d/c/$n/up" ] || exit 1
    exit 0
  fi
` + needle
	if !strings.Contains(fakeOCI, needle) {
		t.Fatal("fake OCI exec boundary missing")
	}
	if err := os.WriteFile(m["oci_cli"], []byte(strings.Replace(fakeOCI, needle, replacement, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	// Populate one ordinary warm box first. A database acquire must make its
	// own fresh box even though the warm image is exactly the requested image.
	warm := readyWarm(t, m)
	db := databaseFixture(t)
	db.Image = m["images"]
	for _, path := range []string{protocol.DatabaseHome, protocol.DatabaseHome + "/data"} {
		_, err := call(t, m, "acquire", "", map[string]any{"request_id": "overlap", "database": db, "volumes": []volReq{{Name: "shared", Path: path}}, "actor": "tester"})
		if code(err) != "bad_request" || !strings.Contains(err.Error(), "overlaps private database") {
			t.Fatalf("database storage mount %s: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "database-gate"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		answer map[string]any
		err    error
	}
	done := make(chan result, 1)
	go func() {
		a, err := call(t, m, "acquire", "", map[string]any{"request_id": "db-start", "database": db, "ttl_s": 60})
		done <- result{a, err}
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "database-starting")); err == nil {
			break
		}
		select {
		case a := <-done:
			t.Fatalf("acquire ended before bootstrap gate: %v", a)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("database never reached startup")
		}
	}
	var r record
	for _, candidate := range (store{m["state_dir"]}).all() {
		if candidate.Database != nil {
			r = candidate
		}
	}
	if r.ID == "" || r.VM == warm {
		t.Fatal("database used ordinary warm box")
	}
	if _, err := call(t, m, "exec", r.Handle, map[string]any{"argv": []string{"true"}}); code(err) != "conflict" {
		t.Fatalf("exec during startup: %v", err)
	}
	if _, err := call(t, m, "release", r.Handle, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case stopped := <-done:
		if stopped.err == nil {
			t.Fatal("released database acquire answered success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("released startup acquire did not finish")
	}
	if _, err := os.Stat(filepath.Join(dir, "c", r.VM)); !os.IsNotExist(err) {
		t.Fatal("released database VM remains")
	}
	// A second request completes, confirms its exact identity, and refuses a
	// replay that changes the manifest. Persistent database boxes are refused.
	_ = os.Remove(filepath.Join(dir, "database-gate"))
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "db-ready", "database": db, "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	defer call(t, m, "release", h, nil)
	confirmed, ok := a["database"].(map[string]any)
	if !ok || confirmed["manifest"] != db.Manifest {
		t.Fatal("database provenance was not confirmed")
	}
	different := *db
	different.Manifest += "\n"
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "db-ready", "database": &different, "ttl_s": 60}); code(err) != "bad_request" {
		t.Fatalf("changed recipe replay: %v", err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "db-persist", "database": db, "persist": true}); code(err) != "bad_request" {
		t.Fatalf("persistent database: %v", err)
	}
}

func TestDatabaseArtifactCannotBeCoveredByVolume(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	if err := os.WriteFile(filepath.Join(dir, "images", "artifact"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "artifact.inspect"), []byte("runner\nHOME=/opt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"potluck-db", "potluck-db/pgdata.tar.gz"} {
		if err := o.CloneVolumes("artifact", "box", 1, 512, []mount{{Vol: "v1", Path: path, Mode: volRW}}); err == nil || !strings.Contains(err.Error(), "system directory") {
			t.Fatalf("artifact mount %s: %v", path, err)
		}
	}
}
