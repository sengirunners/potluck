package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/updatejob"
)

// jobEnv is a machine for the update job: a temp HOME, a temp potluck home
// and install directory, and a recording stand-in for launchctl/systemctl.
// No test writes or loads a real job.
type jobEnv struct {
	t      *testing.T
	home   string
	bin    string
	calls  []string
	loaded bool
	failOn string // a recorded call that fails
}

func newJobEnv(t *testing.T, goos string) *jobEnv {
	t.Helper()
	e := &jobEnv{t: t, home: t.TempDir(), bin: filepath.Join(t.TempDir(), "bin")}
	if err := os.MkdirAll(e.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv(EnvHome, filepath.Join(e.home, ".potluck"))
	t.Setenv("ZM_MACHINE", "job-test")
	oldExe, oldNew := executable, newUpdateSchedule
	t.Cleanup(func() { executable, newUpdateSchedule = oldExe, oldNew })
	executable = func() (string, error) { return filepath.Join(e.bin, "potluck"), nil }
	newUpdateSchedule = func() (*updatejob.Schedule, error) {
		return &updatejob.Schedule{
			Names: updateNames, GOOS: goos, UserHome: e.home, UID: 501,
			Wait:   func(_ time.Duration) {},
			Linger: func() (bool, error) { return false, nil },
			Run: func(name string, args ...string) error {
				call := name + " " + strings.Join(args, " ")
				e.calls = append(e.calls, call)
				if e.failOn != "" && strings.Contains(call, e.failOn) {
					return errors.New("refused")
				}
				switch {
				case strings.HasPrefix(call, "launchctl print"), strings.Contains(call, "is-enabled"):
					if !e.loaded {
						return errors.New("not loaded")
					}
				case strings.Contains(call, "bootstrap"), strings.Contains(call, " enable "):
					e.loaded = true
				case strings.Contains(call, "bootout"), strings.Contains(call, "disable"):
					e.loaded = false
				}
				return nil
			},
		}, nil
	}
	return e
}

func (e *jobEnv) potluck(args ...string) (string, int) {
	e.t.Helper()
	var o, er bytes.Buffer
	code := Run(context.Background(), append([]string{"upgrade"}, args...), strings.NewReader(""), &o, &er)
	return o.String() + er.String(), code
}

func (e *jobEnv) count(sub string) int {
	n := 0
	for _, c := range e.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func (e *jobEnv) plist() string {
	return filepath.Join(e.home, "Library", "LaunchAgents", "sh.potluck.update.plist")
}

func TestUpgradeEnableWritesAndLoadsOnceOnMac(t *testing.T) {
	e := newJobEnv(t, "darwin")
	out, code := e.potluck("enable")
	if code != 0 || !strings.Contains(out, "potluck upgrade disable") {
		t.Fatalf("exit %d: %s", code, out)
	}
	b, err := os.ReadFile(e.plist())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>" + filepath.Join(e.bin, "potluck") + "</string>", "<string>upgrade</string>", "<string>--dir</string>", "<string>" + e.bin + "</string>",
		"<key>POTLUCK_HOME</key><string>" + filepath.Join(e.home, ".potluck") + "</string>",
		"<key>StandardOutPath</key><string>" + filepath.Join(e.home, ".potluck", "update.log") + "</string>",
		"<key>Hour</key><integer>3</integer>", "Written by potluck",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("plist lacks %q:\n%s", want, b)
		}
	}
	if e.count("bootstrap") != 1 {
		t.Errorf("loaded %d times: %v", e.count("bootstrap"), e.calls)
	}
}

func TestUpgradeEnableTwiceReplacesItsOwn(t *testing.T) {
	e := newJobEnv(t, "darwin")
	for range 2 {
		if out, code := e.potluck("enable"); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
	}
	if e.count("bootout") != 2 || e.count("bootstrap") != 2 || !e.loaded {
		t.Errorf("calls %v", e.calls)
	}
	names, _ := filepath.Glob(filepath.Join(e.home, "Library", "LaunchAgents", "*"))
	if len(names) != 1 {
		t.Errorf("files %v", names)
	}
}

func TestUpgradeEnableLeavesAForeignUpdaterAlone(t *testing.T) {
	e := newJobEnv(t, "darwin")
	dir := filepath.Dir(e.plist())
	os.MkdirAll(dir, 0o755)
	foreign := filepath.Join(dir, "com.example.potluck-up.plist")
	body := "<plist><dict><key>ProgramArguments</key><array><string>/opt/bin/potluck</string><string>upgrade</string></array></dict></plist>"
	os.WriteFile(foreign, []byte(body), 0o644)
	out, code := e.potluck("enable")
	if code != 0 || !strings.Contains(out, "com.example.potluck-up.plist") || !strings.Contains(out, "left it as it is") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(e.plist()); err == nil || len(e.calls) != 0 {
		t.Errorf("wrote or loaded beside a foreign updater: %v", e.calls)
	}
	out, _ = e.potluck("disable")
	if strings.Contains(out, "removed") || !strings.Contains(out, "com.example.potluck-up.plist") {
		t.Errorf("disable: %s", out)
	}
	if got, _ := os.ReadFile(foreign); string(got) != body {
		t.Error("the foreign updater changed")
	}
}

func TestUpgradeDisableRemovesOnlyItsOwn(t *testing.T) {
	e := newJobEnv(t, "darwin")
	e.potluck("enable")
	other := filepath.Join(filepath.Dir(e.plist()), "com.example.other.plist")
	os.WriteFile(other, []byte("<plist/>"), 0o644)
	out, code := e.potluck("disable")
	if code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(e.plist()); err == nil {
		t.Error("its own job is still there")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another plist was removed")
	}
	if out, _ = e.potluck("disable"); !strings.Contains(out, "had no daily update") {
		t.Errorf("second disable: %s", out)
	}
}

func TestUpgradeStatus(t *testing.T) {
	e := newJobEnv(t, "darwin")
	if out, _ := e.potluck("status"); !strings.Contains(out, "off") || strings.Contains(out, "last run") {
		t.Errorf("off: %s", out)
	}
	e.potluck("enable")
	if out, _ := e.potluck("status"); !strings.Contains(out, "on (") || !strings.Contains(out, "sh.potluck.update.plist") || strings.Contains(out, "last run") {
		t.Errorf("on: %s", out)
	}
	logf := filepath.Join(e.home, ".potluck", "update.log")
	os.MkdirAll(filepath.Dir(logf), 0o755)
	os.WriteFile(logf, []byte("verified potluck x\npotluck 1.2 is already installed in ~/bin\n"), 0o644)
	if out, _ := e.potluck("status"); !strings.Contains(out, "last run: ") || !strings.HasSuffix(strings.TrimSpace(out), "potluck 1.2 is already installed in ~/bin") {
		t.Errorf("with a log: %s", out)
	}
	if out, _ := e.potluck("--json", "status"); !strings.Contains(out, `"enabled": true`) && !strings.Contains(out, `"enabled":true`) {
		t.Errorf("json: %s", out)
	}
	e.loaded = false
	if out, _ := e.potluck("status"); !strings.Contains(out, "not loaded") {
		t.Errorf("unloaded: %s", out)
	}
}

func TestUpgradeEnableOnLinux(t *testing.T) {
	e := newJobEnv(t, "linux")
	out, code := e.potluck("enable")
	if code != 0 || !strings.Contains(out, "enable-linger") {
		t.Fatalf("exit %d: %s", code, out)
	}
	dir := filepath.Join(e.home, ".config", "systemd", "user")
	svc, _ := os.ReadFile(filepath.Join(dir, "potluck-update.service"))
	tm, _ := os.ReadFile(filepath.Join(dir, "potluck-update.timer"))
	if !strings.Contains(string(svc), "ExecStart="+filepath.Join(e.bin, "potluck")+" upgrade --dir "+e.bin+"\n") ||
		!strings.Contains(string(svc), "Environment=POTLUCK_HOME="+filepath.Join(e.home, ".potluck")) ||
		!strings.Contains(string(tm), "OnCalendar=*-*-* 03:") {
		t.Errorf("service:\n%s\ntimer:\n%s", svc, tm)
	}
	if e.count("--user enable potluck-update.timer") != 1 || e.count("--user restart potluck-update.timer") != 1 {
		t.Errorf("calls %v", e.calls)
	}
	if out, _ := e.potluck("disable"); !strings.Contains(out, "removed") {
		t.Errorf("disable: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "potluck-update.timer")); err == nil {
		t.Error("timer left behind")
	}
}

func TestUpgradeEnableRefusesAnUnwritableInstallDir(t *testing.T) {
	e := newJobEnv(t, "darwin")
	executable = func() (string, error) { return filepath.Join(e.bin, "missing", "potluck"), nil }
	out, code := e.potluck("enable")
	if code == 0 || !strings.Contains(out, "cannot update itself") || len(e.calls) != 0 {
		t.Fatalf("exit %d: %s %v", code, out, e.calls)
	}
}

func TestUpgradeEnableReportsALoadFailure(t *testing.T) {
	e := newJobEnv(t, "darwin")
	e.failOn = "bootstrap"
	if out, code := e.potluck("enable"); code == 0 || !strings.Contains(out, "refused") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// A word that is not a subcommand is refused, not taken as an argument.
func TestUpgradeRefusesAnUnknownSubcommand(t *testing.T) {
	e := newJobEnv(t, "darwin")
	if out, code := e.potluck("bogus"); code == 0 {
		t.Fatalf("unknown subcommand accepted: %s", out)
	}
}

// A machine whose home does not exist yet (no daemon has run) still gets a
// log directory: the job opens its log before upgrade runs.
func TestUpgradeEnableCreatesTheHomeForTheLog(t *testing.T) {
	e := newJobEnv(t, "darwin")
	t.Setenv("DOCKER_HOST", "unix:///run/docker.sock")
	t.Setenv("TS_AUTHKEY", "tskey-secret")
	if out, code := e.potluck("enable"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if fi, err := os.Stat(filepath.Join(e.home, ".potluck")); err != nil || !fi.IsDir() {
		t.Errorf("home: %v", err)
	}
	b, _ := os.ReadFile(e.plist())
	if !strings.Contains(string(b), "<key>DOCKER_HOST</key><string>unix:///run/docker.sock</string>") {
		t.Errorf("DOCKER_HOST did not travel:\n%s", b)
	}
	if strings.Contains(string(b), "tskey-secret") || strings.Contains(string(b), "TS_AUTHKEY") {
		t.Errorf("a secret was written to the plist:\n%s", b)
	}
}
