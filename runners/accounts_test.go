package runners_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// strangerJob is a workflow_job delivery of installation id on repo, labelled
// for the fixture's pool: GitHub signs it with the App's secret like any.
func strangerJob(id int64, repo string, job, run int64) []byte {
	b, _ := json.Marshal(map[string]any{"action": "queued", "installation": map[string]any{"id": id},
		"repository":   map[string]any{"id": repoID(repo), "full_name": repo, "private": true},
		"workflow_job": map[string]any{"id": job, "run_id": run, "labels": []string{"sengi"}}})
	return b
}

func installationEvent(action string, id int64, account string) []byte {
	b, _ := json.Marshal(map[string]any{"action": action, "installation": map[string]any{"id": id, "account": map[string]any{"login": account}}})
	return b
}

// dropsUnstored delivers each of a stranger's deliveries and wants it
// answered 202 and never stored: no inbox row, no job row.
func dropsUnstored(t *testing.T, f *fixture, what string, ds ...func() (string, []byte)) {
	t.Helper()
	ctx := context.Background()
	before := f.svc.Stored(ctx)
	for _, d := range ds {
		event, body := d()
		if code := f.deliver(t, event, body); code != http.StatusAccepted {
			t.Fatalf("%s: %s answered %d, want 202", what, event, code)
		}
	}
	if n := f.svc.Stored(ctx); n != before {
		t.Fatalf("%s: %d deliveries stored, want none", what, n-before)
	}
	for id, j := range f.jobs(t) {
		if strings.HasPrefix(j.Repo, "mallory/") || id >= 9900 {
			t.Fatalf("%s: a job row for %s (%d)", what, j.Repo, id)
		}
	}
}

// The App is public-installable (GitHub offers an App to other accounts only
// then), so any account can install it and GitHub sends that installation's
// deliveries here with a good signature. With runners.accounts empty only the
// installations linked here are heard; with it set, only its accounts', and
// only they can be linked. A dropped delivery is answered 202 and never
// stored, so it leaves no job row for the doctor to probe.
func TestStrangersDeliveriesAreDropped(t *testing.T) {
	ctx := context.Background()
	stranger := []func() (string, []byte){
		func() (string, []byte) { return "installation", installationEvent("created", 99, "mallory") },
		func() (string, []byte) { return "workflow_job", strangerJob(99, "mallory/app", 9901, 991) },
		func() (string, []byte) { return "installation", installationEvent("deleted", 99, "mallory") },
	}

	t.Run("no accounts: the linked installations", func(t *testing.T) {
		f := newFixture(t)
		dropsUnstored(t, f, "before any link", append(stranger,
			func() (string, []byte) { return "workflow_job", strangerJob(inst, "acme/app", 9902, 992) })...)
		if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
			t.Fatalf("link: %d %s", w.Code, w.Body)
		}
		dropsUnstored(t, f, "after acme's link", stranger...)
		f.deliver(t, "workflow_job", jobEvent("queued", 5101, 510, "acme/app", true, "sengi"))
		eventually(t, "the linked installation's job served", func() bool { return f.jobs(t)[5101].State == "serving" })
	})

	t.Run("accounts named: those only, for links too", func(t *testing.T) {
		f := newFixtureWith(t, func(r *config.Runners) { r.Accounts = []string{" ACME ", "sengirunners"} })
		if w := f.link(t, inst, "good"); w.Code != http.StatusOK { // listed, whatever its case
			t.Fatalf("link: %d %s", w.Code, w.Body)
		}
		dropsUnstored(t, f, "a stranger", stranger...)
		f.deliver(t, "workflow_job", jobEvent("queued", 5102, 511, "acme/app", true, "sengi"))
		eventually(t, "a listed account's job served", func() bool { return f.jobs(t)[5102].State == "serving" })

		// Taken off the list: its linked installation's jobs are dropped,
		// its installation events still heard (they can only unlink it),
		// and it cannot be linked again.
		f.restart(t, func(c *config.Config) { c.File.Runners.Accounts = []string{"sengirunners"} })
		dropsUnstored(t, f, "an account taken off the list",
			func() (string, []byte) {
				return "workflow_job", jobEvent("queued", 9903, 512, "acme/app", true, "sengi")
			})
		before := f.svc.Stored(ctx)
		f.deliver(t, "installation", installationEvent("deleted", inst, "acme"))
		if f.svc.Stored(ctx) != before+1 {
			t.Fatal("a linked installation's deleted event was not stored")
		}
		eventually(t, "the deleted installation unlinked", func() bool {
			var out struct {
				Links []json.RawMessage `json:"links"`
			}
			return f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &out) == nil && len(out.Links) == 0
		})
		err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{"installation_id": inst}, nil)
		if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "runners.accounts") {
			t.Fatalf("link of an unlisted account: %v, want forbidden naming runners.accounts", err)
		}
	})
}
