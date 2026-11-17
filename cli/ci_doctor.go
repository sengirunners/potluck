package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/runners"
	"github.com/spf13/cobra"
)

func (a *app) ciCmd() *cobra.Command {
	c := &cobra.Command{Use: "ci", Short: "Diagnose GitHub Actions jobs on lent machines, and move a job onto them"}
	c.AddCommand(a.ciPRCmd())
	c.AddCommand(a.ciDoctorCmd())
	return c
}

const ciDoctorHelp = `Read-only checks explaining why a repository's GitHub Actions jobs are not running on the lent
machines: 17 numbered checks, each with one next step. Any failure makes the command exit nonzero.

With --probe and one repository, check 18 prepares a probe of the whole path: a script that pushes a
throwaway branch holding only a generated one-job workflow, waits for that job on this machine's pool,
prints the machine's OS, Xcode and free disk, and deletes the branch. The GitHub App cannot write to a
repository, so the script does it with your own gh sign-in; the command writes and prints it, and
running it is the next step. A public repository needs its owner's yes first, given with --public.

See docs/ci-doctor.md.`

func (a *app) ciDoctorCmd() *cobra.Command {
	var probe, public bool
	var want probeLabels
	c := &cobra.Command{Use: "doctor [owner/repo]", Short: "Read-only checks explaining why jobs are not running", Long: ciDoctorHelp, Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if probe && len(args) == 0 {
			return fmt.Errorf("--probe needs the repository to probe: %s", a.cmd("ci doctor owner/repo --probe"))
		}
		if !probe && slices.ContainsFunc([]string{"public", "labels", "size", "os"}, cmd.Flags().Changed) {
			return fmt.Errorf("--public, --labels, --size and --os only go with --probe")
		}
		cl, _, err := a.client()
		if err != nil {
			return err
		}
		path := "/v0/runners/doctor"
		if len(args) > 0 {
			path += "?repo=" + url.QueryEscape(args[0])
		}
		var out runners.DoctorResult
		if err := cl.Do(cmd.Context(), "GET", path, nil, &out); err != nil {
			var ae *api.Error
			if !errors.As(err, &ae) || ae.HTTPStatus != http.StatusNotFound {
				return err
			}
			repo := ""
			if len(args) > 0 {
				repo = args[0]
			}
			out.Reports = []runners.DoctorReport{{Repo: repo, Checks: []runners.DoctorCheck{{
				Number: 6, Status: "fail", Message: "This daemon has no CI doctor API; its fork-repository support cannot be checked.",
				Fix: "update " + a.name() + " and restart " + a.cmd("daemon") + " before diagnosing forked repositories",
			}}}}
		}
		failed := false
		for _, r := range out.Reports {
			failed = failed || r.Failed()
		}
		script := ""
		if probe {
			notPrepared := ""
			switch {
			case len(out.Reports) == 0:
				// No checks came back (GitHub's request budget may be spent): nothing says the job would get a machine.
				out.Reports = []runners.DoctorReport{{Repo: args[0]}}
				notPrepared = "the daemon's checks gave no answer for this repository, so nothing says its job would get a machine."
			case failed:
				notPrepared = "its job would wait on the failures above."
			}
			var check runners.DoctorCheck
			check, script = a.ciProbe(cmd.Context(), args[0], want, public, notPrepared)
			out.Reports[0].Checks = append(out.Reports[0].Checks, check)
			failed = failed || check.Status == "fail"
		}
		if a.asJSON() {
			if err := a.printJSON(out); err != nil {
				return err
			}
		} else {
			for _, warning := range out.Warnings {
				a.say("warn: %s", warning)
			}
			for _, r := range out.Reports {
				fmt.Fprintln(a.host.Stdout, r.Repo)
				for _, c := range r.Checks {
					fmt.Fprintf(a.host.Stdout, "%2d %-4s %s\n", c.Number, c.Status, c.Message)
					if c.Fix != "" {
						fmt.Fprintf(a.host.Stdout, "   Next: %s\n", c.Fix)
					}
				}
			}
			if script != "" {
				b, err := os.ReadFile(script)
				if err != nil {
					return err
				}
				a.say("The probe script (pushing a workflow file needs gh's workflow scope: gh auth refresh -s workflow):")
				fmt.Fprint(a.host.Stdout, string(b))
				a.say("next: sh %s", shellQuote(script))
			}
		}
		if failed {
			return fmt.Errorf("repository checks failed")
		}
		return nil
	}}
	c.Flags().BoolVar(&probe, "probe", false, "also prepare check 18: a script that proves the path with a generated test job")
	c.Flags().BoolVar(&public, "public", false, "with --probe: the owner of this public repository agreed to the probe")
	c.Flags().StringVar(&want.labels, "labels", "", "with --probe: the labels the probe job asks for, comma-separated (default: self-hosted, the OS and this machine's pool label)")
	c.Flags().StringVar(&want.size, "size", "", "with --probe: a pool size to ask for (the label becomes <pool label>:<size>)")
	c.Flags().StringVar(&want.osName, "os", "macos", "with --probe: the OS label the probe job asks for")
	return c
}
