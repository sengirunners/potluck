package runtarget

// The secrets door's side of run targets (rt.Door, docs/daemon.md "Secrets
// issuers" and "Job grants"): the authority writes a credential it minted
// itself into a box, asks the class of the machine that serves a box, and
// whether a box still lives. These skip the holder check and the reserved
// names on purpose (the authority is the source of a minted value, and the
// name is reserved for it), so they are reachable only on core.Deps inside
// the daemon, never on a route. Every other gate of a box secrets call
// holds: a ready box on a pinned image before its expiry, a trusted host, a
// driver listing box-secrets; no value is kept, logged or quoted.

import (
	"context"
	"fmt"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

var _ rt.Door = (*Service)(nil)

// rowOf is project's handle as recorded.
func (s *Service) rowOf(ctx context.Context, project, handle string) (row, error) {
	if project == "" || handle == "" {
		return row{}, rt.NoHandle(api.NotFound("handle", handle))
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Read, project)
	if err != nil {
		return row{}, err
	}
	rw, ok, err := byHandle(ctx, db, handle)
	if err != nil {
		return row{}, err
	}
	if !ok || builderRow(rw) { // a dependency layer's builder is the daemon's alone (deps.go)
		return row{}, rt.NoHandle(api.NotFound("handle", handle))
	}
	return rw, nil
}

// boxOf is project's box by its handle and the binding serving it.
func (s *Service) boxOf(ctx context.Context, project, handle string) (computeapi.RunTarget, row, error) {
	rw, err := s.rowOf(ctx, project, handle)
	if err != nil {
		return computeapi.RunTarget{}, row{}, err
	}
	b, _, err := s.bindingNamed(ctx, project, rw.Target)
	if err != nil {
		return computeapi.RunTarget{}, row{}, err
	}
	return b, rw, nil
}

// BoxSecret writes one value into project's box handle under name (the
// secrets verb: a whole-file replace), as the authority.
func (s *Service) BoxSecret(ctx context.Context, project, handle, name string, v secrets.Value) error {
	if !protocol.SecretName(name) {
		return api.Errorf(api.CodeBadRequest, "a secret's name is [A-Z][A-Z0-9_]{0,63}")
	}
	b, rw, err := s.boxOf(ctx, project, handle)
	if err != nil {
		return err
	}
	if err := s.boxTakesSecrets(ctx, b, rw); err != nil {
		return err
	}
	if err := s.hostSwapSafe(ctx, b); err != nil { // swap that could put it on disk (swap.go)
		return err
	}
	if err := s.keepsBoxSecrets(ctx, b); err != nil {
		return err
	}
	a := protocol.BoxSecretsArgs{Secrets: map[string]string{name: v.Reveal()}}
	var out protocol.BoxSecretsResult
	if err := s.call(ctx, b, protocol.VerbSecrets, rw.Handle, a, &out); err != nil {
		err = scrubSecrets(err, sentSecrets(a))
		if notWritten(err) {
			return err // the driver refused: nothing was written
		}
		return fmt.Errorf("%w: %v", rt.ErrMaybeDelivered, err)
	}
	return nil
}

// BoxClass is the class of the machine serving project's box handle
// (api.MachineTrusted or api.MachinePublic), from the authority's record of
// the host: public unless it says trusted.
func (s *Service) BoxClass(ctx context.Context, project, handle string) (string, error) {
	b, _, err := s.boxOf(ctx, project, handle)
	if err != nil {
		return "", err
	}
	return s.boxClass(ctx, b), nil
}

// BoxLive reports whether project's box handle is recorded and not ended. A
// handle this project never had is not live.
func (s *Service) BoxLive(ctx context.Context, project, handle string) (bool, error) {
	rw, err := s.rowOf(ctx, project, handle)
	if rt.IsNoHandle(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !terminal(rw.State), nil
}

// BoxExpires is when project's box handle's lease ends, from its record
// (zero: the record names no end).
func (s *Service) BoxExpires(ctx context.Context, project, handle string) (time.Time, error) {
	rw, err := s.rowOf(ctx, project, handle)
	if err != nil {
		return time.Time{}, err
	}
	if rw.ExpiresAt == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, rw.ExpiresAt)
}
