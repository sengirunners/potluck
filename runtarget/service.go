package runtarget

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// rt.Service: the routes' own handlers, called as sub with the
// project named explicitly, so every check (operate, budget, the class
// rules, the area) is the route's. Never as the machine owner: no WithLocal.

func (s *Service) Acquire(ctx context.Context, sub *authz.Subject, projectRef, target string, req computeapi.AcquireRunTargetRequest) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, s.as(ctx, sub, projectRef, map[string]string{"target": target}, req, s.acquire, &h)
}

func (s *Service) Renew(ctx context.Context, sub *authz.Subject, projectRef, target, id string, ttlS int) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, s.as(ctx, sub, projectRef, map[string]string{"target": target, "id": id}, computeapi.RenewRunTargetRequest{TTLS: ttlS}, s.renew, &h)
}

func (s *Service) Release(ctx context.Context, sub *authz.Subject, projectRef, target, id string) (computeapi.RunTargetRelease, error) {
	var out computeapi.RunTargetRelease
	return out, s.as(ctx, sub, projectRef, map[string]string{"target": target, "id": id}, struct{}{}, s.release, &out)
}

func (s *Service) List(ctx context.Context, sub *authz.Subject, projectRef string, describe bool) (computeapi.RunTargetList, error) {
	var out computeapi.RunTargetList
	q := url.Values{}
	if describe {
		q.Set("describe", "1")
	}
	return out, s.call0(ctx, sub, projectRef, http.MethodGet, q, nil, nil, s.list, &out)
}

func (s *Service) Status(ctx context.Context, sub *authz.Subject, projectRef, target, id string) (computeapi.RunTargetHandle, error) {
	var h computeapi.RunTargetHandle
	return h, s.call0(ctx, sub, projectRef, http.MethodGet, nil, map[string]string{"target": target, "id": id}, nil, s.status, &h)
}

func (s *Service) as(ctx context.Context, sub *authz.Subject, projectRef string, vals map[string]string, body any,
	f func(*http.Request) (any, error), out any) error {
	return s.call0(ctx, sub, projectRef, http.MethodPost, nil, vals, body, f, out)
}

// call0 runs handler f as sub on a request made here (a body for a POST,
// query values for a GET).
func (s *Service) call0(ctx context.Context, sub *authz.Subject, projectRef, method string, q url.Values, vals map[string]string, body any,
	f func(*http.Request) (any, error), out any) error {
	var rd *bytes.Reader
	if method == http.MethodPost {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	ctx = core.WithResolveInput(core.WithSubject(ctx, sub), project.ResolveInput{Explicit: projectRef, ExplicitFrom: project.RuleFlag})
	u := "/v0/run_targets"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	r, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range vals {
		r.SetPathValue(k, v)
	}
	v, err := f(r)
	if err != nil {
		return err
	}
	if raw, err := json.Marshal(v); err == nil {
		return json.Unmarshal(raw, out)
	}
	return api.Errorf(api.CodeInternal, "run target answer")
}
