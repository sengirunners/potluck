package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/softnet"
)

// Test hooks for potluck softnet: the store, this Mac, the admin step's
// command and the download.
var (
	softnetStore                        = softnet.System
	softnetGOOS                         = runtime.GOOS
	softnetRunner      hostprobe.Runner = hostprobe.ExecRunner{}
	softnetFacts       hostprobe.Facts  = hostprobe.SystemFacts{}
	softnetEUID                         = os.Geteuid
	softnetSudo                         = runSudo
	softnetHTTP                         = &http.Client{Timeout: 5 * time.Minute}
	softnetTempDir                      = os.TempDir
	softnetDownloadDir                  = userSoftnetDownloads
)

// userSoftnetDownloads is where softnet install --no-sudo keeps the build it
// downloaded and checked, for the root half to read after the command
// ends: the user's cache directory (on a Mac ~/Library/Caches/potluck/softnet).
// Not the home: a home made only for this would not be one uninstall
// accepts. The root half hashes its own copy, so a file changed here after
// the check is refused.
func userSoftnetDownloads() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "potluck", "softnet"), nil
}

// softnetCmd is potluck softnet: Potluck's managed Softnet on a Mac whose
// macOS no upstream Softnet release that enforces Potluck's rules runs on
// (docs/softnet.md).
func (a *app) softnetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "softnet",
		Short: "Tart's network helper on a Mac (docs/softnet.md): status, install, rollback, uninstall",
		Long: `Softnet is the helper Tart starts, as root, for each sandbox's isolated network.
` + a.cmd("softnet install") + ` installs the build Potluck pins for this Mac's macOS, root owned and
setuid, in ` + softnet.Dir + `, keeping the build it replaces for ` + a.cmd("softnet rollback") + `:
on macOS ` + fmt.Sprint(softnet.UpstreamMinMacOS) + ` or newer an upstream Softnet release, on an older macOS
Potluck's own build when it has one. Potluck's sandboxes then use that copy, whatever other
softnet is on PATH. Without it, they use the softnet on PATH, which must be owned by root
and setuid.`,
	}
	cmd.AddCommand(a.softnetStatusCmd(), a.softnetInstallCmd(), a.softnetRollbackCmd(), a.softnetUninstallCmd())
	return cmd
}

func (a *app) softnetStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Which Softnet this Mac should use, the managed install, and the Softnet Potluck's sandboxes run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if softnetGOOS != "darwin" {
				return errors.New("Softnet is Tart's network helper on a Mac; this is " + softnetGOOS)
			}
			ctx := cmd.Context()
			r, _ := hostprobe.CheckMac(ctx, softnetRunner, softnetFacts)
			st := softnetStore().Status()
			used := hostprobe.CheckSoftnet(ctx, softnetRunner, softnetFacts, r)
			if a.asJSON() {
				return a.printJSON(map[string]any{"macos": r.MacOS, "arch": r.Arch, "advice": hostprobe.SoftnetAdvice(r.MacOS, r.Arch),
					"managed": st, "softnet_path": used.SoftnetPath, "softnet_version": used.SoftnetVersion, "softnet_build": used.SoftnetBuild,
					"problems": used.Problems, "warnings": used.Warnings})
			}
			w := a.host.Stdout
			fmt.Fprintf(w, "this Mac: macOS %s, %s\n", orUnknown(r.MacOS), orUnknown(r.Arch))
			fmt.Fprintf(w, "to get a Softnet that enforces Potluck's rules: %s\n", a.softnetAdvice(r))
			printInstalled(w, "managed, in use", st.Current)
			printInstalled(w, "managed, previous", st.Previous)
			if used.SoftnetPath != "" {
				fmt.Fprintf(w, "Potluck's boxes run: Softnet %s at %s\n", orUnknown(used.SoftnetVersion), used.SoftnetPath)
			}
			for _, p := range used.Problems {
				fmt.Fprintln(w, "  problem: "+p)
			}
			for _, p := range used.Warnings {
				fmt.Fprintln(w, "  warning: "+p)
			}
			return nil
		},
	}
}

func printInstalled(w io.Writer, label string, in *softnet.Installed) {
	switch {
	case in == nil:
		fmt.Fprintf(w, "%s: none\n", label)
	case in.Problem != "":
		fmt.Fprintf(w, "%s: %s, cannot be used: %s\n", label, in.ID, in.Problem)
	default:
		fmt.Fprintf(w, "%s: %s (%s)\n", label, in.ID, in.Path)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// softnetAdvice is softnet.For's words for r, naming this host's command.
func (a *app) softnetAdvice(r hostprobe.Report) string {
	major := hostprobe.MacOSMajor(r.MacOS)
	return softnet.For(major, r.Arch).Text(major, a.cmd("softnet"))
}

func (a *app) softnetInstallCmd() *cobra.Command {
	var from string
	var candidate, adminStep, noSudo bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install or update the Softnet build Potluck pins for this Mac's macOS (asks for an admin password once)",
		Long: `Installs the Softnet build this potluck pins for this Mac's macOS version
(on macOS ` + fmt.Sprint(softnet.UpstreamMinMacOS) + ` or newer, an upstream release): from --from (the
executable), or downloaded from its release. A downloaded archive must have
its pinned SHA-256 and size before it is unpacked. The executable must have
the build's pinned SHA-256 and size, checked before and again as root. It needs your
admin password once (sudo): the build is placed owned by root and setuid in
` + softnet.Dir + `, because Tart starts it as root to set up each sandbox's
network. The build in use before becomes the previous one, for rollback.
Nothing else changes: another softnet on PATH (Homebrew's, say) stays as it
is, and Potluck's sandboxes use the managed copy. Running it again with the
build already in use changes nothing.

--no-sudo does the checks and the download (kept in your cache directory),
then prints the root command in place of running sudo (with --json, as
{"admin_step": {"why": ..., "argv": [...]}}), for a program such as the Mac
app to run as root itself. Run install again afterwards to check the result.`,
		Example: "  " + a.cmd("softnet install") + "\n  " + a.cmd("softnet install --from ./softnet-<build>-darwin-arm64"),
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := noSudoNotAsRoot(adminStep, noSudo); err != nil {
				return err
			}
			return a.adminStepCmd(a.cmd("softnet install")+" again to check it", func() error {
				return a.softnetInstall(cmd.Context(), from, candidate, adminStep, noSudo)
			})
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "install from this file instead of downloading it (it must be the pinned build)")
	cmd.Flags().BoolVar(&candidate, "candidate", false, "install a build not yet validated on a real Mac of this macOS version (to help validate it)")
	cmd.Flags().BoolVar(&adminStep, "admin-step", false, "the root part of install, which install runs with sudo")
	_ = cmd.Flags().MarkHidden("admin-step")
	cmd.Flags().BoolVar(&noSudo, "no-sudo", false, "do not run sudo: print the root command (with --json, as admin_step) for you or an app to run as root")
	return cmd
}

func (a *app) softnetRollbackCmd() *cobra.Command {
	var adminStep, noSudo bool
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Put the managed Softnet in use before the last install back in use (asks for an admin password once)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := noSudoNotAsRoot(adminStep, noSudo); err != nil {
				return err
			}
			return a.adminStepCmd(a.cmd("softnet status")+" to check it, and "+a.cmd("host")+" again if this Mac lends, so its sizes use that Softnet", func() error { return a.softnetRollback(cmd.Context(), adminStep, noSudo) })
		},
	}
	cmd.Flags().BoolVar(&adminStep, "admin-step", false, "the root part of rollback, which rollback runs with sudo")
	_ = cmd.Flags().MarkHidden("admin-step")
	cmd.Flags().BoolVar(&noSudo, "no-sudo", false, "do not run sudo: print the root command (with --json, as admin_step) for you or an app to run as root")
	return cmd
}

func (a *app) softnetUninstallCmd() *cobra.Command {
	var adminStep, noSudo bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the managed Softnet (asks for an admin password once); sandboxes then use the softnet on PATH",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := noSudoNotAsRoot(adminStep, noSudo); err != nil {
				return err
			}
			return a.adminStepCmd(a.cmd("host")+" again if this Mac lends: until then its isolated boxes do not start, and "+a.cmd("host")+" picks the Softnet they use now", func() error { return a.softnetUninstall(cmd.Context(), adminStep, noSudo) })
		},
	}
	cmd.Flags().BoolVar(&adminStep, "admin-step", false, "the root part of uninstall, which uninstall runs with sudo")
	_ = cmd.Flags().MarkHidden("admin-step")
	cmd.Flags().BoolVar(&noSudo, "no-sudo", false, "do not run sudo: print the root command (with --json, as admin_step) for you or an app to run as root")
	return cmd
}

// noSudoNotAsRoot refuses --no-sudo on a root half: it is the person's run
// that hands the root step back, never the root step itself.
func noSudoNotAsRoot(adminStep, noSudo bool) error {
	if adminStep && noSudo {
		return errors.New("--no-sudo is for the run as yourself; --admin-step is the root step it prints")
	}
	return nil
}

// softnetUninstall disarms and removes every managed build. It reads no
// macOS version: a Mac upgraded past the managed build's macOS removes it
// the same way.
func (a *app) softnetUninstall(ctx context.Context, adminStep, noSudo bool) error {
	if softnetGOOS != "darwin" {
		return errors.New("Softnet is Tart's network helper on a Mac; this is " + softnetGOOS)
	}
	store := softnetStore()
	if adminStep {
		if softnetEUID() != 0 {
			return errors.New("--admin-step runs as root: run " + a.cmd("softnet uninstall") + " instead")
		}
		if err := store.Uninstall(); err != nil {
			return err
		}
		fmt.Fprintf(a.host.Stdout, "removed the managed Softnet in %s\n", store.Dir)
		return nil
	}
	removed, err := a.removeManagedSoftnet(ctx, store, noSudo)
	if err != nil || !removed {
		return err
	}
	r, _ := hostprobe.CheckMac(ctx, softnetRunner, softnetFacts)
	used := hostprobe.CheckSoftnet(ctx, softnetRunner, softnetFacts, r)
	// A Mac that lends still pins the removed path: its isolated boxes do
	// not start until host checks it again.
	if used.SoftnetPath != "" {
		fmt.Fprintf(a.host.Stdout, "next: if this Mac lends, run %s again so its isolated boxes use Softnet %s at %s (if it passes its checks); until then they do not start\n", a.cmd("host"), orUnknown(used.SoftnetVersion), used.SoftnetPath)
	} else {
		fmt.Fprintf(a.host.Stdout, "this Mac has no Softnet for isolated boxes now: %s\n", a.softnetAdvice(r))
	}
	return nil
}

// removeManagedSoftnet runs the admin step that removes every managed build
// in store (it asks for the admin password) and checks the directory is gone.
// removed is false when there was none to remove. With noSudo the error is
// the *adminStepLeft that names the step.
func (a *app) removeManagedSoftnet(ctx context.Context, store softnet.Store, noSudo bool) (removed bool, err error) {
	if _, err := os.Lstat(store.Dir); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(a.host.Stdout, "no managed Softnet is installed in %s; nothing to do\n", store.Dir)
		return false, nil
	}
	if softnetEUID() == 0 {
		return false, errors.New("run " + a.cmd("softnet uninstall") + " as yourself, not as root: it asks for the admin password itself")
	}
	if err := a.softnetAdmin(ctx, []string{"uninstall", "--admin-step"}, "disarms and removes every managed Softnet build in "+store.Dir, noSudo); err != nil {
		return false, err
	}
	if _, err := os.Lstat(store.Dir); !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("the admin step ended, but %s is still there", store.Dir)
	}
	return true, nil
}

// softnetBuild is the managed build for this Mac, after the checks every
// install makes: a Mac, Apple silicon, a macOS Potluck has a build for,
// and --candidate for a build not yet validated.
// As root (adminStep) it reads only the Mac's release, never Tart: the
// admin step runs nothing it does not need.
func (a *app) softnetBuild(ctx context.Context, candidate, adminStep bool) (softnet.Build, hostprobe.Report, hostprobe.Layer, error) {
	if softnetGOOS != "darwin" {
		return softnet.Build{}, hostprobe.Report{}, hostprobe.Layer{}, errors.New("Softnet is Tart's network helper on a Mac; this is " + softnetGOOS)
	}
	var r hostprobe.Report
	var l hostprobe.Layer
	if adminStep {
		r = hostprobe.MacRelease(ctx, softnetRunner)
	} else {
		r, l = hostprobe.CheckMac(ctx, softnetRunner, softnetFacts)
	}
	major := hostprobe.MacOSMajor(r.MacOS)
	adv := softnet.For(major, r.Arch)
	switch {
	case r.Arch != "arm64":
		return softnet.Build{}, r, l, errors.New("Tart and Softnet need Apple silicon; this Mac is " + orUnknown(r.Arch))
	case adv.Kind == softnet.Upstream:
		return softnet.Build{}, r, l, fmt.Errorf("this Mac runs macOS %s, which upstream Softnet supports: %s. Potluck has no build of its own for it%s", r.MacOS, a.softnetAdvice(r), a.managedLeft())
	case adv.Kind == softnet.None:
		return softnet.Build{}, r, l, errors.New(a.softnetAdvice(r) + a.managedLeft())
	case adv.Candidate && !candidate:
		return softnet.Build{}, r, l, fmt.Errorf("%s. Nothing was installed", a.softnetAdvice(r))
	}
	return adv.Build, r, l, nil
}

// managedLeft names the uninstall when a managed Softnet is installed on a
// Mac the matrix gives none (it was withdrawn, or the Mac's macOS moved on).
func (a *app) managedLeft() string {
	if cur := softnetStore().Status().Current; cur != nil {
		return fmt.Sprintf(". The managed Softnet %s installed here is not used: remove it with %s", cur.ID, a.cmd("softnet uninstall"))
	}
	return ""
}

func (a *app) softnetInstall(ctx context.Context, from string, candidate, adminStep, noSudo bool) error {
	b, r, l, err := a.softnetBuild(ctx, candidate, adminStep)
	if err != nil {
		return err
	}
	w := a.host.Stdout
	if adminStep {
		if softnetEUID() != 0 {
			return errors.New("--admin-step runs as root: run " + a.cmd("softnet install") + " instead")
		}
		if from == "" {
			return errors.New("--admin-step needs --from")
		}
		res, err := softnetStore().Install(from, b, a.softnetCheck(ctx))
		if err != nil {
			return err
		}
		switch {
		case res.Unchanged:
			fmt.Fprintf(w, "Softnet %s is in use already at %s\n", res.ID, softnetStore().Current())
		case res.Previous != "":
			fmt.Fprintf(w, "Softnet %s is in use at %s (it replaced %s, kept for %s)\n", res.ID, softnetStore().Current(), res.Previous, a.cmd("softnet rollback"))
		default:
			fmt.Fprintf(w, "Softnet %s is in use at %s\n", res.ID, softnetStore().Current())
		}
		return nil
	}
	if softnetEUID() == 0 {
		return errors.New("run " + a.cmd("softnet install") + " as yourself, not as root: it checks Tart as you and asks for the admin password itself")
	}
	if len(l.Problems) > 0 {
		return fmt.Errorf("this Mac cannot run Potluck's Tart boxes yet, so Softnet was not installed: %s", strings.Join(l.Problems, "; "))
	}
	if cur := softnetStore().Status().Current; cur != nil && cur.ID == b.ID && cur.Pinned && from == "" {
		fmt.Fprintf(w, "Softnet %s is in use already at %s; nothing to do\n", b.ID, softnetStore().Current())
		forgetKeptDownload(b)
		return a.softnetAfter(ctx, r)
	}
	if b.Candidate() {
		fmt.Fprintf(w, "note: Softnet %s is a candidate, not yet validated on a real macOS %d Mac. Potluck's own checks (%s) still run before this Mac lends.\n", b.ID, hostprobe.MacOSMajor(r.MacOS), a.cmd("host"))
	}
	file := from
	if file == "" {
		var err error
		var done func()
		if file, done, err = a.downloadSoftnet(ctx, b, noSudo); err != nil {
			return err
		}
		defer done()
	}
	if err := isBuild(file, b); err != nil {
		return err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	args := []string{"install", "--admin-step", "--from", abs}
	if candidate {
		args = append(args, "--candidate")
	}
	if err := a.softnetAdmin(ctx, args, "installs Softnet "+b.ID+" owned by root and setuid in "+softnet.Dir, noSudo); err != nil {
		return err
	}
	if err := inUse(b.ID); err != nil {
		return err
	}
	return a.softnetAfter(ctx, r)
}

// isBuild checks the file at path is build b: its pinned size and SHA-256.
func isBuild(path string, b softnet.Build) error {
	sum, n, err := softnet.FileSHA256(path)
	switch {
	case err != nil:
		return fmt.Errorf("cannot read %s: %w", path, err)
	case sum != b.SHA256 || n != b.Bytes:
		return fmt.Errorf("%s is not Softnet %s: it has %d bytes and SHA-256 %s; the pinned build has %d bytes and SHA-256 %s. Nothing was installed", path, b.ID, n, sum, b.Bytes, b.SHA256)
	}
	return nil
}

// downloadSoftnet fetches build b and returns its file (the caller checks
// it). With sudo the root half runs while this command waits, so the file
// is in a temp directory that done removes; under --no-sudo
// the root half runs after this command, so the file is kept in
// softnetDownloadDir (one already there that is b is used again), and the
// next install that finds b in use removes it.
func (a *app) downloadSoftnet(ctx context.Context, b softnet.Build, keep bool) (file string, done func(), err error) {
	done = func() {}
	var dir string
	if keep {
		if dir, err = softnetDownloadDir(); err != nil {
			return "", nil, fmt.Errorf("cannot find a directory to keep the Softnet download in: %w", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", nil, err
		}
	} else {
		if dir, err = os.MkdirTemp(softnetTempDir(), "potluck-softnet-"); err != nil {
			return "", nil, err
		}
		done = func() { os.RemoveAll(dir) }
	}
	file = filepath.Join(dir, softnet.AssetName(b.ID, b.Arch))
	if keep {
		if isBuild(file, b) == nil {
			return file, done, nil
		}
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
	}
	fmt.Fprintf(a.host.Stdout, "downloading Softnet %s from %s\n", b.ID, b.URL)
	if err := softnetDownload(ctx, b, file); err != nil {
		done()
		if keep {
			os.Remove(file)
		}
		return "", nil, err
	}
	return file, done, nil
}

// forgetKeptDownload removes the copy of b a --no-sudo install kept, once
// b is in use (and the directory, when that leaves it empty).
func forgetKeptDownload(b softnet.Build) {
	d, err := softnetDownloadDir()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(d, softnet.AssetName(b.ID, b.Arch)))
	_ = os.Remove(d)
	_ = os.Remove(filepath.Dir(d))
}

func (a *app) softnetRollback(ctx context.Context, adminStep, noSudo bool) error {
	if softnetGOOS != "darwin" {
		return errors.New("Softnet is Tart's network helper on a Mac; this is " + softnetGOOS)
	}
	if adminStep {
		if softnetEUID() != 0 {
			return errors.New("--admin-step runs as root: run " + a.cmd("softnet rollback") + " instead")
		}
		r := hostprobe.MacRelease(ctx, softnetRunner)
		major, check := hostprobe.MacOSMajor(r.MacOS), a.softnetCheck(ctx)
		res, err := softnetStore().Rollback(func(path string, b softnet.Build) error {
			if !b.Covers(major, r.Arch) {
				return fmt.Errorf("it is not built for this Mac (macOS %s, %s)", orUnknown(r.MacOS), orUnknown(r.Arch))
			}
			return check(path, b)
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(a.host.Stdout, "Softnet %s is in use at %s again (%s is now the previous one)\n", res.ID, softnetStore().Current(), res.Previous)
		return nil
	}
	st := softnetStore().Status()
	switch {
	case st.Previous == nil:
		return errors.New("there is no earlier managed Softnet to roll back to")
	case st.Previous.Problem != "":
		return fmt.Errorf("cannot roll back to Softnet %s: %s", st.Previous.ID, st.Previous.Problem)
	}
	if softnetEUID() == 0 {
		return errors.New("run " + a.cmd("softnet rollback") + " as yourself, not as root: it asks for the admin password itself")
	}
	if err := a.softnetAdmin(ctx, []string{"rollback", "--admin-step"}, "puts Softnet "+st.Previous.ID+" back in use in "+softnet.Dir, noSudo); err != nil {
		return err
	}
	if err := inUse(st.Previous.ID); err != nil {
		return err
	}
	r, _ := hostprobe.CheckMac(ctx, softnetRunner, softnetFacts)
	return a.softnetAfter(ctx, r)
}

// softnetAdmin runs this program's softnet command words args as root
// through sudo, after saying what the admin step does. With noSudo it runs
// nothing and returns an *adminStepLeft naming the step.
func (a *app) softnetAdmin(ctx context.Context, args []string, does string, noSudo bool) error {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fmt.Errorf("cannot find this program to run its admin step: %w", err)
	}
	words := append(strings.Fields(a.cmd("softnet"))[1:], args...)
	why := "The admin step " + does + ", because Tart starts Softnet as root for each box's network."
	if noSudo {
		return &adminStepLeft{adminStep{Why: why, Argv: append([]string{exe}, words...)}}
	}
	fmt.Fprintf(a.host.Stdout, "%s sudo asks for your Mac password once.\n", why)
	if err := softnetSudo(ctx, exe, words, a.host.Stdout, a.host.Stderr); err != nil {
		return fmt.Errorf("the admin step did not finish (%v); the Softnet in use is unchanged", err)
	}
	return nil
}

// inUse checks, after an admin step, that the managed build in use is id
// and sound.
func inUse(id string) error {
	cur := softnetStore().Status().Current
	switch {
	case cur == nil:
		return fmt.Errorf("the admin step ended, but no managed Softnet is in use (want %s)", id)
	case cur.ID != id:
		return fmt.Errorf("the admin step ended, but the managed Softnet in use is %s, not %s", cur.ID, id)
	case cur.Problem != "":
		return fmt.Errorf("the managed Softnet %s cannot be used: %s", id, cur.Problem)
	}
	return nil
}

// runSudo runs exe with args as root through /usr/bin/sudo, on this
// terminal (sudo reads the password from it). -k ignores a cached sudo
// sign-in, so the person always types the password for this step.
func runSudo(ctx context.Context, exe string, args []string, stdout, stderr io.Writer) error {
	argv := sudoArgv(exe, args)
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
	return c.Run()
}

func sudoArgv(exe string, args []string) []string {
	return append([]string{"/usr/bin/sudo", "-k", "--", exe}, args...)
}

// softnetCheck is what a build must pass before it is put in use: it runs
// on this Mac and prints its pinned version, and it accepts the exact
// rules a box boots with.
func (a *app) softnetCheck(ctx context.Context) softnet.Check {
	return func(path string, b softnet.Build) error {
		vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res, err := softnetRunner.Run(vctx, path, "--version")
		if err != nil || res.Exit != 0 {
			return fmt.Errorf("it does not run on this Mac: %s", strings.TrimSpace(fmt.Sprintf("%v exit %d %s", err, res.Exit, res.Stderr)))
		}
		if v := strings.TrimSpace(res.Stdout); v != b.Version {
			return fmt.Errorf("it says it is %q, not %q", v, b.Version)
		}
		if err := hostprobe.CheckSoftnetRules(ctx, softnetRunner, path); err != nil {
			return fmt.Errorf("it refuses Potluck's isolation rules: %w", err)
		}
		return nil
	}
}

// softnetAfter says which Softnet Potluck's boxes now run and what to do
// next.
func (a *app) softnetAfter(ctx context.Context, r hostprobe.Report) error {
	used := hostprobe.CheckSoftnet(ctx, softnetRunner, softnetFacts, r)
	w := a.host.Stdout
	if used.SoftnetPath != "" {
		fmt.Fprintf(w, "Potluck's boxes run Softnet %s at %s\n", orUnknown(used.SoftnetVersion), used.SoftnetPath)
	}
	for _, p := range used.Warnings {
		fmt.Fprintln(w, "  warning: "+p)
	}
	if len(used.Problems) > 0 {
		return fmt.Errorf("Softnet is still not usable: %s", strings.Join(used.Problems, "; "))
	}
	if cur := softnetStore().Current(); used.SoftnetPath != cur {
		return fmt.Errorf("Potluck's boxes would run %s, not the managed Softnet at %s", orUnknown(used.SoftnetPath), cur)
	}
	fmt.Fprintf(w, "next: %s checks and lends this Mac with it (run it again if this Mac lends already, so its sizes use this Softnet)\n", a.cmd("host"))
	return nil
}

// softnetDownload fetches b's executable into path (at most its pinned
// size; the caller checks its SHA-256). A build published in an archive
// (b.Archive) is downloaded beside path, checked against the archive's pin
// and unpacked (softnet.Build.Unpack), and the archive removed.
func softnetDownload(ctx context.Context, b softnet.Build, path string) error {
	if b.Archive == nil {
		return softnetFetch(ctx, b, path, b.Bytes)
	}
	archive := path + ".tar.gz"
	os.Remove(archive)
	defer os.Remove(archive)
	if err := softnetFetch(ctx, b, archive, b.Archive.Bytes); err != nil {
		return err
	}
	return b.Unpack(archive, path)
}

// softnetFetch fetches b.URL into path, a new file, at most limit bytes
// and one more (so a longer file fails its size check).
func softnetFetch(ctx context.Context, b softnet.Build, path string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return err
	}
	resp, err := softnetHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot download Softnet %s: %w", b.ID, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("Softnet %s is not published at %s yet: install it from a copy of the executable with --from <file>", b.ID, b.URL)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("cannot download Softnet %s: %s answered %s", b.ID, b.URL, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("cannot download Softnet %s: %w", b.ID, err)
	}
	return nil
}
