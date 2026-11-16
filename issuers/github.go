package issuers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

// githubBackend is the github-token kind: per box, an installation token of
// the issuer's App for one repository and the requested permissions, inside
// the issuer's ceiling. GitHub's revoke authenticates with the token itself,
// so a token an earlier run held can only lapse (settle).
type githubBackend struct {
	spec Spec
	gh   *ghapp.Client
}

// ghToken is a ghapp.Token as a Credential.
type ghToken struct{ ghapp.Token }

func (t ghToken) Expires() time.Time { return t.ExpiresAt }
func (t ghToken) Hashed() string     { return t.HashedToken() }

// newGitHubBackend reads s's App key and builds its client.
func newGitHubBackend(ctx context.Context, s Spec, o Options) (*githubBackend, error) {
	ref, err := secrets.Parse(s.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("secrets.issuers.%s: private_key: %w", s.Name, err)
	}
	key, err := o.Secrets.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("secrets.issuers.%s: private_key: %w", s.Name, err)
	}
	gh, err := ghapp.New(ghapp.Config{AppID: s.AppID, PrivateKeyPEM: []byte(strings.TrimSpace(key.Reveal())), APIURL: s.APIURL,
		HTTP: o.HTTP, Now: o.Now, Sleep: o.Sleep})
	if err != nil {
		return nil, fmt.Errorf("secrets.issuers.%s: %w", s.Name, err)
	}
	return &githubBackend{spec: s, gh: gh}, nil
}

func (b *githubBackend) kind() string { return KindGitHubToken }

// check refuses a request GitHub must not be asked for. It answers the
// permissions to ask for (metadata read added) and the rule that covers them.
func (b *githubBackend) check(_ context.Context, req Request) (plan, string, error) {
	if !validRepo(req.Repo) {
		return plan{}, "the repository must be owner/name", nil
	}
	if len(req.Permissions) == 0 {
		return plan{}, "no permissions asked", nil
	}
	p := map[string]string{"metadata": "read"}
	for k, v := range req.Permissions {
		if !permNameRE.MatchString(k) {
			return plan{}, "a permission is not a GitHub permission name", nil
		}
		if permTier(k) == "" {
			return plan{}, "permission " + k + " is never minted for a box", nil
		}
		if err := checkLevel(k, v); err != nil {
			return plan{}, err.Error(), nil
		}
		p[k] = v
	}
	if len(elevatedWrite(p)) > 0 && strings.TrimSpace(req.Reason) == "" {
		return plan{}, "an elevated permission at write needs the request's reason", nil
	}
	if j := req.Subject.Job; j != nil {
		if why := jobrule.Check(*j); why != "" {
			return plan{}, why, nil
		}
		if !strings.EqualFold(j.Repo, req.Repo) {
			return plan{}, "a job's token is for its own repository only", nil
		}
	}
	rule, ok := b.spec.match(req.Repo, req.Subject, p)
	if !ok {
		return plan{}, "outside the issuer's ceiling (repository, permission, event or workflow)", nil
	}
	return plan{perms: p, elevated: elevatedWrite(p), ruleReason: rule.Reason}, "", nil
}

func validRepo(repo string) bool { return jobrule.ValidRepo(repo) }

func owner(repo string) string { o, _, _ := strings.Cut(repo, "/"); return o }

// noInstallation is the App not installed on a repository's account: the
// ledger keeps GitHub's answer, the caller is told only which account.
type noInstallation struct {
	owner string
	err   error
}

func (e noInstallation) Error() string {
	return "no installation of the App on the repository's account: " + e.err.Error()
}
func (e noInstallation) public() string {
	return "no installation of the App on " + e.owner + "'s account"
}

// mint finds the App's installation on the repository's account (once: it
// is kept on req), mints and checks GitHub's answer.
func (b *githubBackend) mint(ctx context.Context, req *Request, p plan) (Credential, error) {
	if req.Installation <= 0 {
		lctx, cancel := context.WithTimeout(ctx, mintTimeout)
		id, err := b.gh.RepoInstallation(lctx, req.Repo)
		cancel()
		if err != nil {
			return nil, noInstallation{owner: owner(req.Repo), err: err}
		}
		req.Installation = id
	}
	mctx, cancel := context.WithTimeout(ctx, mintTimeout)
	tok, err := b.gh.MintToken(mctx, req.Installation, req.Repo, ghapp.Perms(p.perms))
	cancel()
	if err != nil {
		var ae *ghapp.AnswerError
		if errors.As(err, &ae) && ae.Unrevoked.Reveal() != "" {
			return nil, &unrevokedError{cred: ghToken{ae.Unrevoked}, err: err}
		}
		return nil, err
	}
	return ghToken{tok}, nil
}

// revoke is DELETE /installation/token; GitHub answering 401 counts as
// revoked (ghapp).
func (b *githubBackend) revoke(ctx context.Context, c Credential) error {
	t, ok := c.(ghToken)
	if !ok {
		return api.Errorf(api.CodeInternal, "github-token: not a GitHub token")
	}
	return b.gh.RevokeToken(ctx, t.Token)
}

// settle: an earlier run's token is gone with that run's memory, and GitHub
// revokes only by the token itself, so it lapses within the hour.
func (b *githubBackend) settle(context.Context, Entry) bool { return false }

func (b *githubBackend) ceiling(raw json.RawMessage) (json.RawMessage, error) {
	c, rules, err := ParseGrantCeiling(raw)
	if err != nil {
		return nil, err
	}
	if err := b.spec.InsideConfig(rules); err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

func (b *githubBackend) within(ceiling, params json.RawMessage) error {
	_, rules, err := ParseGrantCeiling(ceiling)
	if err != nil {
		return err
	}
	var p Params
	if err := strict(params, &p); err != nil {
		return fmt.Errorf("github-token params: %w", err)
	}
	return WithinRules(rules, p)
}
