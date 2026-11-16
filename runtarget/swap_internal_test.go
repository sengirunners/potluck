package runtarget

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
)

// trustedLender is a Service whose host "lender" is trusted and advertises
// binding b with its swap in state, and the box binding on it.
func trustedLender(state string) (*Service, computeapi.RunTarget) {
	s := &Service{d: &core.Deps{Config: &config.Config{Machine: "mini"}}, now: time.Now,
		opClass: func(context.Context, string, string) string { return api.MachineTrusted }}
	lendsWithSwap(s, state)
	return s, computeapi.RunTarget{Name: "b@lender", Kind: computeapi.RunTargetKindBox, Host: "lender", HostBinding: "b", HostProject: "P"}
}

// Every way a caller or the door sets a secret on a box passes the swap
// gate before anything else is weighed: the exec and spawn gate, the secrets
// verb's gate (before any grant is read) and the door's own check.
func TestSecretGatesRefusePlainSwap(t *testing.T) {
	const canary = "swap-gate-canary-5c1e"
	ctx := context.Background()
	for _, state := range []string{hostswap.PlainDisk, hostswap.Unknown, ""} {
		s, b := trustedLender(state)
		if err := s.secretsAllowed(ctx, b, map[string]string{"API_KEY_ZZQ": canary}); code(err) != api.CodeForbidden || strings.Contains(err.Error(), canary) {
			t.Errorf("%q: exec secrets: %v, want forbidden", state, err)
		}
		rw := row{RunTargetHandle: computeapi.RunTargetHandle{Handle: "box:b@lender/r1", State: stateReady,
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}}
		set := protocol.BoxSecretsArgs{Secrets: map[string]string{"API_KEY_ZZQ": canary}}
		if _, err := s.boxSecretsGate(ctx, "P", b, rw, set); code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "swap") || strings.Contains(err.Error(), canary) {
			t.Errorf("%q: secrets verb: %v, want forbidden for swap (no grant is read)", state, err)
		}
		if err := s.hostSwapSafe(ctx, b); code(err) != api.CodeForbidden {
			t.Errorf("%q: door: %v, want forbidden", state, err)
		}
	}
	s, b := trustedLender(hostswap.Encrypted)
	if err := s.hostSwapSafe(ctx, b); err != nil {
		t.Errorf("safe swap: %v", err)
	}
	// A binding on this machine is not gated by the pool.
	local := computeapi.RunTarget{Name: "b", Kind: computeapi.RunTargetKindBox}
	if err := s.hostSwapSafe(ctx, local); err != nil {
		t.Errorf("a local binding: %v", err)
	}
}

// At hand-over only an op that sets a secret is held for swap: a jit_config
// (a runner job's acquire) and an unset carry no secret into the memory
// filesystem, so a host on an older build keeps those.
func TestOpArgsHoldsOnlyAnOpThatSetsASecretForSwap(t *testing.T) {
	for _, c := range []struct {
		args string
		held bool
	}{
		{`{"argv":["true"],"secrets":{"API_KEY":"v"}}`, true},
		{`{"SECRETS":{"API_KEY":"v"}}`, true},
		{`{"secrets":{"API_KEY":"v"},"jit_config":"x"}`, true},
		{`not json`, true},
		{`{"jit_config":"x"}`, false},
		{`{"unset":["API_KEY"]}`, false},
		{`{"secrets":{},"unset":["API_KEY"]}`, false},
		{`{"secrets":null,"jit_config":"x"}`, false},
		{``, false},
	} {
		if got := setsSecrets(json.RawMessage(c.args)); got != c.held {
			t.Errorf("setsSecrets(%s) = %t", c.args, got)
		}
	}
	s := &Service{d: &core.Deps{Config: &config.Config{Machine: "mini"}}, opClass: func(context.Context, string, string) string { return api.MachineTrusted }}
	lendsWithSwap(s, "") // an older build
	for _, args := range []string{`{"jit_config":"x"}`, `{"unset":["API_KEY"]}`} {
		s.rtmem.Put("op-keep", json.RawMessage(args))
		s.secretOps.Store("op-keep", "lender")
		m := &api.MachineOp{ID: "op-keep", Kind: opRunTarget}
		setRTOp(m, &computeapi.RunTargetOp{Binding: "b", Verb: verbExec, ID: "box:b/1"})
		if err := s.opArgs(context.Background(), "P", m); err != nil && strings.Contains(err.Error(), "swap") {
			t.Errorf("%s was held back for swap on an older host: %v", args, err)
		}
	}
}

// A reading cut short (the caller went away, or the commands hung) is unknown
// for that beat and is not kept: the next beat reads again, so one aborted
// heartbeat does not make a safe host unsafe for the cache's lifetime. A
// whole reading is kept for swapFresh.
func TestLocalSwapIsNotCachedWhenCutShort(t *testing.T) {
	old := hostswap.Local
	t.Cleanup(func() { hostswap.Local = old })
	reads := 0
	hostswap.Local = func(context.Context) hostswap.Reading { reads++; return hostswap.Reading{State: hostswap.Encrypted} }
	s := &Service{}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.localSwap(gone); got != hostswap.Unknown {
		t.Errorf("a cut-short reading: %q, want unknown", got)
	}
	if got := s.localSwap(context.Background()); got != hostswap.Encrypted {
		t.Errorf("the next beat: %q, want it read again (encrypted)", got)
	}
	s.localSwap(context.Background())
	if reads != 2 {
		t.Errorf("%d reads, want 2 (the cut-short one and one kept for the next beat)", reads)
	}
}
