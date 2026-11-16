package runtarget_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

// withSwap makes the machine's swap read as state for the host a test starts
// next (its advert carries it).
func withSwap(t *testing.T, state string) {
	t.Helper()
	old := hostswap.Local
	hostswap.Local = func(context.Context) hostswap.Reading { return hostswap.Reading{State: state} }
	t.Cleanup(func() { hostswap.Local = old })
}

// A trusted host whose swap could put a box's memory on a plain disk is
// given no box secret by the door, and the refusal names the fix; one that
// says nothing of its swap (an older build) is refused with the update; a
// host with safe swap passes this gate. The box itself is made on every one:
// secret-free work is not held back.
func TestDoorRefusesATrustedHostWithUnsafeSwap(t *testing.T) {
	const canary = "ghs_swap_canary_value"
	for _, c := range []struct {
		name, state string
		want        []string // in the refusal
	}{
		{"plain disk", hostswap.PlainDisk, []string{"swaps to a plain disk", "swapoff", "zram", "encrypt swap"}},
		{"unknown", hostswap.Unknown, []string{"cannot say its swap is safe", "swapoff", "zram", "encrypt swap"}},
		{"an older build", "", []string{"does not report its swap state", "update Potluck"}},
		{"a made-up state", "fast", []string{"cannot say its swap is safe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			withSwap(t, c.state)
			rp := newRelayPair(t)
			rp.svc.SetHostClass(api.MachineTrusted)
			h := rp.acquire(t, "swap-"+strings.ReplaceAll(c.name, " ", "-"), 120)
			err := rp.svc.BoxSecret(context.Background(), rp.project, h.Handle, "GH_TOKEN", secrets.NewValue(canary))
			if code(err) != api.CodeForbidden || strings.Contains(err.Error(), canary) {
				t.Fatalf("door: %v, want forbidden", err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q lacks %q", err, w)
				}
			}
			if !strings.Contains(err.Error(), "lender") {
				t.Errorf("refusal %q does not name the host", err)
			}
		})
	}
	for _, state := range []string{hostswap.Off, hostswap.Zram, hostswap.Encrypted} {
		t.Run("safe: "+state, func(t *testing.T) {
			withSwap(t, state)
			rp := newRelayPair(t)
			rp.svc.SetHostClass(api.MachineTrusted)
			h := rp.acquire(t, "swap-ok-"+state, 120)
			err := rp.svc.BoxSecret(context.Background(), rp.project, h.Handle, "GH_TOKEN", secrets.NewValue(canary))
			if err != nil && (strings.Contains(err.Error(), "swap") || strings.Contains(err.Error(), "plain disk")) {
				t.Fatalf("a host with %s swap was refused for swap: %v", state, err)
			}
		})
	}
}
