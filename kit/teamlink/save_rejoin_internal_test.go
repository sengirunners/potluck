package teamlink

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// A re-join that replaces a purged record of the same project saves over
// its row: after a restart (Load) the remote is the new join's, its URL,
// key, transport, machine and identity, not the old row's.
func TestSaveOverAPurgedRecordKeepsTheNewJoin(t *testing.T) {
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
	d := &core.Deps{Store: st, Config: &config.Config{Home: home}}
	s := &Service{d: d, remotes: map[string]*Remote{}}
	const id = "01KPROJECT0000000000000000"
	old := &Remote{R: api.Remote{Project: api.ProjectRef{ID: id, Slug: "orchid"}, Authority: "mini", URL: "http://mini.old",
		Fingerprint: "sha256:old", Via: api.ViaTailnet, Machine: "laptop", Identity: "ana", Joined: time.Now().Add(-time.Hour).UTC(),
		State: api.RemotePurged, LastError: "revoked"}}
	if err := s.Save(ctx, old); err != nil {
		t.Fatal(err)
	}
	joined := time.Now().UTC().Truncate(time.Millisecond)
	nu := &Remote{R: api.Remote{Project: api.ProjectRef{ID: id, Slug: "orchid"}, Authority: "mini2", URL: "https://home.example",
		Fingerprint: "sha256:new", Via: "tunnel", Machine: "laptop2", Identity: "ana2", Joined: joined, State: api.RemoteOK}}
	if err := WriteFileAtomic(s.TokenPath(id), []byte("tok\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, nu); err != nil {
		t.Fatal(err)
	}
	after := &Service{d: d, remotes: map[string]*Remote{}}
	if err := after.Load(ctx); err != nil {
		t.Fatal(err)
	}
	x := after.ByName(id)
	if x == nil {
		t.Fatal("no remote after Load")
	}
	got := x.Snapshot()
	if got.Authority != "mini2" || got.URL != "https://home.example" || got.Fingerprint != "sha256:new" || got.Via != "tunnel" ||
		got.Machine != "laptop2" || got.Identity != "ana2" || !got.Joined.Equal(joined) || got.LastError != "" {
		t.Fatalf("after a restart the remote is %+v, want the new join's", got)
	}
}
