package refdriver

import (
	"os"
	"path/filepath"
	"strings"
)

// A box's own /tmp (zm-eakh.12.65.17). The boxes are processes on the host,
// so they all see its /tmp; config host_tmp names the host directory that
// stands in for a guest's /tmp in the checks (Options.LeftoverRoots). A VM's
// /tmp is its own, so exec gives each box its own: an argument that is
// host_tmp, or a path under it, is rewritten to the box's tmpRoot, which is
// under the box's directory and goes when the box ends. The bugs keep the
// one host directory instead, as a pool that wipes HOME but keeps /tmp
// would: tmp-kept and volume-cow-left-in-host-tmp (volumes.go) for every
// box, warm-tmp-kept for a box taken from the warm pool.

// tmpRoot is where box r's /tmp stand-in is.
func tmpRoot(r record) string { return filepath.Join(r.Dir, ".tmp-root") }

// ownTmp rewrites host_tmp in argv to box r's tmpRoot, made if it is not
// there.
func (d *drv) ownTmp(r record, argv []string) []string {
	host := d.cfg["host_tmp"]
	if host == "" || d.is("tmp-kept", "volume-cow-left-in-host-tmp") || r.Warm && d.is("warm-tmp-kept") {
		return argv
	}
	own := tmpRoot(r)
	_ = os.MkdirAll(own, 0o700)
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == host || strings.HasPrefix(a, host+"/") {
			a = own + strings.TrimPrefix(a, host)
		}
		out[i] = a
	}
	return out
}
