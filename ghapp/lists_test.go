package ghapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// purse is a Budget of n counted calls.
type purse struct{ n, spent int }

func (p *purse) Afford() bool { return p.n > p.spent }
func (p *purse) Spend(counted bool) {
	if counted {
		p.spent++
	}
}

func plenty() *purse { return &purse{n: 1 << 30} }

// The lists are conditional: each page's ETag goes back as If-None-Match, a
// 304 keeps the page as it was (not counted, not Fresh), and a changed answer
// brings its new ETag. Every list's token names its one repository: none is
// minted for the whole installation.
func TestConditionalLists(t *testing.T) {
	c, f, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		tag := `W/"v1"`
		if r.Header.Get("If-None-Match") == tag {
			w.Header().Set("ETag", tag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", tag)
		switch r.URL.Path {
		case "/repos/acme/web/actions/runs":
			if r.URL.Query().Get("status") != "queued" {
				t.Errorf("runs asked with %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"total_count":1,"workflow_runs":[{"id":9,"event":"push","status":"queued","updated_at":"2026-11-06T12:00:00Z",
				"path":".github/workflows/ci.yml","head_sha":"abc","repository":{"id":77,"full_name":"acme/web","private":true}}]}`)
		case "/repos/acme/web/actions/runs/9/jobs":
			if r.URL.Query().Get("filter") != "latest" {
				t.Errorf("jobs asked with %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"total_count":1,"jobs":[{"id":3,"status":"queued","labels":["self-hosted","pool"],"run_id":9,"run_attempt":1,
				"name":"build","started_at":"2026-11-06T12:01:00Z"}]}`)
		default:
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	b := plenty()

	runs, err := c.Runs(ctx, 5, "acme/web", "queued", List[Run]{}, b)
	want := Run{ID: 9, Event: "push", Status: "queued", Repo: "acme/web", RepoID: 77, Private: true, Updated: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		Path: ".github/workflows/ci.yml", HeadSHA: "abc", CallsOwnOnly: true}
	if err != nil || len(runs.All()) != 1 || runs.All()[0] != want {
		t.Fatalf("runs: %v %+v", err, runs)
	}
	if again, err := c.Runs(ctx, 5, "acme/web", "queued", runs, b); err != nil || again.Changed() || !slices.Equal(again.All(), runs.All()) {
		t.Fatalf("runs again: %v %+v", err, again)
	}
	jobs, err := c.RunJobs(ctx, 5, "acme/web", 9, List[ListedJob]{}, b)
	if all := jobs.All(); err != nil || len(all) != 1 || all[0].Job != (Job{ID: 3, Status: "queued", RunID: 9, RunAttempt: 1, Name: "build", StartedAt: "2026-11-06T12:01:00Z"}) ||
		!slices.Equal(all[0].Labels, []string{"self-hosted", "pool"}) {
		t.Fatalf("jobs: %v %+v", err, jobs)
	}
	if _, err := c.Runs(ctx, 5, "acme/../x", "queued", List[Run]{}, b); err == nil {
		t.Error("a bad repository name was asked about")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mint) != 1 {
		t.Fatalf("tokens minted: %q, want one, for acme/web", f.mint)
	}
	if !strings.Contains(f.mint[0], `"repositories":["web"]`) || !strings.Contains(f.mint[0], `"actions":"read"`) {
		t.Errorf("the run list's token: %s", f.mint[0])
	}
}

// A list longer than a page is read page by page, each with its own ETag:
// a 304 on page one does not hide a change on page two. Every counted page
// costs the Budget one; one it cannot afford stops the list (ErrBudget) with
// the pages read kept. Past the most pages the list says More.
func TestListsPageByPage(t *testing.T) {
	jobs := 120 // three pages of 50
	gen := map[int]int{}
	c, f, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Query().Get("per_page") != "50" {
			t.Errorf("jobs asked with %q", r.URL.RawQuery)
		}
		tag := fmt.Sprintf(`"p%d-g%d"`, page, gen[page])
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", tag)
		var js []string
		for i := (page-1)*50 + 1; i <= min(page*50, jobs); i++ {
			st := "in_progress"
			if i == 101 || gen[page] > 0 && i == 51 {
				st = "queued"
			}
			js = append(js, fmt.Sprintf(`{"id":%d,"status":%q}`, i, st))
		}
		fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, jobs, strings.Join(js, ","))
	})
	ctx := context.Background()
	queued := func(l List[ListedJob]) (ids []int64) {
		for _, j := range l.All() {
			if j.Status == "queued" {
				ids = append(ids, j.ID)
			}
		}
		return ids
	}
	b := plenty()
	l, err := c.RunJobs(ctx, 5, "acme/web", 9, List[ListedJob]{}, b)
	if err != nil || len(l.All()) != 120 || len(l.Pages) != 3 || l.More || !slices.Equal(queued(l), []int64{101}) || b.spent != 3 {
		t.Fatalf("all pages: %v, %d jobs, %d pages, queued %v, spent %d", err, len(l.All()), len(l.Pages), queued(l), b.spent)
	}
	f.mu.Lock()
	gen[2]++ // job 51 queues: page two alone changes
	f.mu.Unlock()
	l, err = c.RunJobs(ctx, 5, "acme/web", 9, l, b)
	if err != nil || !l.Changed() || l.Pages[0].Fresh || !l.Pages[1].Fresh || l.Pages[2].Fresh || !slices.Equal(queued(l), []int64{51, 101}) || b.spent != 4 {
		t.Fatalf("page two changed: %v, queued %v, spent %d", err, queued(l), b.spent)
	}

	f.mu.Lock()
	gen[1]++
	gen[2]++
	f.mu.Unlock()
	short := &purse{n: 1}
	cut, err := c.RunJobs(ctx, 5, "acme/web", 9, l, short)
	if !errors.Is(err, ErrBudget) || short.spent != 1 || len(cut.Pages) != 3 || cut.Pages[1].ETag != l.Pages[1].ETag || cut.Pages[0].ETag == l.Pages[0].ETag {
		t.Fatalf("budget for one page: %v, spent %d, %+v", err, short.spent, cut.Pages)
	}

	f.mu.Lock()
	jobs = 50*jobsPages + 1
	f.mu.Unlock()
	l, err = c.RunJobs(ctx, 5, "acme/web", 9, List[ListedJob]{}, plenty())
	if err != nil || !l.More || len(l.All()) != 50*jobsPages {
		t.Fatalf("past the most pages: %v, More %v, %d jobs", err, l.More, len(l.All()))
	}
}

// A page over the read limit fails as such, and a refusal for a rate limit
// with no Retry-After leaves the installation's budget low for a minute.
func TestListFailures(t *testing.T) {
	var answer func(w http.ResponseWriter)
	c, f, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { answer(w) })
	ctx := context.Background()

	answer = func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":1,"status":"queued","name":%q}]}`, strings.Repeat("x", maxBody))
	}
	if _, err := c.RunJobs(ctx, 5, "acme/web", 9, List[ListedJob]{}, plenty()); err == nil || !strings.Contains(err.Error(), "over 1 MiB") {
		t.Errorf("a page over the limit: %v", err)
	}

	answer = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	}
	if _, low := c.RateLow(5); low {
		t.Fatal("low before any refusal")
	}
	if _, err := c.Runs(ctx, 5, "acme/web", "queued", List[Run]{}, plenty()); err == nil {
		t.Fatal("a refusal answered no error")
	}
	if until, low := c.RateLow(5); !low || !until.Equal(t0.Add(rateHold)) {
		t.Errorf("after a secondary limit: low %v until %v", low, until)
	}
	f.mu.Lock()
	f.now = f.now.Add(rateHold)
	f.mu.Unlock()
	if _, low := c.RateLow(5); low {
		t.Error("still low after the hold")
	}
}
