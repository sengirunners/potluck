package runtarget

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// sealSvc is a run-target module with its daemon's seal key (the tunnel
// module's) in a temp home, enough for sealing and opening relayed ops.
func sealSvc(t *testing.T) (*Service, *tunnel.Service) {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	tun := tunnel.New(d)
	d.AddTransport(tun.TeamTransport())
	return &Service{d: d}, tun
}

// hostRemote is the host's poll as the authority sees it: the key it named.
func hostRemote(t *testing.T, h *tunnel.Service, via string) *core.Remote {
	t.Helper()
	id, key, ok := keyOf(h)
	if !ok {
		t.Fatal("host names no key")
	}
	rm := &core.Remote{Via: via, NodeID: "nHOST", SealID: id, SealKey: key}
	if via == core.ViaTunnel {
		rm.NodeID = id
	}
	return rm
}

func rtMachineOp(id, binding, verb, box string, args []byte) *api.MachineOp {
	op := &api.MachineOp{ID: id, Kind: opRunTarget}
	setRTOp(op, &computeapi.RunTargetOp{Binding: binding, Verb: verb, ID: box, Args: args})
	return op
}

// sealFor is opArgs's sealing of args for op, as the authority a does it
// for the host whose poll is rm.
func sealFor(t *testing.T, a *Service, rm *core.Remote, op *api.MachineOp, args []byte) *api.MachineOp {
	t.Helper()
	sealed, by, e := a.sealOpArgs(rm, "p1", op, args)
	if e != nil {
		t.Fatalf("seal: %v", e)
	}
	rop := rtOp(op)
	rop.Sealed, rop.SealedBy, rop.Sealing = sealed, by, 1
	out := *op
	setRTOp(&out, rop)
	return &out
}

// The authority seals a relayed op's secrets to the host's key on every
// transport; only that host opens them, for that op, box, verb and project.
func TestRelayedOpSealedOnEveryTransport(t *testing.T) {
	a, atun := sealSvc(t)
	h, htun := sealSvc(t)
	other, otun := sealSvc(t)
	afp, _ := atun.Fingerprint()
	args := []byte(`{"argv":["sh","-c","x"],"secrets":{"CANARY":"c4n4ry-relay"}}`)
	for _, via := range []string{core.ViaTailnet, core.ViaTunnel} {
		t.Run(via, func(t *testing.T) {
			op := rtMachineOp("op1", "sandbox", "exec", "box1", nil)
			sealed := sealFor(t, a, hostRemote(t, htun, via), op, args)
			if bytes.Contains(sealed.Payload, []byte("c4n4ry")) || bytes.Contains(sealed.Payload, []byte("CANARY")) {
				t.Fatalf("op payload carries the secret: %s", sealed.Payload)
			}
			fp := ""
			if via == core.ViaTunnel {
				fp = afp
			}
			got, e := h.opArgsHere(via, fp, "p1", sealed)
			if e != nil || !bytes.Equal(got, args) {
				t.Fatalf("host opens: %s %v", got, e)
			}
			refused := func(what string, got []byte, e *api.Error) {
				t.Helper()
				if e == nil || got != nil || e.Code != api.CodeBadRequest || strings.Contains(e.Message, "c4n4ry") {
					t.Errorf("%s: want the one 400, got %s %v", what, got, e)
				}
			}
			// Another host (a wrong recipient).
			got, e = other.opArgsHere(via, fp, "p1", sealed)
			refused("another host", got, e)
			// Sealed to the caller's (another daemon's) key instead of the
			// host's: the host cannot open it.
			toCaller := sealFor(t, a, hostRemote(t, otun, via), rtMachineOp("op1", "sandbox", "exec", "box1", nil), args)
			got, e = h.opArgsHere(via, fp, "p1", toCaller)
			refused("sealed to the caller's key", got, e)
			// The same blob on another op, box, verb or project.
			for _, x := range []*api.MachineOp{
				rtMachineOp("op2", "sandbox", "exec", "box1", nil),
				rtMachineOp("op1", "sandbox", "exec", "box2", nil),
				rtMachineOp("op1", "sandbox", "spawn", "box1", nil),
				rtMachineOp("op1", "other", "exec", "box1", nil),
			} {
				rop := rtOp(x)
				rop.Sealed, rop.SealedBy, rop.Sealing = rtOp(sealed).Sealed, rtOp(sealed).SealedBy, 1
				setRTOp(x, rop)
				got, e := h.opArgsHere(via, fp, "p1", x)
				refused("moved to "+string(x.Payload), got, e)
			}
			got, e = h.opArgsHere(via, fp, "p2", sealed)
			refused("another project", got, e)
			// Tampered.
			rop := rtOp(sealed)
			b := []byte(rop.Sealed)
			b[len(b)-3] ^= 1
			rop.Sealed = string(b)
			bad := *sealed
			setRTOp(&bad, rop)
			got, e = h.opArgsHere(via, fp, "p1", &bad)
			refused("tampered", got, e)
			// Claimed by another sender.
			rop = rtOp(sealed)
			oid, _, _ := keyOf(otun)
			rop.SealedBy = oid
			moved := *sealed
			setRTOp(&moved, rop)
			if via == core.ViaTailnet {
				got, e = h.opArgsHere(via, fp, "p1", &moved)
				refused("another sender", got, e)
			} else {
				// Over the tunnel the pinned home key decides, not the field.
				got, e = h.opArgsHere(via, "aaaaaaaaaaaaaaaaaaaaaaaaaa", "p1", sealed)
				refused("another pinned home", got, e)
			}
		})
	}
}

// A host that names no key (a Potluck or zm from before sealing on that
// leg) gets no secrets: the authority refuses, plainly. An op holding
// secrets in the clear from an authority that seals is refused by the host;
// one from an older authority (no Sealing mark) is taken as before.
func TestRelayedOpOlderPeers(t *testing.T) {
	a, _ := sealSvc(t)
	h, htun := sealSvc(t)
	args := []byte(`{"argv":["true"],"secrets":{"A":"c4n4ry"}}`)
	op := rtMachineOp("op1", "sandbox", "exec", "box1", nil)
	for _, rm := range []*core.Remote{nil, {Via: core.ViaTailnet, NodeID: "nHOST"}, {Via: core.ViaTunnel, NodeID: "zmkey:x"}} {
		_, _, e := a.sealOpArgs(rm, "p1", op, args)
		if e == nil || e.Code != api.CodeForbidden || !strings.Contains(e.Message, "the host runs a Potluck (or zm) too old") || strings.Contains(e.Message, "c4n4ry") {
			t.Fatalf("older host %+v: %v", rm, e)
		}
	}
	// An authority whose sealing is off (standing in for an older one) seals
	// nothing.
	undo := htun.SealOff()
	if _, _, e := a.sealOpArgs(&core.Remote{Via: core.ViaTailnet, NodeID: "nHOST"}, "p1", op, args); e == nil {
		t.Fatal("sealed to a host that names no key")
	}
	undo()

	clear := rtMachineOp("op1", "sandbox", "exec", "box1", args)
	rop := rtOp(clear)
	rop.Sealing = 1
	setRTOp(clear, rop)
	if got, e := h.opArgsHere(core.ViaTailnet, "", "p1", clear); e == nil || e.Code != api.CodeForbidden || got != nil {
		t.Fatalf("clear secrets from an authority that seals: %s %v", got, e)
	}
	legacy := rtMachineOp("op1", "sandbox", "exec", "box1", args)
	if got, e := h.opArgsHere(core.ViaTailnet, "", "p1", legacy); e != nil || !bytes.Equal(got, args) {
		t.Fatalf("clear secrets from an older authority: %s %v", got, e)
	}
	// Secret-free args go in the clear from anyone.
	free := rtMachineOp("op1", "sandbox", "exec", "box1", []byte(`{"argv":["true"],"secrets":{}}`))
	rop = rtOp(free)
	rop.Sealing = 1
	setRTOp(free, rop)
	if _, e := h.opArgsHere(core.ViaTailnet, "", "p1", free); e != nil {
		t.Fatalf("secret-free op: %v", e)
	}
}

// keyOf is a daemon's seal key id and key, as its headers name them.
func keyOf(tun *tunnel.Service) (string, []byte, bool) {
	idB64, cert, err := tun.SealIdentity()
	if err != nil {
		return "", nil, false
	}
	return sealbox.Peer(idB64, cert)
}

// A host refuses secrets in the clear when the op came over the tunnel or
// its pool's machine named a seal key to it before (a downgrade), whatever
// the op's mark; an authority whose own key cannot be read sends none.
func TestRelayedOpDowngrades(t *testing.T) {
	h, htun := sealSvc(t)
	args := []byte(`{"argv":["true"],"jit_config":"QUJDREVGR0hJSktMTU5PUA=="}`)
	clear := rtMachineOp("op1", "sandbox", "acquire", "", args)
	if _, e := h.opArgsHere(core.ViaTunnel, "fp", "p1", clear); e == nil || e.Code != api.CodeForbidden || !strings.Contains(e.Message, "tunnel") {
		t.Fatalf("clear over the tunnel: %v", e)
	}
	if got, e := h.opArgsHere(core.ViaTailnet, "homefp", "p1", clear); e != nil || !bytes.Equal(got, args) {
		t.Fatalf("clear from an authority that never sealed: %s %v", got, e)
	}
	// A pool's machine that reported preferred and rolled back: taken, warned.
	if err := htun.MarkHomeSealed("homefp", "zmkey:home", "preferred"); err != nil {
		t.Fatal(err)
	}
	if got, e := h.opArgsHere(core.ViaTailnet, "homefp", "p1", clear); e != nil || !bytes.Equal(got, args) {
		t.Fatalf("clear from a preferred pool's machine that stopped sealing: %s %v", got, e)
	}
	// One that reported required: refused until forgotten.
	if err := htun.MarkHomeSealed("homefp", "zmkey:home", "required"); err != nil {
		t.Fatal(err)
	}
	if _, e := h.opArgsHere(core.ViaTailnet, "homefp", "p1", clear); e == nil || e.Code != api.CodeForbidden || !strings.Contains(e.Message, "requires sealing") {
		t.Fatalf("clear from a required pool's machine that sealed before: %v", e)
	}
	if forgot, err := htun.ForgetHomeSealed("homefp"); !forgot || err != nil {
		t.Fatal(forgot, err)
	}
	if _, e := h.opArgsHere(core.ViaTailnet, "homefp", "p1", clear); e != nil {
		t.Fatalf("after forget: %v", e)
	}

	a, atun := sealSvc(t)
	_, htun2 := sealSvc(t)
	key := filepath.Join(a.d.Config.Home, tunnel.SealKeyFile)
	_ = atun
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(strings.Repeat("07", 32)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, e := a.sealOpArgs(hostRemote(t, htun2, core.ViaTailnet), "p1", rtMachineOp("op2", "sandbox", "exec", "b", nil), args); e == nil || e.Code != api.CodeUnavailable || strings.Contains(e.Message, "chmod") || strings.Contains(e.Message, a.d.Config.Home) {
		t.Fatalf("own key fault: %v", e)
	}
}

// A machine that sealed before and names no key now is a downgrade signal:
// warned every time, not once.
func TestStoppedSealingWarnsEveryTime(t *testing.T) {
	s, _ := sealSvc(t)
	var logs bytes.Buffer
	s.d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	if s.stoppedSealing("p1", "box", "x") {
		t.Fatal("a machine never heard sealing counted as stopped")
	}
	s.heardSeal("p1", "box", true, true)
	s.heardSeal("p1", "box", false, true)
	for range 3 {
		if !s.stoppedSealing("p1", "box", "x") {
			t.Fatal("not seen as stopped")
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 3 {
		t.Fatalf("want 3 Warns, got %d: %s", n, logs.String())
	}
}
