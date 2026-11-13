package cli

// potluck host shelf: the owner's commands for the package shelf
// (vmhost/shelf*.go), run through the pinned VM driver of a binding this
// machine serves.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runtarget"
)

// hostShelfRun runs a binding's driver host command (tests swap it).
var hostShelfRun = runtarget.HostCommand

func (a *app) hostShelfCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shelf",
		Short: "The package shelf this machine's oci sandboxes read: fill, status",
	}
	var locks, sums []string
	fill := &cobra.Command{
		Use:   "fill [--npm-lock FILE]... [--go-sum FILE]...",
		Short: "Fetch the packages package-lock.json and go.sum files name onto the shelf",
		Long: `Fill reads each package-lock.json and fetches the packages it names
that were resolved from registry.npmjs.org with a sha512 integrity, from
registry.npmjs.org only and without credentials. A tarball goes on the shelf
only when its sha512 is the registry's own and the lockfile's; anything else is
dropped and counted, never stored.

It reads each go.sum and fetches the module versions (and go.mod files) it
names from proxy.golang.org only, checked against sum.golang.org, in a sandbox of
the size's own on its network. Modules that GOPRIVATE, GONOPROXY, GONOSUMDB or
GOINSECURE match here (go env, else this shell's environment) are left out.

It runs for each vm=oci size this machine shares with shelf_max_gb above 0
(one per state directory), and does nothing when shelf_fill is off.`,
		Example: "  potluck host shelf fill --npm-lock package-lock.json --go-sum go.sum",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(locks)+len(sums) == 0 {
				return errors.New("name at least one package-lock.json (--npm-lock) or go.sum (--go-sum)")
			}
			args := []string{"shelf", "fill"}
			for _, f := range []struct {
				flag  string
				files []string
			}{{"--npm-lock", locks}, {"--go-sum", sums}} {
				for _, l := range f.files {
					p, err := filepath.Abs(l) // the driver runs in another directory
					if err != nil {
						return err
					}
					args = append(args, f.flag, p)
				}
			}
			if len(sums) > 0 {
				private, err := goPrivate()
				if err != nil {
					return err
				}
				args = append(args, "--go-private", private)
			}
			ts, err := a.localBindings(cmd.Context())
			if err != nil {
				return err
			}
			targets := shelfBindings(ts)
			if len(targets) == 0 {
				return errors.New("no size this machine shares has a shelf: it needs vm=oci and shelf_max_gb above 0")
			}
			cfg, err := config.LoadClient()
			if err != nil {
				return err
			}
			return runEach(cmd.Context(), cfg.Home, targets, args, a)
		},
	}
	fill.Flags().StringArrayVar(&locks, "npm-lock", nil, "a package-lock.json to fill from (repeat for more)")
	status := &cobra.Command{
		Use:   "status",
		Short: "What is on the shelf (size only, no names), the fill mode, the limits and the queue",
		Long: `Status asks each vm=oci size this machine shares (one per state directory)
through its driver: the shelf's path, bytes and files (never a package name),
the fill mode, the limits and how much of sandboxes' wants is queued. It also says
where sizes of one engine carry different limits: they share one volume and
the smallest wins. It makes no volume and fetches nothing.`,
		Example: "  potluck host shelf status",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ts, err := a.localBindings(cmd.Context())
			if err != nil {
				return err
			}
			cfg, err := config.LoadClient()
			if err != nil {
				return err
			}
			return a.shelfStatus(cmd.Context(), cfg.Home, ts)
		},
	}
	fill.Flags().StringArrayVar(&sums, "go-sum", nil, "a go.sum to fill from (repeat for more)")
	cmd.AddCommand(fill, status)
	return cmd
}

// localBindings is this machine's shared sizes in both scopes: machine-level
// (a pool it joined) and, when one resolves, the pool it runs.
func (a *app) localBindings(ctx context.Context) ([]computeapi.RunTarget, error) {
	ts, err := a.bindingsIn(ctx, true, true)
	if err != nil {
		return nil, err
	}
	if p, err := a.bindingsIn(ctx, false, false); err == nil {
		ts = append(ts, p...) // no dedup: the pool's record and the machine's of one name are two bindings
	}
	return ts, nil
}

// shelfStatus warns where sizes of one engine differ and asks each oci size's
// driver for its shelf's status.
func (a *app) shelfStatus(ctx context.Context, home string, ts []computeapi.RunTarget) error {
	targets := ociBindings(ts)
	if len(targets) == 0 {
		return errors.New("no size this machine shares is vm=oci: only oci sandboxes have a shelf (potluck host --shelf-gb N)")
	}
	for _, d := range hostprobe.StorageDisagreements(ts) {
		a.say("warning: %s", d)
	}
	return runEach(ctx, home, targets, []string{"shelf", "status"}, a)
}

// goPrivate is this machine's private module patterns, as one
// comma-separated list, from what go env says (it reads the go env file),
// else the environment. It refuses, as the in-box helper names nothing,
// unless this machine's Go goes first to the public proxy and checks the
// public checksum database, and its patterns are plain globs: otherwise a
// go.sum here may name private modules nothing marks private. With no go to
// read a go env file that exists, it refuses too.
func goPrivate() (string, error) {
	keys := []string{"GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOINSECURE"}
	var vs []string
	c := exec.Command("go", append([]string{"env"}, keys...)...)
	c.Dir, c.Env = "/", append(os.Environ(), "GOTOOLCHAIN=local") // no module, no toolchain download
	if out, err := c.Output(); err == nil {
		vs = strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") // one line a key, empty ones too
	} else {
		envFile := os.Getenv("GOENV")
		if envFile == "" {
			if dir, err := os.UserConfigDir(); err == nil {
				envFile = filepath.Join(dir, "go", "env")
			}
		}
		if envFile != "" && envFile != "off" {
			if _, err := os.Stat(envFile); err == nil {
				return "", fmt.Errorf("--go-sum: no go here to read %s with, which may name private modules: put go on PATH", envFile)
			}
		}
		for _, k := range keys {
			vs = append(vs, os.Getenv(k))
		}
		vs[0] = cmp.Or(vs[0], "https://proxy.golang.org,direct")
		vs[1] = cmp.Or(vs[1], "sum.golang.org")
	}
	if len(vs) != len(keys) {
		return "", fmt.Errorf("--go-sum: go env said %q", vs)
	}
	proxies := strings.FieldsFunc(vs[0], func(r rune) bool { return r == ',' || r == '|' })
	if len(proxies) > 1 && strings.HasPrefix(proxies[0], "file://") {
		proxies = proxies[1:]
	}
	if len(proxies) == 0 || strings.TrimSuffix(proxies[0], "/") != "https://proxy.golang.org" {
		return "", fmt.Errorf("--go-sum: GOPROXY here is %q, not the public proxy first: this go.sum may name modules of another proxy", vs[0])
	}
	if vs[1] != "sum.golang.org" && !strings.HasPrefix(vs[1], "sum.golang.org+") {
		return "", fmt.Errorf("--go-sum: GOSUMDB here is %q, not sum.golang.org: this go.sum may name modules it never checked", vs[1])
	}
	globs := strings.Join(slices.DeleteFunc(vs[2:], func(v string) bool { return strings.TrimSpace(v) == "" }), ",")
	if strings.Trim(globs, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~/*?,-") != "" {
		return "", fmt.Errorf("--go-sum: a private pattern here is not a plain glob (%q): name the go.sum's private modules plainly", globs)
	}
	return globs, nil
}

// runEach runs a driver host command for each target, naming it first.
func runEach(ctx context.Context, home string, targets []computeapi.RunTarget, args []string, a *app) error {
	var errs []error
	for _, t := range targets {
		a.say("%s:", t.Name)
		if err := hostShelfRun(ctx, home, t, args, a.stdout, a.stderr); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ociBindings are the vm=oci box bindings this machine serves, one per state
// directory (what status asks, whatever shelf_max_gb says).
func ociBindings(ts []computeapi.RunTarget) []computeapi.RunTarget {
	return onePerStateDir(ts, func(computeapi.RunTarget) bool { return true })
}

// shelfBindings are the box bindings this machine serves that have a shelf
// (vm=oci, shelf_max_gb above 0), one per state directory: bindings that
// share one have one staging and one lock.
func shelfBindings(ts []computeapi.RunTarget) []computeapi.RunTarget {
	return onePerStateDir(ts, func(t computeapi.RunTarget) bool {
		gb, err := strconv.Atoi(t.Config["shelf_max_gb"])
		return err == nil && gb > 0
	})
}

// onePerStateDir is the first vm=oci box binding of this machine that keep
// accepts, for each state directory.
func onePerStateDir(ts []computeapi.RunTarget, keep func(computeapi.RunTarget) bool) []computeapi.RunTarget {
	var out []computeapi.RunTarget
	seen := map[string]bool{}
	for _, t := range ts {
		if t.Kind != "box" || t.Host != "" || t.Config["vm"] != "oci" || !keep(t) || seen[t.Config["state_dir"]] {
			continue
		}
		seen[t.Config["state_dir"]] = true
		out = append(out, t)
	}
	return out
}
