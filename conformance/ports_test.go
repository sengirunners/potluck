package conformance

import (
	"context"
	"strings"
	"testing"
)

func TestPortConformanceRef(t *testing.T) {
	path, opt := fixture(t, "box", "")
	opt.Only = []string{"ports"}
	results := Run(context.Background(), path, opt)
	if Failed(results) > 0 {
		t.Fatal(Format(results))
	}
	t.Log(Format(results))
}

// The peer consumes the seed before the exchange, selecting the half-duplex
// fixture's write direction. Its later read is a genuine opposite-direction
// operation regardless of buffering or request scheduling.
func TestPortConformanceHalfDuplex(t *testing.T) {
	for _, bug := range []string{"", "port-half-duplex"} {
		t.Run("driver-"+bug, func(t *testing.T) {
			path, opt := fixture(t, "box", bug)
			opt.Only = []string{"ports"}
			results := Run(context.Background(), path, opt)
			out := Format(results)
			t.Log(out)
			if bug == "" {
				if Failed(results) > 0 {
					t.Fatal(out)
				}
			} else if !strings.Contains(out, "FAIL  "+portDuplex) {
				t.Fatalf("half-duplex stream escaped public duplex check:\n%s", out)
			}
		})
	}
}
