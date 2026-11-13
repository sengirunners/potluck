// Package runuser holds the tests of setup.sh, run against fake macOS tools (dscl, diskutil, ...) and a
// temporary directory standing in for /.
package runuser

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fakes keep their state under $FAKE: ds/<Users|Groups>/<name>/<key> for dscl, vol/<name> for APFS
// volumes, owners (path user lines) for chown, admins and procs for dseditgroup and ps.
var fakes = map[string]string{
	"uname": `echo Darwin`,
	"id":    `[ "$1" = -u ] && echo 0`,
	"dscl": `shift; op=$1 rec=$FAKE/ds$2; shift 2
case $op in
-read) [ -d "$rec" ] || exit 56; [ $# = 0 ] && exit 0; [ -f "$rec/$1" ] || exit 181; echo "$1: $(cat "$rec/$1")" ;;
-create) mkdir -p "$rec"; echo "$2" >"$rec/$1" ;;
-delete) [ -d "$rec" ] || exit 56; rm -rf "$rec" ;;
-list) for d in "$rec"/*; do [ -f "$d/$1" ] && echo "$(basename "$d") $(cat "$d/$1")"; done; exit 0 ;;
esac`,
	"dseditgroup": `grep -qx "$4" "$FAKE/admins" 2>/dev/null`,
	"ps":          `[ "$1" = -U ] && grep -qx "$2" "$FAKE/procs" 2>/dev/null && echo 4242; exit 0`,
	"sudo":        `[ "$1" = -u ] && shift 2 && exec "$@"`,
	"mdutil":      `exit 0`,
	"tmutil":      `exit 0`,
	"plutil":      `sed -n "s|.*<key>$2</key><string>\(.*\)</string>.*|\1|p" | grep .`,
	"chown": `[ "$1" = -h ] && shift; u=${1%%:*}; shift
for p; do grep -v "^$p " "$FAKE/owners" >"$FAKE/owners.n" 2>/dev/null; echo "$p $u" >>"$FAKE/owners.n"; mv "$FAKE/owners.n" "$FAKE/owners"; done`,
	"stat": `f=$2 p=$3
case $f in
%Su) awk -v p="$p" '$1 == p {print $2}' "$FAKE/owners" ;;
%Lp) ls -ld "$p" | awk '{m=0; for (i = 2; i <= 10; i++) { m = m * 2 + (substr($1, i, 1) != "-") } printf "%o\n", m}' ;;
esac`,
	"diskutil": `echo "$*" >>"$FAKE/diskutil.log"
plist() { printf '<dict><key>%s</key><string>%s</string></dict>\n' "$@"; }
vol() { n=${1#U-}; [ -f "$FAKE/vol/$n" ] || exit 1; }
case $1 in
info) n=$3
	if [ "$n" = /System/Volumes/Data ]; then plist APFSContainerReference disk3; exit 0; fi
	vol "$n"
	plist VolumeName "$n"; plist VolumeUUID "U-$n"; plist DeviceIdentifier "disk3s9"; plist FilesystemType apfs
	plist APFSContainerReference "$(sed -n 2p "$FAKE/vol/$n")"
	if [ "$(sed -n 1p "$FAKE/vol/$n")" = mounted ]; then plist MountPoint "$POTLUCK_SETUP_ROOT/Volumes/$n"; else plist MountPoint ""; fi ;;
mount) vol "$2"; printf 'mounted\n%s\n' "$(sed -n 2p "$FAKE/vol/$n")" >"$FAKE/vol/$n.n"; mv "$FAKE/vol/$n.n" "$FAKE/vol/$n"
	mkdir -p "$POTLUCK_SETUP_ROOT/Volumes/$n" ;;
apfs) case $2 in
	addVolume) mkdir -p "$FAKE/vol" "$POTLUCK_SETUP_ROOT/Volumes/$5"; printf 'mounted\n%s\n' "$3" >"$FAKE/vol/$5" ;;
	deleteVolume) [ "$3" = disk3s9 ] || exit 1; rm -f "$FAKE"/vol/*; rm -rf "$POTLUCK_SETUP_ROOT"/Volumes/* ;;
	esac ;;
esac`,
}

type mac struct {
	t          *testing.T
	root, fake string
	env        []string
}

func newMac(t *testing.T) *mac {
	t.Helper()
	m := &mac{t: t, root: t.TempDir(), fake: t.TempDir()}
	bin := filepath.Join(m.fake, "bin")
	for _, d := range []string{bin, m.root + "/Users/owner", m.root + "/var/db", m.root + "/Volumes"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m.env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE="+m.fake, "POTLUCK_SETUP_ROOT="+m.root)
	m.ds("/Users/owner", "UniqueID", "501")
	m.ds("/Users/owner", "NFSHomeDirectory", m.root+"/Users/owner")
	m.ds("/Groups/staff", "PrimaryGroupID", "20")
	m.ds("/Groups/taken", "PrimaryGroupID", "600")
	return m
}

func (m *mac) ds(rec, key, val string) {
	d := m.fake + "/ds" + rec
	if err := os.MkdirAll(d, 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(d+"/"+key, []byte(val+"\n"), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mac) run(args ...string) (string, error) {
	cmd := exec.Command("sh", append([]string{"setup.sh"}, args...)...)
	cmd.Env = m.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (m *mac) ok(args ...string) string {
	m.t.Helper()
	out, err := m.run(args...)
	if err != nil {
		m.t.Fatalf("setup.sh %v: %v\n%s", args, err, out)
	}
	return out
}

func (m *mac) exists(p string) bool { _, err := os.Lstat(m.root + p); return err == nil }

func (m *mac) mode(p string) os.FileMode {
	m.t.Helper()
	fi, err := os.Stat(m.root + p)
	if err != nil {
		m.t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func (m *mac) dsVal(rec, key string) string {
	b, _ := os.ReadFile(m.fake + "/ds" + rec + "/" + key)
	return strings.TrimSpace(string(b))
}

func TestSetupThenUndo(t *testing.T) {
	m := newMac(t)
	out := m.ok("--owner", "owner", "--quota", "200g", "--lock-owner-home")
	for _, want := range []string{"made group zmrun", "made user zmrun", "made volume zmrun in disk3", "undo with:"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if got := m.dsVal("/Users/zmrun", "UniqueID"); got != "601" {
		t.Errorf("uid %s, want 601, its group's (600 is taken)", got)
	}
	if m.dsVal("/Users/zmrun", "PrimaryGroupID") != m.dsVal("/Groups/zmrun", "PrimaryGroupID") {
		t.Error("user's primary group is not its own group")
	}
	for key, want := range map[string]string{"UserShell": "/usr/bin/false", "Password": "*", "IsHidden": "1"} {
		if got := m.dsVal("/Users/zmrun", key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if m.mode("/Users/zmrun") != 0o700 || m.mode("/Volumes/zmrun") != 0o700 || m.mode("/Users/owner") != 0o700 {
		t.Errorf("modes: home %o, volume %o, owner home %o; want 700 each",
			m.mode("/Users/zmrun"), m.mode("/Volumes/zmrun"), m.mode("/Users/owner"))
	}
	if l, err := os.Readlink(m.root + "/Users/zmrun/.tart"); err != nil || l != m.root+"/Volumes/zmrun/tart" {
		t.Errorf("~zmrun/.tart -> %q (%v), want the volume's tart dir", l, err)
	}
	log, _ := os.ReadFile(m.fake + "/diskutil.log")
	if !strings.Contains(string(log), "apfs addVolume disk3 APFS zmrun -quota 200g") {
		t.Errorf("addVolume call missing or wrong:\n%s", log)
	}

	if st, _ := os.ReadFile(m.root + "/var/db/potluck-run-user.zmrun"); !strings.Contains(string(st), "volume_uuid U-zmrun\n") {
		t.Errorf("state does not record the volume's UUID:\n%s", st)
	}
	if m.dsVal("/Users/zmrun", "Comment") != "potluck-run-user" || m.dsVal("/Groups/zmrun", "Comment") != "potluck-run-user" {
		t.Error("user or group lacks the script's mark")
	}

	// A run cut short (an attribute missing, the volume not mounted) is finished by the next one.
	for _, key := range []string{"UserShell", "NFSHomeDirectory"} {
		if err := os.Remove(m.fake + "/ds/Users/zmrun/" + key); err != nil {
			t.Fatal(err)
		}
	}
	m.file("vol/zmrun", "unmounted\ndisk3")
	out = m.ok("--owner", "owner", "--lock-owner-home")
	if strings.Contains(out, "made ") {
		t.Errorf("second run made something again:\n%s", out)
	}
	if m.dsVal("/Users/zmrun", "UserShell") != "/usr/bin/false" {
		t.Error("second run did not put the shell back")
	}
	if log, _ := os.ReadFile(m.fake + "/diskutil.log"); !strings.Contains(string(log), "mount U-zmrun") {
		t.Error("second run did not mount the volume")
	}

	m.ok("--undo")
	for _, p := range []string{"/Users/zmrun", "/Volumes/zmrun", "/var/db/potluck-run-user.zmrun"} {
		if m.exists(p) {
			t.Errorf("%s still there after --undo", p)
		}
	}
	for _, rec := range []string{"/Users/zmrun", "/Groups/zmrun"} {
		if _, err := os.Stat(m.fake + "/ds" + rec); err == nil {
			t.Errorf("%s still in the directory after --undo", rec)
		}
	}
	if m.mode("/Users/owner") != 0o755 {
		t.Errorf("owner home mode %o after --undo, want its old 755", m.mode("/Users/owner"))
	}
	if out := m.ok("--undo"); strings.Contains(out, "deleted") {
		t.Errorf("second --undo deleted something:\n%s", out)
	}
}

func TestUndoWithoutSetupLeavesThingsAlone(t *testing.T) {
	m := newMac(t)
	if err := os.MkdirAll(m.root+"/Users/zmrun", 0o755); err != nil {
		t.Fatal(err)
	}
	if out := m.ok("--undo"); !strings.Contains(out, "no record of a setup") {
		t.Errorf("want no record of a setup:\n%s", out)
	}
	if !m.exists("/Users/zmrun") {
		t.Error("--undo with no setup deleted a home it did not make")
	}
}

func TestUndoLeavesAHomeItDidNotLock(t *testing.T) {
	m := newMac(t)
	m.ok("--owner", "owner")
	if err := os.Chmod(m.root+"/Users/owner", 0o700); err != nil {
		t.Fatal(err)
	}
	m.ok("--undo")
	if m.mode("/Users/owner") != 0o700 {
		t.Errorf("undo changed a home mode it never set: %o", m.mode("/Users/owner"))
	}
}

func TestSetupRefuses(t *testing.T) {
	cases := []struct {
		name  string
		setup bool // a full setup runs first
		edit  func(m *mac)
		args  []string
		error string
	}{
		{"no owner", false, nil, nil, "--owner is required"},
		{"owner is the run user", false, nil, []string{"--owner", "zmrun", "--user", "zmrun"}, "must not be the owner"},
		{"unknown owner", false, nil, []string{"--owner", "nobody2"}, "names no user"},
		{"bad user name", false, nil, []string{"--owner", "owner", "--user", "Bad Name"}, "not a user name"},
		{"bad quota", false, nil, []string{"--owner", "owner", "--quota", "lots"}, "--quota is a size"},
		{"not root", false, func(m *mac) { m.setFake("id", `echo 501`) }, []string{"--owner", "owner"}, "run it with sudo"},
		{"run user is an admin", false, func(m *mac) { m.file("admins", "zmrun") }, []string{"--owner", "owner"}, "is an admin"},
		{"set up for someone else", false, func(m *mac) { m.rootFile("/var/db/potluck-run-user.zmrun", "owner other") },
			[]string{"--owner", "owner"}, "was set up for other"},
		{"user already there", false, func(m *mac) { m.ds("/Users/zmrun", "UniqueID", "700") },
			[]string{"--owner", "owner"}, "user zmrun already exists"},
		{"home already there", false, func(m *mac) { m.mkdir("/Users/zmrun") }, []string{"--owner", "owner"}, "already exists"},
		{"volume already there", false, func(m *mac) { m.file("vol/zmrun", "mounted\ndisk3") },
			[]string{"--owner", "owner"}, "volume zmrun already exists"},
		{"a dir where the volume mounts", false, func(m *mac) { m.mkdir("/Volumes/zmrun") },
			[]string{"--owner", "owner"}, "/Volumes/zmrun already exists"},
		{"rerun with another volume", true, nil, []string{"--owner", "owner", "--volume", "other"}, "was set up with volume zmrun"},
		{"~/.tart made a directory", true, func(m *mac) {
			if err := os.Remove(m.root + "/Users/zmrun/.tart"); err != nil {
				m.t.Fatal(err)
			}
			m.mkdir("/Users/zmrun/.tart")
		}, []string{"--owner", "owner"}, "is not a link"},
		{"undo the owner", false, nil, []string{"--undo", "--user", "owner"}, "was not made by this script"},
		{"undo naming a volume", true, nil, []string{"--undo", "--volume", "backup"}, "takes only --user"},
		{"undo a state naming the run user as owner", false,
			func(m *mac) { m.rootFile("/var/db/potluck-run-user.zmrun", "owner zmrun") }, []string{"--undo"}, "as the owner"},
		{"undo while running", true, func(m *mac) { m.file("procs", "601") }, []string{"--undo"}, "has processes running"},
		{"undo a volume moved to another container", true, func(m *mac) { m.file("vol/zmrun", "mounted\ndisk9") },
			[]string{"--undo"}, "no longer in the Data volume's container"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMac(t)
			if c.setup {
				m.ok("--owner", "owner")
			}
			if c.edit != nil {
				c.edit(m)
			}
			out, err := m.run(c.args...)
			if err == nil || !strings.Contains(out, c.error) {
				t.Fatalf("want a refusal with %q, got %v:\n%s", c.error, err, out)
			}
			if !m.exists("/Users/owner") || m.dsVal("/Users/owner", "UniqueID") != "501" {
				t.Error("the owner's account or home is gone")
			}
			if c.setup && (m.dsVal("/Users/zmrun", "UniqueID") == "" || !m.exists("/Users/zmrun")) {
				t.Error("deleted the run user before refusing")
			}
			if log, _ := os.ReadFile(m.fake + "/diskutil.log"); !c.setup && strings.Contains(string(log), "addVolume") {
				t.Error("made a volume before refusing")
			}
		})
	}
}

func (m *mac) mkdir(p string) {
	if err := os.MkdirAll(m.root+p, 0o755); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mac) file(name, body string) {
	p := m.fake + "/" + name
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body+"\n"), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mac) rootFile(p, body string) {
	if err := os.WriteFile(m.root+p, []byte(body+"\n"), 0o600); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mac) setFake(name, body string) {
	if err := os.WriteFile(m.fake+"/bin/"+name, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		m.t.Fatal(err)
	}
}
