package runners

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// The App can be public-installable (GitHub offers an App to other accounts
// only then), so any GitHub account may install it, and GitHub then sends
// that installation's events here, signed with the App's own webhook secret:
// a good signature says the delivery is GitHub's, not that it is ours.
// runners.accounts names the accounts the broker hears; with none named, it
// hears only the installations linked here. Everything else is dropped before
// it is stored, so a stranger's job leaves no row, no doctor probe and no
// owner notice behind.

// dropLogEvery is how often one account's dropped deliveries are logged.
const dropLogEvery = time.Hour

// accountRefused answers why the broker does not serve GitHub account
// ("" if it may): runners.accounts is set and does not name it.
func (s *Service) accountRefused(account string) string {
	accounts := s.conf().Accounts
	if len(accounts) == 0 || slices.ContainsFunc(accounts, func(a string) bool { return strings.EqualFold(strings.TrimSpace(a), account) }) {
		return ""
	}
	return "GitHub account " + account + " is not in runners.accounts (" + strings.Join(accounts, ", ") + ")"
}

// unheard answers why a delivery of event is dropped before it is stored
// ("" if it is heard). An installation event's account is the installation's;
// a workflow_job's is its repository's owner, the installation's account. A
// linked installation's own installation events are always heard: they can
// only unlink it.
func (s *Service) unheard(ctx context.Context, event string, body []byte) (string, error) {
	var d struct {
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		} `json:"installation"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "the delivery is not JSON", nil
	}
	account := d.Installation.Account.Login
	if event != "installation" {
		account, _, _ = strings.Cut(d.Repo.FullName, "/")
	}
	linked := true
	if _, err := s.link(ctx, d.Installation.ID); errors.Is(err, sql.ErrNoRows) {
		linked = false
	} else if err != nil {
		return "", err
	}
	switch {
	case linked && event == "installation":
		return "", nil
	case len(s.conf().Accounts) == 0 && !linked:
		return "installation " + itoa(d.Installation.ID) + " (GitHub account " + account + ") is not linked here and runners.accounts is empty", nil
	case account == "":
		return "the delivery names no GitHub account", nil
	}
	return s.accountRefused(account), nil
}

// dropped logs a dropped delivery, once an hour per reason at most: a
// stranger's busy repository must not flood the log.
func (s *Service) dropped(event, why string) {
	now := s.now()
	s.dropsMu.Lock()
	if s.drops == nil || len(s.drops) >= 256 {
		s.drops = map[string]time.Time{}
	}
	last, seen := s.drops[why]
	log := !seen || now.Sub(last) >= dropLogEvery
	if log {
		s.drops[why] = now
	}
	s.dropsMu.Unlock()
	if log {
		s.logf("runners: dropped a GitHub %s delivery: %s (said once an hour)", event, why)
	}
}
