package runtarget_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// describeAnswer is a whole, valid describe answer for a run target.
const describeAnswer = `{"ok":true,"result":{"potluck":"potluck/1","name":"fake","kind":"run","version":"1","verbs":["describe","acquire","status","release","list"],"available":true,"isolation":"ephemeral","credentials":false,"cost":{"unit":"s","amount":0},"reports_spend":false,"ttl_max_s":60,"capabilities":[]}}`

// TestDaemonReadsAnswersByTheSuitesRule: the daemon takes a driver's answer
// only as the conformance suite does (protocol.ReadAnswer): exit 0 and exactly
// one JSON value on stdout. A driver that prints more, or exits non-zero after
// a good answer, is refused, so it cannot pass the daemon and fail the suite.
func TestDaemonReadsAnswersByTheSuitesRule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}

	// Each case's body starts a child (pid in <name>.pid) that sleeps far longer than
	// the test unless the daemon ends the driver's group after a refused answer.
	child := func(name string, redirect string) string {
		return "sleep 60 " + redirect + " & echo $! > '" + filepath.Join(fix, name+".pid") + "'"
	}
	quiet := ">/dev/null 2>&1" // a child that holds no pipe: only a refused answer tells the daemon to end it
	cases := []struct {
		name, body string
		code       string // "" binds
		says       string
	}{
		{"good", "echo '" + describeAnswer + "'", "", ""},
		{"refuses", `echo '{"ok":false,"error":{"code":"conflict","message":"busy"}}'`, api.CodeConflict, "busy"},
		{"noisy", "echo starting up; echo '" + describeAnswer + "'", api.CodeInternal, "not one JSON answer"},
		{"twice", "echo '" + describeAnswer + "'; echo '" + describeAnswer + "'", api.CodeInternal, "more than one answer"},
		{"exits", "echo '" + describeAnswer + "'; exit 3", api.CodeInternal, "every answer exits 0"},
		{"refuses-exits", `echo '{"ok":false,"error":{"code":"conflict","message":"busy"}}'; exit 1`, api.CodeInternal, "every answer exits 0"},
		{"strange-code", `echo '{"ok":false,"error":{"code":"teapot","message":"no"}}'`, api.CodeInternal, "not a known code"},
		{"keeps-pipe", child("keeps-pipe", "") + "; echo '" + describeAnswer + "'", api.CodeInternal, "kept stdout or stderr open"},
		{"noisy-leaves-child", child("noisy-leaves-child", quiet) + "; echo hi; echo '" + describeAnswer + "'", api.CodeInternal, "not one JSON answer"},
		{"bad-result-leaves-child", child("bad-result-leaves-child", quiet) + `; echo '{"ok":true,"result":{"name":5}}'`, api.CodeInternal, "bad result"},
		{"good-leaves-child", child("good-leaves-child", quiet) + "; echo '" + describeAnswer + "'", "", ""},
		{"long-number", `echo '{"ok":false,"error":{"code":"conflict","message":"x","retry_after":1` + strings.Repeat("0", 400) + `}}'`, api.CodeConflict, "x"},
		{"no-ok", `echo '{"result":{}}'`, api.CodeInternal, `no "ok"`},
	}
	drivers := map[string]string{}
	for _, c := range cases {
		p := filepath.Join(fix, "zm-run-"+c.name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\ncat >/dev/null\n"+c.body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		drivers[c.name] = p
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(name string) (string, error) {
				if p, ok := drivers[name]; ok {
					return p, nil
				}
				return "", errors.New("no such driver")
			})
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{fix}, Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		_, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{
			Name: c.name, Driver: c.name, Kind: "run", Isolation: "ephemeral", Repo: repo.Remote, Branch: "main"}})
		if c.code == "" {
			if err != nil {
				t.Errorf("%s: bind: %v", c.name, err)
			}
			continue
		}
		var e *api.Error
		if !errors.As(err, &e) || e.Code != c.code || !strings.Contains(e.Message, c.says) {
			t.Errorf("%s: bind answered %v, want %s saying %q", c.name, err, c.code, c.says)
		}
		if err != nil && strings.Contains(err.Error(), "teapot") {
			t.Errorf("%s: the driver's unknown code reached the caller: %v", c.name, err)
		}
	}
	for _, c := range cases {
		pid, err := childPID(filepath.Join(fix, c.name+".pid"))
		if err != nil {
			continue // this case starts no child
		}
		if c.code == "" { // an answer that is used leaves the driver's own children alone
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if syscall.Kill(pid, 0) != nil {
				t.Errorf("%s: the daemon ended a child of a driver whose answer it used", c.name)
			}
			continue
		}
		deadline := time.Now().Add(10 * time.Second)
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) { // gone once reaped
			time.Sleep(20 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("%s: the child the driver left outlived the refused call", c.name)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// childPID reads the pid a driver script wrote with echo $!.
func childPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
