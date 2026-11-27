package refdriver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The host's fence around a box's network (Softnet's rules on a Mac, the
// filtered network on Linux): this driver's box is this machine, so an nc
// first on a box's PATH does what that fence would. It drops what is sent
// to a multicast group, and a connection to any of this machine's own
// addresses named in config fence_addrs (comma list), as Potluck's rules
// block the gateway and every address the Mac holds. Each bug lets one
// class past:
//
//	multicast-past-fence:  multicast leaves (Softnet counts it as global)
//	host-alias-past-fence: only the first of fence_addrs is dropped, as a
//	                       fence that knows the gateway and no other
//	                       address of the machine (a public alias)
func (d *drv) fenceNC() string {
	mcast := map[bool]string{false: "22[4-9].* | 23[0-9].*) exit 0 ;;\n"}[d.is("multicast-past-fence")]
	var addrs []string
	for _, a := range strings.Split(d.cfg["fence_addrs"], ",") {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	if d.is("host-alias-past-fence") && len(addrs) > 1 {
		addrs = addrs[:1]
	}
	own := ""
	for _, a := range addrs {
		own += strconv.Quote(a) + ") exit 1 ;;\n"
	}
	return `#!/bin/sh
d=${0%/*}
h=; p=; for a; do h=$p; p=$a; done
case "$h" in
` + mcast + own + `esac
PATH=${PATH#"$d":}; exec nc "$@"
`
}

// fencePath is the fence's directory, written once per driver state (a
// temp file renamed in: another exec may be running it).
func (d *drv) fencePath() (string, error) {
	dir := filepath.Join(d.state, "fencebin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// A fence drops packets; it cannot supply a missing guest program.
	if _, err := exec.LookPath("nc"); err != nil {
		return dir, nil
	}
	p := filepath.Join(dir, "nc")
	tmp := p + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(d.fenceNC()), 0o700); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp, p)
}

// LANAddr is this machine's first IPv4 address on an up interface that is
// not loopback or link-local, false when it has none.
func LANAddr() (string, bool) { return lanAddr() }
