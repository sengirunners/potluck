package runtarget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/portstream"
	"github.com/sengirunners/potluck/protocol"
)

type previewState struct {
	mu      sync.Mutex
	entries map[string]*publishedPort
}
type publishedPort struct {
	computeapi.Preview
	request         *http.Request
	handle, project string
	lease           string
	ctx             context.Context
	cancel          context.CancelFunc
	expires         time.Time
}

func (s *Service) previewState() *previewState {
	name := "compute.previews"
	if v := s.d.Service(name); v != nil {
		return v.(*previewState)
	}
	// Called once at route registration, before serving requests.
	p := &previewState{entries: map[string]*publishedPort{}}
	s.d.Provide(name, p)
	core.PublicIngress(s.d).RegisterHostSource(s.previewHost, http.HandlerFunc(s.servePreview))
	s.OnEnd(func(_ context.Context, project string, h computeapi.RunTargetHandle) {
		p.mu.Lock()
		defer p.mu.Unlock()
		for id, e := range p.entries {
			if e.project == project && e.handle == h.Handle {
				e.cancel()
				delete(p.entries, id)
			}
		}
	})
	return p
}
func (s *Service) previews(r *http.Request) (any, error) {
	project, _, rw, err := s.boxHolderIn(r)
	if err != nil {
		return nil, err
	}
	p := s.previewState()
	p.mu.Lock()
	defer p.mu.Unlock()
	list := computeapi.PreviewList{Previews: []computeapi.Preview{}}
	for id, e := range p.entries {
		if time.Now().After(e.expires) {
			e.cancel()
			delete(p.entries, id)
			continue
		}
		if e.project == project && e.handle == rw.Handle {
			if e.lease != capFrom(rw) {
				e.cancel()
				delete(p.entries, id)
				continue
			}
			list.Previews = append(list.Previews, e.Preview)
		}
	}
	return list, nil
}
func (s *Service) publishPreview(r *http.Request) (any, error) {
	project, _, rw, err := s.boxHolderIn(r)
	if err != nil {
		return nil, err
	}
	var req computeapi.PreviewRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	p := s.previewState()
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.Revoke != "" {
		e := p.entries[req.Revoke]
		if e == nil || e.project != project || e.handle != rw.Handle {
			return nil, api.Errorf(api.CodeNotFound, "preview not found")
		}
		e.cancel()
		delete(p.entries, req.Revoke)
		return computeapi.PreviewList{Previews: []computeapi.Preview{}}, nil
	}
	if !req.Public || req.Port < 1 || req.Port > 65535 {
		return nil, api.Errorf(api.CodeBadRequest, "publishing requires public:true and a port in 1..65535")
	}
	base, err := url.Parse("https://" + s.previewDomain())
	if err != nil || base.Scheme != "https" || base.Host == "" || s.previewDomain() == "" || base.User != nil || !core.ValidPublicSuffix(s.previewDomain()) || len(s.previewDomain()) > 204 {
		return nil, api.Errorf(api.CodeUnavailable, "configure an existing wildcard HTTPS preview domain before publishing")
	}
	expires, err := time.Parse(time.RFC3339Nano, rw.ExpiresAt)
	if err != nil {
		return nil, api.Errorf(api.CodeConflict, "sandbox has no lease deadline")
	}
	for id, e := range p.entries {
		if time.Now().After(e.expires) {
			e.cancel()
			delete(p.entries, id)
		}
	}
	if len(p.entries) >= 128 {
		return nil, api.Errorf(api.CodeUnavailable, "too many active previews")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(raw)
	// Hold the authenticated request context's subject, never credentials in
	// the published route. Re-running boxHolder rechecks the producer's row.
	ctx, cancel := context.WithDeadline(context.WithoutCancel(r.Context()), expires)
	request := r.Clone(ctx)
	request.Body = http.NoBody
	request.Header = http.Header{}
	base.Host = id + "." + base.Host
	base.Path = "/"
	base.RawQuery = ""
	base.Fragment = ""
	entry := &publishedPort{lease: capFrom(rw), Preview: computeapi.Preview{ID: id, Port: req.Port, URL: base.String(), ExpiresAt: rw.ExpiresAt}, request: request, handle: rw.Handle, project: project, ctx: ctx, cancel: cancel, expires: expires}
	p.entries[id] = entry
	go func() {
		<-ctx.Done()
		p.mu.Lock()
		if p.entries[id] == entry {
			delete(p.entries, id)
		}
		p.mu.Unlock()
	}()
	return entry.Preview, nil
}
func (s *Service) servePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	domain := s.previewDomain()
	id, ok := strings.CutSuffix(strings.ToLower(host), "."+domain)
	if !ok || !portID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	p := s.previewState()
	p.mu.Lock()
	e := p.entries[id]
	p.mu.Unlock()
	if e == nil || e.ctx.Err() != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, _, err := s.portLease(e.request, e.lease); err != nil {
		e.cancel()
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	// Revalidate live holder, binding, deadline and ready state on each dial
	// and every channel page, including WebSocket pages.
	call := func(ctx context.Context, req protocol.PortRequest) (protocol.PortResult, error) {
		parent := e.request.Context()
		if req.Op == "close" {
			parent = context.WithoutCancel(parent)
		}
		opCtx, stopOp := context.WithCancel(parent)
		defer stopOp()
		stopCaller := context.AfterFunc(ctx, stopOp)
		defer stopCaller()
		if sub := core.SubjectOf(opCtx); sub != nil && sub.TokenID != "" {
			fresh, err := s.d.SubscriptionSubject(opCtx, sub.TokenID)
			if err != nil {
				return protocol.PortResult{}, err
			}
			copy := *sub
			copy.Scope = fresh.Scope
			opCtx = core.WithSubject(opCtx, &copy)
		}
		check := e.request.Clone(opCtx)
		return s.portRequestFor(check, req, e.lease)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return portstream.Dial(ctx, e.Port, call) }, DisableKeepAlives: true, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	defer transport.CloseIdleConnections()
	proxy := httputil.ReverseProxy{Transport: transport, Rewrite: func(p *httputil.ProxyRequest) {
		p.Out.URL.Scheme = "http"
		p.Out.URL.Host = "sandbox"
		p.Out.Host = p.In.Host
		p.Out.URL.Path = p.In.URL.Path
		p.Out.URL.RawPath = p.In.URL.RawPath

		p.Out.Header.Del("Referer")
		p.Out.Header.Del("Forwarded")
		p.Out.Header.Del("X-Forwarded-For")
		p.Out.Header.Del("X-Forwarded-Host")
		p.Out.Header.Set("X-Forwarded-Proto", "https")
		p.Out.Header.Set("X-Forwarded-Host", p.In.Host)
	}, ModifyResponse: func(resp *http.Response) error {
		resp.Header.Set("Cache-Control", "no-store")
		resp.Header.Set("Referrer-Policy", "no-referrer")
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
		http.Error(w, "sandbox preview unavailable", http.StatusBadGateway)
	}}
	r = r.WithContext(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	proxy.ServeHTTP(w, r)
}

func (s *Service) previewDomain() string {
	if t := tunnel.From(s.d); t != nil {
		return t.PreviewDomain()
	}
	return ""
}

func (s *Service) previewHost(host string) bool {
	domain := s.previewDomain()
	if !core.ValidPublicSuffix(domain) {
		return false
	}
	id, ok := strings.CutSuffix(host, "."+domain)
	return ok && portID.MatchString(id)
}
