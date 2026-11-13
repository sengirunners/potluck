package teamlink

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// zm-q5n: scope changes that save concurrently leave the stored scope
// version and header from the same moment, and the last save stores the
// latest of them, so a restart neither holds an old version under a new
// header nor rolls the scope back.
func TestSaveKeepsScopeVersionWithHeader(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, Migrations...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Service{d: &core.Deps{Store: st}}
	x := &Remote{R: api.Remote{Project: api.ProjectRef{ID: "01PROJECT", Slug: "orchid"}, Authority: "mini", State: api.RemoteOK}}
	const n = 64
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(v int64) {
			defer wg.Done()
			js, _ := json.Marshal(api.TeamHeader{ScopeVersion: v})
			x.Mu.Lock()
			x.R.ScopeVersion, x.HeaderRaw = v, string(js)
			x.Mu.Unlock()
			if err := s.Save(ctx, x); err != nil {
				t.Error(err)
			}
		}(int64(i))
	}
	wg.Wait()
	var version int64
	var header string
	if err := st.Registry().QueryRowContext(ctx, `SELECT scope_version, header FROM team_remotes WHERE project = ?`, "01PROJECT").Scan(&version, &header); err != nil {
		t.Fatal(err)
	}
	var h api.TeamHeader
	if err := json.Unmarshal([]byte(header), &h); err != nil {
		t.Fatal(err)
	}
	x.Mu.Lock()
	want := x.R.ScopeVersion
	x.Mu.Unlock()
	if version != h.ScopeVersion || version != want {
		t.Fatalf("stored scope_version %d header %d, latest %d", version, h.ScopeVersion, want)
	}
}
