package server

import (
	"context"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
)

// Test hooks for the external server_test package (which imports
// wiring, which imports this package).
var (
	MarkUnix     = markUnix
	DiscardLog   = discardLog
	ReadServed   = readServed
	RemoveIfOurs = removeIfOurs
)

func (dm *Daemon) Build() api.BuildManifest        { return dm.build() }
func (dm *Daemon) Stopped() <-chan struct{}        { return dm.stop }
func (dm *Daemon) HealthBody(full bool) api.Health { return dm.healthBody(context.Background(), full) }
func (dm *Daemon) ZmState() *api.MachineZm         { return dm.zmState() }
func (dm *Daemon) AddModule(m core.Module)         { dm.modules = append(dm.modules, m) }

// Park is the park a freeze and the fence make, for a move of the test's own.
func (dm *Daemon) Park(move func(hometerm.Term) (hometerm.Term, error)) (hometerm.Term, error) {
	return dm.park(move)
}

// SetFenceEvery shortens how often a serving home asks its named successor
// for its term; the func puts it back.
func SetFenceEvery(d time.Duration) func() {
	was := fenceEvery.Swap(int64(d))
	return func() { fenceEvery.Store(was) }
}
