package issuers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/config"
)

// The two ceilings, each checked in one place. A grant's ceiling (who may ask
// for what) is the grants package's: it stores it through Ceiling and checks
// a request against it through Within, in grants.Check. The config ceiling
// (the operator's bound on what the App may ever mint) is checked in Issue.
// Ceiling also refuses a grant ceiling not inside the config ceiling, so a
// grant that could never be served is not added.

// GrantCeiling is a github-token grant's ceiling:
//
//	{"repos": ["acme/app", "acme/*"], "permissions": {"contents": "write"},
//	 "elevated": ["workflows"], "reason": "release tasks"}
type GrantCeiling struct {
	Repos       []string          `json:"repos"`
	Permissions map[string]string `json:"permissions"`
	Elevated    []string          `json:"elevated,omitempty"`
	Reason      string            `json:"reason,omitempty"`
}

// Params is what a github-token request asks for: Request's repo,
// permissions and reason, in Request's JSON names.
type Params struct {
	Repo        string            `json:"repo"`
	Permissions map[string]string `json:"permissions"`
	Reason      string            `json:"reason,omitempty"`
}

// ParamsOf is req's params.
func ParamsOf(req Request) Params {
	return Params{Repo: req.Repo, Permissions: req.Permissions, Reason: req.Reason}
}

// strict decodes raw into v refusing unknown keys and trailing data.
func strict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// ParseGrantCeiling reads and checks a grant ceiling with the config rules'
// own checks (allowlist, levels, elevated with a reason).
func ParseGrantCeiling(raw json.RawMessage) (GrantCeiling, []Rule, error) {
	var c GrantCeiling
	if err := strict(raw, &c); err != nil {
		return GrantCeiling{}, nil, fmt.Errorf("github-token ceiling: %w", err)
	}
	if len(c.Repos) == 0 {
		return GrantCeiling{}, nil, errors.New("github-token ceiling: no repos")
	}
	rules := make([]Rule, 0, len(c.Repos))
	for _, repo := range c.Repos {
		r, err := checkRule(config.SecretsCeiling{Repo: repo, Permissions: c.Permissions, Elevated: c.Elevated, Reason: c.Reason})
		if err != nil {
			return GrantCeiling{}, nil, fmt.Errorf("github-token ceiling: %w", err)
		}
		rules = append(rules, r)
	}
	return c, rules, nil
}

// WithinRules is Within on parsed rules.
func WithinRules(rules []Rule, p Params) error {
	if !validRepo(p.Repo) || strings.Contains(p.Repo, "*") {
		return errors.New("github-token params: repo must be owner/name")
	}
	asked := map[string]string{"metadata": "read"}
	for k, v := range p.Permissions {
		if permTier(k) == "" {
			return fmt.Errorf("github-token params: permission %q is never minted for a box", k)
		}
		if err := checkLevel(k, v); err != nil {
			return fmt.Errorf("github-token params: %w", err)
		}
		asked[k] = v
	}
	if len(p.Permissions) == 0 {
		return errors.New("github-token params: no permissions")
	}
	for _, r := range rules {
		if r.matches(p.Repo, Subject{IdentityID: "-"}) && r.covers(asked) {
			return nil
		}
	}
	return errors.New("github-token params: outside the grant's ceiling")
}

// InsideConfig refuses rules (a grant's ceiling) that the issuer's config
// ceiling does not cover: each repository pattern and its permissions must
// sit inside one config rule. Workflows and events are the grant's own (its
// "to"), so they are checked at Issue.
func (s Spec) InsideConfig(rules []Rule) error {
	for _, gr := range rules {
		ok := false
		for _, cr := range s.Ceiling {
			if repoCovers(cr.Repo, gr.Repo) && cr.covers(gr.Permissions) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("github-token ceiling: %s with %s is not inside issuer %s's config ceiling", gr.Repo, permString(gr.Permissions), s.Name)
		}
	}
	return nil
}

// repoCovers: a config pattern owner/* covers any pattern of owner; owner/name
// only itself.
func repoCovers(outer, inner string) bool {
	oo, on, _ := strings.Cut(outer, "/")
	io, in, _ := strings.Cut(inner, "/")
	if !strings.EqualFold(oo, io) {
		return false
	}
	return on == "*" || (in != "*" && strings.EqualFold(on, in))
}

// JobRequest is the request the runner broker sends for job's box under a
// job grant g it matched (grants.JobMatches): the grant's kind and name, and
// for github-token the job's own repository and the grant's permissions and
// reason, for potluck-token the grant's role. Issue still decides it whole
// (the config ceiling, the box's class, the grant check with the job as
// subject).
func JobRequest(g computeapi.SecretGrant, job Job, box Box) (Request, error) {
	if g.To.Job == nil {
		return Request{}, errors.New("issuers: not a job grant")
	}
	j := job
	switch g.Kind {
	case KindGitHubToken:
		c, _, err := ParseGrantCeiling(g.Ceiling)
		if err != nil {
			return Request{}, err
		}
		return Request{Kind: g.Kind, Name: g.Name, Subject: Subject{Job: &j}, Box: box, Repo: job.Repo, Permissions: maps.Clone(c.Permissions),
			Reason: c.Reason}, nil
	case KindPotluckToken:
		c, _, err := parsePotluckCeiling(g.Ceiling)
		if err != nil {
			return Request{}, err
		}
		return Request{Kind: g.Kind, Name: g.Name, Subject: Subject{Job: &j}, Box: box, Role: c.Role}, nil
	}
	return Request{}, fmt.Errorf("issuers: %s is not a minted kind", g.Kind)
}
