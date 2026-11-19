package runtarget

// The grant check for renter secrets on a box exec or spawn (the secrets
// door, part 1; package grants). It runs at the authority before anything
// is sent to a driver or a host.

import (
	"context"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// secretsGranted checks a box command's secrets against the pool's grants:
// their names and size first (a refusal names nothing), then the grant
// check, which records a refusal in the ledger. A public host keeps its
// own refusal (hostTrusted's words). It answers nil with no secrets.
func (s *Service) secretsGranted(ctx context.Context, project string, b computeapi.RunTarget, rw row, verb string, secrets map[string]string) (*grants.Decision, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	if baseBuilder(rw) {
		return nil, api.Errorf(api.CodeForbidden, "a push base builder takes no secrets")
	}
	n := 0
	sizes := make(map[string]int, len(secrets))
	for k, v := range secrets {
		if !secretName.MatchString(k) {
			return nil, api.Errorf(api.CodeBadRequest, "a secret name is not [A-Z][A-Z0-9_]{0,63}")
		}
		n += len(k) + len(v)
		sizes[k] = len(v)
	}
	if n > secretsMax {
		return nil, api.Errorf(api.CodeBadRequest, "secrets are over 64 KiB (names and values)")
	}
	class := api.MachineTrusted // a binding on this machine is the owner's own
	if b.Host != "" {
		class = s.hostClass(ctx, b)
	}
	dec, err := grants.Check(ctx, s.d, grants.Request{Project: project, Area: rw.Area, Subject: core.SubjectOf(ctx),
		Kind: computeapi.SecretKindValue, Scope: computeapi.SecretScopeExec, Verb: verb, Target: b.Name, Box: rw.Handle,
		Host: b.Host, HostClass: class, Sizes: sizes})
	if err != nil {
		if class != api.MachineTrusted {
			if e := s.hostTrusted(ctx, b, "secrets"); e != nil {
				return nil, e
			}
		}
		return nil, err
	}
	return &dec, nil
}

// jitGranted is the grant check for a caller's jit_config on a box acquire
// (kind github-jit, scope box): the runner broker's own acquire comes
// through here too, as its linked area's member. The default grant covers
// it as before grants existed. A public host keeps its own refusal.
func (s *Service) jitGranted(ctx context.Context, project, area string, b computeapi.RunTarget) (*grants.Decision, error) {
	class := api.MachineTrusted
	if b.Host != "" {
		class = s.hostClass(ctx, b)
	}
	dec, err := grants.Check(ctx, s.d, grants.Request{Project: project, Area: area, Subject: core.SubjectOf(ctx),
		Kind: computeapi.SecretKindGitHubJIT, Scope: computeapi.SecretScopeBox, Verb: verbAcquire, Target: b.Name,
		Host: b.Host, HostClass: class, Name: computeapi.SecretNameJIT})
	if err != nil {
		if class != api.MachineTrusted {
			if e := s.hostTrusted(ctx, b, "jit_config"); e != nil {
				return nil, e
			}
		}
		return nil, err
	}
	return &dec, nil
}

// secretsSent records, around the call that carries them, that secrets
// were handed to the transport (Granted: nothing is sent if the ledger
// cannot take it) and then that the call answered.
func (s *Service) secretsSent(ctx context.Context, dec *grants.Decision, call func() error) error {
	if dec == nil {
		return call()
	}
	if err := grants.Granted(ctx, s.d, *dec); err != nil {
		return err
	}
	err := call()
	grants.Delivered(ctx, s.d, *dec, err != nil)
	return err
}
