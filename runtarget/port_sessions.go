package runtarget

import (
	"context"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
	"net/http"
	"sync"
	"time"
)

type portSessions struct {
	mu      sync.Mutex
	streams map[string]*portSession
	opening int
}
type portSession struct {
	project, handle string
	lease           string
	ctx             context.Context
	cancel          context.CancelFunc
	expires         time.Time
}

const portSessionService = "compute.port-sessions"

func (s *Service) initPorts() {
	p := &portSessions{streams: map[string]*portSession{}}
	s.d.Provide(portSessionService, p)
	s.OnEnd(func(_ context.Context, project string, h computeapi.RunTargetHandle) {
		p.mu.Lock()
		defer p.mu.Unlock()
		for id, e := range p.streams {
			if e.project == project && e.handle == h.Handle {
				e.cancel()
				delete(p.streams, id)
			}
		}
	})
}

var errPortLeaseChanged = api.Errorf(api.CodeNotFound, "sandbox lease changed")

// portLease pins the existing start-or-create identity, so restarting a kept
// disk cannot revive channels or URLs issued by its previous lease.
func (s *Service) portLease(r *http.Request, want string) (string, computeapi.RunTarget, row, error) {
	project, b, rw, err := s.boxHolderIn(r)
	if err == nil && want != "" && want != capFrom(rw) {
		err = errPortLeaseChanged
	}
	return project, b, rw, err
}
func (s *Service) portRequest(r *http.Request, req protocol.PortRequest) (protocol.PortResult, error) {
	return s.portRequestFor(r, req, "")
}
func (s *Service) portRequestFor(r *http.Request, req protocol.PortRequest, lease string) (protocol.PortResult, error) {
	if err := validatePort(req); err != nil {
		return protocol.PortResult{}, err
	}
	project, b, rw, err := s.portLease(r, lease)
	if err != nil {
		return protocol.PortResult{}, err
	}
	p := s.d.Service(portSessionService).(*portSessions)
	var session *portSession
	p.mu.Lock()
	if req.Op == "open" {
		count := 0
		for id, e := range p.streams {
			if e.ctx.Err() != nil || e.project == project && e.handle == rw.Handle && e.lease != capFrom(rw) {
				e.cancel()
				delete(p.streams, id)
				continue
			}
			if e.project == project && e.handle == rw.Handle {
				count++
			}
		}
		if len(p.streams)+p.opening >= 128 || count+p.opening >= 32 {
			p.mu.Unlock()
			return protocol.PortResult{}, api.Errorf(api.CodeUnavailable, "sandbox port channel limit reached")
		}
		p.opening++
	} else {
		e := p.streams[req.Stream]
		session = e
		if e == nil || e.ctx.Err() != nil || e.project != project || e.handle != rw.Handle {
			p.mu.Unlock()
			return protocol.PortResult{}, api.Errorf(api.CodeNotFound, "port channel not found")
		}
		if e.lease != capFrom(rw) {
			e.cancel()
			delete(p.streams, req.Stream)
			p.mu.Unlock()
			return protocol.PortResult{}, errPortLeaseChanged
		}
	}
	p.mu.Unlock()
	var out protocol.PortResult
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if session != nil {
		stop := context.AfterFunc(session.ctx, cancel)
		defer stop()
	}
	err = s.call(ctx, b, protocol.VerbPort, rw.Handle, req, &out)
	if len(out.Data) > protocol.PortPage {
		err = api.Errorf(api.CodeTooLarge, "port answer exceeds 64 KiB")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.Op == "open" {
		p.opening--
		if err == nil {
			if !portID.MatchString(out.Stream) {
				return protocol.PortResult{}, api.Errorf(api.CodeInternal, "driver returned invalid port channel")
			}
			expires, parseErr := time.Parse(time.RFC3339Nano, rw.ExpiresAt)
			if parseErr != nil {
				return protocol.PortResult{}, api.Errorf(api.CodeConflict, "sandbox has no lease deadline")
			}
			ctx, cancel := context.WithDeadline(context.Background(), expires)
			e := &portSession{lease: capFrom(rw), project: project, handle: rw.Handle, ctx: ctx, cancel: cancel, expires: expires}
			p.streams[out.Stream] = e
			go func(id string) {
				<-ctx.Done()
				p.mu.Lock()
				if p.streams[id] == e {
					delete(p.streams, id)
				}
				p.mu.Unlock()
			}(out.Stream)
		}
	} else if req.Op == "close" {
		if e := p.streams[req.Stream]; e != nil {
			e.cancel()
			delete(p.streams, req.Stream)
		}
	}
	return out, err
}

func (s *Service) Close() error {
	if v := s.d.Service(portSessionService); v != nil {
		p := v.(*portSessions)
		p.mu.Lock()
		for id, e := range p.streams {
			e.cancel()
			delete(p.streams, id)
		}
		p.mu.Unlock()
	}
	if v := s.d.Service("compute.previews"); v != nil {
		p := v.(*previewState)
		p.mu.Lock()
		for id, e := range p.entries {
			e.cancel()
			delete(p.entries, id)
		}
		p.mu.Unlock()
	}
	return nil
}
