package teamlink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// A forget is refused before this machine's joins are loaded and while
// registry.db records a join of the id, even one the joins in memory do
// not hold; a drop that fails keeps the key bound and says what it dropped
// first.
func TestForgetRefusals(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, Migrations...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Service{d: &core.Deps{Store: st, Config: &config.Config{Home: home}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		remotes: map[string]*Remote{}}
	id := ids.New()
	if _, err := st.Registry().ExecContext(ctx, `INSERT INTO team_project_keys (project, via, fingerprint, authority, bound) VALUES (?, ?, ?, ?, ?)`,
		id, api.ViaTailnet, "sha256:k", "mini", store.Millis(time.Now())); err != nil {
		t.Fatal(err)
	}
	forget := func(hostState string) error {
		t.Helper()
		body := `{"host_state":"` + hostState + `","fingerprint":"sha256:k"}`
		r := httptest.NewRequest(http.MethodPost, "/v0/team/project-keys/"+id+"/forget", strings.NewReader(body))
		r.SetPathValue("project", id)
		_, err := s.handleForgetKey(r)
		return err
	}
	bound := func() bool {
		_, err := s.projectKey(ctx, id)
		return err == nil
	}

	if err := forget(api.ForgetKeepHostState); !api.IsCode(err, api.CodeUnavailable) || !bound() {
		t.Fatalf("forget before the joins are loaded: %v (bound %v)", err, bound())
	}
	s.loaded.Store(true)

	// A row with no remote in memory (a delete that failed).
	if err := s.Save(ctx, &Remote{R: api.Remote{Project: api.ProjectRef{ID: id, Slug: "orchid"}, State: api.RemoteRevoked}}); err != nil {
		t.Fatal(err)
	}
	if err := forget(api.ForgetKeepHostState); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "revoked") || !bound() {
		t.Fatalf("forget with a recorded join: %v (bound %v)", err, bound())
	}
	if _, err := st.Registry().ExecContext(ctx, `DELETE FROM team_remotes`); err != nil {
		t.Fatal(err)
	}

	s.Hooks.DropForgotten = []func(context.Context, string) ([]string, error){
		func(context.Context, string) ([]string, error) { return []string{"share of host binding mini-c1"}, nil },
		func(context.Context, string) ([]string, error) { return nil, errors.New("ledger unreadable") },
	}
	err = forget(api.ForgetDropHostState)
	if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "share of host binding mini-c1") ||
		!strings.Contains(err.Error(), "ledger unreadable") || !bound() {
		t.Fatalf("a failed drop: %v (bound %v)", err, bound())
	}
	s.Hooks.DropForgotten = s.Hooks.DropForgotten[:1]
	// An op this process is running for the id refuses it.
	release := make(chan struct{})
	s.RegisterOp("block", OpHandler{Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) {
		<-release
		return nil, nil
	}})
	s.remotes[id] = &Remote{R: api.Remote{Project: api.ProjectRef{ID: id, Slug: "orchid"}, State: api.RemoteOK}}
	s.TakeOps(ctx, id, []api.MachineOp{{ID: "op-run", Kind: "block"}})
	if err := forget(api.ForgetKeepHostState); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "still running") || !bound() {
		close(release)
		t.Fatalf("forget with a running op: %v (bound %v)", err, bound())
	}
	close(release)
	s.WaitOps()
	// The same op again leaves nothing counted.
	s.TakeOps(ctx, id, []api.MachineOp{{ID: "op-run", Kind: "block"}})
	if n := s.runningOps(id); n != 0 {
		t.Fatalf("ops counted running after a repeat of a done op: %d", n)
	}
	if _, err := st.Registry().ExecContext(ctx, `DELETE FROM team_ops_run`); err != nil {
		t.Fatal(err)
	}
	delete(s.remotes, id) // the leave
	// A row left running by a process that stopped mid-op does not refuse
	// it, and goes with the forget.
	if _, err := st.Registry().ExecContext(ctx, `INSERT INTO team_ops_run (id, project, kind, state, created) VALUES ('op-stale', ?, 'run_target', 'running', 0)`, id); err != nil {
		t.Fatal(err)
	}
	// The old authority's ops and unsent reports go; another project's stay.
	other := ids.New()
	for _, p := range []string{id, other} {
		if _, err := st.Registry().ExecContext(ctx, `INSERT INTO team_ops_run (id, project, kind, state, report, created) VALUES (?, ?, 'run_target', 'done', '{}', 0)`,
			"op-"+p, p); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Registry().ExecContext(ctx, `INSERT INTO team_task_refs (id, project, parent, created) VALUES (?, ?, 'x', 0)`, "rt-"+p, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := forget(api.ForgetDropHostState); err != nil || bound() {
		t.Fatalf("forget: %v (bound %v)", err, bound())
	}
	for _, tbl := range []string{"team_ops_run", "team_task_refs"} {
		var n, mine int
		if err := st.Registry().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(project = ?), 0) FROM `+tbl, id).Scan(&n, &mine); err != nil {
			t.Fatal(err)
		}
		if mine != 0 || n != 1 {
			t.Fatalf("%s after the forget: %d rows, %d of the forgotten id", tbl, n, mine)
		}
	}
}
