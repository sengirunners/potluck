// Package updatejob writes, loads, starts, reads and removes one program's
// daily update job as this user's own scheduled job: a systemd --user timer
// on Linux, a LaunchAgent on macOS; never root. It is the one home of that
// writer and its reader. A program names its job (Names) and says what it
// runs (Spec); this package knows nothing about which program that is.
//
// A job another program or person wrote is never changed: only files that
// carry Names.Mark are ever rewritten or removed, and an install never adds a
// second updater beside a foreign one. Starting the job and reading its
// command line do not change it, so they work on any job at these names.
package updatejob

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Names is what a program calls its job.
type Names struct {
	Label   string // the LaunchAgent label, and its plist's file name
	Service string // the systemd service unit, such as "potluck-update.service"
	Timer   string // the systemd timer unit
	// Mark starts every file the job writes; a file without it is another
	// updater's.
	Mark string
	// Remove is the command that removes the job; the files say so.
	Remove string
	// What the job installs, in a unit's description: "the newest signed
	// potluck release".
	Installs string
	// Program is the program's file name ("potluck"), and Foreign the words
	// that follow it when it updates itself ("upgrade"): a unit or agent
	// that runs <Program> <Foreign...> is another updater of this program.
	// Another program's own "upgrade" is not.
	Program string
	Foreign []string
}

// EnvVar is one variable in the job's environment.
type EnvVar struct{ Key, Value string }

// Spec is what the daily job runs.
type Spec struct {
	Argv   []string // the program and its arguments
	Env    []EnvVar
	Log    string // standard output and error go here (appended); empty: the journal or nowhere
	Minute int    // past 03:00, spreading machines over the hour
}

// Minute is name's minute past 03:00: the same machine always takes the same
// one, and machines spread over the hour.
func Minute(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() % 60)
}

// Path is the PATH a job gets: dir (where the program lives), then the
// absolute entries of environ's PATH that need no quoting, then the system
// directories.
func Path(dir, environPath string) string {
	path := []string{dir}
	for _, p := range filepath.SplitList(environPath) {
		if filepath.IsAbs(p) && !strings.ContainsAny(p, "\"\\$%\n") && !slices.Contains(path, p) {
			path = append(path, p)
		}
	}
	for _, p := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		if !slices.Contains(path, p) {
			path = append(path, p)
		}
	}
	return strings.Join(path, ":")
}

// Schedule writes, loads and removes one program's job on this machine.
type Schedule struct {
	Names    Names
	GOOS     string
	UserHome string
	UID      int
	// Run is systemctl or launchctl; tests record it.
	Run func(name string, args ...string) error
	// Linger reports whether this user's systemd services outlive a logout
	// (Linux; nil: unknown).
	Linger func() (bool, error)
	// Wait sleeps between launchctl attempts (nil: time.Sleep).
	Wait func(time.Duration)
}

// New is this machine's Schedule for names.
func New(names Names) (*Schedule, error) {
	uh, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Schedule{Names: names, GOOS: runtime.GOOS, UserHome: uh, UID: os.Getuid(), Linger: linger, Run: run}, nil
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// linger reads systemd's Linger flag for the current user.
func linger() (bool, error) {
	out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "-p", "Linger", "--value").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// Supported is an error when this system has neither systemd nor launchd as
// the job's scheduler.
func (s *Schedule) Supported() error {
	if s.GOOS != "linux" && s.GOOS != "darwin" {
		return fmt.Errorf("scheduled updates are not available on %s (they use systemd on Linux and launchd on macOS)", s.GOOS)
	}
	return nil
}

func (s *Schedule) wait(d time.Duration) {
	if s.Wait != nil {
		s.Wait(d)
		return
	}
	time.Sleep(d)
}

func (s *Schedule) unitDir() string {
	if s.GOOS == "darwin" {
		return filepath.Join(s.UserHome, "Library", "LaunchAgents")
	}
	return filepath.Join(s.UserHome, ".config", "systemd", "user")
}

// Files are the files of this program's job: the plist, or the service and
// the timer.
func (s *Schedule) Files() []string {
	if s.GOOS == "darwin" {
		return []string{filepath.Join(s.unitDir(), s.Names.Label+".plist")}
	}
	return []string{filepath.Join(s.unitDir(), s.Names.Service), filepath.Join(s.unitDir(), s.Names.Timer)}
}

// Marked reports whether path is a regular file this package wrote for a
// program that uses mark.
func Marked(path, mark string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && bytes.Contains(b[:min(len(b), 512)], []byte(mark))
}

func (s *Schedule) mine(path string) bool { return Marked(path, s.Names.Mark) }

// Installed reports whether this program's own job is on disk.
func (s *Schedule) Installed() bool { return slices.ContainsFunc(s.Files(), s.mine) }

// Loaded reports whether the scheduler has the job loaded.
func (s *Schedule) Loaded() bool {
	if s.GOOS == "darwin" {
		return s.Run("launchctl", "print", "gui/"+strconv.Itoa(s.UID)+"/"+s.Names.Label) == nil
	}
	return s.Run("systemctl", "--user", "is-enabled", s.Names.Timer) == nil
}

var (
	plistArgsRE   = regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`)
	plistStringRE = regexp.MustCompile(`<string>([^<]*)</string>`)
)

// plistArgs are a plist's ProgramArguments, one string each, unescaped.
func plistArgs(b []byte) []string {
	m := plistArgsRE.FindSubmatch(b)
	if m == nil {
		return nil
	}
	var args []string
	for _, a := range plistStringRE.FindAllSubmatch(m[1], -1) {
		args = append(args, html.UnescapeString(string(a[1])))
	}
	return args
}

// unitCommands are the commands of a unit's ExecStart lines, each split into
// its arguments as systemd splits them: a line ending in a backslash goes on
// the next, quotes and backslash escapes are undone, a lone ; separates two
// commands, and %% and $$ are made single (other % specifiers and $VAR are
// left as written). An empty ExecStart= clears the lines before it. A
// command's prefixes (-, @, :, +, !) are dropped; after @ the second word is
// the program's argv[0], so the path before it is dropped too. The error
// names a line it could not read (an unknown escape, an open quote); the
// commands are what it read anyway.
func unitCommands(b []byte) ([][]string, error) {
	var (
		cmds [][]string
		errs []error
	)
	text := strings.ReplaceAll(string(b), "\\\n", " ")
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			continue
		}
		if strings.TrimLeft(rest, " \t") == "" {
			cmds = nil
			continue
		}
		parsed, err := unitSplit(rest)
		if err != nil {
			errs = append(errs, fmt.Errorf("ExecStart=%s: %w", rest, err))
		}
		for _, args := range parsed {
			if cmd := unitCommand(args); len(cmd) > 0 {
				cmds = append(cmds, cmd)
			}
		}
	}
	return cmds, errors.Join(errs...)
}

// unitCommand drops a command's prefixes from its first word.
func unitCommand(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	path := strings.TrimLeft(args[0], "-@:+!")
	at := strings.Contains(args[0][:len(args[0])-len(path)], "@")
	cmd := append([]string{path}, args[1:]...)
	if at {
		cmd = cmd[1:]
	}
	return cmd
}

// unitEscapes are systemd's one-character escapes (\; is a literal ;).
var unitEscapes = map[byte]byte{
	'a': '\a', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v',
	's': ' ', '\\': '\\', '"': '"', '\'': '\'', ';': ';',
}

// unitSingle undoes the doubling of % and $ in a unit's word.
var unitSingle = strings.NewReplacer("%%", "%", "$$", "$")

// unitSplit splits one ExecStart value into its commands and their words;
// it undoes what unitQuote does. A lone ; that is neither quoted nor escaped
// ends a command.
func unitSplit(line string) ([][]string, error) {
	var (
		cmds  [][]string
		args  []string
		cur   []byte
		in    bool // a word has started
		lit   bool // the word holds a quoted or escaped character
		quote byte
	)
	end := func() {
		if w := string(cur); w == ";" && !lit {
			cmds, args = append(cmds, args), nil
		} else {
			args = append(args, unitSingle.Replace(w))
		}
		cur, in, lit = nil, false, false
	}
	done := func(err error) ([][]string, error) {
		if in && err == nil {
			end()
		}
		return append(cmds, args), err
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case c == '\\':
			if i+1 == len(line) {
				return done(errors.New("it ends in a backslash"))
			}
			i++
			e := line[i]
			if v, ok := unitEscapes[e]; ok {
				cur = append(cur, v)
			} else if e == 'x' && i+2 < len(line) {
				v, err := strconv.ParseUint(line[i+1:i+3], 16, 8)
				if err != nil {
					return done(fmt.Errorf("bad escape \\x%s", line[i+1:i+3]))
				}
				cur, i = append(cur, byte(v)), i+2
			} else if e >= '0' && e <= '7' && i+2 < len(line) {
				v, err := strconv.ParseUint(line[i:i+3], 8, 8)
				if err != nil {
					return done(fmt.Errorf("bad escape \\%s", line[i:i+3]))
				}
				cur, i = append(cur, byte(v)), i+2
			} else {
				return done(fmt.Errorf("unknown escape \\%c", e))
			}
			in, lit = true, true
		case quote == 0 && (c == '"' || c == '\''):
			quote, in, lit = c, true, true
		case quote == 0 && (c == ' ' || c == '\t'):
			if in {
				end()
			}
		default:
			cur = append(cur, c)
			in = true
		}
	}
	if quote != 0 {
		return done(fmt.Errorf("a %c quote is not closed", quote))
	}
	return done(nil)
}

// words are args split at whitespace, so an updater named inside a quoted
// shell command ("sh -c 'example upgrade'") is still seen.
func words(args []string) []string {
	var w []string
	for _, a := range args {
		w = append(w, strings.Fields(a)...)
	}
	return w
}

// runs reports whether words run the program with the update words: a word
// whose base name is the program, followed (flags between are fine) by the
// update words in a row. It also finds one inside a quoted shell command.
func (n Names) runs(words []string) bool {
	for i, w := range words {
		if filepath.Base(strings.Trim(w, `"'`)) != n.Program {
			continue
		}
		rest := make([]string, 0, len(words)-i-1)
		for _, r := range words[i+1:] {
			rest = append(rest, strings.Trim(r, `"'`))
		}
		for k := range rest {
			if k+len(n.Foreign) <= len(rest) && slices.Equal(rest[k:k+len(n.Foreign)], n.Foreign) {
				return true
			}
		}
	}
	return false
}

// Foreign is the path of an updater this package did not write ("" for
// none): a unit at one of this program's names that is not its own, or any
// other unit or agent that runs the program with Names.Foreign.
func (s *Schedule) Foreign() string {
	for _, p := range s.Files() {
		if _, err := os.Lstat(p); err == nil && !s.mine(p) {
			return p
		}
	}
	if s.Names.Program == "" || len(s.Names.Foreign) == 0 {
		return ""
	}
	pat := "*.service"
	if s.GOOS == "darwin" {
		pat = "*.plist"
	}
	names, _ := filepath.Glob(filepath.Join(s.unitDir(), pat))
	for _, p := range names {
		if slices.Contains(s.Files(), p) {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		cmds, _ := unitCommands(b) // what it could read
		args := slices.Concat(cmds...)
		if s.GOOS == "darwin" {
			args = plistArgs(b)
		}
		if s.Names.runs(words(args)) {
			return p
		}
	}
	return ""
}

// Result is what Install did.
type Result struct {
	Foreign  string // another updater's file: nothing was written
	Replaced bool   // this program's own job was there and is rewritten
}

// Install writes the job and loads it, replacing its own earlier one. With
// another updater on this machine it changes nothing and names it.
func (s *Schedule) Install(sp Spec) (Result, error) {
	if err := s.Supported(); err != nil {
		return Result{}, err
	}
	if f := s.Foreign(); f != "" {
		return Result{Foreign: f}, nil
	}
	r := Result{Replaced: s.mine(s.Files()[0])}
	if err := os.MkdirAll(s.unitDir(), 0o755); err != nil {
		return r, err
	}
	if s.GOOS == "darwin" {
		if err := writeFileAtomic(s.Files()[0], s.plist(sp)); err != nil {
			return r, err
		}
		target := "gui/" + strconv.Itoa(s.UID)
		_ = s.Run("launchctl", "bootout", target+"/"+s.Names.Label) // the old one, when loaded
		var err error
		for i := range 3 { // a bootout may still be unloading the old job
			if err = s.Run("launchctl", "bootstrap", target, s.Files()[0]); err == nil {
				break
			}
			s.wait(time.Duration(i+1) * 300 * time.Millisecond)
		}
		return r, err
	}
	if err := writeFileAtomic(s.Files()[0], s.service(sp)); err != nil {
		return r, err
	}
	if err := writeFileAtomic(s.Files()[1], s.timer(sp)); err != nil {
		return r, err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", s.Names.Timer}, {"restart", s.Names.Timer}} {
		if err := s.Run("systemctl", append([]string{"--user"}, args...)...); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Remove unloads and deletes this program's own job. foreign names another
// updater, which it leaves alone.
func (s *Schedule) Remove() (removed bool, foreign string, err error) {
	if err := s.Supported(); err != nil {
		return false, "", err
	}
	foreign = s.Foreign() // reported, never touched
	if !s.Installed() {
		return false, foreign, nil
	}
	var errs []error
	if s.GOOS == "darwin" {
		if e := s.Run("launchctl", "bootout", "gui/"+strconv.Itoa(s.UID)+"/"+s.Names.Label); e != nil && s.Loaded() {
			errs = append(errs, fmt.Errorf("the job is still loaded and fires until you log out: %w", e))
		}
	} else if e := s.Run("systemctl", "--user", "disable", "--now", s.Names.Timer); e != nil {
		errs = append(errs, e)
	}
	for _, p := range s.Files() {
		if !s.mine(p) { // never delete a file this package did not write
			continue
		}
		if e := os.Remove(p); e != nil && !errors.Is(e, fs.ErrNotExist) {
			errs = append(errs, e)
		}
	}
	if s.GOOS != "darwin" {
		if e := s.Run("systemctl", "--user", "daemon-reload"); e != nil {
			errs = append(errs, e)
		}
	}
	return true, foreign, errors.Join(errs...)
}

// ErrNoJob is Start's and Argv's answer when no job file is at this
// program's names.
var ErrNoJob = errors.New("no update job is installed on this machine")

// Start starts the update job now, once, and returns without waiting for it
// to end: systemctl --user start --no-block on the service, or launchctl
// kickstart on the agent (never -k: a run already going is not killed). It
// starts whatever job is at this program's names, including one this package
// did not write and that carries no Mark: a job at these names is this
// machine's update job whoever wrote it. ErrNoJob when there is no file.
func (s *Schedule) Start() error {
	if err := s.Supported(); err != nil {
		return err
	}
	if _, err := os.Stat(s.Files()[0]); errors.Is(err, fs.ErrNotExist) {
		return ErrNoJob
	} else if err != nil {
		return err
	}
	if s.GOOS == "darwin" {
		return s.Run("launchctl", "kickstart", "gui/"+strconv.Itoa(s.UID)+"/"+s.Names.Label)
	}
	return s.Run("systemctl", "--user", "start", "--no-block", s.Names.Service)
}

// Argv is the command line the installed job runs, read from its file (the
// plist's ProgramArguments, or the service's ExecStart), whoever wrote it.
// ErrNoJob when there is no file (a link to nothing is no file); an error
// when what is there is not a job file readJob takes, or names no command or,
// in a service, more than one.
func (s *Schedule) Argv() ([]string, error) {
	if err := s.Supported(); err != nil {
		return nil, err
	}
	path := s.Files()[0]
	b, err := readJob(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoJob
	} else if err != nil {
		return nil, err
	}
	if s.GOOS == "darwin" {
		args := plistArgs(b)
		if len(args) == 0 {
			return nil, fmt.Errorf("%s has no ProgramArguments this program can read", path)
		}
		return args, nil
	}
	cmds, err := unitCommands(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(cmds) != 1 {
		return nil, fmt.Errorf("%s runs %d commands, not one", path, len(cmds))
	}
	return cmds[0], nil
}

// jobFileMax bounds the job file Argv reads; a unit or a plist is a few
// hundred bytes.
const jobFileMax = 64 << 10

// readJob is the job file at path: a regular file, or a link to one (a
// machine's owner may link their own unit there), of at most jobFileMax
// bytes. Anything else is an error before a byte is read, so a pipe or a
// device left at the job's name does not hold a caller that reads the job
// often.
func readJob(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > jobFileMax {
		return nil, fmt.Errorf("%s is %d bytes, more than the %d a job file may be", path, fi.Size(), jobFileMax)
	}
	return os.ReadFile(path)
}

// LingerNote is a sentence for a Linux user whose timer stops at logout, and
// "" otherwise.
func (s *Schedule) LingerNote(user string) string {
	if s.GOOS != "linux" || s.Linger == nil {
		return ""
	}
	if on, err := s.Linger(); err != nil || on {
		return ""
	}
	return "it runs only while you are logged in unless you run sudo loginctl enable-linger " + user
}

func writeFileAtomic(path, body string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, werr := f.WriteString(body)
	merr := f.Chmod(0o644)
	cerr := f.Close()
	if err := errors.Join(werr, merr, cerr); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}

// unitQuote quotes one systemd word: % is a specifier, $ expands in ExecStart,
// and a lone ; separates two commands.
func unitQuote(s string, exec bool) string {
	r := []string{`\`, `\\`, `"`, `\"`, `%`, `%%`}
	if exec {
		r = append(r, `$`, `$$`)
	}
	q := strings.NewReplacer(r...).Replace(s)
	if q == s && s != "" && s != ";" && !strings.ContainsAny(s, " \t'") {
		return s
	}
	return `"` + q + `"`
}

func (s *Schedule) header() string { return s.Names.Mark + "; " + s.Names.Remove + " removes it." }

func (s *Schedule) service(sp Spec) string {
	words := make([]string, 0, len(sp.Argv))
	for _, a := range sp.Argv {
		words = append(words, unitQuote(a, true))
	}
	var b strings.Builder
	b.WriteString("# " + s.header() + "\n" +
		"[Unit]\nDescription=Install " + s.Names.Installs + "\nAfter=network-online.target\n\n" +
		"[Service]\nType=oneshot\n" +
		"# The upgrade may start a new daemon; nothing it starts may die with this unit.\nKillMode=process\n")
	for _, e := range sp.Env {
		b.WriteString("Environment=" + unitQuote(e.Key+"="+e.Value, false) + "\n")
	}
	if sp.Log != "" {
		// This directive is not unquoted by systemd: the path goes as it is,
		// with % doubled (a specifier).
		b.WriteString("StandardOutput=append:" + strings.ReplaceAll(sp.Log, "%", "%%") + "\n")
		b.WriteString("StandardError=inherit\n")
	}
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n" +
		"TimeoutStartSec=35min\n")
	return b.String()
}

func (s *Schedule) timer(sp Spec) string {
	return "# " + s.header() + "\n" +
		"[Unit]\nDescription=Check for " + s.Names.Installs + " once a day\n\n" +
		fmt.Sprintf("[Timer]\nOnCalendar=*-*-* 03:%02d:00\n", sp.Minute) +
		"# A machine that was off or asleep checks when it comes back.\nPersistent=true\n\n" +
		"[Install]\nWantedBy=timers.target\n"
}

func (s *Schedule) plist(sp Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- ` + xmlComment(s.header()) + ` -->
<plist version="1.0">
<dict>
  <key>Label</key><string>` + xmlEscape(s.Names.Label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range sp.Argv {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	if len(sp.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, e := range sp.Env {
			b.WriteString("    <key>" + xmlEscape(e.Key) + "</key><string>" + xmlEscape(e.Value) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	fmt.Fprintf(&b, `  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>%d</integer></dict>
  <key>AbandonProcessGroup</key><true/>
  <key>ProcessType</key><string>Background</string>
`, sp.Minute)
	if sp.Log != "" {
		log := xmlEscape(sp.Log)
		fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n  <key>StandardErrorPath</key><string>%s</string>\n", log, log)
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// xmlComment makes s safe inside <!-- -->: it may not hold "--".
func xmlComment(s string) string { return strings.ReplaceAll(s, "--", "- -") }
