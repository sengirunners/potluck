package api

import (
	"regexp"
	"strings"
	"testing"
)

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in, want string
		kind     AddressKind
	}{
		{"mini/4bb7545a", "mini/4bb7545a", KindSession},
		{"Mini/4BB7545A", "mini/4bb7545a", KindSession},
		{"mini", "machine:mini", KindMachine},
		{"machine:mini", "machine:mini", KindMachine},
		{"human:ana", "human:ana", KindHuman},
		{"all", "all", KindBroadcast},
		{"Owners", "owners", KindOwners},
		{"local:12", "local:12", KindWork},
		{"bd:Example-3x", "bd:Example-3x", KindWork},
		{"gh:acme/zm#7", "gh:acme/zm#7", KindWork},
	}
	for _, c := range cases {
		a, err := ParseAddress(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if a.Kind != c.kind || a.String() != c.want {
			t.Errorf("%q: got %s %q, want %s %q", c.in, a.Kind, a.String(), c.kind, c.want)
		}
	}
	for _, bad := range []string{"", "mini/abc", "human:", "machine:-x", "a b", ":x", "bd:"} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestSession8(t *testing.T) {
	if got := Session8("4BB7545A-1c2d-..."); got != "4bb7545a" {
		t.Errorf("got %q", got)
	}
	if got := Session8("x"); !ValidSession8(got) {
		t.Errorf("random %q not valid", got)
	}
}

// The hand-written checks match the grammar's regexps exactly.
func TestNameGrammarMatchesRegexp(t *testing.T) {
	nameRe := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	s8Re := regexp.MustCompile(`^[a-z0-9]{8}$`)
	srcRe := regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	alphabet := "az09._-A/: \x00é"
	var cases []string
	for _, a := range alphabet {
		for _, b := range alphabet {
			cases = append(cases, string(a), string(a)+string(b), "x"+string(a)+string(b)+"y")
		}
	}
	cases = append(cases, "", "abcdefgh", "abcdefg", "abcdefghi", "ABCDEFGH", strings.Repeat("a", 63), strings.Repeat("a", 64),
		strings.Repeat("b", 32), strings.Repeat("b", 33), "-ab", "a-b", "9ab")
	for _, c := range cases {
		if ValidName(c) != nameRe.MatchString(c) || ValidSession8(c) != s8Re.MatchString(c) || validSource(c) != srcRe.MatchString(c) {
			t.Errorf("%q: name %v/%v session8 %v/%v source %v/%v", c, ValidName(c), nameRe.MatchString(c),
				ValidSession8(c), s8Re.MatchString(c), validSource(c), srcRe.MatchString(c))
		}
	}
}

func TestClaimLabels(t *testing.T) {
	if got := SessionLabel("mini/4bb7545a", ""); got != "mini/4bb7545a" {
		t.Fatalf("session label %q", got)
	}
	if got := SessionLabel("mini/4bb7545a", "zm-claims"); got != "zm-claims@mini/4bb7545a" {
		t.Fatalf("lane label %q", got)
	}
	for label, want := range map[string][2]string{
		"mini/4bb7545a":           {"mini/4bb7545a", ""},
		"zm-claims@mini/4bb7545a": {"mini/4bb7545a", "zm-claims"},
		"host-a/58565660":         {"host-a/58565660", ""},
	} {
		a, l, ok := LabelSession(label)
		if !ok || a != want[0] || l != want[1] {
			t.Fatalf("LabelSession(%q) = %q %q %v", label, a, l, ok)
		}
	}
	for _, label := range []string{"", "Judy Example", "judyexample", "f779906d", "Bad Lane@mini/4bb7545a", "@mini/4bb7545a", "mini/4BB7545A", "x@y@mini/zz"} {
		if a, l, ok := LabelSession(label); ok {
			t.Fatalf("LabelSession(%q) = %q %q, want no session", label, a, l)
		}
	}
	addr := "mini/4bb7545a"
	for _, c := range []struct {
		cur, next string
		want      bool
	}{
		{"Judy Example", addr, true},
		{"Judy Example", "lane1@" + addr, true},
		{"Judy Example", "mini/00000000", false},
		{"Judy Example", "Carl", false},
		{"lane1@" + addr, addr, false}, // a lane's claim is the lane's
		{"host-a/58565660", addr, false},
		{"", addr, false},
	} {
		if got := LabelAdopts(c.cur, c.next, addr); got != c.want {
			t.Fatalf("LabelAdopts(%q, %q) = %v", c.cur, c.next, got)
		}
	}
}
