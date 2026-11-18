package core

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/project"
)

// Authenticate works out who is calling (SPEC "Who is calling") and returns
// ctx carrying the subject (or the reason it failed, which the router reports
// on every non-public route) and the request's project hints.
//
//   - Unix socket (IsLocal: marked by the Unix listener), no token: the OS
//     user (scope All). Zm-As may name a session on this machine, this
//     machine's human or machine (reply address only; it grants nothing).
//   - Any transport with "Authorization: Bearer <token>": the token's identity,
//     capped by the token. On the socket this is a guard rail, not security.
//     A limited token (authz.Cap.Limit) authenticates only over the tailnet
//     from the device it is pinned to, never from this host; the server then
//     lets it reach only the routes that admit its limit.
//   - Anything else is network, fail closed: without a token, the WhoIs
//     login's identity (from WithRemote) if mapped, else unauthenticated. A
//     listener that forgets WithRemote gets 401s, never the owner. Zm-As
//     must be empty or the identity's own address. Zm-Cwd is ignored. A
//     login is an ambient credential, so its requests must also pass
//     ambientGuard (own Host, not cross-site, Zm-Protocol on writes).
func (d *Deps) Authenticate(r *http.Request) context.Context {
	ctx := r.Context()
	local := IsLocal(ctx)
	in := project.ResolveInput{Explicit: strings.TrimSpace(api.GetHeader(r.Header, api.HeaderProject))}
	if in.Explicit != "" {
		in.ExplicitFrom = project.RuleFlag
		if api.GetHeader(r.Header, api.HeaderProjectFrom) == project.RuleEnv {
			in.ExplicitFrom = project.RuleEnv
		}
	}
	if local {
		in.Cwd = api.GetHeader(r.Header, api.HeaderCwd)
	}
	ctx = WithResolveInput(ctx, in)

	var sub *authz.Subject
	var err error
	as := strings.TrimSpace(api.GetHeader(r.Header, api.HeaderAs))
	switch tok := authz.BearerToken(r.Header.Get(api.HeaderAuthorization)); {
	case tok != "":
		// The caller as the listener identified it: a limited token's
		// device check (FromTokenCaller) runs before anything is written,
		// for every caller of Authenticate.
		c, tunnel := project.Caller{Remote: !local}, false
		if rm := RemoteOf(ctx); rm != nil {
			c.NodeID, c.NodeKey, c.Via, c.Self = rm.NodeID, rm.NodeKey, rm.Via, rm.Self
			tunnel = rm.Via == ViaTunnel
		}
		sub, err = d.Projects.FromTokenCaller(ctx, tok, c)
		if err == nil && tunnel && sub.Node == "" && sub.BoxKey == "" {
			// The tunnel takes only a key-bound token of the signing key
			// (SPEC 17.11): a replica's, or a box's bound to its per-box key
			// (FromTokenAt checked the key). A bearer token alone is not an
			// internet credential.
			sub, err = nil, api.Errorf(api.CodeUnauthenticated, "invalid or expired token")
		}
		if err == nil && sub.Node != "" {
			// A team replica (SPEC 17.4): its answers carry the project header,
			// and a scope_version it sent that is not current is 409 rescope.
			ctx = withTeamSubject(ctx, sub)
			if v := strings.TrimSpace(api.GetHeader(r.Header, api.HeaderScopeVersion)); v != "" && v != strconv.FormatInt(sub.ScopeVersion, 10) {
				return withAuthErr(ctx, api.Errorf(api.CodeRescope, "your access to this project changed (scope version %d, you sent %s)", sub.ScopeVersion, v).
					WithHint("the replica refreshes what it holds; run the command again"))
			}
		}
	case local:
		sub = d.Projects.Local("")
	default:
		login := ""
		if rm := RemoteOf(ctx); rm != nil {
			login = rm.Login
		}
		// A login is ambient (ambient.go): the guard runs first, so its
		// refusal is the same for a mapped and an unmapped login.
		if login != "" {
			if err = d.ambientGuard(r); err != nil {
				return withAuthErr(ctx, err)
			}
		}
		sub, err = d.Projects.FromTailscale(ctx, login)
	}
	if err != nil {
		return withAuthErr(ctx, err)
	}
	sub.PersonalMail = r.Header.Get(api.HeaderPersonalMail) == api.PersonalMailProtocol
	if as != "" {
		if sub, err = d.withAddress(ctx, sub, as); err != nil {
			return withAuthErr(ctx, err)
		}
	}
	return WithSubject(ctx, sub)
}

// asSession resolves a Zm-As naming one of this machine's sessions by its
// name (SPEC 8 "Names") to the session's address now, or refuses it: mail
// would store the name as the sender, and a session given that name later
// would get the replies. A session address (never a name) is as given.
func (d *Deps) asSession(ctx context.Context, as string) (string, error) {
	m, name, named := api.SessionNameRef(as)
	if !named || m != d.Config.Machine {
		return as, nil
	}
	h := d.SessionHooks()
	if h == nil {
		// No session module: no name resolves, and the name, which is no
		// address, is refused where it is parsed.
		return as, nil
	}
	if h.Name != nil {
		if addr, ok := h.Name(ctx, m+"/"+name); ok {
			return addr, nil
		}
	}
	return "", api.Errorf(api.CodeForbidden, "%s %q is no session on this machine", api.HeaderAs, as).
		WithHint("a session acts as its address or its name while it is live; zm ls lists them")
}

// OwnAddress reports whether addr is one of sub's own: its address, its
// identity's, and for a replica's token its machine or any session on that
// machine, or on this machine's socket this machine's human, machine or any
// session here (live or ended). A record of who made what (a sandbox's
// actor) is the caller's own by it, whichever of their sessions made it.
func (d *Deps) OwnAddress(sub *authz.Subject, addr string) bool {
	if sub == nil || addr == "" {
		return false
	}
	if addr == sub.Address {
		return true
	}
	a, err := api.ParseAddress(addr)
	if err != nil {
		return false
	}
	if sub.Identity.Name != "" && a.Kind == api.KindHuman && sub.Identity.Kind != api.IdentityMachine && a.Human == sub.Identity.Name {
		return true
	}
	onMachine := func(m string) bool {
		return m != "" && (a.Kind == api.KindSession || a.Kind == api.KindMachine) && a.Machine == m
	}
	cfg := d.Config
	switch {
	case sub.Node != "" && sub.Machine != "":
		// As withAddress: never this machine's own name.
		return sub.Machine != cfg.Machine && onMachine(sub.Machine)
	case sub.Via == authz.ViaLocal && !sub.Remote:
		return onMachine(cfg.Machine) || (a.Kind == api.KindHuman && a.Human == cfg.Human)
	}
	return false
}

// withAddress applies a Zm-As header: it may only pick a reply address the
// subject already owns, never add rights.
func (d *Deps) withAddress(ctx context.Context, sub *authz.Subject, as string) (*authz.Subject, error) {
	if sub.Node == "" && sub.Via == authz.ViaLocal {
		var err error
		if as, err = d.asSession(ctx, as); err != nil {
			return nil, err
		}
	}
	a, err := api.ParseAddress(as)
	if err != nil {
		return nil, err
	}
	cfg := d.Config
	if sub.Node != "" && sub.Machine != "" {
		// A replica's token answers as its own human, its machine, or any
		// session on the machine name recorded at redemption (SPEC 17.3).
		// Never this machine's own name (redemption refuses it; a token from
		// before that, or an authority renamed onto a replica's name, still
		// gets nothing of this machine's sessions or group mailbox).
		own := sub.Machine == cfg.Machine
		switch {
		case a.String() == sub.Address:
			return sub, nil
		case own:
		case a.Kind == api.KindSession && a.Machine == sub.Machine:
			return sub.ForSession(a.String()), nil
		case a.Kind == api.KindMachine && a.Machine == sub.Machine:
			c := *sub
			c.Address = a.String()
			return &c, nil
		}
		return nil, api.Errorf(api.CodeForbidden, "%s must be %s, machine:%s or a session on %s", api.HeaderAs, sub.Address, sub.Machine, sub.Machine)
	}
	if sub.Via != authz.ViaLocal {
		if a.String() != sub.Address {
			return nil, api.Errorf(api.CodeForbidden, "%s must be empty or %s for this token", api.HeaderAs, sub.Address)
		}
		return sub, nil
	}
	switch a.Kind {
	case api.KindSession:
		if a.Machine != cfg.Machine {
			return nil, api.Errorf(api.CodeForbidden, "%s %q is not on this machine (%s)", api.HeaderAs, as, cfg.Machine)
		}
		c := sub.ForSession(a.String())
		if h := d.SessionHooks(); h != nil && h.Cap != nil {
			if project, role, capped := h.Cap(c.Session); capped {
				c.Scope = c.Scope.Capped(project, role)
			}
		}
		return c, nil
	case api.KindMachine:
		if a.Machine != cfg.Machine {
			return nil, api.Errorf(api.CodeForbidden, "%s %q is not this machine", api.HeaderAs, as)
		}
	case api.KindHuman:
		if a.Human != cfg.Human {
			return nil, api.Errorf(api.CodeForbidden, "on this socket you are %s, not %s", cfg.HumanAddress(), as)
		}
	default:
		return nil, api.Errorf(api.CodeBadRequest, "%s must be a session, machine or human address", api.HeaderAs)
	}
	c := *sub
	c.Address = a.String()
	return &c, nil
}
