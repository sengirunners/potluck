package cli

import (
	"slices"
	"testing"

	"github.com/sengirunners/potluck/kit/store"
)

// The potluck program registers only the shell's migrations from init()
// (kit/project, kit/events: the tables a daemon reads while its wiring
// builds). Every module adds its own through its wiring
// (core.Deps.AddMigrations), so they reach only daemons that wire the
// module, and run in the one name order pass after the wiring has built.
func TestOnlyTheShellRegistersMigrations(t *testing.T) {
	if !slices.Contains(store.Registered(store.KindRegistry), "project/0001_init") {
		t.Fatalf("the shell's migrations are not registered: %v", store.Registered(store.KindRegistry))
	}
	for _, k := range []store.Kind{store.KindRegistry, store.KindProject, store.KindReplica} {
		for _, name := range store.Registered(k) {
			if !store.Shell(name) {
				t.Errorf("%s is registered from init(); a module adds its migrations with core.Deps.AddMigrations", name)
			}
		}
	}
}
