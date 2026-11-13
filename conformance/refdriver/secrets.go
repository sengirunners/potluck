package refdriver

// Box secrets (docs/protocol.md, Box secrets): the secrets verb, with a Bug
// switch for each way a driver can get it wrong. This driver's boxes are
// host directories, and so is its memory filesystem: a directory per box
// beside the state directory (memRoot), which the stand-in stat on each
// box's PATH (root.go) calls a tmpfs, as a guest's /dev/shm is one. It
// stands in for what conformance asks; nothing here is in memory.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// memRoot is the stand-in memory filesystem of state's boxes: beside the
// state directory, never in it.
func memRoot(state string) string { return filepath.Clean(state) + "-mem" }

// memBox is one box's part of it, which ends with the box.
func memBox(state, handle string) string {
	return filepath.Join(memRoot(state), strings.NewReplacer("/", "_", ":", "_").Replace(handle))
}

// boxSecretsServed: this driver's boxes take the secrets verb (bug
// no-box-secrets: they do not; box-secrets-unlisted: they do, unlisted).
func (d *drv) boxSecretsServed() bool { return d.cfg["kind"] == "box" && !d.is("no-box-secrets") }

func (d *drv) boxSecretsListed() bool { return d.boxSecretsServed() && !d.is("box-secrets-unlisted") }

// boxSecretsDir is where a box keeps its secrets (bug box-secrets-on-disk:
// in the box's own directory, its disk).
func (d *drv) boxSecretsDir(r record) string {
	if d.is("box-secrets-on-disk") {
		return filepath.Join(r.Dir, "potluck-secrets")
	}
	return filepath.Join(memBox(d.state, r.Handle), "potluck-secrets")
}

// boxSecretsEnv is what a command without secrets of its own adds to its
// environment: the box's secrets directory, when it has one (bug
// box-secrets-not-visible: nothing; box-secrets-in-env: each value too).
func (d *drv) boxSecretsEnv(r record) []string {
	dir := d.boxSecretsDir(r)
	if !d.privateDir(dir) || d.is("box-secrets-not-visible") {
		return nil
	}
	env := []string{"ZM_SECRETS=" + dir, "POTLUCK_SECRETS=" + dir}
	if d.is("box-secrets-in-env") {
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && protocol.SecretName(e.Name()) {
				env = append(env, e.Name()+"="+string(b))
			}
		}
	}
	return env
}

// privateDir says whether dir is a directory (never a link) of this user
// (bug box-secrets-follows-links: a link to one will do; box-secrets-any-owner:
// anyone's). Root's is marked by the stand-in sudo's chown (root.go): this
// machine's user can make no directory another's.
func (d *drv) privateDir(dir string) bool {
	stat := os.Lstat
	if d.is("box-secrets-follows-links") {
		stat = os.Stat
	}
	fi, err := stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	if d.is("box-secrets-any-owner") {
		return true
	}
	_, marked := os.Lstat(dir + ".root-owned")
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && marked != nil
}

// boxSecrets is the secrets verb.
func (d *drv) boxSecrets(req request) (any, error) {
	p, err := d.parse(req.ID)
	if err != nil {
		return nil, err
	}
	var a protocol.BoxSecretsArgs
	if d.is("box-secrets-any-name") { // no name, size or shape rule
		_ = json.Unmarshal(req.Args, &a)
	} else if a, err = protocol.ParseBoxSecrets(req.Args); err != nil {
		return nil, refuse("bad_request", "%v", err)
	}
	if d.is("box-secrets-refuse-echo") { // a driver quoting what it was sent (the daemon's tests)
		for k, v := range a.Secrets {
			return nil, refuse("unavailable", "cannot place %s=%s", k, v)
		}
	}
	r, err := load(p)
	if err != nil {
		return nil, err
	}
	if (r.State != "ready" || time.Now().After(r.Deadline)) && !d.is("box-secrets-after-release") {
		return nil, refuse("conflict", "%s is %s, not ready", r.Handle, r.State)
	}
	dir := d.boxSecretsDir(r)
	dmode, fmode := os.FileMode(0o700), os.FileMode(0o600)
	if d.is("box-secrets-world-readable") {
		dmode, fmode = 0o755, 0o644
	}
	var out map[string]any
	err = locked(d.state, func() error { // one call at a time: the 64 KiB check holds
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				return err
			}
			if err := os.Mkdir(dir, dmode); err != nil {
				return err
			}
		}
		if !d.privateDir(dir) {
			return refuse("forbidden", "something not a private directory of the box's user is at its secrets path")
		}
		if err := os.Chmod(dir, dmode); err != nil {
			return err
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(left) > 0 { // what a killed call wrote
			for _, p := range left {
				_ = os.Remove(p)
			}
		}
		held := 0
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			_, set := a.Secrets[e.Name()]
			if fi, err := e.Info(); err == nil && protocol.SecretName(e.Name()) && !set && !slices.Contains(a.Unset, e.Name()) {
				held += len(e.Name()) + int(fi.Size())
			}
		}
		for k, v := range a.Secrets {
			held += len(k) + len(v)
		}
		if held > protocol.SecretsMax && !d.is("box-secrets-any-name") {
			return refuse("bad_request", "%v", protocol.ErrSecretsBig)
		}
		if !d.is("box-secrets-no-unset") {
			for _, k := range a.Unset {
				if err := os.Remove(filepath.Join(dir, k)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
		for k, v := range a.Secrets {
			if err := d.writeSecret(dir, k, v, fmode); err != nil {
				return err
			}
		}
		es, _ = os.ReadDir(dir)
		n := 0
		for _, e := range es {
			if protocol.SecretName(e.Name()) {
				n++
			}
		}
		if n == 0 {
			_ = os.Remove(dir)
		}
		out = map[string]any{"dir": dir, "count": n}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for k, v := range a.Secrets {
		switch d.bug {
		case "box-secrets-in-answer":
			out["value"] = v
		case "box-secrets-in-state": // a log beside the records
			if f, err := os.OpenFile(filepath.Join(d.state, "secrets.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				_, _ = f.WriteString("set " + k + "=" + v + "\n")
				f.Close()
			}
		case "box-secrets-in-argv": // a process whose argv carries it, alive a while
			c := exec.Command("sh", "-c", "sleep 8; :", v)
			c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if c.Start() == nil {
				go func() { _ = c.Wait() }()
			}
		}
	}
	if d.is("box-secrets-in-argv") {
		time.Sleep(200 * time.Millisecond) // the process is up before the answer
	}
	return out, nil
}

// writeSecret replaces one secret's file whole: written beside it and renamed
// over it (bug box-secrets-partial-write: rewritten in place, half at a time).
func (d *drv) writeSecret(dir, name, v string, mode os.FileMode) error {
	f := filepath.Join(dir, name)
	if d.is("box-secrets-partial-write") {
		w, err := os.OpenFile(f, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		defer w.Close()
		_, _ = w.WriteString(v[:len(v)/2])
		time.Sleep(150 * time.Millisecond)
		_, err = w.WriteString(v[len(v)/2:])
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, []byte(v), mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil { // past the umask
		return err
	}
	return os.Rename(tmp, f)
}

// endMem ends a box's memory filesystem, as a VM's end or power-off does
// (bug box-secrets-survive-release: it stays).
func endMem(state string, r record, bug string) {
	if bug != "box-secrets-survive-release" && r.Handle != "" {
		_ = os.RemoveAll(memBox(state, r.Handle))
	}
}

// statStandIn is each box's stat (first on its PATH, root.go): stat -f -c
// %T names a tmpfs under memRoot and a disk anywhere else, as a guest's stat
// names /dev/shm and its disk; any other use is this machine's stat.
func (d *drv) statStandIn() string {
	mem := memRoot(d.state)
	_ = os.MkdirAll(mem, 0o700)
	if real, err := filepath.EvalSymlinks(mem); err == nil {
		mem = real
	}
	realStat, _ := exec.LookPath("stat")
	return fmt.Sprintf(`#!/bin/sh
if [ $# = 4 ] && [ "$1" = -f ] && [ "$2" = -c ] && [ "$3" = %%T ]; then
	case $(cd "$4" 2>/dev/null && pwd -P) in %q|%q/*) echo tmpfs ;; *) echo ext2/ext3 ;; esac
	exit 0
fi
exec %q "$@"
`, mem, mem, realStat)
}

// catStandIn is each box's cat (root.go): /proc/swaps, as conformance reads
// it, is the guest's swap table, which lists none (bug box-secrets-swapped: a
// swap file on a disk); anything else is this machine's cat.
func (d *drv) catStandIn() string {
	table := `Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n`
	if d.is("box-secrets-swapped") {
		table += `/swapfile                               file\t\t4194300\t\t0\t\t-2\n`
	}
	realCat, _ := exec.LookPath("cat")
	return fmt.Sprintf("#!/bin/sh\nif [ $# = 1 ] && [ \"$1\" = /proc/swaps ]; then printf '%s'; exit 0; fi\nexec %q \"$@\"\n", table, realCat)
}
