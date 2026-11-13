package owners

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// With no notifier, a project's failing org checks are logged once per
// failure: not on every recheck, and again after the checks ran.
func TestOrgChecksFailingLoggedOncePerFailure(t *testing.T) {
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
	cfg := &config.Config{Home: home, Human: "judy", Machine: "mini"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	s := &Service{d: d}
	lines := func() int { return strings.Count(log.String(), "level=WARN msg=\"notice for a person") }
	for range 3 {
		s.orgChecksFailing(ctx, p.ID, "bad token")
	}
	if n := lines(); n != 1 {
		t.Fatalf("%d warn lines for one failure, want 1:\n%s", n, log.String())
	}
	if !strings.Contains(log.String(), "level=DEBUG") || strings.Contains(warnLines(log.String()), "bad token") {
		t.Fatalf("the body is not only at debug:\n%s", log.String())
	}
	s.orgChecksRan(ctx, p.ID)
	s.orgChecksFailing(ctx, p.ID, "bad token")
	if n := lines(); n != 2 {
		t.Fatalf("%d warn lines after a new failure, want 2", n)
	}
}

func warnLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "level=WARN") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
