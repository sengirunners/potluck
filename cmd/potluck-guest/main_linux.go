//go:build linux

// Command potluck-guest is the agent inside a cloud-hypervisor VM
// (docs/cloudhv.md): it answers the host on the VM's vsock, with no SSH and
// no credential, and sets the guest's network from the VM's context id.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sengirunners/potluck/vmhost/cloudhvguest"
)

func main() {
	dns := flag.String("dns", "1.1.1.1,8.8.8.8", "resolvers written to /etc/resolv.conf, comma-separated (empty: leave it alone)")
	boxUser := flag.String("user", "box", "the account commands run as: an ordinary one, which has sudo only where the VM's binding has root=on")
	flag.Parse()
	var servers []string
	for _, s := range strings.Split(*dns, ",") {
		if s = strings.TrimSpace(s); s != "" {
			servers = append(servers, s)
		}
	}
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, "potluck-guest: "+f+"\n", a...) }
	if err := cloudhvguest.Run(*boxUser, servers, logf); err != nil {
		logf("%v", err)
		os.Exit(1)
	}
}
