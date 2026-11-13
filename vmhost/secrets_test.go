package vmhost

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// secretsBox is a box binding over the fake layer, its handle, and the
// fake VM's directory (its memory filesystem is shm in it).
func secretsBox(t *testing.T, edit func(m map[string]string), acquire map[string]any) (map[string]string, string, string) {
	t.Helper()
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	if edit != nil {
		edit(m)
	}
	a := map[string]any{"request_id": "k", "ttl_s": 120}
	for k, v := range acquire {
		a[k] = v
	}
	res, err := call(t, m, "acquire", "", a)
	if err != nil {
		t.Fatal(err)
	}
	h := res["handle"].(string)
	r, err := store{m["state_dir"]}.load(idOf(h))
	if err != nil {
		t.Fatal(err)
	}
	return m, h, dirLayer{filepath.Join(m["state_dir"], "fakevms")}.dir(r.VM)
}

const boxCanary = "zm-boxsecret-canary-5d1e0b"

// setSecrets sends one secrets call and wants it taken.
func setSecrets(t *testing.T, m map[string]string, h string, args map[string]any) map[string]any {
	t.Helper()
	res, err := call(t, m, "secrets", h, args)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	return res
}

// The secrets verb lands each value as a 0600 file in a 0700 directory on
// the guest's memory filesystem, which every later exec, spawn and terminal
// sees in POTLUCK_SECRETS; a renewal replaces it, unset removes one name;
// an exec's own secrets get a directory of their own and leave the box's in
// place; refusals name nothing; the value is in no answer and in no file of
// the driver's state outside the guest.
func TestBoxSecretsLanding(t *testing.T) {
	m, h, vmDir := secretsBox(t, nil, nil)
	d, err := call(t, m, "describe", "", nil)
	if err != nil || !strings.Contains(fmt.Sprint(d["capabilities"]), "box-secrets") || !strings.Contains(fmt.Sprint(d["verbs"]), "secrets") {
		t.Fatalf("describe lists no box-secrets: %v %v", d, err)
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", `test -z "${POTLUCK_SECRETS:-}${ZM_SECRETS:-}"`}}); err != nil {
		t.Fatal(err)
	}
	res := setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_A": boxCanary, "ZM_B": "bee"}})
	shm := filepath.Join(vmDir, "shm") // the fake guest's guestMem
	dir := shm + "/potluck-secrets"
	if res["dir"] != dir || res["count"] != 2.0 || strings.Contains(fmt.Sprint(res), boxCanary) {
		t.Fatalf("secrets answered %v, want dir %s and count 2, no value", res, dir)
	}
	exit, out := sh(t, m, h, `d=$POTLUCK_SECRETS; [ "$d" = "$ZM_SECRETS" ] || exit 9; printf '%s\n' "$d"; cat "$d/ZM_A"; echo; ls -ld "$d" | cut -c1-10; ls -l "$d/ZM_A" | cut -c1-10; env | grep -c canary`)
	if want := dir + "\n" + boxCanary + "\ndrwx------\n-rw-------\n0\n"; exit != 1 || out != want { // grep -c 0 exits 1
		t.Fatalf("a later exec: exit %v, %q, want %q", exit, out, want)
	}
	// A renewal replaces the value whole; unset removes one name.
	res = setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_A": "renewed"}, "unset": []string{"ZM_B"}})
	if res["count"] != 1.0 {
		t.Errorf("after a renewal and an unset: %v, want count 1", res)
	}
	if _, out := sh(t, m, h, `cat "$POTLUCK_SECRETS/ZM_A"; test -e "$POTLUCK_SECRETS/ZM_B" && echo B; ls -a "$POTLUCK_SECRETS" | grep -c '^\.ZM'`); out != "renewed0\n" {
		t.Errorf("after a renewal and an unset the directory reads %q", out)
	}
	// A spawned process and a terminal see it too.
	sp, err := call(t, m, "spawn", h, map[string]any{"request_id": "s1", "argv": []string{"sh", "-c", `wc -c < "$POTLUCK_SECRETS/ZM_A" | tr -d ' '`}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		o, err := call(t, m, "output", h, map[string]any{"pid": sp["pid"], "wait_s": 1})
		if err != nil {
			t.Fatal(err)
		}
		if o["running"] == false {
			if o["stdout"] != "7\n" { // its output is kept on the host: no value in it
				t.Errorf("a spawned process read %q", o["stdout"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the spawned process did not end")
		}
	}
	if argv := ttyWrapped(record{}, "", "pf", "xterm", []string{"true"}); argv[8] != guestMem || !strings.Contains(ttyWrap, `POTLUCK_SECRETS=$b`) {
		t.Errorf("a terminal is not told the box's secrets directory: %q", argv)
	}
	// An exec's own secrets: their own directory, gone after; the box's stays.
	ex, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", `printf '%s' "$POTLUCK_SECRETS"; cat "$POTLUCK_SECRETS/ZM_OWN" >/dev/null`},
		"secrets": map[string]string{"ZM_OWN": "own"}})
	if err != nil || ex["exit"] != 0.0 || ex["stdout"] == dir || !strings.HasPrefix(ex["stdout"].(string), shm+"/potlucksec.") {
		t.Errorf("an exec with its own secrets: %v %v", ex, err)
	}
	if _, out := sh(t, m, h, `cat "$POTLUCK_SECRETS/ZM_A"`); out != "renewed" {
		t.Errorf("after an exec's own secrets the box's read %q", out)
	}
	// Refusals: each names nothing sent, and changes nothing.
	for name, args := range map[string]map[string]any{
		"a bad name":     {"secrets": map[string]string{"zz_lower": "x"}},
		"a bad unset":    {"unset": []string{"ZZ BAD"}},
		"nothing to do":  {},
		"set and unset":  {"secrets": map[string]string{"ZZQ": "x"}, "unset": []string{"ZZQ"}},
		"over 64 KiB":    {"secrets": map[string]string{"ZZQ_BIG": strings.Repeat("y", 64<<10)}},
		"over 64 KiB in": {"secrets": map[string]string{"ZZQ_1": strings.Repeat("y", 40<<10)}, "unset": []string{"ZM_NONE"}},
		"a string":       {"secrets": "zzqvalue"},
	} {
		if name == "over 64 KiB in" { // within one call's cap, past the box's
			setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_C": strings.Repeat("c", 30<<10)}})
		}
		_, err := call(t, m, "secrets", h, args)
		if code(err) != "bad_request" || strings.Contains(err.Error(), "zz") || strings.Contains(err.Error(), "ZZ") || strings.Contains(err.Error(), "yyyy") {
			t.Errorf("%s: %v, want a bad_request naming nothing", name, err)
		}
	}
	if _, out := sh(t, m, h, `ls "$POTLUCK_SECRETS" | tr '\n' ' '; cat "$POTLUCK_SECRETS/ZM_A"`); out != "ZM_A ZM_C renewed" {
		t.Errorf("after the refusals the directory reads %q", out)
	}
	// Nothing outside the guest holds the value: records, logs, answers.
	_ = filepath.WalkDir(m["state_dir"], func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || strings.HasPrefix(p, shm+"/") {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && (bytes.Contains(b, []byte(boxCanary)) || bytes.Contains(b, []byte("renewed"))) {
			t.Errorf("%s holds a secret", p)
		}
		return nil
	})
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "secrets", h, map[string]any{"secrets": map[string]string{"ZM_A": "x"}}); code(err) != "conflict" {
		t.Errorf("secrets after release: %v, want conflict", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the secrets directory outlived the release: %v", err)
	}
}

// A guest without a memory filesystem refuses secrets, the box's and an
// exec's, and writes nothing anywhere; a macOS guest is never offered them.
func TestBoxSecretsNeedAMemoryFilesystem(t *testing.T) {
	m, h, vmDir := secretsBox(t, nil, nil)
	if err := os.WriteFile(filepath.Join(vmDir, "mounts"), []byte("/dev/disk1 / ext4 rw 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "secrets", h, map[string]any{"secrets": map[string]string{"ZM_A": boxCanary}}); code(err) != "forbidden" || strings.Contains(err.Error(), boxCanary) {
		t.Errorf("box secrets on a disk-only guest: %v, want forbidden", err)
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"touch", "ran"}, "secrets": map[string]string{"ZM_A": boxCanary}}); code(err) != "forbidden" {
		t.Errorf("exec secrets on a disk-only guest: %v, want forbidden", err)
	}
	if _, err := call(t, m, "spawn", h, map[string]any{"request_id": "s", "argv": []string{"touch", "ran"}, "secrets": map[string]string{"ZM_A": boxCanary}}); code(err) != "forbidden" {
		t.Errorf("spawn secrets on a disk-only guest: %v, want forbidden (an answer, not a process exiting 125)", err)
	}
	_ = filepath.WalkDir(vmDir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(boxCanary)) {
				t.Errorf("%s holds the secret", p)
			}
		}
		if err == nil && e.Name() == "ran" {
			t.Errorf("the command ran without its secrets: %s", p)
		}
		return nil
	})

	_, _, mac := fakeBinding(t, "mac-img")
	delete(mac, "image")
	mac["target"], mac["kind"], mac["macos_slots"], mac["size"], mac["cpu"], mac["memory"] = "t", "box", "2", "small", "1", "512"
	mac["images"] = "mac/img@sha256:" + strings.Repeat("ab", 32)
	d, err := call(t, mac, "describe", "", nil)
	if err != nil || strings.Contains(fmt.Sprint(d["capabilities"]), "secrets") || strings.Contains(fmt.Sprint(d["verbs"]), "secrets") {
		t.Errorf("a macOS binding offers secrets: %v %v", d, err)
	}
	a, err := call(t, mac, "acquire", "", map[string]any{"request_id": "m", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, mac, "secrets", a["handle"].(string), map[string]any{"secrets": map[string]string{"ZM_A": "x"}}); code(err) != "bad_request" {
		t.Errorf("box secrets in a macOS guest: %v, want bad_request (no capability listed)", err)
	}
	for _, verb := range []string{"exec", "spawn"} {
		if _, err := call(t, mac, verb, a["handle"].(string), map[string]any{"request_id": "s", "argv": []string{"true"}, "secrets": map[string]string{"ZM_A": "x"}}); code(err) != "bad_request" {
			t.Errorf("%s secrets in a macOS guest: %v, want bad_request (no capability listed)", verb, err)
		}
	}
	_, _ = call(t, mac, "release", a["handle"].(string), nil)
}

// The secrets directory cannot outlive the box, whichever way it ends: it
// lives in the guest's memory, which every end takes with the VM.
func TestBoxSecretsEndWithTheBox(t *testing.T) {
	window := func(m map[string]string) { // open now, closing in 3 s (local time)
		now := time.Now()
		m["window"] = now.Add(-time.Hour).Format("15:04:05") + "-" + now.Add(3*time.Second).Format("15:04:05")
		m["drain_deadline_s"] = "0" // leaving the window reclaims at once
	}
	persist := func(m map[string]string) { m["persist_max"] = "1" }
	for _, c := range []struct {
		name    string
		edit    func(map[string]string)
		acquire map[string]any
		end     func(t *testing.T, m map[string]string, h, vmDir string)
	}{
		{"release", nil, nil, func(t *testing.T, m map[string]string, h, _ string) {
			if _, err := call(t, m, "release", h, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"expiry", nil, map[string]any{"ttl_s": 3}, nil},
		{"reclaim", window, nil, nil},
		{"the supervisor's death (the layer's deadman)", nil, nil, func(t *testing.T, m map[string]string, h, _ string) {
			r, _ := store{m["state_dir"]}.load(idOf(h))
			_ = syscall.Kill(r.Supervisor, syscall.SIGKILL)
		}},
		{"a host restart (the sweep)", nil, nil, func(t *testing.T, m map[string]string, h, vmDir string) {
			st := store{m["state_dir"]}
			r, _ := st.load(idOf(h))
			// Everything dies at once and nothing runs its cleanup: the
			// supervisor stopped first, so it never sees the VM go.
			_ = syscall.Kill(r.Supervisor, syscall.SIGSTOP)
			if b, err := os.ReadFile(filepath.Join(vmDir, "boot")); err == nil {
				if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
			}
			_ = syscall.Kill(r.Supervisor, syscall.SIGKILL)
			if _, err := st.update(r.ID, func(o *record) error { o.Supervisor = deadPID(t); return nil }); err != nil {
				t.Fatal(err)
			}
			time.Sleep(300 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(vmDir, "shm", "potluck-secrets")); err != nil {
				t.Fatalf("before the sweep, the dead VM's memory is already gone: %v", err) // the case is not what it claims
			}
			if _, err := call(t, m, "list", "", nil); err != nil { // the next call's sweep
				t.Fatal(err)
			}
		}},
		{"a kept disk's stop", persist, map[string]any{"persist": true}, func(t *testing.T, m map[string]string, h, _ string) {
			if _, err := call(t, m, "stop", h, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"a kept disk's deadline", persist, map[string]any{"persist": true, "ttl_s": 3}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, h, vmDir := secretsBox(t, c.edit, c.acquire)
			setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_A": boxCanary}})
			dir := filepath.Join(vmDir, "shm", "potluck-secrets")
			if _, err := os.Stat(filepath.Join(dir, "ZM_A")); err != nil {
				t.Fatal(err)
			}
			if c.end != nil {
				c.end(t, m, h, vmDir)
			}
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
				_, _ = call(t, m, "status", h, nil)
				if _, err := os.Stat(dir); os.IsNotExist(err) {
					break
				}
				if time.Now().After(deadline) {
					s, _ := call(t, m, "status", h, nil)
					t.Fatalf("the secrets directory outlived the box (%v)", s)
				}
			}
			if c.acquire["persist"] == true { // a start boots an empty memory filesystem
				if _, err := call(t, m, "start", h, map[string]any{"ttl_s": 60}); err != nil {
					t.Fatal(err)
				}
				if _, out := sh(t, m, h, `printf '%s' "${POTLUCK_SECRETS:-none}"`); out != "none" {
					t.Errorf("a started box still has secrets at %q", out)
				}
				_, _ = call(t, m, "release", h, nil)
			}
		})
	}
}

// A guest that swaps to a disk takes no secrets (a tmpfs page can be
// swapped out); zram, compressed memory, is no disk.
func TestBoxSecretsRefuseSwap(t *testing.T) {
	m, h, vmDir := secretsBox(t, nil, nil)
	swaps := filepath.Join(vmDir, "swaps")
	disk := fakeSwaps + "/swapfile                               file\t\t4194300\t\t0\t\t-2\n"
	if err := os.WriteFile(swaps, []byte(disk), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "secrets", h, map[string]any{"secrets": map[string]string{"ZM_A": boxCanary}}); code(err) != "forbidden" || !strings.Contains(err.Error(), "swap") {
		t.Errorf("box secrets in a guest swapping to a disk: %v, want forbidden naming swap", err)
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"true"}, "secrets": map[string]string{"ZM_A": boxCanary}}); code(err) != "forbidden" || !strings.Contains(err.Error(), "swap") {
		t.Errorf("exec secrets in a guest swapping to a disk: %v, want forbidden naming swap", err)
	}
	if _, err := call(t, m, "spawn", h, map[string]any{"request_id": "s", "argv": []string{"true"}, "secrets": map[string]string{"ZM_A": boxCanary}}); code(err) != "forbidden" {
		t.Errorf("spawn secrets in a guest swapping to a disk: %v, want forbidden", err)
	}
	if _, err := os.Stat(filepath.Join(vmDir, "shm", "potluck-secrets")); !os.IsNotExist(err) {
		t.Errorf("a refused call made the secrets directory: %v", err)
	}
	if err := os.WriteFile(swaps, []byte(fakeSwaps+"/dev/zram0                              partition\t4194300\t\t0\t\t100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_A": boxCanary}})
	// No swap table at all: meminfo beside it must say SwapTotal 0.
	if err := os.Remove(swaps); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "secrets", h, map[string]any{"secrets": map[string]string{"ZM_A": "x"}}); code(err) != "forbidden" {
		t.Errorf("no swap table and no meminfo: %v, want forbidden", err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "meminfo"), []byte("MemTotal: 1 kB\nSwapTotal:             0 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_A": "x"}})
}

// Something planted at the secrets path is never used: a symlink is refused
// by the verb (its target gets nothing) and given to no later command; a
// write a killed call left behind is removed by the next call.
func TestBoxSecretsPlantedPath(t *testing.T) {
	m, h, vmDir := secretsBox(t, nil, nil)
	dir := filepath.Join(vmDir, "shm", "potluck-secrets")
	target := t.TempDir()
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "secrets", h, map[string]any{"secrets": map[string]string{"ZM_A": boxCanary}}); code(err) != "forbidden" {
		t.Errorf("a symlink at the secrets path: %v, want forbidden", err)
	}
	if es, _ := os.ReadDir(target); len(es) != 0 {
		t.Errorf("the symlink's target got %v", es)
	}
	if _, out := sh(t, m, h, `printf '%s' "${POTLUCK_SECRETS:-none}"`); out != "none" {
		t.Errorf("a later exec is pointed at a planted symlink: %q", out)
	}
	if strings.Count(execWrap, "POSIX::S_ISDIR($bs[2]) && $bs[4] == $<") != 1 || !strings.Contains(ttyWrap, `[ -O "$b" ]`) || !strings.Contains(secretsProg, "$st[4] == $<") {
		t.Error("a wrapper exports a secrets directory without checking its owner is the command's user")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_A": "one"}})
	left := filepath.Join(dir, ".ZM_A.4242")
	if err := os.WriteFile(left, []byte(boxCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	setSecrets(t, m, h, map[string]any{"secrets": map[string]string{"ZM_B": "two"}})
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Errorf("a killed call's write is still there: %v", err)
	}
}

// Calls take turns in the guest: one waits while another holds the lock,
// so an earlier call the host gave up on cannot land after a later one.
func TestBoxSecretsCallsTakeTurns(t *testing.T) {
	m, h, vmDir := secretsBox(t, nil, nil)
	f, err := os.OpenFile(filepath.Join(vmDir, "shm", ".potluck-secrets.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := call(t, m, "secrets", h, map[string]any{"secrets": map[string]string{"ZM_A": "later"}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a call went ahead while another held the lock: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, out := sh(t, m, h, `cat "$POTLUCK_SECRETS/ZM_A"`); out != "later" {
		t.Errorf("after the lock: %q", out)
	}
}
