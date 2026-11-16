package runtarget

// Box secrets (docs/protocol.md, Box secrets; zm-eakh.12.48.3): a secret
// for the life of a box, not one command. POST .../handles/{id}/secrets
// carries the secrets verb to the box's driver, here or relayed to its host
// (sealed on a tunnel leg: its values travel under "secrets", which
// carriesSecret seals). It is the holder's, on a trusted host only, under a
// grant at scope box (grants.Check; no built-in default covers box scope),
// never for a name the authority mints itself (grants.Reserved), to a
// driver listing box-secrets. Nothing here keeps,
// logs or quotes a value: a driver's refusal is scrubbed of every name and
// value sent before it reaches the caller.

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
)

// boxSecrets sets and unsets a box's secrets, for its holder.
func (s *Service) boxSecrets(r *http.Request) (any, error) {
	pid, b, _, rw, err := s.loadHandle(r, authz.Operate)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	if b.Kind != computeapi.RunTargetKindBox {
		return nil, api.Errorf(api.CodeBadRequest, "secrets are for a box target; %s is a %s target", b.Name, b.Kind)
	}
	if err := holder(ctx, rw, false); err != nil {
		return nil, err
	}
	var body struct {
		Secrets json.RawMessage `json:"secrets"`
		Unset   json.RawMessage `json:"unset"`
	}
	if api.DecodeJSON(r, &body) != nil { // never the decoder's words: they may quote the body
		return nil, api.Errorf(api.CodeBadRequest, "the body is a JSON object of secrets and unset")
	}
	raw, _ := json.Marshal(body)
	a, err := protocol.ParseBoxSecrets(raw)
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "%v", err)
	}
	// The names it sets are recorded before the gate's reserved check
	// (boxsecret_names.go), and forgotten when nothing was written. One call
	// per box at a time: a name another call recorded is never forgotten.
	mu, _ := s.secretsMu.LoadOrStore(pid+"/"+rw.Handle, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	added, err := s.holderNames(ctx, pid, rw.Handle, slices.Sorted(maps.Keys(a.Secrets)))
	if err != nil {
		return nil, err
	}
	dec, err := s.boxSecretsGate(ctx, pid, b, rw, a)
	if err != nil {
		s.forgetNames(ctx, pid, rw.Handle, added)
		return nil, err
	}
	var out protocol.BoxSecretsResult
	if err := s.secretsSent(ctx, dec, func() error { return s.call(ctx, b, protocol.VerbSecrets, rw.Handle, a, &out) }); err != nil {
		if notWritten(err) {
			s.forgetNames(ctx, pid, rw.Handle, added)
		}
		return nil, scrubSecrets(err, sentSecrets(a))
	}
	s.forgetNames(ctx, pid, rw.Handle, a.Unset)
	return out, nil
}

// boxSecretsGate is everything a secrets call must pass before its driver
// is called, in this order: a ready box; a trusted host (a public host gets
// nothing, whatever a grant says, and the grants are never asked); no name
// the authority mints; the grant for what it sets (scope box, kind value;
// unsetting one's own names needs none); a driver listing box-secrets. Swap
// on the host that could put what it sets on disk (swap.go) is refused
// before the grants are read, as a public host is. It
// answers the ledger's decision when anything is set.
func (s *Service) boxSecretsGate(ctx context.Context, pid string, b computeapi.RunTarget, rw row, a protocol.BoxSecretsArgs) (*grants.Decision, error) {
	if err := s.boxTakesSecrets(ctx, b, rw); err != nil {
		return nil, err
	}
	if len(a.Secrets) > 0 { // unsetting writes nothing: it is never held back
		if err := s.hostSwapSafe(ctx, b); err != nil { // swap that could put them on disk (swap.go); no grant is read for it
			return nil, err
		}
	}
	class := s.boxClass(ctx, b)
	sizes := map[string]int{}
	for k, v := range a.Secrets {
		sizes[k] = len(v)
	}
	req := grants.Request{Project: pid, Area: rw.Area, Subject: core.SubjectOf(ctx), Kind: computeapi.SecretKindValue,
		Scope: computeapi.SecretScopeBox, Verb: protocol.VerbSecrets, Target: b.Name, Box: rw.Handle, Host: b.Host,
		HostClass: class, Sizes: sizes}
	if err := grants.CheckReserved(ctx, s.d, req, a.Unset); err != nil {
		return nil, err
	}
	var dec *grants.Decision
	if len(a.Secrets) > 0 {
		d, err := grants.Check(ctx, s.d, req)
		if err != nil {
			return nil, err
		}
		dec = &d
	}
	if err := s.keepsBoxSecrets(ctx, b); err != nil {
		return nil, err
	}
	return dec, nil
}

// boxTakesSecrets is the box's side of every box secrets write, the
// holder's and the authority's (door.go): a ready box on a pinned image
// before its expiry, on a trusted host (a public host gets nothing).
func (s *Service) boxTakesSecrets(ctx context.Context, b computeapi.RunTarget, rw row) error {
	switch {
	case b.Kind != computeapi.RunTargetKindBox:
		return api.Errorf(api.CodeBadRequest, "secrets are for a box target; %s is a %s target", b.Name, b.Kind)
	case rw.State != stateReady:
		return api.Errorf(api.CodeConflict, "sandbox %s is %s, not ready", sandboxID(rw.Handle), rw.State)
	case offPin(b, rw):
		return api.Errorf(api.CodeConflict, "sandbox %s booted %s, not one of the pinned images; it is being released", sandboxID(rw.Handle), rw.ImageDigest)
	}
	if exp, err := time.Parse(time.RFC3339Nano, rw.ExpiresAt); err == nil && !time.Now().Before(exp) {
		return api.Errorf(api.CodeConflict, "sandbox %s is past its expires_at", sandboxID(rw.Handle))
	}
	return s.hostTrusted(ctx, b, "secrets")
}

// boxClass is the class of the machine serving b's boxes: this machine is
// trusted; a host is what the authority says, public unless trusted.
func (s *Service) boxClass(ctx context.Context, b computeapi.RunTarget) string {
	if b.Host == "" {
		return api.MachineTrusted
	}
	return s.hostClass(ctx, b)
}

// keepsBoxSecrets refuses a binding whose driver lists no box-secrets.
func (s *Service) keepsBoxSecrets(ctx context.Context, b computeapi.RunTarget) error {
	desc, err := s.describeOf(ctx, b)
	if err != nil {
		return err
	}
	if !slices.Contains(desc.Capabilities, protocol.CapBoxSecrets) {
		return api.Errorf(api.CodeBadRequest, "run target %s keeps no box secrets (its driver lists no box-secrets capability)", b.Name)
	}
	return nil
}

// sentSecrets is every name and value a secrets call sent, as scrubSecrets
// takes them.
func sentSecrets(a protocol.BoxSecretsArgs) map[string]string {
	m := map[string]string{}
	for k, v := range a.Secrets {
		m[k] = v
	}
	for _, k := range a.Unset {
		m[k] = ""
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
