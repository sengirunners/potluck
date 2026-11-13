package cli

import (
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// potluck host with no pool named shares with the only pool this machine is
// in; with none it says to create or join one, and with several it names
// them and --pool. No pool name is assumed.
func TestHostWithoutPoolNamedNeverAssumesOne(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	says := func(want string) {
		t.Helper()
		_, e, code := m.run(t, "host")
		if code == 0 || !strings.Contains(e, want) {
			t.Fatalf("host: exit %d, stderr %q, want it to say %q", code, e, want)
		}
	}
	says("no pool yet")
	m.ok(t, "create", "--via", "tunnel", "alpha")
	m.ok(t, "create", "--via", "tunnel", "beta")
	says("more than one pool (alpha, beta): name one with --pool")
}
