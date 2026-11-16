package runtarget

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// lendsWithSwap makes "lender" a heard host of project P advertising binding
// b with its swap in state.
func lendsWithSwap(s *Service, state string) {
	s.link = &teamlink.Service{}
	s.enrolled = func(context.Context, string, string) bool { return true }
	s.adverts.set("P", "lender", []computeapi.RunTargetAdvert{{Binding: "b", Describe: computeapi.RunTargetDescribe{
		Kind: computeapi.RunTargetKindBox, Swap: state}}})
}

// A relayed op carrying secrets is handed to its host only while its swap is
// still safe: one that began swapping to a plain disk after the check at the
// call gets nothing, and the refusal names the fix and no secret.
func TestOpArgsAsksTheSwapAgain(t *testing.T) {
	s := &Service{d: &core.Deps{Config: &config.Config{Machine: "mini"}}, opClass: func(context.Context, string, string) string { return api.MachineTrusted }}
	args := json.RawMessage(`{"argv":["true"],"secrets":{"API_KEY":"swap-canary-value"}}`)
	for _, c := range []struct {
		state string
		want  string // in the refusal; "" lets the op on
	}{
		{hostswap.PlainDisk, "swaps to a plain disk"},
		{"", "does not report its swap state"},
		{hostswap.Zram, ""},
	} {
		lendsWithSwap(s, c.state)
		s.rtmem.Put("op-swap", args)
		s.secretOps.Store("op-swap", "lender")
		m := &api.MachineOp{ID: "op-swap", Kind: opRunTarget}
		setRTOp(m, &computeapi.RunTargetOp{Binding: "b", Verb: verbExec, ID: "box:b/1"})
		err := s.opArgs(context.Background(), "P", m)
		switch {
		case c.want == "" && err != nil && strings.Contains(err.Error(), "swap"):
			t.Errorf("%q: refused for swap: %v", c.state, err)
		case c.state == hostswap.PlainDisk && (err == nil || !strings.Contains(err.Error(), hostswap.Fix)):
			t.Errorf("a plain disk: the refusal does not name the fix: %v", err)
		case c.want != "" && (code(err) != api.CodeForbidden || !strings.Contains(err.Error(), c.want) ||
			strings.Contains(err.Error(), "swap-canary-value") || strings.Contains(string(m.Payload), "swap-canary-value")):
			t.Errorf("%q: %v %s", c.state, err, m.Payload)
		}
	}
	s.adverts.set("P", "lender", nil) // no longer advertised: nothing is known of its swap
	s.rtmem.Put("op-swap", args)
	m := &api.MachineOp{ID: "op-swap", Kind: opRunTarget}
	setRTOp(m, &computeapi.RunTargetOp{Binding: "b", Verb: verbExec, ID: "box:b/1"})
	if err := s.opArgs(context.Background(), "P", m); code(err) != api.CodeUnavailable || !strings.Contains(err.Error(), "advertises no run target") {
		t.Errorf("a host that advertises nothing: %v, want unavailable, not an older build's refusal", err)
	}
}

// A relayed op carrying secrets is handed to its host only while the host
// is still trusted: one demoted after the grant check gets nothing, and the
// refusal names no secret. opArgs never answers the waiting call itself:
// opRefused does, once teamlink ended the op with that refusal.
func TestOpArgsAsksTheClassAgain(t *testing.T) {
	class := api.MachineTrusted
	// No seal key module here: a trusted host's op goes on to sealing,
	// which refuses it for want of a key; the class check comes first.
	s := &Service{d: &core.Deps{Config: &config.Config{Machine: "mini"}}, opClass: func(context.Context, string, string) string { return class }}
	lendsWithSwap(s, hostswap.Encrypted)
	args := json.RawMessage(`{"argv":["true"],"secrets":{"API_KEY":"class-canary-value"}}`)
	var call *teamlink.OpCall
	op := func(id string) *api.MachineOp {
		call = s.rtmem.Put(id, args)
		s.secretOps.Store(id, "lender")
		m := &api.MachineOp{ID: id, Kind: opRunTarget}
		setRTOp(m, &computeapi.RunTargetOp{Binding: "b", Verb: verbExec, ID: "box:b/1"})
		return m
	}
	ok := op("op-1")
	if err := s.opArgs(context.Background(), "P", ok); code(err) == api.CodeForbidden || strings.Contains(string(ok.Payload), "class-canary-value") {
		t.Fatalf("a trusted host: %v %s", err, ok.Payload)
	}
	class = api.MachinePublic
	gone := op("op-2")
	err := s.opArgs(context.Background(), "P", gone)
	if code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "public host") || strings.Contains(string(gone.Payload), "class-canary-value") || strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("a demoted host: %v %s", err, gone.Payload)
	}
	select {
	case <-call.Done():
		t.Fatalf("opArgs answered the waiting call: %v", call.Result().Err())
	default:
	}
	s.opRefused(context.Background(), "P", *gone, err)
	select {
	case <-call.Done():
		if err := call.Result().Err(); code(err) != api.CodeForbidden || strings.Contains(err.Error(), "API_KEY") {
			t.Fatalf("the waiting call's answer: %v", err)
		}
	default:
		t.Fatal("opRefused did not answer the waiting call")
	}
}

func code(err error) string {
	if e, ok := err.(*api.Error); ok {
		return e.Code
	}
	return ""
}

// Refused on an answer (it cannot be sealed for the host now, or the host
// was demoted), an op goes out with no secret and opArgs leaves its waiting
// call alone: an overlapping answer may have carried the op, and only
// teamlink knows whether the refusal ended it (opRefused then answers the
// call, keeping the refusal's code).
func TestOpArgsNeverAnswersTheCaller(t *testing.T) {
	for _, c := range []struct {
		name  string
		class string
	}{
		{"cannot be sealed now", api.MachineTrusted}, // no seal key module: sealing refuses
		{"demoted", api.MachinePublic},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Service{d: &core.Deps{Config: &config.Config{Machine: "mini"}}, opClass: func(context.Context, string, string) string { return c.class }}
			lendsWithSwap(s, hostswap.Encrypted)
			ctx := core.WithRemote(context.Background(), core.Remote{Node: "lender", Via: core.ViaTunnel})
			args := json.RawMessage(`{"argv":["true"],"secrets":{"API_KEY":"sent-canary-value"}}`)
			call := s.rtmem.Put("op-1", args)
			s.secretOps.Store("op-1", "lender")
			m := &api.MachineOp{ID: "op-1", Kind: opRunTarget}
			setRTOp(m, &computeapi.RunTargetOp{Binding: "b", Verb: verbExec, ID: "box:b/1"})
			err := s.opArgs(ctx, "P", m)
			if code(err) == "" || strings.Contains(string(m.Payload), "sent-canary-value") {
				t.Fatalf("refusal: %v %s", err, m.Payload)
			}
			select {
			case <-call.Done():
				t.Fatalf("opArgs answered the waiting call: %v", call.Result().Err())
			default:
			}
			s.opRefused(ctx, "P", *m, err)
			select {
			case <-call.Done():
				if got := call.Result().Err(); code(got) != code(err) {
					t.Fatalf("the waiting call's answer %v, want %v", got, err)
				}
			default:
				t.Fatal("opRefused did not answer the waiting call")
			}
		})
	}
}

// A host's adverts are listed only while it is enrolled now: one unenrolled
// since its last beat (a member's grant lowered to guest) drops out of the
// list at once, and stays out until a beat finds it enrolled again.
func TestAdvertsRecheckEnrollment(t *testing.T) {
	enrolled := true
	s := &Service{d: &core.Deps{Config: &config.Config{Machine: "mini"}}, link: &teamlink.Service{},
		enrolled: func(context.Context, string, string) bool { return enrolled }}
	ad := computeapi.RunTargetAdvert{Binding: "sandbox", Describe: computeapi.RunTargetDescribe{Kind: computeapi.RunTargetKindBox}}
	s.adverts.set("P", "lender", []computeapi.RunTargetAdvert{ad})
	ctx := context.Background()
	if got := s.RunTargetAdverts(ctx, "P"); len(got) != 1 || got[0].Machine != "lender" {
		t.Fatalf("an enrolled host's adverts: %+v", got)
	}
	enrolled = false
	if got := s.RunTargetAdverts(ctx, "P"); len(got) != 0 {
		t.Fatalf("an unenrolled host is still listed: %+v", got)
	}
	enrolled = true
	if got := s.RunTargetAdverts(ctx, "P"); len(got) != 0 {
		t.Fatalf("an unenrolled host's dropped adverts came back without a beat: %+v", got)
	}
}
