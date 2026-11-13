package project

import (
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// A tailnet join code may carry the authority node's full MagicDNS name
// (zm-my5): a node shared in from another tailnet resolves only by it.
// Older codes with the short name still parse.
func TestJoinCodeFullMagicDNSName(t *testing.T) {
	fp := strings.Repeat("a", 16)
	c, err := ParseJoinCode("zm-host-c.example.ts.net:" + fp + ":ABCD-EFGH")
	if err != nil || c.Machine != "host-c" || c.Host != "zm-host-c.example.ts.net" || c.AuthorityHost() != "zm-host-c.example.ts.net" {
		t.Fatalf("full name: %+v %v", c, err)
	}
	c, err = ParseJoinCode("zm-host-c:" + fp + ":ABCD-EFGH")
	if err != nil || c.Machine != "host-c" || c.Host != "" || c.AuthorityHost() != "zm-host-c" {
		t.Fatalf("short name: %+v %v", c, err)
	}
	for _, bad := range []string{"zm-host-c..ts.net:" + fp + ":ABCD-EFGH", "zm-host-c.-x.ts.net:" + fp + ":ABCD-EFGH",
		"zm-host-c.ex_ample.net:" + fp + ":ABCD-EFGH", "zm-.tail.ts.net:" + fp + ":ABCD-EFGH",
		// Only a tailnet name: never an internet host.
		"zm-host-c.example.com:" + fp + ":ABCD-EFGH", "zm-host-c.ts.net.evil.com:" + fp + ":ABCD-EFGH", "zm-host-c.ts.net:" + fp + ":ABCD-EFGH"} {
		if _, err := ParseJoinCode(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// An invite carries the node's full name only when a code may (a .ts.net
// tailnet); a Headscale tailnet's own domain leaves the short code, which
// reaches the node through the pin-picked fallback (zm-my5 review).
func TestInviteFullNameOnlyWhenACodeMayCarryIt(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{
		"zm-mini.example.ts.net":  "zm-mini.example.ts.net:",
		"zm-mini.hs.example.com":  "zm-mini:",
		"zm-other.example.ts.net": "zm-mini:", // not this node's own name
	} {
		inv, err := f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", As: "ana", Role: "member"},
			Authority: "mini", Host: host, Fingerprint: NodeFingerprint("nMINI")})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(inv.Code, want) {
			t.Errorf("host %s: code %q, want %s...", host, inv.Code, want)
		}
		if _, err := ParseJoinCode(inv.Code); err != nil {
			t.Errorf("host %s: the code it minted does not parse: %v", host, err)
		}
	}
}
