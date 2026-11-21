package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// coreRoutes are the daemon's own endpoints (API.md "Core").
func (dm *Daemon) coreRoutes(r *core.Router) {
	d := dm.Deps
	r.HandleFunc("GET /v0/health", core.Public, "none; machine for everyone, pid/started/hedges/holders only for local callers", dm.health)
	r.HandleFunc("GET /v0/whoami", core.Authed, "none: describes the caller", dm.whoami)
	r.Admit(core.AnyLimit, "GET /v0/whoami")
	r.HandleFunc("POST /v0/daemon/stop", core.Local, "local; refuses while sessions that would end run (none with holders) unless force, and during another caller's swap", dm.daemonStop)
	r.HandleFunc("GET /v0/daemon/manifest", core.Local, "local", dm.manifest)
	r.HandleFunc("POST /v0/daemon/swaps", core.Local, "local; the caller is recorded as who swapped", dm.recordSwap)
	r.Handle("GET /v0/events", core.Authed, "read: per-event Scope.Can(read, project/area); mentions redacted", d.Events.Handler(events.HandlerOptions{
		Subject: func(r *http.Request) *authz.Subject { return core.SubjectOf(r.Context()) },
		Project: func(r *http.Request, ref string) (string, error) {
			if ref == api.ProjectNone {
				if core.SubjectOf(r.Context()).Scope.Machine() {
					return api.ProjectNone, nil
				}
				return "", api.NotFound("project", ref)
			}
			p, err := d.Projects.Get(core.SubjectOf(r.Context()).Scope, ref)
			return p.ID, err
		},
		Done:   dm.closing,
		Tunnel: func(r *http.Request) bool { return core.OverTunnel(r.Context()) },
		Still: func(r *http.Request, project string) error {
			was := core.SubjectOf(r.Context())
			if was == nil || was.Via == authz.ViaLocal {
				return nil // the machine's owner, on its own socket
			}
			// The version this request sent is stale by design: the scope itself
			// is compared, for this stream's project.
			g := r.Clone(r.Context())
			api.DelHeader(g.Header, api.HeaderScopeVersion)
			ctx := d.Authenticate(g)
			now := core.SubjectOf(ctx)
			if err := core.AuthErr(ctx); now == nil || err != nil {
				if err != nil && !api.IsCode(err, api.CodeUnauthenticated) && !api.IsCode(err, api.CodeRescope) {
					return nil // not an answer about access: the next ping asks again
				}
				if api.IsCode(err, api.CodeRescope) || now == nil && err == nil {
					return api.Errorf(api.CodeRescope, "your access to this project changed; start the stream again")
				}
				return api.Errorf(api.CodeUnauthenticated, "your access ended")
			}
			if !was.Scope.SameFor(now.Scope, project) {
				return api.Errorf(api.CodeRescope, "your access to this project changed; start the stream again")
			}
			return nil
		},
	}))

	r.HandleFunc("GET /v0/projects", core.Authed, "read: projects the caller sees, areas filtered", jsonH(func(r *http.Request) (any, error) {
		return api.ProjectList{Projects: nonNil(d.Projects.List(core.SubjectOf(r.Context()).Scope))}, nil
	}))
	r.HandleFunc("POST /v0/projects", core.Local, "local; the caller's identity becomes owner", jsonH(func(r *http.Request) (any, error) {
		var req api.NewProjectRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		p, err := d.Projects.Create(r.Context(), core.SubjectOf(r.Context()), req)
		if err == nil {
			dm.publish(r.Context(), api.EventProjectCreated, p.ID, p.ID, "", p)
		}
		return p, err
	}))
	r.HandleFunc("GET /v0/projects/resolve", core.Authed, "read: only projects the caller sees are candidates", jsonH(func(r *http.Request) (any, error) {
		return d.Resolve(r.Context()), nil
	}))
	r.HandleFunc("DELETE /v0/projects/{project}", core.Local, "local; admin on the project; its tokens end and its folder goes; refused while sandboxes run in it unless ?force=1", jsonH(func(r *http.Request) (any, error) {
		return d.Projects.Remove(r.Context(), core.SubjectOf(r.Context()), r.PathValue("project"),
			project.RemoveOptions{Force: r.URL.Query().Get("force") == "1"})
	}))
	r.HandleFunc("GET /v0/projects/{project}", core.Authed, "read on the project, else not_found", jsonH(func(r *http.Request) (any, error) {
		return d.Projects.Get(core.SubjectOf(r.Context()).Scope, r.PathValue("project"))
	}))
	r.HandleFunc("PATCH /v0/projects/{project}", core.Authed, "admin on the project; repos/trackers: machine owner on the socket only", jsonH(func(r *http.Request) (any, error) {
		var req api.UpdateProjectRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		p, err := d.Projects.Update(r.Context(), core.SubjectOf(r.Context()), r.PathValue("project"), req)
		if err == nil {
			dm.publish(r.Context(), api.EventProjectUpdated, p.ID, p.ID, "", p)
		}
		return p, err
	}))
	r.HandleFunc("GET /v0/projects/{project}/areas", core.Authed, "read on the project; only visible areas", jsonH(func(r *http.Request) (any, error) {
		p, err := d.Projects.Get(core.SubjectOf(r.Context()).Scope, r.PathValue("project"))
		return api.AreaList{Areas: nonNil(p.Areas)}, err
	}))
	r.HandleFunc("POST /v0/projects/{project}/areas", core.Authed, "admin on the project; paths/remotes: machine owner on the socket only", jsonH(func(r *http.Request) (any, error) {
		var a api.AddAreaRequest
		if err := api.DecodeJSON(r, &a); err != nil {
			return nil, err
		}
		p, err := d.Projects.AddArea(r.Context(), core.SubjectOf(r.Context()), r.PathValue("project"), a)
		if err == nil {
			for _, x := range p.Areas {
				if x.Name == strings.ToLower(strings.TrimSpace(a.Name)) {
					dm.publish(r.Context(), api.EventAreaAdded, x.Name, p.ID, x.Name, x)
				}
			}
		}
		return p, err
	}))
	r.HandleFunc("GET /v0/projects/{project}/areas/{area}", core.Authed, "read on the area; hidden area or project = not_found", jsonH(func(r *http.Request) (any, error) {
		return d.Projects.GetArea(core.SubjectOf(r.Context()).Scope, r.PathValue("project"), r.PathValue("area"))
	}))
	r.HandleFunc("PATCH /v0/projects/{project}/areas/{area}", core.Authed, "read on the area, else not_found; admin on the project; paths/remotes: machine owner on the socket only", jsonH(func(r *http.Request) (any, error) {
		var req api.UpdateAreaRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		v, err := d.Projects.UpdateArea(r.Context(), core.SubjectOf(r.Context()), r.PathValue("project"), r.PathValue("area"), req)
		if err == nil {
			dm.publish(r.Context(), api.EventAreaUpdated, v.Name, v.Project.ID, v.Name, v.Area)
		}
		return v, err
	}))
	r.HandleFunc("POST /v0/projects/{project}/export", core.Local, "local; admin on the project", jsonH(func(r *http.Request) (any, error) {
		var req api.ExportRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return d.Projects.Export(r.Context(), core.SubjectOf(r.Context()), r.PathValue("project"), req.Dir)
	}))

	r.HandleFunc("GET /v0/grants", core.Authed, "admin on each listed object (?project= must be visible)", jsonH(func(r *http.Request) (any, error) {
		sub, project := core.SubjectOf(r.Context()), r.URL.Query().Get("project")
		gs, err := d.Projects.Grants(r.Context(), sub, project)
		if err != nil {
			return nil, err
		}
		cs, err := d.Projects.Ceilings(r.Context(), sub, project)
		return api.GrantList{Grants: nonNil(gs), Ceilings: nonNil(cs)}, err
	}))
	r.HandleFunc("POST /v0/grants", core.Authed, "admin on the project/area", jsonH(func(r *http.Request) (any, error) {
		var req api.GrantRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		g, err := d.Projects.Grant(r.Context(), core.SubjectOf(r.Context()), req)
		if err == nil {
			dm.publish(r.Context(), api.EventGranted, g.Identity, "", "", g)
		}
		return g, err
	}))
	r.HandleFunc("DELETE /v0/grants", core.Authed, "admin on the project/area", jsonH(func(r *http.Request) (any, error) {
		var req api.GrantRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		g, err := d.Projects.Revoke(r.Context(), core.SubjectOf(r.Context()), req)
		if err == nil {
			dm.publish(r.Context(), api.EventRevoked, g.Identity, "", "", g)
			d.ReapPresence(r.Context()) // a replica whose identity lost its last grant leaves the lists now
		}
		return g, err
	}))
	r.HandleFunc("GET /v0/identities", core.Local, "local", jsonH(func(r *http.Request) (any, error) {
		ids, err := d.Projects.Identities(r.Context())
		return api.IdentityList{Identities: nonNil(ids)}, err
	}))
	r.HandleFunc("POST /v0/identities", core.Local, "local", jsonH(func(r *http.Request) (any, error) {
		var req api.NewIdentityRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return d.Projects.AddIdentity(r.Context(), req)
	}))
	r.HandleFunc("GET /v0/tokens", core.Local, "local", jsonH(func(r *http.Request) (any, error) {
		ts, err := d.Projects.Tokens(r.Context())
		return api.TokenList{Tokens: nonNil(ts)}, err
	}))
	r.HandleFunc("POST /v0/tokens", core.Local, "local; admin on the project/area (adds the grant)", jsonH(func(r *http.Request) (any, error) {
		var req api.MintTokenRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		var t api.MintTokenResponse
		var err error
		if req.Limit != "" || req.Node != "" {
			t, err = dm.mintLimited(r.Context(), req)
		} else {
			t, err = d.Projects.Mint(r.Context(), core.SubjectOf(r.Context()), req)
		}
		if err == nil {
			dm.publish(r.Context(), api.EventTokenMinted, t.Token.ID, "", "", t.Token)
		}
		return t, err
	}))
	r.HandleFunc("DELETE /v0/tokens/{id}", core.Local, "local", jsonH(func(r *http.Request) (any, error) {
		t, err := d.Projects.RevokeToken(r.Context(), r.PathValue("id"))
		if err == nil {
			dm.publish(r.Context(), api.EventTokenRevoked, t.ID, "", "", t)
			if t.Node != "" {
				d.ReapPresence(r.Context()) // a replica's sessions end with its token
			}
		}
		return t, err
	}))
}

func (dm *Daemon) publish(ctx context.Context, typ, subject, project, area string, data any) {
	if _, err := dm.Deps.Events.Publish(ctx, events.Input{Type: typ, Subject: subject, Project: project, Area: area, Data: data}); err != nil {
		dm.Deps.Log.Warn("publish", "type", typ, "err", err)
	}
}

// jsonH adapts fn to a handler writing 200 + JSON, or the error.
func jsonH(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func (dm *Daemon) health(w http.ResponseWriter, r *http.Request) {
	sub := core.SubjectOf(r.Context())
	api.WriteJSON(w, http.StatusOK, dm.healthBody(r.Context(), sub != nil && sub.Via == authz.ViaLocal))
}

func (dm *Daemon) whoami(w http.ResponseWriter, r *http.Request) {
	cfg := dm.Deps.Config
	sub := core.SubjectOf(r.Context())
	a, _ := api.ParseAddress(sub.Address)
	out := api.Whoami{As: sub.Address, Kind: a.Kind, Identity: sub.Identity, Via: sub.Via, Session: sub.Session, Limit: sub.Limit(),
		Machine: cfg.Machine, Protocol: api.WireProtocol, Version: api.Version, Commit: api.Commit}
	if !sub.Remote {
		out.Home, out.Socket = cfg.Home, cfg.ListenSocketPath()
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (dm *Daemon) daemonStop(w http.ResponseWriter, r *http.Request) {
	var req api.DaemonStopRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if err := dm.stopGuard(r.Context(), req); err != nil {
		api.WriteError(w, err)
		return
	}
	n := 0
	sh := dm.Deps.SessionHooks()
	if sh != nil && sh.Live != nil {
		n = sh.Live(r.Context())
	}
	if req.DryRun {
		api.WriteJSON(w, http.StatusOK, api.DaemonStopResponse{ManagedSessions: n})
		return
	}
	var notEnded []api.DaemonSessionFailure
	if req.Force {
		if sh != nil && sh.End != nil {
			var err error
			n, notEnded, err = sh.End(r.Context())
			if err != nil {
				api.WriteError(w, err)
				return
			}
		} else if n > 0 {
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "cannot report which managed sessions ended; stop them individually first"))
			return
		}
		if sh != nil && sh.Keep != nil {
			sh.Keep() // Remaining sessions were not verified safe to stop.
		}
	}
	api.WriteJSON(w, http.StatusOK, api.DaemonStopResponse{Stopping: true, ManagedSessions: n, Ended: req.Force, NotEnded: notEnded})
	dm.once.Do(func() {
		by := core.SubjectOf(r.Context()).Address
		dm.stopBy = &stopNote{By: by, What: req.What, Force: req.Force}
		attrs := []any{"by", by, "what", req.What, "force", req.Force}
		if sh != nil { // a daemon with sessions (zm), not potluck
			attrs = append(attrs, "managed_sessions", n)
		}
		dm.Deps.Log.Warn("daemon stop requested", attrs...)
		close(dm.stop)
	})
}
