package teamlink

import (
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// Replicates answers for every recorded remote of the ULID, whatever its
// state and however the ULID is cased, and false once the remote is gone
// (zm-hayc).
func TestReplicatesByULIDInAnyState(t *testing.T) {
	const id = "01JOINEDORCHID0000000000AA"
	s := &Service{remotes: map[string]*Remote{
		id: {R: api.Remote{Project: api.ProjectRef{ID: id, Slug: "orchid"}, State: api.RemoteRevoked}},
	}}
	for _, ref := range []string{id, "01joinedorchid0000000000aa"} {
		if !s.Replicates(ref) {
			t.Fatalf("Replicates(%q) = false for a revoked remote", ref)
		}
	}
	if s.Replicates("orchid") || s.Replicates("01OTHER00000000000000000AA") || s.Replicates("") {
		t.Fatal("Replicates is true for a project with no remote (slugs are not ULIDs)")
	}
	s.remotes[id].R.State = api.RemoteOK
	if !s.Replicates(id) {
		t.Fatal("Replicates = false for a live remote")
	}
	delete(s.remotes, id)
	if s.Replicates(id) {
		t.Fatal("Replicates = true after the remote was removed")
	}
}
