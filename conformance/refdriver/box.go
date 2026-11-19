package refdriver

// A box's exec and renter secrets (SPEC 20 "Boxes"), split from refdriver.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/boxproc"
)

// exec runs one command in a ready box: literal argv, the box's directory as
// HOME and (unless cwd says otherwise) as its working directory, or the box's
// checkout when it has one. Output is capped; a timeout is exit 124.
func (d *drv) exec(req request) (any, error) {
	p, err := d.parse(req.ID)
	if err != nil {
		return nil, err
	}
	var a struct {
		Argv    json.RawMessage   `json:"argv"`
		Stdin   *string           `json:"stdin"`
		Enc     string            `json:"encoding"`
		Timeout int               `json:"timeout_s"`
		Cwd     string            `json:"cwd"`
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(req.Args, &a); err != nil {
		return nil, refuse("bad_request", "bad args: %v", err)
	}
	stdinB, err := boxproc.Decode(a.Stdin, a.Enc)
	if err != nil {
		return nil, asRefusal(err)
	}
	if err := d.checkSecrets(a.Secrets); err != nil {
		return nil, err
	}
	var argv []string
	err = json.Unmarshal(a.Argv, &argv)
	if err == nil && len(argv) == 0 && d.is("box-empty-argv") {
		argv = []string{"true"}
	}
	if err != nil || len(argv) == 0 {
		var line string
		if d.is("box-shell-string") && json.Unmarshal(a.Argv, &line) == nil && line != "" {
			argv = []string{"sh", "-c", line}
		} else {
			return nil, refuse("bad_request", "argv must be a non-empty array of strings")
		}
	}
	if a.Timeout <= 0 {
		a.Timeout = 30
	}
	if a.Timeout > 600 && !d.is("box-timeout-no-cap") {
		return nil, refuse("bad_request", "timeout_s past 600")
	}
	r, err := load(p)
	if err != nil {
		return nil, err
	}
	// Config exec_late_ms stands in for a loaded host: an exec on a box whose
	// deadline is nearer than that arrives that much later, after it.
	if late, left := time.Duration(atoi(d.cfg["exec_late_ms"]))*time.Millisecond, time.Until(r.Deadline); left > 0 && late > left {
		time.Sleep(late)
	}
	if (r.State != "ready" || time.Now().After(r.Deadline)) && !d.is("box-exec-after-release") {
		return nil, refuse("conflict", "%s is %s, not ready", r.Handle, r.State)
	}
	if d.is("box-exec-fake") {
		return map[string]any{"exit": 0, "stdout": "", "stderr": "", "truncated": false}, nil
	}
	dir := r.Dir
	if r.Checkout != "" {
		dir = r.Checkout
	}
	switch { // the box's directory stands in for the guest's root
	case filepath.IsAbs(a.Cwd):
		dir = filepath.Join(r.Dir, filepath.Clean(a.Cwd))
	case a.Cwd != "":
		dir = filepath.Join(dir, filepath.Clean("/"+a.Cwd))
	}
	if d.is("box-exec-fresh") { // each exec in a new, empty home
		fresh, err := os.MkdirTemp(d.state, "fresh-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(fresh)
		r.Dir, dir = fresh, fresh
	}
	if left := time.Until(r.Deadline); time.Duration(a.Timeout)*time.Second > left {
		a.Timeout = max(1, int(left/time.Second))
	}
	timeout := time.Duration(a.Timeout) * time.Second
	if d.is("box-no-timeout") {
		timeout = time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	argv = d.ownTmp(r, argv) // hosttmp.go
	if d.is("box-serve-stops") {
		argv = d.serveStops(argv) // servestops.go
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	tmp := d.boxTmp(r) // volumes.go
	path := os.Getenv("PATH")
	if d.cfg["kind"] == "box" { // root.go: a stand-in sudo and docker
		bin, err := d.rootPath()
		if err != nil {
			return nil, err
		}
		path = bin + string(os.PathListSeparator) + path
	}
	if err := d.showShelf(r); err != nil { // shelf.go
		return nil, err
	}
	cmd.Env = []string{"PATH=" + path, "HOME=" + r.Dir, "TMPDIR=" + tmp, "LANG=C"}
	if len(a.Secrets) > 0 && !d.is("secrets-ignored") {
		dir, env, err := d.writeSecrets(r, a.Secrets)
		if err != nil {
			return nil, err
		}
		if !d.is("secrets-left") {
			defer func() {
				if !d.is("secrets-left-on-timeout") || ctx.Err() == nil {
					_ = os.RemoveAll(dir) // before the answer, also after a timeout
				}
			}()
		}
		cmd.Env = append(cmd.Env, env...)
	} else {
		cmd.Env = append(cmd.Env, d.boxSecretsEnv(r)...) // secrets.go
	}
	if !d.is("box-no-stdin") {
		cmd.Stdin = strings.NewReader(string(stdinB))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if d.is("box-kill-leader-only") {
			return syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second // a child that keeps the output open never holds the answer
	limit, errLimit := execCap, execCap
	if d.is("box-no-cap") {
		limit, errLimit = 1<<30, 1<<30
	}
	if d.is("box-stderr-uncapped") {
		errLimit = 1 << 30
	}
	if d.is("box-serve-late") && slices.Contains(argv, "serve") { // a listener that starts after the scan
		time.Sleep(20 * time.Second)
	}
	stdout := &capWriter{max: limit, tail: d.is("box-cap-tail")}
	stderr := &capWriter{max: errLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return map[string]any{"exit": 127, "stdout": "", "stderr": err.Error(), "truncated": false}, nil
	}
	// end kills what is still running in the box: it finds each exec here.
	mark := filepath.Join(r.Dir, ".execs", strconv.Itoa(cmd.Process.Pid))
	if os.MkdirAll(filepath.Dir(mark), 0o700) == nil {
		_ = os.WriteFile(mark, nil, 0o600)
	}
	_ = cmd.Wait()
	_ = os.Remove(mark)
	code := cmd.ProcessState.ExitCode()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if timedOut {
		code = 124
	}
	enc := a.Enc
	if d.is("base64-text-out") {
		enc = ""
	}
	res := map[string]any{"exit": code, "stdout": boxproc.Encode([]byte(stdout.b.String()), enc), "stderr": boxproc.Encode([]byte(stderr.b.String()), enc),
		"truncated": stdout.cut || stderr.cut}
	if enc != "" {
		res["encoding"] = enc
	}
	if timedOut { // the protocol's optional fact: stopped at timeout_s, not its own 124
		res["timed_out"], res["timeout_s"] = true, a.Timeout
	}
	return res, nil
}

// capWriter keeps the first max bytes (the last, with tail: a bug) and notes
// that it cut the rest.
type capWriter struct {
	b    strings.Builder
	max  int
	cut  bool
	tail bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.tail && c.b.Len()+len(p) > c.max {
		all := c.b.String() + string(p)
		c.b.Reset()
		c.b.WriteString(all[len(all)-c.max:])
		c.cut = true
		return len(p), nil
	}
	if room := c.max - c.b.Len(); room < len(p) {
		c.b.Write(p[:max(room, 0)])
		c.cut = true
		return len(p), nil
	}
	c.b.Write(p)
	return len(p), nil
}

var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// secretsCapable says whether this driver lists the secrets capability.
func (d *drv) secretsCapable() bool { return !d.is("box-no-secrets", "secrets-ignored") }

// checkSecrets refuses what SPEC 20 refuses: secrets on a driver without the
// capability, a bad name, more than 64 KiB in all.
func (d *drv) checkSecrets(secrets map[string]string) error {
	if len(secrets) == 0 || d.is("secrets-ignored") {
		return nil
	}
	if d.is("secrets-refuse-echo") { // a driver quoting what it was sent
		for k, v := range secrets {
			return refuse("unavailable", "cannot place %s=%s", k, v)
		}
	}
	if !d.secretsCapable() {
		return refuse("bad_request", "this target takes no secrets")
	}
	n := 0
	for k, v := range secrets {
		if !secretName.MatchString(k) && !d.is("secrets-any-name") {
			if d.is("secrets-name-echo") {
				return refuse("bad_request", "secret name %q is not [A-Z][A-Z0-9_]{0,63}", k)
			}
			return refuse("bad_request", "a secret name is not [A-Z][A-Z0-9_]{0,63}") // never the name itself
		}
		n += len(k) + len(v)
	}
	limit := 64 << 10
	if d.is("secrets-small-cap") {
		limit = 1 << 10
	}
	if n > limit && !d.is("secrets-any-name") {
		return refuse("bad_request", "secrets past 64 KiB (names and values)")
	}
	return nil
}

// writeSecrets puts each value in a fresh 0700 directory as a 0600 file
// on the box's stand-in memory filesystem (secrets.go; a real driver: a
// guest tmpfs, the values sent on the guest exec's stdin) and answers the
// directory and the command's extra environment: the path, never a value
// (bug box-secrets-exec-clobbers: the box's own secrets directory, which
// the command's end then removes).
func (d *drv) writeSecrets(r record, secrets map[string]string) (string, []string, error) {
	base := memBox(d.state, r.Handle)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "potlucksec-")
	if d.is("box-secrets-exec-clobbers") {
		dir, err = d.boxSecretsDir(r), os.MkdirAll(d.boxSecretsDir(r), 0o700)
	}
	if err != nil {
		return "", nil, err
	}
	mode := os.FileMode(0o600)
	if d.is("secrets-loose") {
		mode = 0o644
	}
	env := []string{"ZM_SECRETS=" + dir, "POTLUCK_SECRETS=" + dir}
	switch d.bug { // not bugs: a driver with only one of the two names must pass
	case "zm-names":
		env = env[:1]
	case "potluck-names":
		env = env[1:]
	}
	for k, v := range secrets {
		f := filepath.Join(dir, k)
		if err := os.WriteFile(f, []byte(v), mode); err != nil {
			return "", nil, err
		}
		_ = os.Chmod(f, mode) // past the umask
		switch d.bug {
		case "secrets-env":
			env = append(env, k+"="+v)
		case "secrets-stderr":
			fmt.Fprintln(os.Stderr, "wrote", k, v)
		case "secrets-argv": // a process whose argv carries it, alive while the command runs
			c := exec.Command("sh", "-c", "sleep 5; :", v) // "; :" keeps sh (and its argv) alive
			c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if c.Start() == nil {
				go func() { _ = c.Wait() }()
			}
		}
	}
	return dir, env, nil
}
