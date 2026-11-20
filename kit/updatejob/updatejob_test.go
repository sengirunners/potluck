package updatejob

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

var testNames = Names{
	Label: "sh.example.update", Service: "example-update.service", Timer: "example-update.timer",
	Mark: "Written by example (example upgrade enable)", Remove: "example upgrade disable",
	Installs: "the newest example release", Program: "example", Foreign: []string{"upgrade"},
}

type recorder struct {
	calls []string
	fail  string
}

func (r *recorder) run(name string, args ...string) error {
	c := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, c)
	if r.fail != "" && strings.Contains(c, r.fail) {
		return errors.New("refused")
	}
	return nil
}

func newSchedule(t *testing.T, goos string) (*Schedule, *recorder) {
	t.Helper()
	r := &recorder{}
	return &Schedule{Names: testNames, GOOS: goos, UserHome: t.TempDir(), UID: 7, Run: r.run, Wait: func(time.Duration) {}}, r
}

var spec = Spec{
	Argv:   []string{"/opt/my bin/example", "upgrade", "--dir", "/opt/my bin"},
	Env:    []EnvVar{{"PATH", "/opt/my bin:/usr/bin"}, {"EXAMPLE_HOME", "/h/$x%y"}},
	Log:    "/h/update.log",
	Minute: 17,
}

func TestPlistText(t *testing.T) {
	s, _ := newSchedule(t, "darwin")
	got := s.plist(Spec{Argv: []string{"/a&b/example", "up<grade"}, Env: []EnvVar{{"K", `"v"`}}, Log: "/l.log", Minute: 5})
	for _, want := range []string{
		"<!-- " + testNames.Mark + "; example upgrade disable removes it. -->",
		"<key>Label</key><string>sh.example.update</string>",
		"<string>/a&amp;b/example</string>", "<string>up&lt;grade</string>",
		"<key>K</key><string>&#34;v&#34;</string>",
		"<key>Minute</key><integer>5</integer>",
		"<key>StandardOutPath</key><string>/l.log</string>", "<key>StandardErrorPath</key><string>/l.log</string>",
		"<key>AbandonProcessGroup</key><true/>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(s.plist(Spec{Argv: []string{"x"}}), "StandardOutPath") {
		t.Error("a log path appears with none given")
	}
}

func TestSystemdText(t *testing.T) {
	s, _ := newSchedule(t, "linux")
	svc := s.service(spec)
	for _, want := range []string{
		"# " + testNames.Mark + "; example upgrade disable removes it.",
		`ExecStart="/opt/my bin/example" upgrade --dir "/opt/my bin"` + "\n",
		`Environment="PATH=/opt/my bin:/usr/bin"` + "\n",
		`Environment="EXAMPLE_HOME=/h/$x%%y"` + "\n", // % is a specifier; $ only expands in ExecStart
		"StandardOutput=append:/h/update.log\n", "KillMode=process",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service lacks %q:\n%s", want, svc)
		}
	}
	if tm := s.timer(spec); !strings.Contains(tm, "OnCalendar=*-*-* 03:17:00\n") || !strings.Contains(tm, "Persistent=true") {
		t.Errorf("timer:\n%s", tm)
	}
	if strings.Contains(s.service(Spec{Argv: []string{"x"}}), "StandardOutput") {
		t.Error("a log appears with none given")
	}
}

func TestUnitQuote(t *testing.T) {
	for _, c := range []struct {
		in   string
		exec bool
		want string
	}{
		{"plain", true, "plain"}, {"", true, `""`}, {"a b", true, `"a b"`}, {"it's", true, `"it's"`},
		{`a"b`, true, `"a\"b"`}, {`a\b`, true, `"a\\b"`}, {"50%", true, `"50%%"`},
		{"$HOME", true, `"$$HOME"`}, {"$HOME", false, "$HOME"}, {";", true, `";"`}, {"a;", true, "a;"},
	} {
		if got := unitQuote(c.in, c.exec); got != c.want {
			t.Errorf("unitQuote(%q, %v) = %s, want %s", c.in, c.exec, got, c.want)
		}
	}
}

func TestPathAndMinute(t *testing.T) {
	got := Path("/i", "/usr/bin:rel:/a\"b:/i:/x:/usr/local/bin")
	if want := "/i:/usr/bin:/x:/usr/local/bin:/bin"; got != want {
		t.Errorf("Path = %s, want %s", got, want)
	}
	if m := Minute("some-machine"); m < 0 || m > 59 || m != Minute("some-machine") {
		t.Errorf("Minute = %d", m)
	}
}

func TestForeignDetection(t *testing.T) {
	plist := func(args ...string) string {
		b := "<plist><dict><key>ProgramArguments</key>\n<array>\n"
		for _, a := range args {
			b += "<string>" + a + "</string>\n"
		}
		return b + "</array></dict></plist>"
	}
	for _, goos := range []string{"darwin", "linux"} {
		not := map[string][]string{ // other programs' upgrades, and look-alikes
			"brew":       {"/opt/homebrew/bin/brew", "upgrade"},
			"apt":        {"/usr/bin/apt-get", "upgrade", "-y"},
			"zm":         {"/x/zm", "daemon", "update"},
			"other-name": {"/x/example-other", "upgrade"},
			"upgrader":   {"/x/example", "upgrader"},
		}
		yes := map[string][]string{
			"plain":    {"/x/example", "upgrade", "--dir", "/x"},
			"flags":    {"/x/example", "--json", "upgrade"},
			"quoted":   {"/bin/sh", "-c", "example upgrade"},
			"relative": {"example", "upgrade"},
		}
		write := func(s *Schedule, name string, args []string) {
			os.MkdirAll(s.unitDir(), 0o755)
			body := "[Service]\nExecStart=" + strings.Join(args, " ") + "\n"
			ext := ".service"
			if goos == "darwin" {
				body, ext = plist(args...), ".plist"
			}
			os.WriteFile(filepath.Join(s.unitDir(), name+ext), []byte(body), 0o644)
		}
		for name, args := range not {
			s, _ := newSchedule(t, goos)
			write(s, name, args)
			if f := s.Foreign(); f != "" {
				t.Errorf("%s: %s is not a foreign updater", goos, f)
			}
		}
		for name, args := range yes {
			s, _ := newSchedule(t, goos)
			write(s, name, args)
			if f := s.Foreign(); !strings.HasPrefix(filepath.Base(f), name) {
				t.Errorf("%s: %s: foreign = %q", goos, name, f)
			}
		}
	}
}

// Its own file names that something else wrote are foreign, and neither
// install nor remove touches them.
func TestAFileAtItsNameThatIsNotItsOwnIsLeftAlone(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		s, r := newSchedule(t, goos)
		os.MkdirAll(s.unitDir(), 0o755)
		theirs := s.Files()[0]
		os.WriteFile(theirs, []byte("not ours"), 0o644)
		res, err := s.Install(spec)
		if err != nil || res.Foreign != theirs || len(r.calls) != 0 {
			t.Fatalf("%s: install = %+v, %v, calls %v", goos, res, err, r.calls)
		}
		removed, foreign, err := s.Remove()
		if removed || foreign != theirs || err != nil || len(r.calls) != 0 {
			t.Fatalf("%s: remove = %v %q %v, calls %v", goos, removed, foreign, err, r.calls)
		}
		if b, _ := os.ReadFile(theirs); string(b) != "not ours" {
			t.Errorf("%s: the file changed", goos)
		}
	}
}

func TestInstallAndRemoveCalls(t *testing.T) {
	s, r := newSchedule(t, "linux")
	if res, err := s.Install(spec); err != nil || res.Replaced {
		t.Fatalf("install = %+v, %v", res, err)
	}
	if want := []string{"systemctl --user daemon-reload", "systemctl --user enable example-update.timer", "systemctl --user restart example-update.timer"}; !slices.Equal(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}
	if res, _ := s.Install(spec); !res.Replaced {
		t.Error("a second install does not say it replaced the first")
	}
	if !s.Installed() {
		t.Error("not installed")
	}
	r.calls = nil
	if removed, _, err := s.Remove(); !removed || err != nil || s.Installed() {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if want := []string{"systemctl --user disable --now example-update.timer", "systemctl --user daemon-reload"}; !slices.Equal(r.calls, want) {
		t.Errorf("calls %v", r.calls)
	}

	m, mr := newSchedule(t, "darwin")
	mr.fail = "bootstrap"
	if _, err := m.Install(spec); err == nil {
		t.Fatal("a refused bootstrap is not an error")
	}
	if n := strings.Count(strings.Join(mr.calls, "\n"), "bootstrap"); n != 3 {
		t.Errorf("tried to load %d times", n)
	}
}

func TestUnsupportedSystem(t *testing.T) {
	s, _ := newSchedule(t, "windows")
	if _, err := s.Install(spec); err == nil || !strings.Contains(err.Error(), "windows") {
		t.Errorf("install on windows: %v", err)
	}
	if _, _, err := s.Remove(); err == nil {
		t.Error("remove on windows")
	}
}

func TestLingerNote(t *testing.T) {
	s, _ := newSchedule(t, "linux")
	if s.LingerNote("me") != "" {
		t.Error("a note with no linger check")
	}
	s.Linger = func() (bool, error) { return false, nil }
	if !strings.Contains(s.LingerNote("me"), "enable-linger") {
		t.Error("no note when linger is off")
	}
	s.Linger = func() (bool, error) { return true, nil }
	if s.LingerNote("me") != "" {
		t.Error("a note when linger is on")
	}
}

func TestRemoveSaysWhenTheJobStaysLoaded(t *testing.T) {
	s, r := newSchedule(t, "darwin")
	if _, err := s.Install(spec); err != nil {
		t.Fatal(err)
	}
	r.fail = "bootout" // the job is still there when print asks
	if removed, _, err := s.Remove(); !removed || err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Errorf("remove = %v, %v", removed, err)
	}
}

func TestBootoutComesBeforeBootstrap(t *testing.T) {
	s, r := newSchedule(t, "darwin")
	s.Install(spec)
	if want := []string{"launchctl bootout gui/7/sh.example.update"}; !strings.HasPrefix(r.calls[0], want[0]) || !strings.HasPrefix(r.calls[1], "launchctl bootstrap gui/7 ") {
		t.Errorf("calls %v", r.calls)
	}
}

func TestUnitLogPathIsNotQuoted(t *testing.T) {
	s, _ := newSchedule(t, "linux")
	got := s.service(Spec{Argv: []string{"x"}, Log: "/h m/50%/update.log"})
	if !strings.Contains(got, "StandardOutput=append:/h m/50%%/update.log\n") {
		t.Errorf("service:\n%s", got)
	}
}

func TestInstallLeavesNoTempFile(t *testing.T) {
	s, _ := newSchedule(t, "linux")
	s.Install(spec)
	names, _ := filepath.Glob(filepath.Join(s.unitDir(), ".*"))
	if len(names) != 0 {
		t.Errorf("left behind: %v", names)
	}
}

func TestStartRunsTheJobOnceWithoutWaiting(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":  "systemctl --user start --no-block example-update.service",
		"darwin": "launchctl kickstart gui/7/sh.example.update",
	} {
		s, r := newSchedule(t, goos)
		if _, err := s.Install(spec); err != nil {
			t.Fatal(err)
		}
		r.calls = nil
		if err := s.Start(); err != nil {
			t.Fatalf("%s: start: %v", goos, err)
		}
		if !slices.Equal(r.calls, []string{want}) {
			t.Errorf("%s: calls %v, want %q", goos, r.calls, want)
		}
	}
}

func TestStartAndArgvWithNoJob(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		s, r := newSchedule(t, goos)
		if err := s.Start(); !errors.Is(err, ErrNoJob) {
			t.Errorf("%s: start = %v, want ErrNoJob", goos, err)
		}
		if len(r.calls) != 0 {
			t.Errorf("%s: calls %v with no job", goos, r.calls)
		}
		if argv, err := s.Argv(); !errors.Is(err, ErrNoJob) || argv != nil {
			t.Errorf("%s: argv = %q, %v, want ErrNoJob", goos, argv, err)
		}
	}
}

func TestStartAndArgvOnUnsupportedSystem(t *testing.T) {
	s, r := newSchedule(t, "windows")
	want := s.Supported()
	if err := s.Start(); err == nil || err.Error() != want.Error() || len(r.calls) != 0 {
		t.Errorf("start = %v, calls %v", err, r.calls)
	}
	if _, err := s.Argv(); err == nil || err.Error() != want.Error() {
		t.Errorf("argv = %v", err)
	}
}

func TestArgvOfAJobThisPackageWrote(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		s, _ := newSchedule(t, goos)
		sp := spec
		sp.Argv = append(slices.Clone(spec.Argv), "--note", `50% of $HOME's "x" \ y`, "", ";")
		if _, err := s.Install(sp); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Argv(); err != nil || !slices.Equal(got, sp.Argv) {
			t.Errorf("%s: argv = %q, %v, want %q", goos, got, err, sp.Argv)
		}
	}
}

// A job at the program's names that someone wrote by hand carries no mark;
// it is still the machine's update job: Start starts it and Argv reads it.
func TestHandWrittenJob(t *testing.T) {
	for _, c := range []struct {
		goos, body, start string
		want              []string
	}{
		{
			goos: "linux",
			body: "[Unit]\nDescription=mine\n[Service]\nType=oneshot\nExecStart=/bin/false\nExecStart=\n" +
				"ExecStart=-'/opt/my tools/example' upgrade --min-age 72h \"--label=a b\" 100%%\n",
			start: "systemctl --user start --no-block example-update.service",
			want:  []string{"/opt/my tools/example", "upgrade", "--min-age", "72h", "--label=a b", "100%"},
		},
		{
			goos: "darwin",
			body: "<?xml version=\"1.0\"?>\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>sh.example.update</string>\n" +
				"<key>ProgramArguments</key>\n<array>\n\t<string>/Users/x/my tools/example</string>\n\t<string>upgrade</string>\n" +
				"\t<string>--tag=a&amp;b</string>\n</array>\n</dict></plist>\n",
			start: "launchctl kickstart gui/7/sh.example.update",
			want:  []string{"/Users/x/my tools/example", "upgrade", "--tag=a&b"},
		},
	} {
		s, r := newSchedule(t, c.goos)
		os.MkdirAll(s.unitDir(), 0o755)
		if err := os.WriteFile(s.Files()[0], []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if s.Installed() {
			t.Fatalf("%s: a hand-written job counts as this package's", c.goos)
		}
		if got, err := s.Argv(); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: argv = %q, %v, want %q", c.goos, got, err, c.want)
		}
		if err := s.Start(); err != nil || !slices.Equal(r.calls, []string{c.start}) {
			t.Errorf("%s: start = %v, calls %v", c.goos, err, r.calls)
		}
	}
}

// Escapes and continued lines that people write in units by hand.
func TestArgvOfAHandWrittenUnitReadsAsSystemdDoes(t *testing.T) {
	for body, want := range map[string][]string{
		`ExecStart=/opt/my\x20tools/example upgrade`:            {"/opt/my tools/example", "upgrade"},
		`ExecStart=/opt/my\040tools/example up\sgrade`:          {"/opt/my tools/example", "up grade"},
		"ExecStart=/x/example \\\n  upgrade --min-age 72h":      {"/x/example", "upgrade", "--min-age", "72h"},
		`ExecStart=@/x/example example upgrade "a;" \; b`:       {"example", "upgrade", "a;", ";", "b"},
		"ExecStart=/x/example 'it''s' \"tab\\there\" \\\\ \\\"": {"/x/example", "its", "tab\there", `\`, `"`},
	} {
		s, _ := newSchedule(t, "linux")
		os.MkdirAll(s.unitDir(), 0o755)
		os.WriteFile(s.Files()[0], []byte("[Service]\n"+body+"\n"), 0o644)
		if got, err := s.Argv(); err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: argv = %q, %v, want %q", body, got, err, want)
		}
	}
}

func TestArgvRefusesAServiceWithNoOrManyCommands(t *testing.T) {
	for name, body := range map[string]string{
		"none":      "[Service]\nType=oneshot\n",
		"two":       "[Service]\nExecStart=/x/example upgrade\nExecStart=/x/other\n",
		"separated": "[Service]\nExecStart=/x/example upgrade ; /x/other\n",
		"escape":    "[Service]\nExecStart=/x/example \\q\n",
		"quote":     "[Service]\nExecStart=/x/example \"upgrade\n",
		"backslash": "[Service]\nExecStart=/x/example \\",
	} {
		s, _ := newSchedule(t, "linux")
		os.MkdirAll(s.unitDir(), 0o755)
		os.WriteFile(s.Files()[0], []byte(body), 0o644)
		if argv, err := s.Argv(); err == nil || errors.Is(err, ErrNoJob) {
			t.Errorf("%s: argv = %q, %v", name, argv, err)
		}
	}
}

// A job file that is not a plain file of a job's size is refused before it
// is read: a pipe at the job's name would hold the reader for good.
func TestArgvRefusesWhatIsNotAPlainJobFile(t *testing.T) {
	link := func(put func(string) error) func(*testing.T, string) error {
		return func(t *testing.T, path string) error {
			to := filepath.Join(t.TempDir(), "target")
			if err := put(to); err != nil {
				return err
			}
			return os.Symlink(to, path)
		}
	}
	direct := func(put func(string) error) func(*testing.T, string) error {
		return func(_ *testing.T, path string) error { return put(path) }
	}
	pipe := func(path string) error { return syscall.Mkfifo(path, 0o644) }
	dir := func(path string) error { return os.Mkdir(path, 0o755) }
	big := func(path string) error { return os.WriteFile(path, make([]byte, jobFileMax+1), 0o644) }
	for _, goos := range []string{"linux", "darwin"} {
		for name, put := range map[string]func(*testing.T, string) error{
			"pipe": direct(pipe), "directory": direct(dir), "too big": direct(big),
			"link to a pipe": link(pipe), "link to a directory": link(dir), "link to a file too big": link(big),
			"link to a device": func(_ *testing.T, path string) error { return os.Symlink("/dev/zero", path) },
		} {
			s, _ := newSchedule(t, goos)
			if err := os.MkdirAll(s.unitDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := put(t, s.Files()[0]); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := s.Argv(); done <- err }()
			select {
			case err := <-done:
				if err == nil || errors.Is(err, ErrNoJob) {
					t.Errorf("%s, %s: argv error = %v, want a refusal that is not ErrNoJob", goos, name, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s, %s: Argv did not return", goos, name)
			}
		}
	}
}

// A link to a unit elsewhere is read as the unit: an owner may keep their
// own job file in another place. A link to nothing is no job.
func TestArgvFollowsALinkToAJobFile(t *testing.T) {
	s, _ := newSchedule(t, "linux")
	if err := os.MkdirAll(s.unitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "own.service")
	if err := os.Symlink(real, s.Files()[0]); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Argv(); !errors.Is(err, ErrNoJob) || got != nil {
		t.Errorf("a link to nothing: argv = %q, %v, want ErrNoJob", got, err)
	}
	if err := os.WriteFile(real, []byte("[Service]\nExecStart=/x/example upgrade\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Argv(); err != nil || !slices.Equal(got, []string{"/x/example", "upgrade"}) {
		t.Errorf("argv = %q, %v", got, err)
	}
}

// A job file of exactly the largest size is still read.
func TestArgvReadsAJobFileOfTheLargestSize(t *testing.T) {
	s, _ := newSchedule(t, "linux")
	if err := os.MkdirAll(s.unitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[Service]\nExecStart=/x/example upgrade\n"
	body += "#" + strings.Repeat("x", jobFileMax-len(body)-2) + "\n"
	if len(body) != jobFileMax {
		t.Fatalf("body is %d bytes", len(body))
	}
	if err := os.WriteFile(s.Files()[0], []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Argv(); err != nil || !slices.Equal(got, []string{"/x/example", "upgrade"}) {
		t.Errorf("argv = %q, %v", got, err)
	}
}
