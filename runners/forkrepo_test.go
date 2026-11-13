package runners_test

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// A repository that is itself a GitHub fork (alice/widget, a fork of
// CapSoftware/Cap) is served for its own runs: GitHub answers each of them
// with head_repository.fork true, and the broker asks whether the head
// repository is the repository, not whether it is a fork. A pull request
// from any other repository into it (a fork of it, its own upstream, one
// whose head is gone), and the events outsiders cause, are still refused,
// as they are for a repository that is not a fork. allow_fork_prs is off
// throughout.
func TestForkedRepoServesItsOwnRuns(t *testing.T) {
	f := newFixture(t)
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	head := func(s string) *string { return &s }
	type row struct {
		name   string
		event  string
		head   *string // nil: the repository itself
		fork   bool    // the head is "stranger/app", a fork
		served bool
		why    string // a refusal's recorded reason holds this
	}
	rows := []row{
		{"a push", "push", nil, false, true, ""},
		{"a manual run", "workflow_dispatch", nil, false, true, ""},
		{"a schedule", "schedule", nil, false, true, ""},
		{"a pull request from its own branch", "pull_request", nil, false, true, ""},
		{"a pull request from a fork of it", "pull_request", nil, true, false, "from a fork"},
		{"a pull request from another repository, not a fork", "pull_request", head("upstream/app"), false, false, "from a fork"},
		{"a pull request whose head repository is gone", "pull_request", head(""), false, false, "from a fork"},
		{"a pull_request_target run from its own branch", "pull_request_target", nil, false, false, "pull_request_target run"},
		{"an issue_comment run", "issue_comment", nil, false, false, "issue_comment run"},
	}
	type cas struct {
		repo string
		row
		job, run int64
	}
	var cases []cas
	n := int64(0)
	for _, repo := range []string{"acme/app", "acme/lib"} { // acme/app is a fork; acme/lib is not
		for _, r := range rows {
			n++
			cases = append(cases, cas{repo, r, 4000 + n, 400 + n})
		}
	}
	f.gh.mu.Lock()
	f.gh.repoFork["acme/app"] = true
	for _, c := range cases {
		f.gh.events[c.run] = c.event
		f.gh.forks[c.run] = c.fork
		if c.head != nil {
			f.gh.heads[c.run] = c.head
		}
	}
	f.gh.mu.Unlock()
	deliver := func(c cas) {
		t.Helper()
		if code := f.deliver(t, "workflow_job", jobEvent("queued", c.job, c.run, c.repo, true, "self-hosted", "sengi")); code != http.StatusAccepted {
			t.Fatalf("%s in %s: delivery %d", c.name, c.repo, code)
		}
	}
	// The forked repository's push first, alone: it takes the fixture's one
	// box, guarded as the broker guards any box (the job's repository and the
	// trusted events), nothing relaxed for being a fork. The other served
	// jobs wait for a box.
	deliver(cases[0])
	eventually(t, "a runner for the forked repository's push", func() bool {
		minted, _ := f.gh.snapshot()
		for _, m := range minted {
			if m["name"] == fmt.Sprintf("potluck-%d", cases[0].job) && m["scope"] == "repos/acme/app" {
				return true
			}
		}
		return false
	})
	guard := `"jit_guard":{"repo":"acme/app","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"]}`
	eventually(t, "the forked repository's box guarded", func() bool {
		b, _ := os.ReadFile(f.seen)
		return strings.Contains(string(b), guard)
	})
	for _, c := range cases[1:] {
		deliver(c)
	}
	// Deliveries apply in order, one lane, and the last row is a refusal: once
	// it is ignored every served row before it has been through trusted, so
	// a served row still not ignored was not refused. Keep a refused row last.
	if c := cases[len(cases)-1]; c.served {
		t.Fatal("the last row must be a refusal (see above)")
	}
	eventually(t, "every job recorded, every refusal ignored", func() bool {
		js := f.jobs(t)
		for _, c := range cases {
			j, ok := js[c.job]
			if !ok || (!c.served && j.State != "ignored") {
				return false
			}
		}
		return true
	})
	js := f.jobs(t)
	for _, c := range cases {
		j := js[c.job]
		switch {
		case c.served && j.State == "ignored":
			t.Errorf("%s in %s: refused (%s), want served", c.name, c.repo, j.Why)
		case !c.served && !strings.Contains(j.Why, c.why):
			t.Errorf("%s in %s: why %q, want it to say %q", c.name, c.repo, j.Why, c.why)
		}
	}
}
