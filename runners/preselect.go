package runners

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

var preselectMigrations = []store.Migration{
	// preselect: the repositories an install URL pre-selects (a Preselect as JSON); '' for none.
	{Kind: store.KindRegistry, Name: "runners/0012_pending_preselect", Script: `ALTER TABLE runners_pending ADD COLUMN preselect TEXT NOT NULL DEFAULT '';`},
}

// maxPreselect is the most repositories one install URL names; GitHub's own page takes a short list, and the URL stays short.
const maxPreselect = 20

// Preselect is what GitHub's install page starts with chosen: one account (its id) and some of its repositories
// (their ids). It rides in the App's creation state and the install URL's state, so the install URL minted after the
// App exists still carries it.
type Preselect struct {
	Owner   string  `json:"owner"` // the account's login, as GitHub spells it
	Org     bool    `json:"-"`     // the account is an organization (known when read, not stored)
	OwnerID int64   `json:"owner_id"`
	RepoIDs []int64 `json:"repo_ids"`
}

// encode is the column's text; "" for none.
func (p *Preselect) encode() string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func decodePreselect(col string) *Preselect {
	var p Preselect
	if col == "" || json.Unmarshal([]byte(col), &p) != nil || p.OwnerID <= 0 || len(p.RepoIDs) == 0 {
		return nil
	}
	return &p
}

// query is the install URL's pre-selection: the account to install on and the repositories to tick.
func (p *Preselect) query(v url.Values) {
	if p == nil {
		return
	}
	v.Set("target_id", itoa(p.OwnerID))
	for _, id := range p.RepoIDs {
		v.Add("repository_ids[]", itoa(id))
	}
}

// preselect resolves the repositories named in a setup request (owner/repo, all of one owner) to the ids GitHub's
// install page takes, reading each as the machine's owner does (personRead). None named is nil. A repository it
// cannot read is refused, not left out: the person asked for it to be ticked.
func (s *Service) preselect(ctx context.Context, repos []string) (*Preselect, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	if len(repos) > maxPreselect {
		return nil, api.Errorf(api.CodeBadRequest, "repos: at most %d repositories", maxPreselect)
	}
	var out Preselect
	var owner string
	for _, repo := range repos {
		if !jobrule.ValidRepo(repo) {
			return nil, api.Errorf(api.CodeBadRequest, "repos: %q is not owner/repo", repo)
		}
		o := strings.ToLower(strings.SplitN(repo, "/", 2)[0])
		if owner != "" && o != owner {
			return nil, api.Errorf(api.CodeBadRequest, "repos: %q and %q belong to different accounts; the App installs on one account at a time", repos[0], repo)
		}
		owner = o
		var r struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
			Owner    struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"owner"`
		}
		if err := s.personRead(ctx, "/repos/"+repo, &r); err != nil || r.ID <= 0 || r.Owner.ID <= 0 || !strings.EqualFold(r.FullName, repo) {
			return nil, api.Errorf(api.CodeUnavailable, "repos: GitHub would not say which repository %q is", repo).
				WithHint("check the name, and that gh is signed in as someone who can see it; or leave --repo out and choose the repositories on GitHub's page")
		}
		if out.OwnerID != 0 && out.OwnerID != r.Owner.ID {
			return nil, api.Errorf(api.CodeBadRequest, "repos: %q and %q belong to different accounts; the App installs on one account at a time", repos[0], repo)
		}
		out.OwnerID, out.Owner, out.Org = r.Owner.ID, r.Owner.Login, r.Owner.Type == "Organization"
		if !slices.Contains(out.RepoIDs, r.ID) {
			out.RepoIDs = append(out.RepoIDs, r.ID)
		}
	}
	return &out, nil
}
