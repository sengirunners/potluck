package hostprobe

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strings"
)

// Per-port anti-spoofing on Potluck's box bridge (zm-eakh.12.32.7, and the
// requirement zm-eakh.12.32.5 names for gVisor). A Kata box with docker gets
// its guest's /proc unmasked (vmhost ociKataDockerProc: nested containers
// need a fresh procfs), so root in that box can reconfigure its own guest
// kernel and with it send any frame from its interface. icc off stops IP
// between boxes, but not ARP: measured, root in such a box took a neighbour's
// address at the host (the host's ARP entry moved to its MAC and the
// neighbour lost its traffic). The guard is that fence, kept outside every
// guest, on the bridge:
//   - every frame from a box port must carry the MAC and IPv4 Docker gave
//     that box (ARP: Ethernet source, sender MAC and sender IPv4);
//   - no frame is forwarded from one box port to another;
//   - any other ethertype (IPv6, VLAN tags, the rest) is dropped.
//
// The allowed (port, MAC, IPv4) set comes from Docker's record of the
// network, never from a box, and a box not yet in it sends nothing: the guard
// fails closed. Root runs boxguard.sh (nft, nsenter, ip, awk, docker; never
// Potluck or zm) from the network's guard unit. The driver makes a Kata
// docker box on a Potluck network only while that unit is active (vmhost
// ociGuardReady); potluck host share --docker on Kata installs it with its
// other root steps.

//go:embed boxguard.sh
var boxGuardScript string

// BoxGuardBin is the guard's script, root's own file; each network's unit
// runs it with that network's names.
const BoxGuardBin = "/usr/local/libexec/potluck-box-guard"

// guardUnit is the unit that keeps network n's guard: potluck-box-guard.service
// for potluck-box, zm-box-guard.service for a machine zm set up.
func (n boxNetNames) guardUnit() string {
	return strings.TrimSuffix(n.Unit, "-net.service") + "-guard.service"
}

// guardTable is the nft bridge table the guard keeps for n.
func (n boxNetNames) guardTable() string { return strings.ReplaceAll(n.Net, "-", "_") + "_guard" }

// BoxGuardUnit keeps the guard on a machine set up now.
var BoxGuardUnit = currentBoxNet.guardUnit()

// BoxGuardUnitFile is the guard unit's text for a machine set up now.
func BoxGuardUnitFile() []string { return currentBoxNet.guardUnitFile() }

// guardUnitFile is the text of n's guard unit. It needs n (its unit makes
// the network) and restarts with Docker. ExecStartPre loads the tables and
// the set (the script's --once), so the unit is active only once they are
// in place; it then runs until stopped, and restarts when it ends, however
// often (no start limit: a guard given up on cuts every new box off). Its table stays in the kernel when it stops: the set it last
// wrote still admits only the boxes that were there, and the driver makes no
// new Kata docker box until the unit is active again.
func (n boxNetNames) guardUnitFile() []string {
	return []string{
		"[Unit]",
		"Description=Potluck box network " + n.Net + ": per-port anti-spoofing on " + n.Bridge + " (potluck host share --docker on Kata)",
		"After=docker.service " + n.Unit,
		"Requires=docker.service " + n.Unit,
		"PartOf=docker.service",
		"StartLimitIntervalSec=0",
		"[Service]",
		"ExecStartPre=" + BoxGuardBin + " " + n.Net + " " + n.Bridge + " " + n.guardTable() + " --once",
		"ExecStart=" + BoxGuardBin + " " + n.Net + " " + n.Bridge + " " + n.guardTable(),
		"PrivateTmp=yes",
		"Restart=always",
		"RestartSec=2",
		"[Install]",
		"WantedBy=docker.service",
	}
}

// BoxGuardSteps are the root steps that install the guard on a machine set
// up now.
func BoxGuardSteps() []string { return currentBoxNet.guardSteps() }

// guardSteps are the root steps that install the script and n's guard unit
// and (re)start it. The script goes as base64, so no character of it meets
// RootCommand's quoting; it replaces the old one by rename.
func (n boxNetNames) guardSteps() []string {
	u := n.guardUnitFile()
	q := make([]string, 0, len(u))
	for _, l := range u {
		q = append(q, `"`+strings.ReplaceAll(l, `"`, `\"`)+`"`)
	}
	return []string{
		"mkdir -p /usr/local/libexec",
		"printf %s " + base64.StdEncoding.EncodeToString([]byte(boxGuardScript)) + " | base64 -d >" + BoxGuardBin + ".new",
		"chmod 755 " + BoxGuardBin + ".new",
		"mv -f " + BoxGuardBin + ".new " + BoxGuardBin,
		`printf "%s\n" ` + strings.Join(q, " ") + " >/etc/systemd/system/" + n.guardUnit(),
		"systemctl daemon-reload",
		"systemctl enable " + n.guardUnit(),
		"systemctl restart " + n.guardUnit(),
	}
}

// detectBoxGuard sets l.Guard when the guard of the layer's network runs as
// Potluck installs it (its script and unit file exactly these, the unit
// active), else GuardProblem and, where root can fix it, GuardSteps. Only a
// Kata docker share needs it (sudo.go, share_runtime.go). Call it after
// detectBoxNet, which says whose names the network goes by.
func detectBoxGuard(ctx context.Context, run Runner, f Facts, l *Layer) {
	n := boxNetOf(*l)
	unit := n.guardUnit()
	if l.CLI != "docker" {
		l.GuardProblem = "Potluck's anti-spoofing guard watches docker's network " + n.Net + " only, and this host's container CLI is " + l.CLI
		return
	}
	if _, ok := f.LookPath("systemctl"); !ok {
		l.GuardProblem = "Potluck's anti-spoofing guard (" + unit + ") needs systemd, and there is none"
		return
	}
	read := func(name string, args ...string) string {
		if r, err := run.Run(ctx, name, args...); err == nil && r.Exit == 0 {
			return r.Stdout
		}
		return ""
	}
	state := "" // is-active exits non-zero for every state but active
	if r, err := run.Run(ctx, "systemctl", "is-active", unit); err == nil {
		state = strings.TrimSpace(r.Stdout)
	}
	script := read("cat", BoxGuardBin)
	text := read("cat", "/etc/systemd/system/"+unit)
	want := strings.Join(n.guardUnitFile(), "\n") + "\n"
	// The bridge must learn no address from link-local frames, which skip
	// every nft hook but its input (the script sets it each round).
	learn := strings.Contains(read("ip", "-d", "link", "show", n.Bridge), "no_linklocal_learn 1")
	if text != "" && state != "active" {
		// Its tables stay in the kernel: every box new on the network, of
		// any runtime, gets no traffic until the guard runs again.
		l.Warnings = append(l.Warnings, unit+" is installed but "+cmp.Or(state, "not running")+": every box new on "+n.Net+" has no network until it runs again (journalctl -u "+unit+" says why; docs/docker-services.md says how to remove it)")
	}
	switch {
	case state == "active" && script == boxGuardScript && text == want && learn:
		l.Guard = true
		return
	case state == "active" && script == boxGuardScript && text == want:
		l.GuardProblem = n.Bridge + " still learns addresses from link-local frames (no_linklocal_learn is not 1) though " + unit + " runs: the root line restarts it"
	case script != "" && script != boxGuardScript, text != "" && text != want:
		l.GuardProblem = "Potluck's anti-spoofing guard (" + unit + ") is not this Potluck's: the root line installs this one"
	case state == "failed":
		l.GuardProblem = unit + " failed (journalctl -u " + unit + " says why; it needs nft, nsenter and the network " + n.Net + "): the root line restarts it"
	default:
		l.GuardProblem = "Potluck's per-port anti-spoofing on " + n.Bridge + " (" + unit + ") is not running"
	}
	l.GuardSteps = n.guardSteps()
}

// kataDockerGuard is what a share binding Kata boxes with docker (rt, docker
// on) on network asks of the guard: nothing with no network; on a Potluck
// network, that its guard runs (else the root step that installs it); on an
// owner's network, never, since no guard watches it (the driver refuses
// such a box: vmhost ociGuardReady).
func kataDockerGuard(l Layer, docker bool, rt, network string) error {
	n := boxNetOf(l)
	switch {
	case !docker || !strings.Contains(rt, "kata") || network == "" || network == "none" || network == n.Net && l.Guard:
		return nil
	case !IsBoxNet(network):
		return fmt.Errorf("--docker on Kata with oci network %q: a Kata box with docker controls its own guest kernel, so it needs Potluck's network %s (with its anti-spoofing guard) or none", network, n.Net)
	case network != n.Net:
		// The guard read and installed is this machine's network's; the
		// driver checks the one the box is on.
		return fmt.Errorf("--docker on Kata with oci network %q: this machine's Potluck network is %s, whose anti-spoofing guard share checks and installs; name that one, or none", network, n.Net)
	}
	why := "sandboxes with docker on Kata control their own guest kernel, so they need Potluck's per-port anti-spoofing on " + boxNetOf(l).Bridge + ": " + l.GuardProblem
	if len(l.GuardSteps) == 0 {
		return fmt.Errorf("--docker: %s", why)
	}
	return &NotReadyError{Layer: l.Name, Problems: []string{why}, RootCommand: RootCommand(l.GuardSteps)}
}

// kataGuardNeeded says whether share's root line must install the guard: a
// Kata docker share whose boxes go on this machine's Potluck network (named,
// or the one a runner takes: Check's choice, including a network the same
// root line makes first; its steps come before these) with no guard running.
func (o ShareOptions) kataGuardNeeded(l Layer) bool {
	on, _ := o.docker()
	if !on || l.Name != "oci" || l.Guard || !strings.Contains(cmp.Or(o.OCIRuntime, l.Runtime), "kata") {
		return false
	}
	n := o.OCINetwork
	if n == "" && o.runnerOn(l.Name) {
		n = l.Network
		if n == "" && len(l.NetworkSteps) > 0 { // the same root line makes it first
			n = boxNetOf(l).Net
		}
	}
	return n == boxNetOf(l).Net
}
