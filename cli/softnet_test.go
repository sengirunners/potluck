package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/softnet"
)

// softnetMac fakes a Mac for potluck softnet: its macOS, Tart (with the
// run options Potluck needs) and a pinned test build whose executable is
// a script printing its version and accepting the rules.
type softnetMac struct {
	t         *testing.T
	macOS     string
	build     softnet.Build
	file      string // the build's file, as a download would give it
	store     softnet.Store
	sudo      [][]string // each admin step's command line
	ran       []string   // every command the fake Mac ran
	noTart    bool
	asRoot    bool
	refuses   bool   // the build refuses Potluck's rules
	downloads string // where softnet install --no-sudo keeps its download
}

func newSoftnetMac(t *testing.T, macOS string) *softnetMac {
	m := &softnetMac{t: t, macOS: macOS}
	content := "#!/bin/sh\necho test\n"
	sum := sha256.Sum256([]byte(content))
	m.build = softnet.Build{ID: "0.23.0-test.1", Version: "softnet 0.23.0-test.1-abc", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)),
		Arch: "arm64", MinMacOS: 15, MaxMacOS: 15, LicenseSHA256: softnet.Builds[0].LicenseSHA256, URL: "unset"}
	m.file = filepath.Join(t.TempDir(), "softnet-download")
	if err := os.WriteFile(m.file, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	top := t.TempDir()
	m.store = softnet.Store{Dir: filepath.Join(top, "Softnet"), Owner: os.Geteuid(), Group: os.Getegid(), Top: top}
	oldBuilds, oldStore, oldGOOS, oldRunner, oldFacts, oldEUID, oldSudo := softnet.Builds, softnetStore, softnetGOOS, softnetRunner, softnetFacts, softnetEUID, softnetSudo
	t.Cleanup(func() {
		softnet.Builds, softnetStore, softnetGOOS, softnetRunner, softnetFacts, softnetEUID, softnetSudo = oldBuilds, oldStore, oldGOOS, oldRunner, oldFacts, oldEUID, oldSudo
	})
	softnet.Builds = []softnet.Build{m.build}
	softnetStore = func() softnet.Store { return m.store }
	softnetGOOS = "darwin"
	softnetRunner = runnerFunc(m.run)
	softnetFacts = m
	softnetEUID = func() int {
		if m.asRoot {
			return 0
		}
		return 1000
	}
	softnetSudo = func(_ context.Context, exe string, args []string, _, _ io.Writer) error {
		m.sudo = append(m.sudo, append([]string{exe}, args...))
		return nil
	}
	m.downloads = filepath.Join(t.TempDir(), "Caches", "potluck", "softnet")
	oldDownloads := softnetDownloadDir
	softnetDownloadDir = func() (string, error) { return m.downloads, nil }
	t.Cleanup(func() { softnetDownloadDir = oldDownloads })
	return m
}

func (m *softnetMac) run(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	m.ran = append(m.ran, line)
	switch {
	case line == "/usr/sbin/sysctl -n hw.optional.arm64":
		return hostprobe.Result{Stdout: "1\n"}, nil
	case line == "/usr/bin/sw_vers -productVersion":
		return hostprobe.Result{Stdout: m.macOS + "\n"}, nil
	case line == "/fake/tart --version":
		return hostprobe.Result{Stdout: "2.42.0\n"}, nil
	case line == "/fake/tart run --help":
		return hostprobe.Result{Stdout: "--no-graphics --no-clipboard --no-audio --net-softnet --net-softnet-allow --net-softnet-block --root-disk-opts\n"}, nil
	case strings.HasSuffix(name, "/softnet") && line == name+" --version":
		if sum, _, err := softnet.FileSHA256(name); err == nil {
			if b, ok := softnet.BySHA256(sum); ok {
				return hostprobe.Result{Stdout: b.Version + "\n"}, nil
			}
		}
		return hostprobe.Result{Stdout: m.build.Version + "\n"}, nil
	case strings.HasSuffix(name, "/softnet") && slices.Contains(args, "--sudo-escalation-probing"):
		if m.refuses {
			return hostprobe.Result{Exit: 2, Stderr: "error: invalid value"}, nil
		}
		return hostprobe.Result{}, nil
	case strings.HasPrefix(line, "stat -L -f %Sp %Su "):
		return hostprobe.Result{Stdout: "-rwsr-xr-x root\n"}, nil
	}
	return hostprobe.Result{Exit: 127, Stderr: "not faked: " + line}, nil
}

// The fake Mac's facts: Tart on PATH, and the store's managed Softnet.
func (m *softnetMac) OS() string                         { return "darwin" }
func (m *softnetMac) Arch() string                       { return "arm64" }
func (m *softnetMac) Cores() int                         { return 8 }
func (m *softnetMac) RAMMB() int64                       { return 16384 }
func (m *softnetMac) FreeDiskGB(string) (float64, error) { return 500, nil }
func (m *softnetMac) Exists(string) bool                 { return false }
func (m *softnetMac) OpenRW(string) error                { return os.ErrNotExist }
func (m *softnetMac) OCIEngine(string, string) error     { return os.ErrNotExist }
func (m *softnetMac) LookPath(n string) (string, bool) {
	if n == "tart" && !m.noTart {
		return "/fake/tart", true
	}
	if n == "softnet" {
		return "/opt/homebrew/bin/softnet", true
	}
	return "", false
}
func (m *softnetMac) ManagedSoftnet() (string, softnet.Status, bool) {
	if _, err := os.Stat(m.store.Current()); err != nil {
		return "", softnet.Status{}, false
	}
	return m.store.Current(), m.store.Status(), true
}

func softnetRun(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errs bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &errs)
	return out.String() + errs.String(), code
}

func TestSoftnetInstallRoutesEachMacToItsSoftnet(t *testing.T) {
	for _, c := range []struct{ macOS, says string }{
		{"26.1", "upstream Softnet supports"},
		{"14.7", "no Softnet known"},
		{"15.6", "--candidate"},
	} {
		m := newSoftnetMac(t, c.macOS)
		out, code := softnetRun(t, "softnet", "install", "--from", m.file)
		if code == 0 || !strings.Contains(out, c.says) || len(m.sudo) != 0 {
			t.Errorf("macOS %s: exit %d, %d admin steps: %s", c.macOS, code, len(m.sudo), out)
		}
	}
	m := newSoftnetMac(t, "15.6")
	softnetGOOS = "linux"
	if out, code := softnetRun(t, "softnet", "install"); code == 0 || !strings.Contains(out, "on a Mac") {
		t.Errorf("on Linux: exit %d: %s", code, out)
	}
	_ = m
}

func TestSoftnetInstallChecksTheFileAndTartBeforeTheAdminStep(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	bad := filepath.Join(t.TempDir(), "softnet")
	os.WriteFile(bad, []byte("#!/bin/sh\necho other\n"), 0o755)
	if out, code := softnetRun(t, "softnet", "install", "--candidate", "--from", bad); code == 0 || !strings.Contains(out, "is not Softnet 0.23.0-test.1") {
		t.Fatalf("wrong file: exit %d: %s", code, out)
	}
	m.noTart = true
	if out, code := softnetRun(t, "softnet", "install", "--candidate", "--from", m.file); code == 0 || !strings.Contains(out, "tart is not installed") {
		t.Fatalf("no Tart: exit %d: %s", code, out)
	}
	if len(m.sudo) != 0 {
		t.Fatalf("admin step ran: %v", m.sudo)
	}
}

func TestSoftnetInstallRunsItsAdminStepThroughSudo(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	out, code := softnetRun(t, "softnet", "install", "--candidate", "--from", m.file)
	// The fake sudo installs nothing, which the user step notices.
	if code == 0 || len(m.sudo) != 1 || !strings.Contains(out, "no managed Softnet is in use") {
		t.Fatalf("exit %d, admin steps %v: %s", code, m.sudo, out)
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	want := []string{exe, "softnet", "install", "--admin-step", "--from", m.file, "--candidate"}
	if !slices.Equal(m.sudo[0], want) {
		t.Fatalf("admin step %q, want %q", m.sudo[0], want)
	}
	if !strings.Contains(out, "sudo asks for your Mac password once") || !strings.Contains(out, "not yet validated") {
		t.Fatalf("admin step not explained: %s", out)
	}
}

func TestSoftnetAdminStepInstallsUpgradesAndRollsBack(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	m.asRoot = true
	if out, code := softnetRun(t, "softnet", "install", "--candidate", "--from", m.file); code == 0 || !strings.Contains(out, "not as root") {
		t.Fatalf("user step as root: exit %d: %s", code, out)
	}
	out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", m.file)
	if code != 0 || !strings.Contains(out, "Softnet 0.23.0-test.1 is in use at "+m.store.Current()) {
		t.Fatalf("admin step: exit %d: %s", code, out)
	}
	m.asRoot = false
	// Potluck's boxes now run the managed copy, not Homebrew's first on PATH.
	out, code = softnetRun(t, "softnet", "status")
	if code != 0 || !strings.Contains(out, "Potluck's boxes run: Softnet 0.23.0-test.1-abc at "+m.store.Current()) ||
		!strings.Contains(out, "the other softnet (/opt/homebrew/bin/softnet) is not the one they run") {
		t.Fatalf("status after install: exit %d: %s", code, out)
	}
	if out, code := softnetRun(t, "softnet", "install", "--candidate"); code != 0 || !strings.Contains(out, "nothing to do") || len(m.sudo) != 0 {
		t.Fatalf("second install: exit %d, %v: %s", code, m.sudo, out)
	}
	if out, code := softnetRun(t, "softnet", "rollback"); code == 0 || !strings.Contains(out, "no earlier managed Softnet") {
		t.Fatalf("rollback with one build: exit %d: %s", code, out)
	}
	if out, code := softnetRun(t, "softnet", "status"); code != 0 || !strings.Contains(out, "managed, in use: 0.23.0-test.1") {
		t.Fatalf("status: exit %d: %s", code, out)
	}
}

func TestSoftnetAdminStepRefusesABuildThatRefusesTheRules(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	m.asRoot, m.refuses = true, true
	out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", m.file)
	if code == 0 || !strings.Contains(out, "refuses Potluck's isolation rules") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if st := m.store.Status(); st.Current != nil {
		t.Fatalf("a refusing build is in use: %+v", st.Current)
	}
	// The admin step itself runs no Tart.
	for _, l := range m.ran {
		if strings.Contains(l, "tart") {
			t.Fatalf("admin step ran %q", l)
		}
	}
}

func TestSoftnetInstallSaysWhenTheBuildIsNotPublished(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	m.build.URL = srv.URL + "/softnet"
	softnet.Builds = []softnet.Build{m.build}
	out, code := softnetRun(t, "softnet", "install", "--candidate")
	if code == 0 || !strings.Contains(out, "not published at "+m.build.URL+" yet") || !strings.Contains(out, "--from") || len(m.sudo) != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestSoftnetAdminStepRollsBack(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	m.asRoot = true
	content := "#!/bin/sh\necho older\n"
	sum := sha256.Sum256([]byte(content))
	older := m.build
	older.ID, older.Version, older.SHA256, older.Bytes = "0.23.0-test.0", "softnet 0.23.0-test.0-abc", hex.EncodeToString(sum[:]), int64(len(content))
	olderFile := filepath.Join(t.TempDir(), "softnet")
	os.WriteFile(olderFile, []byte(content), 0o755)
	softnet.Builds = []softnet.Build{older}
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", olderFile); code != 0 {
		t.Fatalf("older: exit %d: %s", code, out)
	}
	softnet.Builds = []softnet.Build{m.build, older}
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", m.file); code != 0 || !strings.Contains(out, "it replaced 0.23.0-test.0") {
		t.Fatalf("newer: exit %d: %s", code, out)
	}
	m.asRoot = false
	// The fake sudo changes nothing, which the user step notices.
	if out, code := softnetRun(t, "softnet", "rollback"); code == 0 || len(m.sudo) != 1 || !slices.Contains(m.sudo[0], "--admin-step") || !strings.Contains(out, "is 0.23.0-test.1, not 0.23.0-test.0") {
		t.Fatalf("rollback's admin step: exit %d %v: %s", code, m.sudo, out)
	}
	m.asRoot = true
	if out, code := softnetRun(t, "softnet", "rollback", "--admin-step"); code != 0 || !strings.Contains(out, "Softnet 0.23.0-test.0 is in use at "+m.store.Current()+" again") {
		t.Fatalf("rollback: exit %d: %s", code, out)
	}
	if st := m.store.Status(); st.Current.ID != older.ID || st.Previous.ID != m.build.ID {
		t.Fatalf("after rollback: %+v %+v", st.Current, st.Previous)
	}
}

// The admin step always asks for the password: sudo -k ignores a cached
// sign-in.
func TestSoftnetSudoIgnoresACachedSignIn(t *testing.T) {
	got := sudoArgv("/x/potluck", []string{"softnet", "install", "--admin-step"})
	want := []string{"/usr/bin/sudo", "-k", "--", "/x/potluck", "softnet", "install", "--admin-step"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv %q, want %q", got, want)
	}
}

func TestSoftnetUninstallDisarmsAndRemovesTheManagedInstall(t *testing.T) {
	m := newSoftnetMac(t, "26.1") // a Mac upgraded past the build's macOS uninstalls it the same way
	if out, code := softnetRun(t, "softnet", "uninstall"); code != 0 || !strings.Contains(out, "nothing to do") || len(m.sudo) != 0 {
		t.Fatalf("nothing installed: exit %d: %s", code, out)
	}
	m.macOS = "15.6"
	m.asRoot = true
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", m.file); code != 0 {
		t.Fatalf("install: exit %d: %s", code, out)
	}
	m.macOS = "26.1"
	if out, code := softnetRun(t, "softnet", "status"); code != 0 || !strings.Contains(out, "is not for this Mac's macOS (26.1); Potluck's boxes do not use it") ||
		!strings.Contains(out, "Potluck's boxes run: Softnet 0.23.0-test.1-abc at /opt/homebrew/bin/softnet") {
		t.Fatalf("status on macOS 26: exit %d: %s", code, out)
	}
	m.asRoot = false
	if out, code := softnetRun(t, "softnet", "uninstall"); code == 0 || len(m.sudo) != 1 || !slices.Equal(m.sudo[0][1:], []string{"softnet", "uninstall", "--admin-step"}) || !strings.Contains(out, "still there") {
		t.Fatalf("user step: exit %d %v: %s", code, m.sudo, out)
	}
	m.asRoot = true
	if out, code := softnetRun(t, "softnet", "uninstall", "--admin-step"); code != 0 || !strings.Contains(out, "removed the managed Softnet") {
		t.Fatalf("admin step: exit %d: %s", code, out)
	}
	if _, err := os.Lstat(m.store.Dir); !os.IsNotExist(err) {
		t.Fatalf("store still there: %v", err)
	}
}

// Rollback's admin step never puts back a build that is not for this
// Mac's macOS.
func TestSoftnetRollbackRefusesABuildForAnotherMacOS(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	m.asRoot = true
	content := "#!/bin/sh\necho older\n"
	sum := sha256.Sum256([]byte(content))
	older := m.build
	older.ID, older.Version, older.SHA256, older.Bytes = "0.23.0-test.0", "softnet 0.23.0-test.0-abc", hex.EncodeToString(sum[:]), int64(len(content))
	older.MinMacOS, older.MaxMacOS = 14, 14
	olderFile := filepath.Join(t.TempDir(), "softnet")
	os.WriteFile(olderFile, []byte(content), 0o755)
	m.macOS = "14.7"
	softnet.Builds = []softnet.Build{older}
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", olderFile); code != 0 {
		t.Fatalf("older: exit %d: %s", code, out)
	}
	m.macOS = "15.6"
	softnet.Builds = []softnet.Build{m.build, older}
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", m.file); code != 0 {
		t.Fatalf("newer: exit %d: %s", code, out)
	}
	if out, code := softnetRun(t, "softnet", "rollback", "--admin-step"); code == 0 || !strings.Contains(out, "not built for this Mac (macOS 15.6") {
		t.Fatalf("rollback to a macOS 14 build: exit %d: %s", code, out)
	}
}

// softnetRunJSON runs potluck with --json and returns stdout (the answer)
// apart from the rest.
func softnetRunJSON(t *testing.T, args ...string) (answer []byte, words string, code int) {
	t.Helper()
	var out, errs bytes.Buffer
	code = Run(context.Background(), append([]string{"--json"}, args...), strings.NewReader(""), &out, &errs)
	return out.Bytes(), errs.String(), code
}

// adminStepOf reads {"admin_step": ...} from a --json answer.
func adminStepOf(t *testing.T, answer []byte) adminStep {
	t.Helper()
	var a struct {
		AdminStep *adminStep `json:"admin_step"`
	}
	if err := json.Unmarshal(answer, &a); err != nil || a.AdminStep == nil {
		t.Fatalf("no admin_step in %s (%v)", answer, err)
	}
	return *a.AdminStep
}

// rootHalf runs a step's argv as the fake Mac's root would: this program's
// words after argv[0], as root.
func (m *softnetMac) rootHalf(t *testing.T, s adminStep) string {
	t.Helper()
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if len(s.Argv) < 2 || s.Argv[0] != exe || !filepath.IsAbs(s.Argv[0]) {
		t.Fatalf("argv %q does not start with this program %s", s.Argv, exe)
	}
	m.asRoot = true
	defer func() { m.asRoot = false }()
	out, code := softnetRun(t, s.Argv[1:]...)
	if code != 0 {
		t.Fatalf("root half %q: exit %d: %s", s.Argv, code, out)
	}
	return out
}

// --no-sudo checks and downloads as the person, keeps the download for the
// root half, answers the root step as data and runs no sudo; the answer's
// argv is what the root half accepts; then install says nothing to do and
// drops the kept download.
func TestSoftnetInstallNoSudoAnswersTheAdminStep(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	body, _ := os.ReadFile(m.file)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	m.build.URL = srv.URL + "/softnet"
	softnet.Builds = []softnet.Build{m.build}

	answer, words, code := softnetRunJSON(t, "softnet", "install", "--candidate", "--no-sudo")
	if code != 0 || len(m.sudo) != 0 {
		t.Fatalf("exit %d, sudo %v: %s %s", code, m.sudo, answer, words)
	}
	step := adminStepOf(t, answer)
	kept := filepath.Join(m.downloads, softnet.AssetName(m.build.ID, m.build.Arch))
	if want := []string{"softnet", "install", "--admin-step", "--from", kept, "--candidate"}; !slices.Equal(step.Argv[1:], want) {
		t.Fatalf("argv %q, want %q after the program", step.Argv, want)
	}
	if !strings.Contains(step.Why, "owned by root and setuid") || !strings.Contains(words, "downloading Softnet") {
		t.Fatalf("why %q, words %s", step.Why, words)
	}
	if fi, err := os.Stat(filepath.Dir(kept)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("download directory: %v %v", fi, err)
	}
	if out := m.rootHalf(t, step); !strings.Contains(out, "Softnet 0.23.0-test.1 is in use at "+m.store.Current()) {
		t.Fatalf("root half: %s", out)
	}
	answer, words, code = softnetRunJSON(t, "softnet", "install", "--candidate", "--no-sudo")
	if code != 0 || strings.TrimSpace(string(answer)) != "{}" || !strings.Contains(words, "nothing to do") || len(m.sudo) != 0 {
		t.Fatalf("second run: exit %d: %s %s", code, answer, words)
	}
	if _, err := os.Lstat(kept); !os.IsNotExist(err) {
		t.Fatalf("the kept download is still there: %v", err)
	}
}

// Without --json, --no-sudo says the root command, quoted for a shell.
func TestSoftnetInstallNoSudoSaysTheRootCommand(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	from := filepath.Join(t.TempDir(), "my softnet")
	b, _ := os.ReadFile(m.file)
	os.WriteFile(from, b, 0o755)
	out, code := softnetRun(t, "softnet", "install", "--candidate", "--no-sudo", "--from", from)
	if code != 0 || len(m.sudo) != 0 || !strings.Contains(out, "Run this as root") || !strings.Contains(out, "--from '"+from+"'") {
		t.Fatalf("exit %d, sudo %v: %s", code, m.sudo, out)
	}
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--no-sudo", "--from", from); code == 0 || !strings.Contains(out, "--no-sudo is for the run as yourself") {
		t.Fatalf("--admin-step --no-sudo: exit %d: %s", code, out)
	}
}

// softnet uninstall and rollback hand their root step back the same way.
func TestSoftnetUninstallAndRollbackNoSudo(t *testing.T) {
	m := newSoftnetMac(t, "15.6")
	content := "#!/bin/sh\necho older\n"
	sum := sha256.Sum256([]byte(content))
	older := m.build
	older.ID, older.Version, older.SHA256, older.Bytes = "0.23.0-test.0", "softnet 0.23.0-test.0-abc", hex.EncodeToString(sum[:]), int64(len(content))
	olderFile := filepath.Join(t.TempDir(), "softnet")
	os.WriteFile(olderFile, []byte(content), 0o755)
	m.asRoot = true
	for _, c := range []struct {
		file   string
		builds []softnet.Build
	}{{olderFile, []softnet.Build{older}}, {m.file, []softnet.Build{m.build, older}}} {
		softnet.Builds = c.builds
		if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", c.file); code != 0 {
			t.Fatalf("install %s: exit %d: %s", c.file, code, out)
		}
	}
	m.asRoot = false

	answer, words, code := softnetRunJSON(t, "softnet", "rollback", "--no-sudo")
	if code != 0 {
		t.Fatalf("rollback: exit %d: %s", code, words)
	}
	m.rootHalf(t, adminStepOf(t, answer))
	if cur := m.store.Status().Current; cur == nil || cur.ID != older.ID {
		t.Fatalf("after rollback: %+v", cur)
	}

	answer, words, code = softnetRunJSON(t, "softnet", "uninstall", "--no-sudo")
	if code != 0 {
		t.Fatalf("uninstall: exit %d: %s", code, words)
	}
	step := adminStepOf(t, answer)
	if !slices.Equal(step.Argv[1:], []string{"softnet", "uninstall", "--admin-step"}) {
		t.Fatalf("argv %q", step.Argv)
	}
	m.rootHalf(t, step)
	if out, code := softnetRun(t, "softnet", "uninstall", "--no-sudo"); code != 0 || !strings.Contains(out, "nothing to do") {
		t.Fatalf("second uninstall: exit %d: %s", code, out)
	}
	if len(m.sudo) != 0 {
		t.Fatalf("sudo ran: %v", m.sudo)
	}
}

// upstreamArchive makes m's build an upstream release for macOS 26 and
// newer, published as a gzipped tar served by a test server, and returns
// the archive.
func (m *softnetMac) upstreamArchive(t *testing.T, serve func([]byte) []byte) []byte {
	t.Helper()
	exe, _ := os.ReadFile(m.file)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "LICENSE", Mode: 0o644, Size: int64(len(softnet.License)), Typeflag: tar.TypeReg})
	tw.Write([]byte(softnet.License))
	tw.WriteHeader(&tar.Header{Name: "softnet", Mode: 0o755, Size: int64(len(exe)), Typeflag: tar.TypeReg})
	tw.Write(exe)
	tw.Close()
	zw.Close()
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(serve(data)) }))
	t.Cleanup(srv.Close)
	m.build.MinMacOS, m.build.MaxMacOS, m.build.UpstreamRelease = 26, softnet.AnyNewer, true
	m.build.URL = srv.URL + "/softnet.tar.gz"
	m.build.Archive = &softnet.Archive{SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)), Member: "softnet"}
	softnet.Builds = []softnet.Build{m.build}
	return data
}

// On macOS 26 the pinned upstream release installs like Potluck's own
// builds: no --candidate, the archive downloaded, checked and unpacked as
// the person, one root step that the root half accepts, then nothing to do.
func TestSoftnetInstallOnMacOS26UnpacksThePinnedUpstreamRelease(t *testing.T) {
	m := newSoftnetMac(t, "26.1")
	m.upstreamArchive(t, func(d []byte) []byte { return d })
	answer, words, code := softnetRunJSON(t, "softnet", "install", "--no-sudo")
	if code != 0 || len(m.sudo) != 0 || strings.Contains(words, "candidate") {
		t.Fatalf("exit %d, sudo %v: %s %s", code, m.sudo, answer, words)
	}
	step := adminStepOf(t, answer)
	kept := filepath.Join(m.downloads, softnet.AssetName(m.build.ID, m.build.Arch))
	if want := []string{"softnet", "install", "--admin-step", "--from", kept}; !slices.Equal(step.Argv[1:], want) {
		t.Fatalf("argv %q, want %q after the program", step.Argv, want)
	}
	if es, _ := os.ReadDir(m.downloads); len(es) != 1 {
		t.Fatalf("the download directory holds %d files, want the executable alone", len(es))
	}
	if out := m.rootHalf(t, step); !strings.Contains(out, "Softnet 0.23.0-test.1 is in use at "+m.store.Current()) {
		t.Fatalf("root half: %s", out)
	}
	answer, words, code = softnetRunJSON(t, "softnet", "install", "--no-sudo")
	if code != 0 || strings.TrimSpace(string(answer)) != "{}" || !strings.Contains(words, "nothing to do") {
		t.Fatalf("second run: exit %d: %s %s", code, answer, words)
	}
	if out, code := softnetRun(t, "softnet", "status"); code != 0 || !strings.Contains(out, "Potluck's boxes run: Softnet 0.23.0-test.1-abc at "+m.store.Current()) {
		t.Fatalf("status: exit %d: %s", code, out)
	}
}

// An archive that is not the pinned one is refused before it is unpacked,
// with no root step and nothing kept.
func TestSoftnetInstallOnMacOS26RefusesAnotherArchive(t *testing.T) {
	m := newSoftnetMac(t, "26.1")
	m.upstreamArchive(t, func(d []byte) []byte {
		d = bytes.Clone(d)
		d[len(d)-10] ^= 1
		return d
	})
	out, code := softnetRun(t, "softnet", "install")
	if code == 0 || len(m.sudo) != 0 || !strings.Contains(out, "is not Softnet 0.23.0-test.1's archive") {
		t.Fatalf("exit %d, sudo %v: %s", code, m.sudo, out)
	}
	if _, err := os.Lstat(m.store.Dir); !os.IsNotExist(err) {
		t.Fatalf("the store was made: %v", err)
	}
}

// A macOS 26 Mac that set up upstream Softnet itself, owned by root and
// setuid, keeps using it: the pinned release is offered, never required.
func TestSoftnetStatusOnMacOS26AcceptsASoftnetSetUpAsRoot(t *testing.T) {
	m := newSoftnetMac(t, "26.1")
	m.upstreamArchive(t, func(d []byte) []byte { return d })
	out, code := softnetRun(t, "softnet", "status")
	if code != 0 || !strings.Contains(out, "Potluck's boxes run: Softnet 0.23.0-test.1-abc at /opt/homebrew/bin/softnet") ||
		strings.Contains(out, "problem:") || !strings.Contains(out, "with `potluck softnet install`") {
		t.Fatalf("exit %d: %s", code, out)
	}
}
