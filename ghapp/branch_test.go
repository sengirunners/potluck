package ghapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const sha40 = "0123456789abcdef0123456789abcdef01234567"

// OnBranch reads GitHub's compare of the branch with the commit: identical
// (the tip) and behind are on the branch, ahead and diverged are not, and an answer
// it does not know is an error, never a yes. It asks with a token of its
// own (contents read), and refuses a branch or commit id it cannot pass on
// as they are before calling anything.
func TestOnBranch(t *testing.T) {
	var status string
	var paths []string
	c, f, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/repos/acme/web/compare/main..."+sha40 {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"status":%q}`, status)
	})
	ctx := context.Background()
	for _, tc := range []struct {
		status  string
		want    bool
		wantTip bool
		err     bool
	}{
		{"identical", true, true, false},
		{"behind", true, false, false},
		{"ahead", false, false, false},
		{"diverged", false, false, false},
		{"", false, false, true},
		{"IDENTICAL", false, false, true},
		{"unknown", false, false, true},
	} {
		status = tc.status
		on, tip, err := c.OnBranch(ctx, 5, "acme/web", "main", sha40)
		if on != tc.want || tip != tc.wantTip || (err != nil) != tc.err {
			t.Errorf("status %q: on %v tip %v err %v, want on %v tip %v err %v", tc.status, on, tip, err, tc.want, tc.wantTip, tc.err)
		}
	}
	f.mu.Lock()
	mints := append([]string(nil), f.mint...)
	f.mu.Unlock()
	if want := `{"permissions":{"contents":"read","metadata":"read"},"repositories":["web"]}`; len(mints) != 1 || mints[0] != want {
		t.Fatalf("compare minted with %q, want once %s", mints, want)
	}

	paths = nil
	for _, bad := range []struct{ repo, branch, sha string }{
		{"acme/web", "main/../x", sha40},
		{"acme/web", "main?x", sha40},
		{"acme/web", "", sha40},
		{"acme/web", "main", "abc"},
		{"acme/web", "main", strings.ToUpper(sha40)},
		{"acme/web", "main", sha40 + "0"},
		{"acme/../x", "main", sha40},
	} {
		if on, _, err := c.OnBranch(ctx, 5, bad.repo, bad.branch, bad.sha); on || err == nil {
			t.Errorf("%+v: on %v err %v, want refused", bad, on, err)
		}
	}
	f.mu.Lock()
	n := len(f.mint)
	f.mu.Unlock()
	if len(paths) != 0 || n != 1 {
		t.Fatalf("a refused compare called GitHub: %q, %d mints", paths, n)
	}
}

// Run reads head_branch and head_repository.id, and CallsOwnOnly from
// referenced_workflows: true with none, or every one a file of the run's own
// repository at the run's head commit; false for another repository's
// workflow, another commit, a missing commit or path, or a run without a
// head commit.
func TestRunHeadAndReferencedWorkflows(t *testing.T) {
	ctx := context.Background()
	own := func(sha string) string {
		return fmt.Sprintf(`{"path":"acme/web/.github/workflows/build.yml@%s","sha":%q,"ref":"refs/heads/main"}`, sha, sha)
	}
	for _, tc := range []struct {
		name, refs, head string
		want             bool
	}{
		{"none", `[]`, sha40, true},
		{"absent", ``, sha40, true},
		{"own at head", `[` + own(sha40) + `]`, sha40, true},
		{"own, case of the owner", `[{"path":"ACME/web/.github/workflows/b.yml@x","sha":"` + sha40 + `"}]`, sha40, true},
		{"another repository", `[{"path":"evil/web/.github/workflows/b.yml@main","sha":"` + sha40 + `"}]`, sha40, false},
		{"a repository named like a prefix", `[{"path":"acme/web2/.github/workflows/b.yml@main","sha":"` + sha40 + `"}]`, sha40, false},
		{"another commit", `[` + own(strings.Repeat("f", 40)) + `]`, sha40, false},
		{"no commit", `[{"path":"acme/web/.github/workflows/b.yml@main"}]`, sha40, false},
		{"no path", `[{"sha":"` + sha40 + `"}]`, sha40, false},
		{"outside workflows", `[{"path":"acme/web/.github/actions/b.yml@main","sha":"` + sha40 + `"}]`, sha40, false},
		{"escaping workflows", `[{"path":"acme/web/.github/workflows/../../x/b.yml@main","sha":"` + sha40 + `"}]`, sha40, false},
		{"own and another", `[` + own(sha40) + `,{"path":"evil/x/.github/workflows/b.yml@main","sha":"` + sha40 + `"}]`, sha40, false},
		{"a run without a head commit", `[` + own(sha40) + `]`, "", false},
		{"not an object", `["acme/web/.github/workflows/b.yml"]`, sha40, false},
	} {
		c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
			refs := ""
			if tc.refs != "" {
				refs = `,"referenced_workflows":` + tc.refs
			}
			fmt.Fprintf(w, `{"id":9,"event":"push","status":"queued","run_attempt":1,"path":".github/workflows/ci.yml","head_sha":%q,"head_branch":"main",`+
				`"head_repository":{"id":77,"full_name":"acme/web"},"repository":{"id":77,"full_name":"acme/web"}%s}`, tc.head, refs)
		})
		run, err := c.Run(ctx, 5, "acme/web", 9)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if run.CallsOwnOnly != tc.want {
			t.Errorf("%s: CallsOwnOnly %v, want %v", tc.name, run.CallsOwnOnly, tc.want)
		}
		if run.HeadBranch != "main" || run.HeadRepoID != 77 || run.RepoID != 77 || run.HeadSHA != tc.head {
			t.Errorf("%s: %+v", tc.name, run)
		}
	}
}

// A head repository with no id reads as 0; Repo reads default_branch.
func TestRunNoHeadIDAndRepoDefaultBranch(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/web":
			fmt.Fprint(w, `{"id":77,"full_name":"acme/web","private":true,"visibility":"private","default_branch":"trunk"}`)
		case "/repos/acme/web/actions/runs/9":
			fmt.Fprint(w, `{"id":9,"event":"push","head_branch":"v1","head_repository":{"full_name":"acme/web"},"repository":{"id":77,"full_name":"acme/web"}}`)
		default:
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	repo, err := c.Repo(ctx, 5, "acme/web")
	if err != nil || repo.DefaultBranch != "trunk" || repo.ID != 77 {
		t.Fatalf("%v %+v", err, repo)
	}
	run, err := c.Run(ctx, 5, "acme/web", 9)
	if err != nil || run.HeadRepoID != 0 || run.HeadBranch != "v1" || !run.CallsOwnOnly {
		t.Fatalf("%v %+v", err, run)
	}
}

// TagExists reads GitHub's answer for the exact ref tags/<name>: 200 is a
// tag, 404 is none, anything else is an error, never a no. It asks with a
// token of its own (contents read), and refuses a name it cannot pass on as
// it is before calling anything.
func TestTagExists(t *testing.T) {
	var code int
	var paths []string
	c, f, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/repos/acme/web/git/ref/tags/main" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(code)
		if code == 200 {
			fmt.Fprintf(w, `{"ref":"refs/tags/main","object":{"sha":%q,"type":"commit"}}`, sha40)
		} else {
			fmt.Fprint(w, `{"message":"x"}`)
		}
	})
	ctx := context.Background()
	for _, tc := range []struct {
		code int
		want bool
		err  bool
	}{
		{200, true, false},
		{404, false, false},
		{500, false, true},
		{403, false, true},
	} {
		code = tc.code
		tag, err := c.TagExists(ctx, 5, "acme/web", "main")
		if tag != tc.want || (err != nil) != tc.err {
			t.Errorf("GitHub %d: tag %v err %v, want tag %v err %v", tc.code, tag, err, tc.want, tc.err)
		}
	}
	f.mu.Lock()
	mints := append([]string(nil), f.mint...)
	f.mu.Unlock()
	if want := `{"permissions":{"contents":"read","metadata":"read"},"repositories":["web"]}`; len(mints) == 0 || mints[0] != want {
		t.Fatalf("tag read minted with %q, want %s", mints, want)
	}
	for _, m := range mints {
		if m != mints[0] {
			t.Fatalf("tag read minted with %q, want only %s", mints, mints[0])
		}
	}

	paths = nil
	for _, bad := range []struct{ repo, name string }{
		{"acme/web", "main/../x"},
		{"acme/web", "main?x"},
		{"acme/web", "main#x"},
		{"acme/web", ""},
		{"acme/web", strings.Repeat("a", 201)},
		{"acme/../x", "main"},
	} {
		if tag, err := c.TagExists(ctx, 5, bad.repo, bad.name); tag || err == nil {
			t.Errorf("%+v: tag %v err %v, want refused", bad, tag, err)
		}
	}
	if len(paths) != 0 {
		t.Fatalf("a refused name called GitHub: %q", paths)
	}
}
