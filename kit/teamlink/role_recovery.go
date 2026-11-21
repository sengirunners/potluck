package teamlink

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Recovery tunables: the first retry and the most between two, and one
// probe's bound.
const (
	recoverMin   = 250 * time.Millisecond
	recoverMax   = 5 * time.Second
	probeTimeout = 5 * time.Second
)

// RoleRecovery is protected by Remote.Mu. A role header authorizes recovery, never reuse of
// the revoked copy: drop must finish before publishing OK or reopening it.
type RoleRecovery struct {
	Drop, Ready, Queued, Closing bool
	Opening, Dirty               bool // recovery is unfinished until save+open succeed
	Revision                     uint64
	Generation                   uint64
	RetryAt                      time.Time
	Backoff                      time.Duration
}

type RoleReplyFence struct {
	X          *Remote
	Snap       api.Remote
	generation uint64
}

func (x *Remote) RoleReply() RoleReplyFence {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	return RoleReplyFence{x, x.R, x.Recovery.Generation}
}

// No network or SQL while guarded. A revocation/leave transition waits for
// admitted metadata publication, then purges it; an old lifetime cannot
// publish after recovery even if its scope/cap happen to match again, nor
// after a repoint, nor while the home is behind.
func (f RoleReplyFence) Guard() (func(), bool) {
	f.X.PublicationMu.RLock()
	f.X.Mu.Lock()
	current := !f.X.Left && !f.X.Recovery.Closing && f.X.R.State == api.RemoteOK && f.X.R.ScopeVersion == f.Snap.ScopeVersion && f.X.R.Cap == f.Snap.Cap && f.X.Recovery.Generation == f.generation
	// The same home (a repoint makes another: an old home's answer is not
	// the new one's), and one that has not lost its term (successor.go).
	current = current && f.X.R.URL == f.Snap.URL && f.X.R.Fingerprint == f.Snap.Fingerprint && !f.X.home.Behind
	f.X.Mu.Unlock()
	if !current {
		f.X.PublicationMu.RUnlock()
		return nil, false
	}
	return f.X.PublicationMu.RUnlock, true
}

// Admission for an external effect: never retain the publication lock across
// SQL, a PTY delivery or a session callback. An admitted irreversible effect
// can finish, but later effects must recheck the lifetime independently.
func (f RoleReplyFence) Current() bool {
	unguard, ok := f.Guard()
	if ok {
		unguard()
	}
	return ok
}

// recordRoleHeader runs only for an accepted, authenticated non-401 header,
// with x.mu held. Header ordering/project matching remains observe's job.
func (x *Remote) recordRoleHeader() bool {
	if x.Left || x.Recovery.Closing {
		return false
	}
	if x.R.Role == "" && x.R.State == api.RemoteOK {
		x.R.State = api.RemoteRevoked
		x.R.LastError = "the authority " + x.R.Authority + " revoked " + x.R.Identity + "'s access to " + x.R.Project.Slug
		x.Recovery.Drop = true
		x.Recovery.Generation++
		x.Recovery.Ready = false
		x.Recovery.Opening = false
		x.Recovery.Revision++
		return true
	}
	if x.R.State == api.RemoteRevoked {
		x.Recovery.Ready = x.R.Role != "" && x.Token != ""
		x.Recovery.Revision++
	}
	return false
}

// A queued probe from before leave cannot reinsert the removed registry row.
// persistMu orders this check/save with remove's terminal mark.
func (s *Service) SaveRoleRemote(ctx context.Context, x *Remote) error {
	x.PersistMu.Lock()
	defer x.PersistMu.Unlock()
	x.Mu.Lock()
	left := x.Left || x.Recovery.Closing
	x.Mu.Unlock()
	if left || !s.OwnsRoleRemote(x) {
		return nil
	}
	return s.Save(ctx, x)
}

// A foreground observer may finish after a replacement join. Retiring that
// object and deleting its token share persistMu, held through the file I/O:
// either this deletes the old token first, or retirement makes it a no-op.
func (s *Service) PurgeRoleToken(x *Remote) {
	x.PersistMu.Lock()
	defer x.PersistMu.Unlock()
	x.Mu.Lock()
	left := x.Left
	x.Mu.Unlock()
	if left || !s.OwnsRoleRemote(x) {
		return
	}
	_ = removeFile(s.TokenPath(x.Snapshot().Project.ID))
	x.Mu.Lock()
	x.Token, x.HeaderRaw = "", ""
	x.Mu.Unlock()
}

func (s *Service) OwnsRoleRemote(x *Remote) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.remotes[x.Snapshot().Project.ID] == x
}

// Terminal before either registry deletion or replacement of a remote object
// sharing the same project copy path. No lifecycle wait under these locks.
func (x *Remote) RetireRole() {
	x.PersistMu.Lock()
	x.RetireRoleLocked()
	x.PersistMu.Unlock()
}

func (x *Remote) RetireRoleLocked() {
	x.PublicationMu.Lock()
	x.Mu.Lock()
	x.Left, x.Recovery.Ready, x.Recovery.Opening = true, false, false
	x.R.State, x.R.LastError = api.RemotePurged, "this replica was retired"
	x.Recovery.Generation++
	x.Mu.Unlock()
	x.PublicationMu.Unlock()
}

func (s *Service) RetireRoleRemote(x *Remote) error {
	x.RetireRole()
	x.ReplicaMu.Lock()
	defer x.ReplicaMu.Unlock()
	return s.dropRoleCopyLocked(x)
}

// dropRoleCopyLocked drops x's copy (Hooks.DropCopy); the caller holds
// x.ReplicaMu.
func (s *Service) dropRoleCopyLocked(x *Remote) error {
	if s.Hooks.DropCopy == nil {
		return nil
	}
	return s.Hooks.DropCopy(x)
}

func (s *Service) QueueRoleReconcile(x *Remote) {
	x.Mu.Lock()
	if x.Left || x.Recovery.Closing || x.Recovery.Queued || (x.R.State == api.RemoteOK && !x.Recovery.Drop && !x.Recovery.Opening) {
		x.Mu.Unlock()
		return
	}
	if x.R.State == api.RemoteOK && x.Recovery.Opening {
		if time.Now().Before(x.Recovery.RetryAt) {
			x.Mu.Unlock()
			return
		}
		if x.Recovery.Backoff == 0 {
			x.Recovery.Backoff = recoverMin
		}
		x.Recovery.RetryAt = time.Now().Add(x.Recovery.Backoff)
		x.Recovery.Backoff = min(2*x.Recovery.Backoff, recoverMax)
	}
	x.Recovery.Queued = true
	x.RoleWG.Add(1) // closing excludes future Add before shutdown waits
	x.Mu.Unlock()
	go func() {
		defer x.RoleWG.Done()
		x.ReplicaMu.Lock()
		defer x.ReplicaMu.Unlock()
		for {
			x.Mu.Lock()
			revision := x.Recovery.Revision
			x.Mu.Unlock()
			ctx := s.rctx
			if ctx == nil {
				ctx = context.Background()
			}
			err := s.ReconcileRoleLocked(ctx, x)
			x.Mu.Lock()
			again := !x.Left && !x.Recovery.Closing && err == nil && revision != x.Recovery.Revision
			if !again {
				x.Recovery.Queued = false
			}
			x.Mu.Unlock()
			if err != nil {
				s.d.Log.Warn("team: role recovery cleanup", "project", x.Snapshot().Project.ID, "err", err)
			}
			if !again {
				return
			}
		}
	}()
}

// Caller holds replicaMu. observe must queue this: it can be called by one
// of the very loops whose termination stopReplicaLocked awaits.
type RoleRecoveryOps struct {
	Save func(context.Context, *Remote) error
	Open func(context.Context, *Remote)
}

func (s *Service) ReconcileRoleLocked(ctx context.Context, x *Remote, injected ...RoleRecoveryOps) error {
	ops := RoleRecoveryOps{Save: s.SaveRoleRemote, Open: s.Hooks.OpenCopy}
	if ops.Open == nil {
		ops.Open = func(context.Context, *Remote) {}
	}
	if len(injected) > 0 {
		if injected[0].Save != nil {
			ops.Save = injected[0].Save
		}
		if injected[0].Open != nil {
			ops.Open = injected[0].Open
		}
	}
	if !s.OwnsRoleRemote(x) {
		return nil
	}
	x.Mu.Lock()
	if x.Left || x.Recovery.Closing {
		x.Mu.Unlock()
		return nil
	}
	if x.R.State == api.RemoteRevoked && x.Token == "" {
		// load preserves a missing token's revoked state; it is terminal,
		// just like the existing OK/missing-token case, never probeable.
		x.R.State, x.R.LastError = api.RemotePurged, "token file unavailable"
		x.Recovery.Ready = false
		x.HeaderRaw, x.HeaderB64 = "", ""
		x.Recovery.Dirty = true
	}
	drop := x.Recovery.Drop || x.R.State != api.RemoteOK
	x.Mu.Unlock()
	if drop {
		if err := s.dropRoleCopyLocked(x); err != nil {
			return err
		}
		x.Mu.Lock()
		x.Recovery.Drop = false
		x.Mu.Unlock()
	}
	x.PublicationMu.Lock()
	x.Mu.Lock()
	if !x.Left && !x.Recovery.Closing && ctx.Err() == nil && s.rctx != nil && s.rctx.Err() == nil && x.R.State == api.RemoteRevoked && x.Recovery.Ready && x.R.Role != "" && x.Token != "" {
		x.R.State, x.R.LastError = api.RemoteOK, ""
		x.Recovery.Ready = false
		x.Recovery.Opening, x.Recovery.Dirty = true, true
		x.Recovery.RetryAt, x.Recovery.Backoff = time.Time{}, 0
	}
	ok := !x.Left && !x.Recovery.Closing && x.R.State == api.RemoteOK
	dirty := x.Recovery.Dirty
	x.Mu.Unlock()
	x.PublicationMu.Unlock()
	if dirty {
		if err := ops.Save(context.WithoutCancel(ctx), x); err != nil {
			return err
		}
		x.Mu.Lock()
		x.Recovery.Dirty = false
		x.Mu.Unlock()
	}
	if ok {
		ops.Open(ctx, x)
		x.Mu.Lock()
		if x.Copy != nil || s.Hooks.OpenCopy == nil {
			x.Recovery.Opening = false
			x.Recovery.RetryAt, x.Recovery.Backoff = time.Time{}, 0
		}
		x.Mu.Unlock()
	}
	return nil
}

// A revoked identity still has its node-bound token. Only a safe read is
// allowed until the authority genuinely grants it a role again. The regular
// heartbeat scheduler provides wakeups; its kicks obey this bounded backoff.
func (s *Service) ProbeRole(ctx context.Context, x *Remote) {
	if ctx.Err() != nil || !s.OwnsRoleRemote(x) {
		return
	}
	x.Mu.Lock()
	if x.Left || x.Recovery.Closing || x.R.State != api.RemoteRevoked || time.Now().Before(x.Recovery.RetryAt) {
		x.Mu.Unlock()
		return
	}
	if x.Token == "" {
		x.Mu.Unlock()
		s.QueueRoleReconcile(x)
		return
	}
	if x.Recovery.Backoff == 0 {
		x.Recovery.Backoff = recoverMin
	}
	x.Recovery.RetryAt = time.Now().Add(x.Recovery.Backoff)
	x.Recovery.Backoff = min(2*x.Recovery.Backoff, recoverMax)
	id := x.R.Project.ID
	x.Mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// A hidden/missing route may still carry the genuine authenticated team
	// header; observe, not the response body's success, decides recovery.
	_ = s.CallRemote(cctx, x, http.MethodGet, "/v0/projects/"+url.PathEscape(id), nil, nil)
	s.QueueRoleReconcile(x)
}
