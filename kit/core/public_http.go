package core

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

// PublicHTTP is the native module seam for capability-authorized HTTP ingress.
// It runs before the signed API transport; a module owns authorization and
// bounded streaming for its matched hostnames. Registration happens before Start.
type PublicHTTP struct {
	mu      sync.RWMutex
	dynamic []publicHostSource
}

const PublicHTTPService = "kit.public-http"

func PublicIngress(d *Deps) *PublicHTTP {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if v, ok := d.services[PublicHTTPService].(*PublicHTTP); ok {
		return v
	}
	p := &PublicHTTP{}
	if d.services == nil {
		d.services = map[string]any{}
	}
	d.services[PublicHTTPService] = p
	return p
}

type publicHostSource struct {
	matches func(string) bool
	handler http.Handler
}

// RegisterHostSource registers a feature owner's matcher for normalized Hosts.
// The owner checks its live configuration and exact capability-host shape.
func (p *PublicHTTP) RegisterHostSource(matches func(string) bool, h http.Handler) {
	if matches == nil || h == nil {
		panic("nil public hostname source")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dynamic = append(p.dynamic, publicHostSource{matches, h})
}
func (p *PublicHTTP) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.RLock()
		var handler http.Handler
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(host)
		for _, source := range p.dynamic {
			if source.matches(host) {
				handler = source.handler
				break
			}
		}
		p.mu.RUnlock()
		if handler != nil {
			handler.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ValidPublicSuffix accepts a normalized DNS suffix, never an IP or URL.
func ValidPublicSuffix(s string) bool {
	if s != strings.ToLower(s) || len(s) > 253 || net.ParseIP(s) != nil {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
