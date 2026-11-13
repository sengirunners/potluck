package ghapp

// Lists the broker asks for again and again (its checking of GitHub for
// waiting jobs). Each is read page by page, each page a conditional request:
// the caller hands back the list as last answered, every page with its own
// ETag; GitHub answers 304 for a page that did not change, and a 304 costs
// none of the installation's hourly budget (GitHub's REST docs, "Use
// conditional requests if appropriate"). The caller's Budget is asked before
// every page and told what each answer cost.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// Budget is what a list may spend: Afford is asked before each page, Spend
// told after each answer whether it counted against the installation's
// hourly budget (every answer but a 304 does; an error too).
type Budget interface {
	Afford() bool
	Spend(counted bool)
}

// ErrBudget is a list stopped before a page because its Budget could not
// afford it.
var ErrBudget = errors.New("the checker's own budget for this installation is spent")

// ErrRateLow is a list stopped between pages because GitHub's budget for the
// installation is low (or it refused for a rate limit): RateLow says until
// when.
var ErrRateLow = errors.New("GitHub's API budget for this installation is nearly spent")

// Page is one page of a list as GitHub last answered it.
type Page[T any] struct {
	ETag  string
	Items []T
	Total int // total_count, as this page said it
	// Fresh: the page came with its items in the read that made this List
	// (not a 304).
	Fresh bool
}

// List is a paged list as last answered, page by page.
type List[T any] struct {
	Pages []Page[T]
	// More: the list goes on past the most pages read (its call's max); those
	// items are not read.
	More bool
}

// All is the items of every page, in order.
func (l List[T]) All() []T {
	var out []T
	for _, p := range l.Pages {
		out = append(out, p.Items...)
	}
	return out
}

// Changed reports whether any page came with new items in the read that
// made l.
func (l List[T]) Changed() bool {
	return slices.ContainsFunc(l.Pages, func(p Page[T]) bool { return p.Fresh })
}

// listIf reads a paged list: path (with its query, without per_page and
// page) as installation inst with a token for repo (owner/name) only,
// perPage items a page, at most maxPages pages. Each page
// goes with its ETag in prev. decode makes a page's answer to fill and the
// function that reads it after.
//
// On an error (ErrBudget included) the answer is the pages read so far, then
// prev's from there: each page keeps the ETag that matches its items.
func listIf[T any](ctx context.Context, c *Client, inst int64, repo string, p Perms, path string, perPage, maxPages int,
	prev List[T], b Budget, decode func() (any, func() ([]T, int))) (List[T], error) {
	var next List[T]
	rest := func(i int) List[T] {
		for _, p := range prev.Pages[min(i, len(prev.Pages)):] {
			p.Fresh = false
			next.Pages = append(next.Pages, p)
		}
		next.More = prev.More
		return next
	}
	seen := 0
	for i := 0; ; i++ {
		if i == maxPages {
			next.More = true
			return next, nil
		}
		var old Page[T]
		if i < len(prev.Pages) {
			old = prev.Pages[i]
		}
		if !b.Afford() {
			return rest(i), ErrBudget
		}
		if _, low := c.RateLow(inst); low && i > 0 { // the first page's caller has asked
			return rest(i), ErrRateLow
		}
		out, read := decode()
		cond, err := c.getIf(ctx, inst, repo, p, path+"&per_page="+strconv.Itoa(perPage)+"&page="+strconv.Itoa(i+1), old.ETag, out)
		b.Spend(err != nil || !cond.NotModified)
		if err != nil {
			return rest(i), err
		}
		pg := old
		pg.Fresh = !cond.NotModified
		if pg.Fresh {
			items, total := read()
			pg = Page[T]{Items: items, Total: total, Fresh: true}
		}
		pg.ETag = cond.ETag
		next.Pages = append(next.Pages, pg)
		if seen += len(pg.Items); len(pg.Items) < perPage || seen >= pg.Total {
			return next, nil
		}
	}
}

// cond is what a conditional request answered: the ETag to send next time,
// and whether GitHub answered 304 (nothing changed since that ETag; out is
// then left as it was).
type cond struct {
	ETag        string
	NotModified bool
}

// rateHold is how long an installation counts as limited after GitHub
// refused for a rate limit without saying until when (its docs: wait at
// least a minute).
const rateHold = time.Minute

// getIf is a conditional GET as installation inst, with a token for repo
// (owner/name) only and permissions p. A refusal for
// a rate limit leaves RateLow true until GitHub's Retry-After or reset, or
// for rateHold when it named neither.
func (c *Client) getIf(ctx context.Context, inst int64, repo string, p Perms, path, etag string, out any) (cond, error) {
	t, err := c.InstallationToken(ctx, inst, repo, p)
	if err != nil {
		return cond{}, err
	}
	var hdr http.Header
	if etag != "" {
		hdr = http.Header{"If-None-Match": {etag}}
	}
	h, status, err := callHdr(ctx, c.hc, http.MethodGet, c.api, path, t, hdr, nil, out, http.StatusOK, http.StatusNotModified)
	c.noteRate(inst, h)
	if err != nil {
		if rateLimited(err, h) {
			c.holdRate(inst, rateHold)
		}
		return cond{}, err
	}
	cd := cond{ETag: h.Get("ETag"), NotModified: status == http.StatusNotModified}
	if cd.ETag == "" {
		cd.ETag = etag
	}
	return cd, nil
}

// How much of each list is read: a page of runs is a few kilobytes each, so
// 30 keep a page well under maxBody; jobs carry their steps.
const (
	runsPerPage, runsPages = 30, 10
	jobsPerPage, jobsPages = 50, 20
)

// Runs lists repository fullName's workflow runs with status (queued,
// in_progress, waiting, ...): GET /repos/{o}/{r}/actions/runs?status=, newest
// first, up to 300.
func (c *Client) Runs(ctx context.Context, inst int64, fullName, status string, prev List[Run], b Budget) (List[Run], error) {
	if err := validRepo(fullName); err != nil {
		return prev, err
	}
	path := "/repos/" + fullName + "/actions/runs?status=" + status
	return listIf(ctx, c, inst, fullName, readPerms, path, runsPerPage, runsPages, prev, b, func() (any, func() ([]Run, int)) {
		var r struct {
			Total int       `json:"total_count"`
			Runs  []runJSON `json:"workflow_runs"`
		}
		return &r, func() ([]Run, int) {
			out := make([]Run, 0, len(r.Runs))
			for _, x := range r.Runs {
				out = append(out, x.run(fullName))
			}
			return out, r.Total
		}
	})
}

// ListedJob is a job as a run's job list answers it: with the labels it asks
// for.
type ListedJob struct {
	Job
	Labels []string
}

// RunJobs lists the jobs of a run's latest attempt: GET
// /repos/{o}/{r}/actions/runs/{id}/jobs?filter=latest, up to 1,000.
func (c *Client) RunJobs(ctx context.Context, inst int64, fullName string, runID int64, prev List[ListedJob], b Budget) (List[ListedJob], error) {
	if err := validRepo(fullName); err != nil {
		return prev, err
	}
	path := "/repos/" + fullName + "/actions/runs/" + strconv.FormatInt(runID, 10) + "/jobs?filter=latest"
	return listIf(ctx, c, inst, fullName, readPerms, path, jobsPerPage, jobsPages, prev, b, func() (any, func() ([]ListedJob, int)) {
		var r struct {
			Total int       `json:"total_count"`
			Jobs  []jobJSON `json:"jobs"`
		}
		return &r, func() ([]ListedJob, int) {
			out := make([]ListedJob, 0, len(r.Jobs))
			for _, x := range r.Jobs {
				out = append(out, ListedJob{x.job(), x.Labels})
			}
			return out, r.Total
		}
	})
}
