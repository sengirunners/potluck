package server

import (
	"context"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
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
