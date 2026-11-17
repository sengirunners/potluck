package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/internal/forkguard"
	"github.com/sengirunners/potluck/internal/workflowlint"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/runners"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const ciPRHelp = `Change one job in a repository's workflow so the repository's own branches run it on the Mac
this machine lends, and prepare that change as a pull request.

The job's runs-on becomes a fork-guarded value: a pull request from a fork keeps the GitHub-hosted
runner the job uses today (macos-15, say); pushes, dispatches and pull requests from the repository's
own branches ask for this machine's pool (self-hosted, macos and the pool's label). Nothing else in
the file changes: comments, quoting and line endings stay as they are. The diff is printed first.

Without --job it takes the one job that runs on a GitHub-hosted macOS label (in every workflow, or the
one --workflow names); with none or several it says which and asks for --job. With --json it prints one
object (state done, merge, script or dry_run, and the url, script, next and diff that go with it).

It refuses a job that does not exist, calls a reusable workflow, or whose runs-on is not one
GitHub-hosted label (a matrix, another expression, a runner group, several labels). A job that
already has this change is reported as done.

The workflow is read with your gh sign-in. The GitHub App this machine uses cannot write workflow
files or open pull requests (it has Contents: read only), so the command writes the edited file and a
short script that opens the pull request with your gh sign-in, and prints the script; running it is
the next step. With --dry-run it prints the diff only.

See docs/ci-pr.md.`

func (a *app) ciPRCmd() *cobra.Command {
	var job, workflow, labels, size, osName string
	var dryRun bool
	c := &cobra.Command{
		Use:   "pr <owner/repo> [--job <id>]",
		Short: "Change one job's runs-on so own branches run on the lent Mac, as a pull request",
		Long:  ciPRHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			if !jobrule.ValidRepo(repo) {
				return fmt.Errorf("repository must be owner/repo, got %q", repo)
			}
			own, err := a.ciPRLabels(cmd.Context(), labels, size, osName)
			if err != nil {
				return err
			}
			rerun := a.cmd("ci pr " + repo)
			cmd.Flags().Visit(func(f *pflag.Flag) {
				if f.Name != "dry-run" && f.Name != "workflow" && cmd.InheritedFlags().Lookup(f.Name) == nil { // not --json and the like
					rerun += " --" + f.Name + " " + shellQuote(f.Value.String())
				}
			})
			res, err := a.ciPR(cmd.Context(), repo, job, workflow, own, dryRun, rerun)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(res)
			}
			return nil
		},
	}
	c.Flags().StringVar(&job, "job", "", "the job id (its key under jobs:) to move (default: the one job that runs on a GitHub-hosted macOS label)")
	c.Flags().StringVar(&workflow, "workflow", "", "the workflow file (name in .github/workflows, or its path); default: the one file that has the job")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the diff only")
	c.Flags().StringVar(&labels, "labels", "", "the labels own branches ask for, comma-separated (default: self-hosted, the OS and this machine's pool label)")
	c.Flags().StringVar(&size, "size", "", "a pool size to ask for (the label becomes <pool label>:<size>)")
	c.Flags().StringVar(&osName, "os", "macos", "the OS label own branches ask for")
	return c
}

// ciPRLabels is the labels own branches will ask for: --labels as given, else self-hosted, the OS and this machine's
// pool label, which the broker's own label rule (runners.ParseLabels) must accept.
func (a *app) ciPRLabels(ctx context.Context, labels, size, osName string) ([]string, error) {
	if labels != "" {
		var out []string
		for _, l := range strings.Split(labels, ",") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		if !slices.ContainsFunc(out, func(l string) bool { return slices.Contains([]string{"linux", "macos", "windows"}, strings.ToLower(l)) }) {
			return nil, errors.New("--labels must name the OS (macos, linux or windows): the broker places a job that names none on linux")
		}
		return out, nil
	}
	if osName == "" {
		return nil, errors.New("--os must name the OS (macos, linux or windows)")
	}
	st, err := a.poolSetup(ctx)
	if err != nil {
		return nil, err
	}
	if st.Label == "" {
		return nil, errors.New("this machine lends to no pool yet: " + a.cmd("runners setup <pool>") + " first, or pass --labels")
	}
	pool := strings.ToLower(st.Label)
	if size != "" {
		pool += ":" + size
	}
	out := []string{"self-hosted", osName, pool}
	if ask := runners.ParseLabels(out, st); !ask.Ours || ask.Why != "" {
		return nil, fmt.Errorf("the pool would not take a job asking for %s: %s", strings.Join(out, ", "), ask.Why)
	}
	return out, nil
}

// poolSetup is this machine's pool setup, as its daemon answers it.
func (a *app) poolSetup(ctx context.Context) (runners.Setup, error) {
	cl, _, err := a.client()
	if err != nil {
		return runners.Setup{}, err
	}
	st := runners.Setup{}
	return st, cl.Do(ctx, "GET", "/v0/runners/setup", nil, &st)
}

// ghContent is a file as GitHub's contents API answers it.
type ghContent struct {
	Name, Path, Type, SHA string
	Encoding, Content     string
}

// ciPR does the work of potluck ci pr; rerun is the command line without --dry-run, --workflow and the global flags. It prints what a
// person reads (nothing with --json: the caller prints the result) and answers the result.
func (a *app) ciPR(ctx context.Context, repo, job, workflow string, own []string, dryRun bool, rerun string) (ciPRResult, error) {
	res := ciPRResult{Repo: repo, Labels: own}
	say := a.say
	if a.asJSON() {
		say = func(string, ...any) {}
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := ghGet(ctx, "repos/"+repo, &info); err != nil {
		return res, err
	}
	if info.DefaultBranch == "" {
		return res, fmt.Errorf("GitHub did not say %s's default branch", repo)
	}
	ref := "?ref=" + url.QueryEscape(info.DefaultBranch)

	var files []string
	if workflow != "" {
		p := workflow
		if !strings.Contains(p, "/") {
			p = ".github/workflows/" + p
		}
		if !jobrule.ValidWorkflow(p) {
			return res, fmt.Errorf("--workflow must name a .yml or .yaml file directly in .github/workflows (letters, digits, '.', '_', '-'), got %q", workflow)
		}
		files = []string{p}
	} else {
		var list []ghContent
		if err := ghGet(ctx, "repos/"+repo+"/contents/.github/workflows"+ref, &list); err != nil {
			return res, err
		}
		for _, f := range list {
			if f.Type != "file" || !(strings.HasSuffix(f.Name, ".yml") || strings.HasSuffix(f.Name, ".yaml")) {
				continue
			}
			if !jobrule.ValidWorkflow(f.Path) {
				res.note(say, "note: skipped %q: %s edits only files named with letters, digits, '.', '_' and '-'", f.Path, a.cmd("ci pr"))
				continue
			}
			files = append(files, f.Path)
		}
	}

	type source struct {
		file ghContent
		src  []byte
	}
	var sources []source
	for _, p := range files {
		var f ghContent
		if err := ghGet(ctx, "repos/"+repo+"/contents/"+escapePath(p)+ref, &f); err != nil {
			return res, err
		}
		if f.Encoding != "base64" {
			return res, fmt.Errorf("GitHub sent %s in %q encoding, not base64", p, f.Encoding)
		}
		src, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			return res, fmt.Errorf("%s: %v", p, err)
		}
		f.Path = p
		sources = append(sources, source{f, src})
	}
	if job == "" {
		var found []string
		var pickedFrom source
		for _, s := range sources {
			for _, j := range forkguard.MacJobs(s.src) {
				found = append(found, j+" ("+s.file.Path+")")
				job, pickedFrom = j, s
			}
		}
		switch {
		case len(found) == 0:
			var names []string
			for _, s := range sources {
				names = append(names, s.file.Path)
			}
			if len(names) == 0 {
				names = []string{"no workflow files"}
			}
			return res, fmt.Errorf("no job in %s at %s (%s) runs on a GitHub-hosted macOS label; name the job with --job <id>", repo, info.DefaultBranch, strings.Join(names, ", "))
		case len(found) > 1:
			return res, fmt.Errorf("%d jobs in %s run on a GitHub-hosted macOS label (%s); name one with --job <id>", len(found), repo, strings.Join(found, ", "))
		}
		// Another workflow may have a job of the same id on another runner; the picked one is in this file.
		sources = []source{pickedFrom}
		rerun += " --job " + shellQuote(job)
	}
	res.Job = job

	type found struct {
		source
		c   forkguard.Change
		err error
	}
	var hits []found
	for _, s := range sources {
		c, err := forkguard.Apply(s.src, job, own)
		if errors.Is(err, forkguard.ErrNoJob) && workflow == "" {
			continue
		}
		hits = append(hits, found{s, c, err})
	}
	switch {
	case len(hits) == 0:
		return res, fmt.Errorf("no workflow in %s at %s has a job %q; name the file with --workflow", repo, info.DefaultBranch, job)
	case len(hits) > 1:
		var names []string
		for _, h := range hits {
			names = append(names, h.file.Path)
		}
		return res, fmt.Errorf("job %q is in several workflows (%s); name one with --workflow", job, strings.Join(names, ", "))
	}
	h := hits[0]
	res.Workflow = h.file.Path
	if h.err != nil {
		return res, fmt.Errorf("%s: %v", h.file.Path, h.err)
	}
	if !h.c.Changed {
		say("Job %s in %s already asks for %s on %s's own branches; pull requests from forks stay on GitHub.", job, h.file.Path, strings.Join(own, ", "), repo)
		say("done")
		res.State = ciPRDone
		return res, nil
	}
	// The lint must find nothing new in the edited file; what it found before is the file's own, and only noted.
	had := workflowlint.Lint(string(h.src))
	for _, p := range workflowlint.Lint(string(h.c.Text)) {
		if !slices.Contains(had, p) {
			return res, fmt.Errorf("%s: the fork-safe lint refuses the edited file (%s); write job %s's runs-on as `runs-on: <label>` in a plainly laid-out file, or edit it by hand", h.file.Path, p, job)
		}
	}
	for _, p := range had {
		res.note(say, "note: the fork-safe lint (docs/ci-pr.md) also reports, before and after this change: %s", p)
	}

	g, _ := forkguard.Parse(h.c.After)
	say("Job %s in %s: own branches will ask for %s (this machine's pool); pull requests from forks stay on GitHub's %s.", job, h.file.Path, strings.Join(own, ", "), g.Fork)
	res.Diff = unifiedDiff(h.file.Path, h.src, h.c.Text)
	if !a.asJSON() {
		fmt.Fprint(a.host.Stdout, res.Diff)
	}
	if dryRun {
		res.State = ciPRDryRun
		res.Next = fmt.Sprintf("%s --workflow %s", rerun, shellQuote(path.Base(h.file.Path)))
		say("Dry run: nothing written.")
		say("next: %s", res.Next)
		return res, nil
	}

	branch, open, err := ciPRBranch(ctx, repo, job)
	if err != nil {
		return res, err
	}
	if open != "" {
		res.State, res.URL = ciPRMerge, open
		say("A pull request with this change is already open: %s", open)
		say("next: merge %s", open)
		return res, nil
	}
	var base struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := ghGet(ctx, "repos/"+repo+"/git/ref/heads/"+escapePath(info.DefaultBranch), &base); err != nil {
		return res, err
	}
	if base.Object.SHA == "" {
		return res, fmt.Errorf("GitHub did not say %s's head commit", info.DefaultBranch)
	}
	script, err := writePRScript(repo, info.DefaultBranch, base.Object.SHA, branch, job, g.Fork, own, h.file, h.c.Text, a.cmd("ci pr"))
	if err != nil {
		return res, err
	}
	b, err := os.ReadFile(script)
	if err != nil {
		return res, err
	}
	res.State, res.Script, res.Next = ciPRScript, script, "sh "+shellQuote(script)
	say("The GitHub App this machine uses can read workflows but not change them or open pull requests, so the pull request is opened with your own gh sign-in. This script does it (pushing a workflow file needs gh's workflow scope: gh auth refresh -s workflow):")
	if !a.asJSON() {
		fmt.Fprint(a.host.Stdout, string(b))
	}
	say("next: %s", res.Next)
	return res, nil
}

// ciPRBranch is the branch the change goes on, potluck/runs-on-<job>, and the URL of an open pull request from it
// when there is one. A branch of that name with no open pull request is left alone: the next free -2, -3 ... is used.
func ciPRBranch(ctx context.Context, repo, job string) (branch, open string, err error) {
	owner := strings.SplitN(repo, "/", 2)[0]
	stem := "potluck/runs-on-" + strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '-'
	}, job)
	for i := 1; i <= 9; i++ {
		b := stem
		if i > 1 {
			b = fmt.Sprintf("%s-%d", stem, i)
		}
		var pulls []struct {
			HTMLURL string `json:"html_url"`
		}
		if err := ghGet(ctx, "repos/"+repo+"/pulls?state=open&head="+url.QueryEscape(owner+":"+b), &pulls); err != nil {
			return "", "", err
		}
		if len(pulls) > 0 {
			return b, pulls[0].HTMLURL, nil
		}
		var br struct{}
		err := ghGet(ctx, "repos/"+repo+"/branches/"+escapePath(b), &br)
		if errors.Is(err, errGHNotFound) {
			return b, "", nil
		}
		if err != nil {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("branches %s to %s-9 all exist in %s; delete the old ones first", stem, stem, repo)
}

// writePRScript writes, in a new temporary directory, the request bodies and a script that creates branch at base,
// commits text as file's new content there, and opens the pull request. It returns the script's path. by is
// the command that prepared it, as the person typed it (the commit and pull request name it).
func writePRScript(repo, defaultBranch, base, branch, job, fork string, own []string, file ghContent, text []byte, by string) (string, error) {
	title := "Run job " + job + " on the lent Mac for own branches"
	refBody, _ := json.Marshal(map[string]string{"ref": "refs/heads/" + branch, "sha": base})
	putBody, _ := json.Marshal(map[string]string{
		"message": title + "\n\nPull requests from forks stay on GitHub's " + fork + ". Prepared by " + by + ".",
		"content": base64.StdEncoding.EncodeToString(text),
		"sha":     file.SHA,
		"branch":  branch,
	})
	body := fmt.Sprintf("This changes only the `runs-on` of job `%s` in `%s`.\n\n"+
		"- Pushes, dispatches and pull requests from this repository's own branches ask for `%s`: the Mac lent to this repository's Potluck pool.\n"+
		"- Pull requests from forks keep running on GitHub's `%s`, as before.\n\n"+
		"A fork's pull request runs the workflow from its own merge commit, so it can change `runs-on` itself. The Potluck broker refuses jobs from forks, so those never reach the lent Mac; if this repository is private and its owner turned on `allow_fork_prs`, that refusal is off and they can.\n\n"+
		"Undo: change `runs-on` back to `%s`.\n\nPrepared by `%s`.\n",
		job, file.Path, strings.Join(own, ", "), fork, fork, by)
	files := map[string][]byte{"ref.json": refBody, "put.json": putBody, "body.md": []byte(body), "workflow.yml": text}
	return writeGHScript("potluck-ci-pr-", "open-pr.sh", files, func(q func(string) string) string {
		return fmt.Sprintf(`#!/bin/sh
# Opens the pull request %[13]s prepared (written %[2]s). The new workflow file is
# %[3]s. Needs gh signed in with the workflow scope.
set -e
dir=%[12]s
trap 'rm -rf "$dir"' EXIT
gh api --method POST %[4]s --input %[5]s >/dev/null
gh api --method PUT %[6]s --input %[7]s >/dev/null
gh pr create -R %[1]s --base %[8]s --head %[9]s --title %[10]s --body-file %[11]s
`, shellQuote(repo), time.Now().UTC().Format(time.RFC3339), q("workflow.yml"), shellQuote("repos/"+repo+"/git/refs"), q("ref.json"),
			shellQuote("repos/"+repo+"/contents/"+escapePath(file.Path)), q("put.json"),
			shellQuote(defaultBranch), shellQuote(branch), shellQuote(title), q("body.md"), q(""), by)
	})
}

// writeGHScript writes files (owner-only) in a new temporary directory named with prefix, and the script that
// script returns, given a function quoting one of those files' paths for the shell, as name there. The script
// does with the person's gh sign-in what the GitHub App may not (write to a repository). q("") quotes the directory
// itself: a script removes it when it ends, so nothing it wrote is left behind. It returns its path.
func writeGHScript(prefix, name string, files map[string][]byte, script func(q func(string) string) string) (string, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	for f, b := range files {
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			return "", err
		}
	}
	q := func(f string) string { return shellQuote(filepath.Join(dir, f)) }
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script(q)), 0o700); err != nil {
		return "", err
	}
	return p, nil
}

var errGHNotFound = errors.New("not found on GitHub")

// ghGet reads one GitHub API path with the person's gh sign-in.
func ghGet(ctx context.Context, apiPath string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "api", "--method", "GET", apiPath)
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "HTTP 404") {
			return fmt.Errorf("%w: %s", errGHNotFound, apiPath)
		}
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("gh is not installed: install GitHub's gh and run gh auth login")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("gh could not read %s (%s); check gh auth status and your access to the repository", apiPath, msg)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("gh answered %s with something other than JSON: %v", apiPath, err)
	}
	return nil
}

// escapePath escapes each segment of a repository path for a URL.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// unifiedDiff is a unified diff of before and after with three lines of context around the one changed region.
func unifiedDiff(name string, before, after []byte) string {
	split := func(b []byte) []string {
		s := strings.SplitAfter(string(b), "\n")
		if s[len(s)-1] == "" {
			s = s[:len(s)-1]
		}
		return s
	}
	x, y := split(before), split(after)
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	from, toX, toY := max(pre-3, 0), min(len(x)-suf+3, len(x)), min(len(y)-suf+3, len(y))
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", name, name, from+1, toX-from, from+1, toY-from)
	line := func(mark, l string) {
		b.WriteString(mark + strings.TrimRight(l, "\r\n") + "\n")
	}
	for _, l := range x[from:pre] {
		line(" ", l)
	}
	for _, l := range x[pre : len(x)-suf] {
		line("-", l)
	}
	for _, l := range y[pre : len(y)-suf] {
		line("+", l)
	}
	for _, l := range x[len(x)-suf : toX] {
		line(" ", l)
	}
	return b.String()
}
