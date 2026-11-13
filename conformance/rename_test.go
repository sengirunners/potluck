package conformance

import (
	"context"
	"strings"
	"testing"
)

// describe may name the protocol under "potluck" (this build's drivers send
// it, beside "zm") or "zm" alone (a step 1 or third-party driver), with
// zm/v0 or potluck/1.
func TestDescribeProtocolKeys(t *testing.T) {
	protoErr := func(d description) string {
		err := describeShape(d, "")
		if err == nil {
			return ""
		}
		for _, l := range strings.Split(err.Error(), "; ") {
			if strings.HasPrefix(l, "zm ") || strings.HasPrefix(l, "potluck ") || strings.Contains(l, "zm \"") || strings.Contains(l, "potluck \"") {
				return l
			}
		}
		return ""
	}
	for _, c := range []struct {
		zm, potluck string
		ok          bool
	}{
		{"zm/v0", "", true},
		{"potluck/1", "", true},
		{"", "potluck/1", true},
		{"", "zm/v0", true},
		{"zm/v0", "potluck/1", true},
		{"", "", false},
		{"zm/v9", "", false},
		{"", "potluck/2", false},
		{"zm/v0", "potluck/2", false},
	} {
		got := protoErr(description{Spec: c.zm, SpecNext: c.potluck})
		if (got == "") != c.ok {
			t.Errorf("zm %q potluck %q: %q", c.zm, c.potluck, got)
		}
	}
}

// A driver that names things only the old way (describe's "zm": "zm/v0",
// ZM_SECRETS: a step 1 or third-party driver) or only the new way
// ("potluck": "potluck/1", POTLUCK_SECRETS) passes the describe and secrets
// checks.
func TestDriverNamesEitherWay(t *testing.T) {
	for _, variant := range []string{"zm-names", "potluck-names"} {
		path, opt := fixture(t, "box", variant)
		opt.Only = []string{"lifecycle", "procs"}
		out := Format(Run(context.Background(), path, opt))
		for _, want := range []string{"PASS  describe", "PASS  exec secrets", "PASS  spawn secrets"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: no %q:\n%s", variant, want, out)
			}
		}
		if strings.Contains(out, "FAIL") {
			t.Errorf("%s fails:\n%s", variant, out)
		}
	}
}
