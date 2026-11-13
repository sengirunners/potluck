package hostprobe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// guardFakes are the commands the guard script runs, answering as a host
// with three containers on potluck-box: c1 a box on the bridge, c2 whose port
// is on another bridge, c3 whose record is malformed. nft writes what it is
// given to $NFT_LOG; docker fails its network inspect while $DOCKER_DOWN is set.
var guardFakes = map[string]string{
	"docker": `#!/bin/sh
case "$1" in
network) [ -n "${DOCKER_DOWN:-}" ] && exit 1; echo "c1 c2 c3 " ;;
inspect) eval c=\${$#}; [ -n "${INSPECT_DOWN:-}" ] && [ "$c" = c2 ] && exit 1; case $c in
  c1) echo "02:42:0a:c7:00:02 10.199.0.2 /run/docker/netns/n1" ;;
  c2) echo "02:42:0a:c7:00:03 10.199.0.3 /run/docker/netns/n2" ;;
  c3) echo "zz;flush ruleset 10.199.0.4 /run/docker/netns/n3" ;;
  esac ;;
esac`,
	"nsenter": `#!/bin/sh
case "$1" in
--net=/run/docker/netns/n1) echo "2: eth0@if7: <BROADCAST,UP> mtu 1500 qdisc noqueue state UP mode DEFAULT\    link/ether 02:42:0a:c7:00:02 brd ff:ff:ff:ff:ff:ff" ;;
--net=/run/docker/netns/n2) echo "2: eth0@if8: <BROADCAST,UP> mtu 1500 qdisc noqueue state UP mode DEFAULT\    link/ether 02:42:0a:c7:00:03 brd ff:ff:ff:ff:ff:ff" ;;
--net=/run/docker/netns/n3) echo "2: eth0@if9: <BROADCAST,UP> mtu 1500 qdisc noqueue state UP mode DEFAULT\    link/ether zz;flush brd ff:ff:ff:ff:ff:ff" ;;
esac`,
	"ip": `#!/bin/sh
[ "$1 $2" = "link set" ] && { echo "ip $*" >>"$NFT_LOG"; exit 0; }
if [ "$*" = "-o link show master br-potluckbox" ]; then
echo "7: veth1a2b3c@if2: <BROADCAST,UP> mtu 1500 master br-potluckbox"
echo "9: veth7a8b9c@if2: <BROADCAST,UP> mtu 1500 master br-potluckbox"
exit 0
fi
echo "1: lo: <LOOPBACK,UP> mtu 65536"
echo "7: veth1a2b3c@if2: <BROADCAST,UP> mtu 1500 master br-potluckbox"
echo "8: veth4d5e6f@if2: <BROADCAST,UP> mtu 1500 master docker0"
echo "9: veth7a8b9c@if2: <BROADCAST,UP> mtu 1500 master br-potluckbox"`,
	"readlink": `#!/bin/sh
case "$1" in
/sys/class/net/veth1a2b3c/master|/sys/class/net/veth7a8b9c/master) echo ../br-potluckbox ;;
*) echo ../docker0 ;;
esac`,
	"nft":     "#!/bin/sh\n[ \"$1\" = -f ] && cat >>\"$NFT_LOG\"\n",
	"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
}

func runGuardOnce(t *testing.T, env ...string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range guardFakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "guard")
	if err := os.WriteFile(script, []byte(boxGuardScript), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "nft.log")
	cmd := exec.Command("sh", script, BoxNet, BoxNetBridge, currentBoxNet.guardTable(), "--once")
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "NFT_LOG=" + log, "TMPDIR=" + dir}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guard --once: %v: %s", err, out)
	}
	b, _ := os.ReadFile(log)
	return string(b)
}

// One pass of the real script: the tables, then the sets with exactly the
// box whose interface, peer and bridge all check out.
func TestBoxGuardScriptOnce(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	nft := runGuardOnce(t)
	for _, want := range []string{
		"add chain bridge potluck_box_guard from_box { type filter hook prerouting priority -300; policy accept; }",
		"add chain bridge potluck_box_guard to_host { type filter hook input priority -300; policy accept; }",
		`add rule bridge potluck_box_guard between_boxes meta ibrname "br-potluckbox" counter drop`,
		"add rule inet potluck_box_guard on_port iifname @ports counter drop",
		"add rule arp potluck_box_guard on_port iifname @ports counter drop",
		"flush set bridge potluck_box_guard boxes",
		"add element bridge potluck_box_guard boxes { veth1a2b3c . 02:42:0a:c7:00:02 . 10.199.0.2 }",
		"ip link set br-potluckbox type bridge no_linklocal_learn 1",
		// Every port of the bridge, admitted or not (veth7a8b9c is not).
		"add element inet potluck_box_guard ports { veth1a2b3c,veth7a8b9c }",
		"add element arp potluck_box_guard ports { veth1a2b3c,veth7a8b9c }",
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("nft input lacks %q:\n%s", want, nft)
		}
	}
	if strings.Contains(nft, "veth7a8b9c . ") {
		t.Errorf("a port with no admitted box is in the boxes set:\n%s", nft)
	}
	for _, bad := range []string{"veth4d5e6f", "zz", "flush ruleset", "10.199.0.3", "10.199.0.4"} {
		if strings.Contains(nft, bad) {
			t.Errorf("nft input admits %q:\n%s", bad, nft)
		}
	}
	// Docker not answering leaves the sets as they are: tables, no flush.
	down := runGuardOnce(t, "DOCKER_DOWN=1")
	if !strings.Contains(down, "add table bridge potluck_box_guard") || strings.Contains(down, "flush set") {
		t.Errorf("with docker down the sets were rewritten:\n%s", down)
	}
	// Docker not answering for one box: the others are added, none removed.
	partial := runGuardOnce(t, "INSPECT_DOWN=1")
	if strings.Contains(partial, "flush set bridge") || !strings.Contains(partial, "add element bridge potluck_box_guard boxes { veth1a2b3c . 02:42:0a:c7:00:02 . 10.199.0.2 }") {
		t.Errorf("one box's lookup failing removed admitted boxes:\n%s", partial)
	}
	nftCheck(t, nft)
}

// nftCheck has the kernel parse what the script gives nft (nft -c, in a
// network namespace of this user's own), where this machine allows it.
func nftCheck(t *testing.T, rules string) {
	t.Helper()
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Log("no unshare: nft syntax unchecked")
		return
	}
	nftBin, err := exec.LookPath("nft")
	if err != nil {
		t.Log("no nft: nft syntax unchecked")
		return
	}
	if err := exec.Command("unshare", "-rn", "true").Run(); err != nil {
		t.Log("no unprivileged network namespace here: nft syntax unchecked")
		return
	}
	var keep []string
	for _, l := range strings.Split(rules, "\n") {
		if !strings.HasPrefix(l, "ip ") {
			keep = append(keep, l)
		}
	}
	f := filepath.Join(t.TempDir(), "rules.nft")
	if err := os.WriteFile(f, []byte(strings.Join(keep, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("unshare", "-rn", nftBin, "-c", "-f", f).CombinedOutput(); err != nil {
		t.Fatalf("nft rejects the guard's rules: %v: %s", err, out)
	}
}
