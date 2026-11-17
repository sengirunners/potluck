package runners

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Host capacity reports (docs/daemon.md, "Host capacity reports"). A lending
// host that runs its own admission controller publishes what it has room for,
// and routers on cloud CI runners, which cannot reach the host, read it from
// the broker's public URL. The broker keeps each report as an opaque JSON
// object for its ttl, in memory: a host republishes every few seconds, so a
// restart loses one report at most.

const (
	capacityMaxBody    = 64 << 10
	capacityTTLMin     = 10
	capacityTTLMax     = 600
	capacityTTLDefault = 60
	capacityHostsMax   = 256
)

var capacityHost = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type capacityReport struct {
	body    []byte
	expires time.Time
}

type capacityStore struct {
	mu      sync.Mutex
	reports map[string]capacityReport
}

// put keeps body for host until expires, dropping expired reports; false
// when the store holds capacityHostsMax live hosts and host is not one.
func (c *capacityStore) put(host string, body []byte, now, expires time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reports == nil {
		c.reports = map[string]capacityReport{}
	}
	for h, r := range c.reports {
		if !now.Before(r.expires) {
			delete(c.reports, h)
		}
	}
	if _, ok := c.reports[host]; !ok && len(c.reports) >= capacityHostsMax {
		return false
	}
	c.reports[host] = capacityReport{body: body, expires: expires}
	return true
}

func (c *capacityStore) get(host string, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.reports[host]
	if !ok || !now.Before(r.expires) {
		return nil, false
	}
	return r.body, true
}

// mayPublishCapacity reports whether sub may publish host's report to the
// pool project: the host's own machine by its node-bound token (a member's
// replica, whose Machine is the name it redeemed with), seeing the pool, or
// the authority's owner on its own socket.
func mayPublishCapacity(sub *authz.Subject, host, pool string) bool {
	if sub == nil {
		return false
	}
	if sub.Scope.IsAll() && !sub.Remote {
		return true
	}
	return sub.Node != "" && sub.Machine == host && sub.Scope.SeesProject(pool)
}

// capacityPut: PUT /v0/runners/capacity/{host}?ttl_s=N with the report (a
// JSON object) as the body.
func (s *Service) capacityPut(r *http.Request) (any, error) {
	host := r.PathValue("host")
	// The pool and the caller's right come before the name's form, so a
	// caller who may not publish learns the same for any host, well formed
	// or not (only the owner on the socket gets as far as the form).
	p, err := s.readablePool(r)
	if err != nil {
		return nil, err
	}
	if !mayPublishCapacity(core.SubjectOf(r.Context()), host, p.ID) {
		return nil, api.Errorf(api.CodeForbidden, "only %q's own machine publishes its capacity report", host)
	}
	if !capacityHost.MatchString(host) {
		return nil, api.Errorf(api.CodeBadRequest, "host %q: a machine name (lowercase letters, digits and -)", host)
	}
	ttl := capacityTTLDefault
	if v := r.URL.Query().Get("ttl_s"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < capacityTTLMin || n > capacityTTLMax {
			return nil, api.Errorf(api.CodeBadRequest, "ttl_s %q: whole seconds, %d to %d", v, capacityTTLMin, capacityTTLMax)
		}
		ttl = n
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, capacityMaxBody+1))
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "reading the report: %v", err)
	}
	if len(body) > capacityMaxBody {
		return nil, api.Errorf(api.CodeBadRequest, "the report is over %d KiB", capacityMaxBody>>10)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, api.Errorf(api.CodeBadRequest, "the report must be a JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "the report must be a JSON object")
	}
	now := s.now()
	expires := now.Add(time.Duration(ttl) * time.Second)
	if !s.capacity.put(host, compact.Bytes(), now, expires) {
		return nil, api.Errorf(api.CodeUnavailable, "the broker holds reports for %d hosts already; try again once one expires", capacityHostsMax)
	}
	return map[string]any{"host": host, "expires_at": expires.Format(time.RFC3339)}, nil
}

// capacityGet: GET /v0/runners/capacity/{host}, public. The host's live
// report exactly as published (compacted), else 404.
func (s *Service) capacityGet(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	body, ok := []byte(nil), false
	if capacityHost.MatchString(host) {
		body, ok = s.capacity.get(host, s.now())
	}
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		api.WriteError(w, api.NotFound("capacity report", host))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
