// Command softnet-sim checks a Softnet build on this Mac's own vmnet without
// booting a guest: it runs the softnet executable the way Tart does (a Unix
// datagram socket pair as --vm-fd 0, --vm-mac-address, Potluck's own block
// and allow rules) with a fake VM on the socket, and shows frame by frame
// what left Softnet, what was answered and what came in. See
// docs/softnet.md, "Rehearsing a build in a macOS VM".
//
//	sudo softnet-sim -softnet <executable> -mutant <executable> [-json report.json] [-md report.md]
//
// Run it only in a throwaway macOS VM: as root it makes fake network
// interfaces (feth), adds routes and address aliases, and listens on many
// ports. It puts all of it back when it ends, interrupted too.
//
// A probe that Potluck's rules must stop counts as stopped only when the same
// probe, sent through Softnet with no rules in the way, left Softnet and its
// endpoint answered (for one Softnet stops under any rules: when a Softnet
// built to filter nothing, -mutant, let it out). The deliberately broken rule sets must fail, each in
// every group of probes it breaks.
//
// It is a rehearsal, not the validation of a build: no guest kernel, no Tart
// and no Virtualization framework take part.
package main
