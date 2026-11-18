package teamlink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// handleJoin is POST /v0/team/join (zm join): the code's transport readies
// this machine, the code is redeemed at the pinned home, then the token
// file, the access check, the member's checkouts, and what the member sees.
// Which transport a code belongs to, and everything about reaching its
// home, is the transport driver's (core.TeamTransport, SPEC 17.11).
func (s *Service) handleJoin(r *http.Request) (any, error) {
	ctx := r.Context()
	var req api.JoinRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.Code == "" && req.Project != "" && project.LooksLikeJoinCode(req.Project) {
		req.Code, req.Project = req.Project, "" // a malformed code is refused as a code, never a GitHub join
	}
	if req.Code == "" && (req.Project != "" || req.Login != "") {
		return s.joinGitHub(ctx, req) // join_github.go: no code (SPEC 22)
	}
	// An invite link carries its code (project.JoinLink): every transport sees the code itself.
	code, err := project.JoinCodeOf(req.Code)
	if err != nil {
		return nil, err
	}
	req.Code = code
	t, target, err := s.parseCode(req.Code)
	if err != nil {
		return nil, err
	}
	machine, early, err := s.joinPrep(ctx, req, t, target)
	if err != nil || early != nil {
		return early, err
	}
	extra, err := t.Extra(req)
	if err != nil {
		return nil, err
	}
	// 3. Redeem at the pinned home.
	rr, err := t.RedeemRequest(target, machine)
	if err != nil {
		return nil, err
	}
	hc := s.client(target, extra)
	var red api.RedeemResponse
	if err := s.call(ctx, target.Via, hc, http.MethodPost, target.URL+"/v0/invites/redeem", "", rr, &red); err != nil {
		return nil, err
	}
	return s.finishJoin(ctx, req, red, hc, target.URL, target.Fingerprint, "", target.Via, extra)
}

// parseCode finds the transport whose join code code is.
func (s *Service) parseCode(code string) (core.TeamTransport, core.JoinTarget, error) {
	for _, t := range s.d.Transports() {
		target, ok, err := t.ParseCode(code)
		if !ok {
			continue
		}
		if err != nil {
			return nil, core.JoinTarget{}, err
		}
		target.Via = t.Via()
		return t, target, nil
	}
	if _, err := project.ParseJoinCode(code); err != nil {
		return nil, core.JoinTarget{}, err // the formats it knows, in its words
	}
	return nil, core.JoinTarget{}, api.Errorf(api.CodeBadRequest, "no transport on this daemon reads this join code")
}

// joinMachine is the machine name a join records: the daemon's own.
func (s *Service) joinMachine(req api.JoinRequest) (string, error) {
	cfg := s.d.Config
	machine := strings.TrimSpace(req.Machine)
	if machine == "" {
		machine = cfg.Machine
	}
	if machine != cfg.Machine {
		// Session addresses on this daemon are <machine>/<session8>; the
		// authority accepts only those of the name it recorded.
		return "", api.Errorf(api.CodeBadRequest, "this daemon is machine %s, not %s", cfg.Machine, machine).
			WithHint(fmt.Sprintf("set \"machine\": %q in %s (or ZM_MACHINE=%s), restart the daemon, then zm join again", machine, filepath.Join(cfg.Home, "config.json"), machine))
	}
	return machine, nil
}

// joinPrep is a join's steps 1 and 2: the machine name, then the
// transport readies this machine (the tailnet: its node up, under its own
// name). A browser login still needed is an early answer.
func (s *Service) joinPrep(ctx context.Context, req api.JoinRequest, t core.TeamTransport, target core.JoinTarget) (string, *api.JoinResponse, error) {
	machine, err := s.joinMachine(req)
	if err != nil {
		return "", nil, err
	}
	if t == nil {
		return "", nil, api.Errorf(api.CodeUnavailable, "this daemon has no %s transport", target.Via)
	}
	login, note, err := t.Prepare(ctx, req, machine, target)
	if err != nil {
		return "", nil, err
	}
	if login != "" {
		return "", &api.JoinResponse{AuthURL: login, Notes: []string{note}}, nil
	}
	return machine, nil, nil
}

// client reaches target's home through its transport (pinned; signed and
// verified where the transport does that).
func (s *Service) client(target core.JoinTarget, extra http.Header) *http.Client {
	t := s.d.Transport(target.Via)
	if t == nil {
		return &http.Client{Transport: errTransport{fmt.Errorf("this daemon has no %q transport", target.Via)}}
	}
	return t.Client(target, core.ClientOptions{DialTimeout: DialTimeout, Extra: extra, Peer: s.ownerSeen})
}

// finishJoin is a join's steps 4 to 7, once the authority admitted this
// node (red): the token file, the first call, the checkouts, the copy.
// owner is the authority node's tailnet owner for a GitHub join ("" for a
// code): no code is left to retry with, so every refusal from here gives
// the new token back (POST /v0/team/leave).
//
// via is the transport (core.TeamTransport.Via); extra are headers every
// call to the home carries (core.TeamTransport.Extra), stored next to the
// token.
func (s *Service) finishJoin(ctx context.Context, req api.JoinRequest, red api.RedeemResponse, hc *http.Client, base, fingerprint, owner, via string, extra http.Header) (any, error) {
	viaGitHub := owner != "" || (req.Code == "" && req.Project != "")
	old := s.ByName(red.Cap.Project)
	if old != nil && old.Snapshot().State == api.RemoteOK {
		// The new token is not needed: it revokes itself.
		_ = s.call(ctx, via, hc, http.MethodPost, base+"/v0/team/leave", red.Token, struct{}{}, nil)
		return nil, api.Errorf(api.CodeConflict, "this machine already joined %s", red.Cap.Slug).WithHint("zm leave " + red.Cap.Slug + " first")
	}
	// Refusals that leave the code usable come before the token file: a
	// file written for a join that is refused would be an orphan zm leave
	// cannot see.
	if p, ok := s.d.Projects.Lookup(red.Cap.Slug); ok {
		if viaGitHub {
			_ = s.call(ctx, via, hc, http.MethodPost, base+"/v0/team/leave", red.Token, struct{}{}, nil)
			return nil, api.Errorf(api.CodeConflict, "this machine already has a project named %s (%s)", red.Cap.Slug, p.ID).
				WithHint("rename it (zm project set " + red.Cap.Slug + " --slug <other>), then zm join " + red.Cap.Slug + " again")
		}
		return nil, api.Errorf(api.CodeConflict, "this machine already has a project named %s (%s)", red.Cap.Slug, p.ID).
			WithHint("rename it (zm project set " + red.Cap.Slug + " --slug <other>), then run the same zm join again: the code still works until its first use")
	}
	// The id is the authority's to choose: one this machine knows belongs
	// to the authority it first came from (project_keys.go). A refused
	// join's token goes back; the code is spent at that authority anyway.
	if err := s.checkProjectKey(ctx, red.Cap, via, fingerprint, red.Authority); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_ = s.call(cctx, via, hc, http.MethodPost, base+"/v0/team/leave", red.Token, struct{}{}, nil)
		cancel()
		return nil, err
	}
	// 4. The token file, atomically, before anything reports success. Any
	// failure from here until the remote is saved removes it (and, once
	// the token has been used, revokes it: the code is spent either way).
	tokenFile := s.TokenPath(red.Cap.Project)
	if old != nil {
		// Both objects share a copy path. The old object's queued teardown
		// and observer saves must be retired before the new token/copy exists.
		if err := s.RetireRoleRemote(old); err != nil {
			// Redemption already minted a token, but no local file owns it
			// yet. Give it back if the new copy cannot safely replace the old.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_ = s.call(cctx, via, hc, http.MethodPost, base+"/v0/team/leave", red.Token, struct{}{}, nil)
			cancel()
			return nil, api.Errorf(api.CodeUnavailable, "retire previous replica: %v", err)
		}
	}
	if old != nil && old.HasToken() {
		// A revoked remote of the same project: its token goes before the
		// new one takes its file (a purged one has none left).
		ctx3, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = s.CallRemote(ctx3, old, http.MethodPost, "/v0/team/leave", struct{}{}, nil)
		cancel()
	}
	if err := WriteFileAtomic(tokenFile, []byte(red.Token+"\n")); err != nil {
		return nil, err
	}
	headersFile := s.headersPath(red.Cap.Project)
	if extra != nil {
		if err := WriteFileAtomic(headersFile, encodeHeaders(extra)); err != nil {
			_ = removeFile(tokenFile)
			return nil, err
		}
	} else {
		_ = removeFile(headersFile)
	}
	saved, used := false, false
	defer func() {
		if saved {
			return
		}
		if used || viaGitHub {
			_ = s.call(context.WithoutCancel(ctx), via, hc, http.MethodPost, base+"/v0/team/leave", red.Token, struct{}{}, nil)
		}
		_ = removeFile(tokenFile)
		_ = removeFile(headersFile)
	}()
	x := &Remote{Token: red.Token, HC: hc, R: api.Remote{Project: api.ProjectRef{ID: red.Cap.Project, Slug: red.Cap.Slug},
		Authority: red.Authority, URL: base, Fingerprint: fingerprint, Via: via, Cap: red.Cap, ScopeVersion: red.ScopeVersion,
		Machine: red.Machine, Identity: red.Identity, Joined: time.Now().UTC(), State: api.RemoteOK, TokenFile: tokenFile,
		AuthorityOwner: owner}}
	// 5. Check access: the first authenticated call (redemption stops being
	// repeatable) and the project header.
	var p api.Project
	if err := s.CallRemote(ctx, x, http.MethodGet, "/v0/projects/"+red.Cap.Project, nil, &p); err != nil {
		return nil, fmt.Errorf("the token was issued but the first call failed: %w", err)
	}
	used = true
	// 6. The member's checkouts: asked for, or found by the remote.
	resp := api.JoinResponse{}
	paths := req.Paths
	if len(paths) == 0 && req.Cwd != "" {
		if root := project.GitRoot(project.ExpandPath(req.Cwd)); root != "" {
			if origin := project.NormalizeRemote(project.OriginURL(root)); origin != "" && slices.Contains(x.HeaderRemotes(), origin) {
				paths = []string{root}
				resp.Notes = append(resp.Notes, "recorded this checkout: "+root)
			}
		}
	}
	for i, pth := range paths {
		if abs, err := filepath.Abs(project.ExpandPath(pth)); err == nil {
			paths[i] = abs
		}
	}
	x.R.Paths = paths
	if len(paths) == 0 {
		resp.Notes = append(resp.Notes, "no checkout recorded: pass --path <dir>, or use --project "+red.Cap.Slug+" for writes")
	}
	if err := s.bindAndSave(ctx, x, red.Cap, via, fingerprint, red.Authority); err != nil {
		return nil, err // the token goes back (used)
	}
	saved = true
	// 7. What the member sees: the layer above starts its copy and counts
	// from it.
	if s.Hooks.Joined != nil {
		s.Hooks.Joined(ctx, x, &resp)
	}
	snap := x.Snapshot()
	resp.Remote = &snap
	return resp, nil
}

func (x *Remote) HasToken() bool {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	return x.Token != ""
}

func (x *Remote) HeaderRemotes() []string {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	var h api.TeamHeader
	_ = json.Unmarshal([]byte(x.HeaderRaw), &h)
	var out []string
	for _, r := range h.Remotes {
		out = append(out, project.NormalizeRemote(r))
	}
	return out
}

// handleLeave is DELETE /v0/team/remotes/{project} (zm leave): the
// authority is asked to revoke the token (best effort), then the token file
// and the record go.
func (s *Service) handleLeave(r *http.Request) (any, error) {
	x := s.ByName(r.PathValue("project"))
	if x == nil {
		return nil, api.NotFound("joined project", r.PathValue("project"))
	}
	out := api.LeaveResponse{Project: x.Snapshot().Project}
	x.RetireRole() // no recovery while the best-effort token revocation waits
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/leave", struct{}{}, nil)
	cancel()
	out.Revoked = err == nil
	out.Removed = nonNil(s.Remove(r.Context(), x))
	return out, nil
}

// CallRemote is one call to x's authority as the replica (core API JSON),
// with x's token and scope_version; it records what the answer says
// (callAs).
func (s *Service) CallRemote(ctx context.Context, x *Remote, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.Snapshot().URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Send(ctx, x, req, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return Decode(resp, out)
}

// call is one plain JSON call over transport via (no token: redemption).
func (s *Service) call(ctx context.Context, via string, hc *http.Client, method, url, token string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(api.HeaderAuthorization, "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return s.Unreachable(via, url, err)
	}
	defer resp.Body.Close()
	return Decode(resp, out)
}

func Decode(resp *http.Response, out any) error {
	if resp.StatusCode/100 != 2 {
		var er api.ErrorResponse
		if json.NewDecoder(resp.Body).Decode(&er) == nil && er.Error != nil {
			er.Error.HTTPStatus = resp.StatusCode
			return er.Error
		}
		return api.Errorf(api.CodeUnavailable, "authority answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// unreachable is what a caller sees when the home could not be reached
// over via: the transport's own words (a pin that did not match, a hijacked
// hostname), else unavailable.
func (s *Service) Unreachable(via, url string, err error) error {
	if t := s.d.Transport(via); t != nil {
		if e := t.Explain(url, err); e != nil {
			return e
		}
	}
	return api.Errorf(api.CodeUnavailable, "authority %s unreachable: %v", url, err).WithHint("check this machine is online, and the home")
}
