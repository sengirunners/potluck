package owners

// Key logins (purpose "key"): a person's client binds its signing key by a
// GitHub login this authority runs. The key rides inside the login; when
// GitHub says who approved, the collect links the starting identity to that
// account (never joining two people) and binds the key, in one registry
// transaction, through core.Deps.KeyLogin. The client asserts no identity:
// the person is the identity that started the login, the account is
// GitHub's answer, and the node is the credential's.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// keyNode is where a key started by sub answers from: the node of a
// replica's node-bound token, or core.KeyNodeLocal for the socket owner.
// ok is false for any other caller (linkable's rule).
func keyNode(sub *authz.Subject) (node string, ok bool) {
	if linkable(sub) != nil {
		return "", false
	}
	if sub.Via == authz.ViaLocal {
		return core.KeyNodeLocal, true
	}
	return sub.Node, true
}

// startKey starts a key login for sub: refused before anything is reserved
// unless sub is linkable and human and KeyLogin's check passes.
func (s *Service) startKey(ctx context.Context, sub *authz.Subject, hdr http.Header, req api.GitHubLoginRequest) (api.GitHubLoginStart, error) {
	node, ok := keyNode(sub)
	if !ok {
		return api.GitHubLoginStart{}, linkable(sub)
	}
	if sub.Identity.Kind != api.IdentityHuman {
		return api.GitHubLoginStart{}, api.Errorf(api.CodeForbidden, "only a person binds a signing key; identity %s is a %s", sub.Identity.Name, sub.Identity.Kind)
	}
	if req.Project == "" {
		return api.GitHubLoginStart{}, api.Errorf(api.CodeBadRequest, "name the project the key answers in: ?project=")
	}
	p, err := s.d.Projects.Get(sub.Scope, req.Project)
	if err != nil {
		return api.GitHubLoginStart{}, err
	}
	machine := sub.Machine
	if node == core.KeyNodeLocal {
		machine = s.d.Config.Machine
	}
	ks := &core.KeyStart{Project: api.ProjectRef{ID: p.ID, Slug: p.Slug}, Node: node, Machine: machine, Key: req.KeyAttach}
	if err := s.d.KeyLogin.Check(ctx, sub, hdr, *ks); err != nil {
		return api.GitHubLoginStart{}, err
	}
	return s.begin(ctx, &login{purpose: PurposeKey, identity: sub.Identity, from: s.from(sub),
		local: sub.Via == authz.ViaLocal, key: ks})
}

// replaceKeyLogins runs once GitHub gave l a code: the latest key login
// of an identity from one node wins, by start order. It drops every
// earlier one, collected or not (its poll answers the 404 of a missing
// login), and answers true, dropping l, when a later one already has its
// code. A start that fails never drops anything. So an agent cannot hold
// a person's sign-in by starting first, and either side sees it was
// replaced. A login already collecting is not stopped: it finishes, and
// the bind's own one-key-per-node rule ends the older key. Called with
// s.mu held.
func (s *Service) replaceKeyLogins(l *login) (replaced bool) {
	for _, x := range s.logins {
		if x == l || x.key == nil || x.owner() != l.owner() {
			continue
		}
		switch {
		case x.keySeq < l.keySeq:
			s.drop(x)
		case x.started:
			replaced = true
		}
	}
	if replaced {
		s.drop(l)
	}
	return replaced
}

// replacedLogin is the answer to a start replaced while GitHub was asked.
func replacedLogin() error {
	return api.Errorf(api.CodeConflict, "another sign-in from this machine replaced this one")
}

// bindKey links l's identity to acc and binds its key, both or neither.
// A key login never joins two people: an identity not yet linked is
// refused when acc is already linked to another identity (an explicit
// link login joins them). The mail goes out after the commit.
func (s *Service) bindKey(ctx context.Context, l *login, acc Account) (api.GitHubLoginOutcome, error) {
	var b api.KeyBinding
	var first bool
	err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var err error
		if _, first, err = s.linkTx(ctx, tx, l.identity, acc); err != nil {
			return err
		}
		if first {
			others, err := linkedOthers(ctx, tx, acc.ID, l.identity.ID)
			if err != nil {
				return err
			}
			if len(others) > 0 {
				return api.Errorf(api.CodeConflict, "this GitHub account already belongs to another identity here; join them first with zm login github")
			}
		}
		if b, err = s.d.KeyLogin.Bind(ctx, tx, core.KeyBind{KeyStart: *l.key, Identity: l.identity, Account: acc.ID, Login: acc.Login}); err != nil {
			return err
		}
		b.Account, b.Login, b.Machine = acc.ID, acc.Login, l.key.Machine
		return nil
	})
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	if first {
		s.tellLinked(ctx, l.identity, acc, l.from)
	}
	s.tell(ctx, l.identity, []string{l.key.Project.ID}, fmt.Sprintf("A signing key (%s) now answers as GitHub @%s (identity %s) from machine %s, bound by a GitHub sign-in on %s. "+
		"If that was not you: zm approver rm %s on %s.",
		b.Fingerprint, acc.Login, l.identity.Name, l.key.Machine, s.d.Config.Machine, b.Approver, s.d.Config.Machine))
	return api.GitHubLoginOutcome{Key: &b}, nil
}
