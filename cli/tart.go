package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/tartpin"
	"github.com/sengirunners/potluck/vmhost"
)

// Test hooks for potluck tart: this Mac, where Tart is looked for, where
// an install goes and the download.
var (
	tartGOOS                    = runtime.GOOS
	tartRunner hostprobe.Runner = hostprobe.ExecRunner{}
	tartSearch                  = vmhost.SystemTartSearch
	tartPlace                   = tartpin.UserPlace
	tartHTTP                    = &http.Client{Timeout: 10 * time.Minute}
)

// tartCmd is potluck tart: the Tart this Mac's boxes run in.
func (a *app) tartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tart",
		Short: "Tart, which runs the sandboxes this Mac lends (docs/hosts.md): install, path",
		Long: `Tart runs the virtual machines a Mac lends. ` + a.cmd("tart install") + ` downloads the Tart release Potluck pins
from its maker (` + tartpin.Upstream + `), checks its size and SHA-256, and puts tart.app in your
own Applications folder. No root is needed. Tart is under its own licence
(FSL-1.1-ALv2, OpenAI), not Potluck's.`,
	}
	cmd.AddCommand(a.tartInstallCmd(), a.tartPathCmd())
	return cmd
}

func (a *app) tartPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the Tart program Potluck runs on this Mac (exit 1 when there is none)",
		Long: `Prints the Tart program Potluck finds on this Mac: on PATH, in the usual bin
folders (~/.local/bin, Homebrew's), then tart.app in /Applications and
~/Applications. A script uses it to run the same Tart Potluck does.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			found := vmhost.FindTart(tartSearch())
			if a.asJSON() {
				if err := a.printJSON(map[string]any{"path": found.Path, "hollow": found.Hollow}); err != nil {
					return err
				}
			} else if found.Path != "" {
				fmt.Fprintln(a.host.Stdout, found.Path)
			}
			if found.Path == "" {
				msg := "Tart is not installed: " + a.cmd("tart install") + " installs it"
				for _, app := range found.Hollow {
					msg += fmt.Sprintf("; %s has no Contents/MacOS/tart in it", app)
				}
				return exitWith{code: 1, err: errors.New(msg)}
			}
			return nil
		},
	}
}

func (a *app) tartInstallCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "install [--dry-run]",
		Short: "Download the Tart release Potluck pins and put tart.app in ~/Applications",
		Long: `Downloads tart.tar.gz of the Tart release Potluck pins from ` + tartpin.Upstream + `,
checks its size and then its SHA-256 against the pin before unpacking it, and
moves tart.app into ~/Applications in one step. On any mismatch nothing is
installed.

When Tart is already on this Mac (on PATH, from Homebrew, or a tart.app in
/Applications) it is left as it is and nothing is installed. A
~/Applications/tart.app that Potluck did not install is never replaced; one
it installed of an older release is replaced, and one of a newer release
(installed by a newer potluck) is kept.

--json answers {"installed", "already_present", "path", "version"}.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.tartInstall(cmd.Context(), dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what it would do and change nothing")
	return cmd
}

// tartInstallAnswer is potluck tart install's --json answer.
type tartInstallAnswer struct {
	Installed      bool     `json:"installed"`
	AlreadyPresent bool     `json:"already_present"`
	Path           string   `json:"path"`              // the Tart program Potluck runs
	Version        string   `json:"version,omitempty"` // known when it is a pinned release Potluck installed
	Replaced       string   `json:"replaced,omitempty"`
	DryRun         bool     `json:"dry_run,omitempty"`
	Plan           []string `json:"plan,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

func (a *app) tartInstall(ctx context.Context, dryRun bool) error {
	if tartGOOS != "darwin" {
		return errors.New("Tart runs on a Mac; this is " + tartGOOS)
	}
	b, ok := tartpin.Latest()
	if !ok {
		return errors.New("this potluck pins no Tart release that may be installed; get Tart from " + tartpin.Upstream + "/releases")
	}
	place, err := tartPlace()
	if err != nil {
		return fmt.Errorf("cannot find your Applications folder: %w", err)
	}
	found := vmhost.FindTart(tartSearch())
	plan, planErr := place.Plan(b)
	ours := planErr == nil && plan.Action != tartpin.Fresh
	ans := tartInstallAnswer{Path: place.Program(), Version: b.Version, DryRun: dryRun}
	for _, h := range found.Hollow {
		if h != place.App() {
			ans.Notes = append(ans.Notes, h+" has no Contents/MacOS/tart in it; it is left as it is")
		}
	}
	switch {
	case found.Path != "" && !(ours && samePath(found.Path, place.Program())):
		ans = tartInstallAnswer{AlreadyPresent: true, Path: found.Path, DryRun: dryRun, Notes: ans.Notes}
		if ours {
			ans.Notes = append(ans.Notes, fmt.Sprintf("Potluck's own Tart %s at %s is not the one found first", plan.Installed.Version, place.App()))
		}
		return a.tartAnswer(ans, fmt.Sprintf("Tart is already on this Mac at %s; it is left as it is and nothing was installed", found.Path))
	case errors.Is(planErr, tartpin.ErrNotOurs):
		return fmt.Errorf("%s is there but Potluck did not install it, so it is left as it is and nothing was installed; if it is not wanted, move it to the Bin and run %s again", place.App(), a.cmd("tart install"))
	case planErr != nil:
		return planErr
	case plan.Action == tartpin.Keep:
		ans.AlreadyPresent, ans.Version = true, plan.Installed.Version
		if !dryRun {
			if err := place.Sweep(); err != nil {
				ans.Notes = append(ans.Notes, "cannot look for an interrupted install's leftovers: "+err.Error())
			}
		}
		return a.tartAnswer(ans, fmt.Sprintf("Tart %s, which Potluck installed, is already at %s; nothing was installed", plan.Installed.Version, place.App()))
	case plan.Action == tartpin.Replace:
		ans.Replaced = plan.Installed.Version
	}
	r := hostprobe.MacRelease(ctx, tartRunner)
	if !b.Runs(hostprobe.MacOSMajor(r.MacOS), r.Arch) {
		return fmt.Errorf("Tart %s needs macOS %d or newer on %v; this Mac runs macOS %s on %s. Nothing was installed", b.Version, b.MinMacOS, b.Arches, orUnknown(r.MacOS), orUnknown(r.Arch))
	}
	if dryRun {
		ans.Plan = []string{
			fmt.Sprintf("download %s (%d bytes)", b.URL, b.Bytes),
			fmt.Sprintf("check its size is %d bytes and its SHA-256 is %s; on a mismatch install nothing", b.Bytes, b.SHA256),
			"unpack tart.app from it into a hidden folder in " + place.AppsDir,
		}
		if ans.Replaced != "" {
			ans.Plan = append(ans.Plan, fmt.Sprintf("swap it with Potluck's Tart %s at %s", ans.Replaced, place.App()))
		} else {
			ans.Plan = append(ans.Plan, "move it to "+place.App())
		}
		if a.asJSON() {
			return a.printJSON(ans)
		}
		fmt.Fprintf(a.host.Stdout, "would install Tart %s:\n", b.Version)
		for _, p := range ans.Plan {
			fmt.Fprintln(a.host.Stdout, "  "+p)
		}
		a.tartNotes(ans.Notes)
		return nil
	}
	dir, err := os.MkdirTemp("", "potluck-tart-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "tart.tar.gz")
	if !a.asJSON() {
		fmt.Fprintf(a.host.Stdout, "downloading Tart %s from %s\n", b.Version, b.URL)
	}
	if err := tartpin.Fetch(ctx, tartHTTP, b, file); err != nil {
		return err
	}
	res, err := place.Install(file, b)
	if err != nil {
		return err
	}
	ans.Installed, ans.AlreadyPresent = !res.Already, res.Already
	ans.Version, ans.Replaced = res.Version, res.Replaced
	if res.Already {
		return a.tartAnswer(ans, fmt.Sprintf("Tart %s, which Potluck installed, is already at %s; nothing was installed", res.Version, place.App()))
	}
	words := fmt.Sprintf("installed Tart %s at %s", b.Version, place.App())
	if res.Replaced != "" {
		words += fmt.Sprintf(" (it replaced Potluck's Tart %s)", res.Replaced)
	}
	return a.tartAnswer(ans, words+"; next: "+a.cmd("host")+" checks and lends this Mac")
}

// samePath is true when both paths name one file, through any links.
func samePath(x, y string) bool {
	rx, errx := filepath.EvalSymlinks(x)
	ry, erry := filepath.EvalSymlinks(y)
	return errx == nil && erry == nil && rx == ry
}

// tartAnswer prints ans (--json) or words and its notes.
func (a *app) tartAnswer(ans tartInstallAnswer, words string) error {
	if a.asJSON() {
		return a.printJSON(ans)
	}
	fmt.Fprintln(a.host.Stdout, words)
	a.tartNotes(ans.Notes)
	return nil
}

func (a *app) tartNotes(notes []string) {
	for _, n := range notes {
		fmt.Fprintln(a.host.Stdout, "  note: "+n)
	}
}
