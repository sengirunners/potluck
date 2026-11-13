package cli

// potluck runners setup|set|link|unlink|allow|disallow|repos|ls|jobs (copied from zm's cmd/zm
// runners.go and runners_jobs.go): a pool's owner turns GitHub Actions on
// for the pool. The daemon writes config.json's runners block and opens its
// listener. zm's setup also routed the broker's paths through a tunnel
// `zm tunnel setup` made; potluck has no tunnel setup command, so GitHub
// reaches the broker at --url (or runners.public_url) through a proxy its
// owner runs.

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

func (a *app) runnersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runners",
		Short: "Run a GitHub org's Actions jobs in the pool's sandboxes: setup, set, link, unlink, allow, disallow, repos, ls, jobs",
	}
	cmd.AddCommand(a.runnersSetupCmd(), a.runnersSetCmd(), a.runnersLinkCmd(), a.runnersUnlinkCmd(),
		a.runnersAllowCmd(), a.runnersDisallowCmd(), a.runnersReposCmd(), a.runnersLsCmd(), a.runnersJobsCmd())
	return cmd
}

func (a *app) runnersSetupCmd() *cobra.Command {
	var req runners.SetupRequest
	c := &cobra.Command{
		Use:   "setup <pool> --url https://<host>",
		Short: "Turn the runner broker on for a pool this machine runs: config, listener, then the one URL to open",
		Long: `Turns GitHub Actions on for a pool on this machine (its owner). Potluck writes
config.json's runners block and opens the broker's listener on a free
loopback port (live: no restart). GitHub reaches the broker at --url, an
https URL you forward to that listener.

Then it prints one URL. Open it to create the GitHub App; GitHub then offers
to install it, and installing brings you back to sign in and say yes to the
link. The App is private: it installs only on the account that owns it, so
pass --org for an org's jobs. With --repo owner/repo (repeated for more, all of
one account), the install page opens on that account with those repositories
already chosen. Re-running it is safe.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			req.Pool = args[0]
			var st runners.Setup
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/setup", req, &st); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(map[string]any{"setup": st})
			}
			a.say("potluck: runner broker on for pool %s: GitHub reaches it at %s (listener %s, live); forward that URL to the listener", st.Pool, st.PublicURL, st.Listening)
			switch st.Next {
			case "create_app":
				if req.Org == "" {
					a.say("potluck: no --org: the App goes on your own GitHub account and installs only there (an org's jobs need --org <org>)")
				}
				a.say("Open this URL to create the GitHub App (until %s); GitHub then offers the install, which brings you back to link it:\n    %s", st.ExpiresAt, st.URL)
			case "install":
				a.say("The App %s exists. Open this URL to install it and link it (until %s):\n    %s", st.App.Slug, st.ExpiresAt, st.URL)
				a.say("(If GitHub does not bring you back after installing, set the App's Setup URL on GitHub to %s, or run potluck runners link <installation-id>.)", st.SetupURL)
			}
			return nil
		},
	}
	c.Flags().StringVar(&req.PublicURL, "url", "", "the https URL GitHub calls (default: runners.public_url)")
	c.Flags().StringVar(&req.Org, "org", "", "create the App under this GitHub org (default: your user account)")
	c.Flags().StringVar(&req.Name, "name", "", "the App's name on GitHub")
	c.Flags().StringVar(&req.Area, "area", "", "the pool area the first linked org's jobs run in (default: the whole pool)")
	c.Flags().StringSliceVar(&req.Repos, "repo", nil, "an owner/repo to tick on GitHub's install page (repeat for more; all of one account)")
	return c
}

func (a *app) runnersSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <setting> <value>",
		Short: "Change one broker setting, from the next job on: label, job_ttl_s",
		Long: `Changes one setting of the runner broker on this machine (the pool's owner).
It is written to config.json and applies to the next job: no restart, no
hand edit. A job that runs already keeps the time limit it started with.
potluck runners ls shows the values. Which repositories are served is
potluck runners allow.

  label <name>|default        the label jobs ask for (runs-on: <name>);
                              default is the pool's own name. Jobs still
                              waiting under the old label are not served
  job_ttl_s <seconds>         fallback runtime, 900 seconds at least; a resolved
                              workflow timeout-minutes takes precedence.
                              Hosts also need 300 seconds for startup.
                              (5400 is 90 minutes)`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			var st runners.Setup
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/settings", runners.SetRequest{Key: args[0], Value: args[1]}, &st); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(map[string]any{"setup": st})
			}
			a.say("potluck: %s set, from the next job on", args[0])
			a.printSettings(&st)
			return nil
		},
	}
}

// printSettings prints what potluck runners set changes, as it is now.
func (a *app) printSettings(st *runners.Setup) {
	fmt.Fprintf(a.stdout, "job limit   %d s (job_ttl_s: fallback runtime; a resolved workflow timeout-minutes takes precedence; host needs 300s extra for startup)\n", st.JobTTLS)
	for _, n := range st.Notices {
		fmt.Fprintf(a.stdout, "notice      %s\n", n)
	}
}

func (a *app) runnersLinkCmd() *cobra.Command {
	var area string
	c := &cobra.Command{
		Use:   "link [<installation-id>]",
		Short: "Link an org's install of the App to the pool: prints the URL to open",
		Long: `Without an installation id, prints the App's install URL: install it on an org
(or account), and GitHub brings you back to sign in as its admin and say yes.
With one (an installation that exists already), prints GitHub's sign-in URL
for the same proof. Either way the link is made only for an admin of the
installation's account, on their yes.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			body := map[string]any{"area": area}
			if len(args) == 1 {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || id <= 0 || strconv.FormatInt(id, 10) != args[0] {
					return fmt.Errorf("%q is not an installation id", args[0])
				}
				body["installation_id"] = id
			}
			var out struct {
				Install   string `json:"install_url"`
				Authorize string `json:"authorize_url"`
				Expires   string `json:"expires_at"`
			}
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/links", body, &out); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(out)
			}
			if out.Install != "" {
				a.say("Open this URL to install the App and link it (until %s):\n    %s", out.Expires, out.Install)
				a.say("(If GitHub does not bring you back after installing, the App lacks its Setup URL: potluck runners link <installation-id>.)")
			} else {
				a.say("Open this URL as an admin of the installation's account to link it (until %s):\n    %s", out.Expires, out.Authorize)
			}
			return nil
		},
	}
	c.Flags().StringVar(&area, "area", "", "the pool area its jobs run in (default: the whole pool)")
	return c
}

func (a *app) runnersUnlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <installation-id>",
		Short: "Stop serving an installation's jobs (revokes its token)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != args[0] {
				return fmt.Errorf("%q is not an installation id", args[0])
			}
			if err := cl.Do(cmd.Context(), "DELETE", "/v0/runners/links/"+strconv.FormatInt(id, 10), nil, nil); err != nil {
				return err
			}
			a.say("potluck: installation %s unlinked", args[0])
			return nil
		},
	}
}

func (a *app) runnersAllowCmd() *cobra.Command {
	var req runners.AllowRequest
	c := &cobra.Command{
		Use:   "allow <owner/repo> [--public]",
		Short: "Serve a repository's jobs: one of a linked installation's (a public one needs --public)",
		Long: `Connects a repository to the pool (its owner, on this machine): its jobs that
ask for the pool are served from the next one on. The App must be installed
on the repository's account, given the repository, and linked (potluck
runners link). A repository not connected is never served.

A public repository needs --public, your yes for it, given while it is
public (it is refused for a private one). It serves the
repository's own branches: pushes, manual runs, schedules, and pull requests
from its own branches, that is code from people who can push to it. A pull
request from a fork is never served for a public repository, and neither is
a pull_request_target, workflow_run or issue_comment run. Potluck then reads
the repository's rule for outside contributors' runs, now and every hour,
and says so if it lets any start without your approval.

Each job's repository is asked of GitHub again just before its runner: one
made public while its job waited is not served without the yes, and a
repository that took a connected one's name (that one renamed or deleted)
is not connected.

Running it again changes the yes; potluck runners disallow undoes it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			req.Repo = args[0]
			var rp runners.Repo
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/repos", req, &rp); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(map[string]any{"repo": rp})
			}
			how := "served"
			if rp.Public {
				how = "served while public: its own branches only, never a fork's pull request"
			}
			a.say("potluck: %s connected (%s), from its next job on", rp.Repo, how)
			if rp.Warning != "" {
				a.say("potluck: warning: %s", rp.Warning)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&req.Public, "public", false, "yes: serve this public repository's own branches (never a fork's pull request)")
	return c
}

// repoPath is the daemon's path for a connected repository: owner/name as
// GitHub spells it, each part escaped, so what the person typed is what the
// daemon is asked about and never a query or another path.
func repoPath(repo string) (string, error) {
	if !jobrule.ValidRepo(repo) {
		return "", fmt.Errorf("%q is not owner/repo", repo)
	}
	o, n, _ := strings.Cut(repo, "/")
	return "/v0/runners/repos/" + url.PathEscape(o) + "/" + url.PathEscape(n), nil
}

func (a *app) runnersDisallowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disallow <owner/repo>",
		Short: "Stop serving a repository's jobs (a job that runs already finishes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			path, err := repoPath(args[0])
			if err != nil {
				return err
			}
			if err := cl.Do(cmd.Context(), "DELETE", path, nil, nil); err != nil {
				return err
			}
			a.say("potluck: %s disconnected: its next jobs are not served", args[0])
			return nil
		},
	}
}

func (a *app) runnersReposCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repos",
		Short: "The repositories whose jobs are served, and any warning about their settings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Repos []runners.Repo `json:"repos"`
			}
			if err := cl.Do(cmd.Context(), "GET", "/v0/runners/repos", nil, &out); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(out)
			}
			if len(out.Repos) == 0 {
				a.say("no repositories connected: potluck runners allow <owner/repo>")
				return nil
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "REPO\tVISIBILITY\tPUBLIC YES\tBY\tCHECKED\tWARNING")
			for _, r := range out.Repos {
				yes := "no"
				if r.Public {
					yes = "yes"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Repo, dash(r.Visibility), yes, r.By, dash(r.Checked), dash(r.Warning))
			}
			return tw.Flush()
		},
	}
}

func (a *app) runnersLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "The runner broker: its App, listener, pool, settings, sizes and linked installations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			st, links, err := runnersList(cmd.Context(), cl)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(map[string]any{"setup": st, "links": links})
			}
			a.printRunners(st, links)
			return nil
		},
	}
}

// runnersList is the setup and the links the caller may read.
func runnersList(ctx context.Context, cl *client.Client) (*runners.Setup, []runners.Link, error) {
	st := &runners.Setup{}
	if err := cl.Do(ctx, "GET", "/v0/runners/setup", nil, st); err != nil {
		return nil, nil, err
	}
	if st.Pool == "" {
		return st, nil, nil
	}
	var out struct {
		Links []runners.Link `json:"links"`
	}
	if err := cl.Do(ctx, "GET", "/v0/runners/links", nil, &out); err != nil {
		return st, nil, err
	}
	return st, out.Links, nil
}

func (a *app) printRunners(st *runners.Setup, links []runners.Link) {
	w := a.stdout
	if st.Pool == "" {
		fmt.Fprintln(w, "the runner broker is off on this machine (potluck runners setup <pool>)")
		return
	}
	fmt.Fprintf(w, "pool        %s (jobs ask runs-on: [self-hosted, %s])\n", st.Pool, st.Label)
	fmt.Fprintf(w, "public url  %s\n", st.PublicURL)
	if st.Listening == "" {
		fmt.Fprintf(w, "listener    not serving (potluck runners setup %s)\n", st.Pool)
	} else {
		fmt.Fprintf(w, "listener    %s\n", st.Listening)
	}
	if st.App == nil {
		fmt.Fprintf(w, "app         none yet (potluck runners setup %s prints the URL to create it)\n", st.Pool)
	} else {
		fmt.Fprintf(w, "app         %s (id %d)\n", st.App.Slug, st.App.ID)
	}
	a.printSettings(st)
	sizes := make([]string, 0, len(st.Sizes))
	for k := range st.Sizes {
		sizes = append(sizes, k)
	}
	sort.Strings(sizes)
	for i, k := range sizes {
		head := "sizes      "
		if i > 0 {
			head = "           "
		}
		def := ""
		if k == st.DefaultSize {
			def = " (default)"
		}
		fmt.Fprintf(w, "%s %s -> %s%s\n", head, k, st.Sizes[k], def)
	}
	if len(sizes) == 0 {
		fmt.Fprintln(w, "sizes       none")
	}
	if len(links) == 0 {
		fmt.Fprintln(w, "links       none (potluck runners link)")
		return
	}
	fmt.Fprintln(w, "links")
	for _, l := range links {
		area := l.Area
		if area == "" {
			area = "(whole pool)"
		}
		fmt.Fprintf(w, "  %-10d %s (%s)  area %s  by %s  %s\n", l.Installation, l.Account, strings.ToLower(l.AccountType), area, l.LinkedBy, l.Created)
	}
}

func (a *app) runnersJobsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Recent GitHub Actions jobs the broker saw: where each ran, and why one waits, failed or was ignored",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Jobs []struct {
					ID          int64    `json:"job_id"`
					Repo        string   `json:"repo"`
					Run         int64    `json:"run_id"`
					Labels      []string `json:"labels"`
					State       string   `json:"state"`
					Why         string   `json:"why,omitempty"`
					Target      string   `json:"target,omitempty"`
					Runner      string   `json:"runner,omitempty"`
					Created     string   `json:"created"`
					Updated     string   `json:"updated"`
					LimitS      int      `json:"limit_s"`
					LimitSource string   `json:"limit_source"`
					StartedAt   int64    `json:"started_at,omitempty"`
					Deadline    int64    `json:"deadline,omitempty"`
				} `json:"jobs"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v0/runners/jobs?limit="+strconv.Itoa(limit), nil, &out); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(out)
			}
			if len(out.Jobs) == 0 {
				a.say("no jobs yet: a workflow asks for the pool with runs-on: <pool name> (or <pool name>:<size>)")
				return nil
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "JOB\tREPO\tSTATE\tWHERE\tLIMIT\tSOURCE\tUPDATED\tWHY")
			for _, j := range out.Jobs {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%ds\t%s\t%s\t%s\n", j.ID, j.Repo, j.State, dash(j.Target), j.LimitS, j.LimitSource, j.Updated, dash(strings.Join(strings.Fields(j.Why), " ")))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "how many jobs, newest first (at most 500)")
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
