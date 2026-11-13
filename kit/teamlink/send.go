package teamlink

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/sengirunners/potluck/kit/api"
)

// Ended is the error a remote that stopped answers with (purged after
// 401s, or revoked: its identity has no grant left in the project), nil
// while it is ok.
func Ended(r api.Remote) error {
	if r.State == api.RemoteOK {
		return nil
	}
	err := api.Errorf(api.CodeUnauthenticated, "this machine's access to %s ended (%s)", r.Project.Slug, r.LastError)
	if r.State == api.RemoteRevoked {
		return err.WithHint("access is rechecked automatically; zm leave " + r.Project.Slug + " forgets it")
	}
	return err.WithHint("ask its owner for a new invite (then zm join <code>); zm leave " + r.Project.Slug + " forgets it")
}

// Send is every call to an authority: the token, the scope_version, the
// header it answers with, 401 counting and the purge, one retry after 409
// rescope (nothing ran: the authority refuses before the route).
func (s *Service) Send(ctx context.Context, x *Remote, req *http.Request, body []byte) (*http.Response, error) {
	return s.SendWith(ctx, x, x.HC, req, body)
}

// SendWith is Send through hc (a hedge's client: internal/team hedge.go).
func (s *Service) SendWith(ctx context.Context, x *Remote, hc *http.Client, req *http.Request, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		x.Mu.Lock()
		leave := req.Method == http.MethodPost && req.URL.Path == "/v0/team/leave"
		probe := !x.Left && !x.Recovery.Closing && x.R.State == api.RemoteRevoked && req.Method == http.MethodGet && req.URL.Path == "/v0/projects/"+x.R.Project.ID
		if !leave && (x.Left || x.Recovery.Closing || (x.R.State != api.RemoteOK && !probe)) {
			err := Ended(x.R)
			if err == nil { // left is terminal even before its map entry is removed
				err = api.Errorf(api.CodeUnauthenticated, "this replica was left")
			}
			x.Mu.Unlock()
			return nil, err
		}
		token, sv := x.Token, x.R.ScopeVersion
		x.Mu.Unlock()
		api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
		req.Header.Set(api.HeaderPersonalMail, api.PersonalMailProtocol)
		req.Header.Set(api.HeaderAuthorization, "Bearer "+token)
		if sv > 0 {
			api.SetHeader(req.Header, api.HeaderScopeVersion, strconv.FormatInt(sv, 10))
		}
		seq := x.Sent.Add(1)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, s.Unreachable(x.Snapshot().Via, req.URL.Host, err)
		}
		s.Observe(ctx, x, resp, seq, sv)
		if resp.StatusCode == http.StatusConflict && attempt == 0 && isRescope(resp) {
			resp.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(body))
			continue
		}
		return resp, nil
	}
}

// isRescope reads a 409's code, putting the body back.
func isRescope(resp *http.Response) bool {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	var er api.ErrorResponse
	return json.Unmarshal(b, &er) == nil && er.Error != nil && er.Error.Code == api.CodeRescope
}
