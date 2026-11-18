package project

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A link carries a tailnet or a tunnel code and gives it back unchanged;
// anything that is not a link passes through; a damaged link, one of another
// version, or one that wraps a link is refused.
func TestJoinLink(t *testing.T) {
	const base = "https://potluck.sh/join"
	for _, code := range []string{
		"potluck-mini.tail1234.ts.net:abcdefabcdefabcd:ABCD-EFGH",
		"https://pool.example.com#abcdefghijklmnopqrstuvwxyz:ABCDEFGHJKMNPQRSTVWXYZ234",
	} {
		l := JoinLink(base, code)
		if strings.ContainsAny(strings.TrimPrefix(l, base+"#"), "#:/'\" ") {
			t.Errorf("link %q has characters a chat app may mangle", l)
		}
		if got, err := JoinCodeOf(l); err != nil || got != code {
			t.Errorf("JoinCodeOf(%q) = %q, %v; want %q", l, got, err, code)
		}
		if got, err := JoinCodeOf("  " + l + "\n"); err != nil || got != code {
			t.Errorf("with spaces: %q %v", got, err)
		}
		if !LooksLikeJoinCode(l) {
			t.Errorf("LooksLikeJoinCode(%q) = false", l)
		}
		a, errA := ParseJoinCode(l)
		b, errB := ParseJoinCode(code)
		if errA != nil || errB != nil || a != b {
			t.Errorf("ParseJoinCode: link %+v %v, code %+v %v", a, errA, b, errB)
		}
		if got, err := JoinCodeOf(code); err != nil || got != code {
			t.Errorf("a code passes through: %q %v", got, err)
		}
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, l := range map[string]string{
		"bad base64":      base + "#v1.not*base64",
		"empty":           base + "#v1.",
		"another version": base + "#v2." + enc("potluck-a:b:c"),
		"no version":      base + "#" + enc("potluck-a:b:c"),
		"a link inside":   base + "#v1." + enc(JoinLink(base, "potluck-a:b:c")),
		"a newline":       base + "#v1." + enc("potluck-a:b:c\nrm -rf"),
		"a space":         base + "#v1." + enc("potluck-a:b c"),
		"too long":        base + "#v1." + strings.Repeat("A", maxJoinLink),
	} {
		if got, err := JoinCodeOf(l); err == nil {
			t.Errorf("%s: JoinCodeOf(%q) = %q, want a refusal", name, l, got)
		}
		if _, err := ParseJoinCode(l); err == nil {
			t.Errorf("%s: ParseJoinCode accepted %q", name, l)
		}
	}
	// A tunnel code is not a link: it has no /join path.
	if got, err := JoinCodeOf("https://pool.example.com#fp:secret"); err != nil || got != "https://pool.example.com#fp:secret" {
		t.Errorf("a tunnel code: %q %v", got, err)
	}
}
