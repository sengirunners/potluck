// Package depscli is the client side of dependency layers (docs/daemon.md,
// Dependency layers) for any command line: the --deps, --deps-wait and
// --deps-scripts flags (potluck sandbox create and potluck run; zm imports
// them too), the acquire's deps built from the project's files read here,
// the line saying what the answer made of them, and the warning when the
// files are not what the checkout will hold.
package depscli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
)

// Flags are the flags that ask for dependency layers.
type Flags struct {
	kinds   []string // kind[:path], as given
	wait    int
	scripts string
}

// Add defines the flags on f.
func (d *Flags) Add(f *pflag.FlagSet) {
	f.StringArrayVar(&d.kinds, "deps", nil, "kind[:path]: a dependency layer ("+strings.Join(runtarget.DepsKindNames(), ", ")+") built from this directory's lockfile and package.json files and mounted at path in the sandbox (default: the checkout, --dir, plus this directory's place in its repository); needs a trusted machine (repeatable)")
	f.IntVar(&d.wait, "deps-wait", computeapi.DepsWaitDefaultS, "seconds to wait while another sandbox builds the layer; 0 takes the sandbox at once without it")
	f.StringVar(&d.scripts, "deps-scripts", "on", "off: do not run dependencies' install scripts when building the layer")
}

// Request fills req's deps from the project in dir ("" = the working
// directory), mounting a layer without its own path at DefaultPath. f is
// the flag set Add defined them on. With no --deps it changes nothing, and
// --deps-wait and --deps-scripts alone are refused.
func (d *Flags) Request(ctx context.Context, f *pflag.FlagSet, dir, dest string, req *computeapi.AcquireRunTargetRequest) error {
	if len(d.kinds) == 0 {
		for _, n := range []string{"deps-wait", "deps-scripts"} {
			if f.Changed(n) {
				return fmt.Errorf("--%s goes with --deps", n)
			}
		}
		return nil
	}
	switch d.scripts {
	case "on":
	case computeapi.DepsScriptsOff:
	default:
		return fmt.Errorf("--deps-scripts is on or off, not %q", d.scripts)
	}
	if d.wait < 0 || d.wait > computeapi.DepsWaitMaxS {
		return fmt.Errorf("--deps-wait is between 0 and %d seconds", computeapi.DepsWaitMaxS)
	}
	if len(d.kinds) > computeapi.DepsMax {
		return fmt.Errorf("--deps: at most %d layers", computeapi.DepsMax)
	}
	if dir == "" {
		dir = "."
	}
	// The project's directory is a root files cannot leave: a symlink out of
	// it is refused, never followed.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("--deps: %w", err)
	}
	defer root.Close()
	filesLeft, bytesLeft := computeapi.DepsFilesMax, computeapi.DepsBytesMax
	var out []computeapi.BoxDeps
	for _, v := range d.kinds {
		kind, path, _ := strings.Cut(v, ":")
		if path == "" {
			path = DefaultPath(ctx, dir, dest)
		}
		if !protocol.VolumePathOK(path) {
			return fmt.Errorf("--deps %q: %q is not usable as a path in the sandbox (relative to its HOME, plain characters: letters, digits and . _ - + @ /); name one: --deps %s:<path>", v, path, kind)
		}
		files, err := runtarget.CollectDepsFiles(kind, root.FS(), filesLeft, bytesLeft)
		if err != nil {
			return fmt.Errorf("--deps %s: %w", v, err)
		}
		for _, f := range files {
			filesLeft, bytesLeft = filesLeft-1, bytesLeft-len(f.Data)
		}
		bd := computeapi.BoxDeps{Kind: kind, Path: path, Files: files}
		if d.scripts == computeapi.DepsScriptsOff {
			bd.Scripts = computeapi.DepsScriptsOff
		}
		out = append(out, bd)
	}
	req.Deps = out
	req.DepsWaitS = &d.wait
	return nil
}

// DefaultPath is where a layer asked without a path is mounted: dest (the
// checkout, relative to the sandbox's HOME) plus dir's place in its
// repository, where a push puts that project's files (run from app/, the
// layer is at dest/app). An absolute dest is taken as it is.
func DefaultPath(ctx context.Context, dir, dest string) string {
	if dir == "" {
		dir = "."
	}
	if pre := boxpush.Prefix(ctx, dir); pre != "" && !strings.HasPrefix(dest, "/") {
		return strings.TrimSuffix(dest, "/") + "/" + pre
	}
	return dest
}

// Print says on w, as program prog, in one line, what the acquire's
// answer made of each layer asked for. A layer the answer does not report is said so: the
// sandbox has no layer then, and a caller must not take it for one.
func Print(w io.Writer, prog string, asked []computeapi.BoxDeps, got []computeapi.BoxDepsAnswer) {
	if len(asked) == 0 {
		return
	}
	parts := make([]string, 0, len(asked))
	for i, a := range asked {
		if i >= len(got) || got[i].Kind != a.Kind {
			parts = append(parts, fmt.Sprintf("%s at %s: not reported (not mounted: install for yourself)", a.Kind, a.Path))
			continue
		}
		g := got[i]
		switch {
		case g.Built:
			parts = append(parts, fmt.Sprintf("%s at %s: built (key %s)", g.Kind, a.Path, shortKey(g.Key)))
		case g.Hit:
			parts = append(parts, fmt.Sprintf("%s at %s: hit (key %s)", g.Kind, a.Path, shortKey(g.Key)))
		default:
			parts = append(parts, fmt.Sprintf("%s at %s: not mounted (install for yourself)", g.Kind, a.Path))
		}
	}
	fmt.Fprintf(w, "%s: deps %s\n", prog, strings.Join(parts, "; "))
}

// WarnMismatch warns on w, as program prog (whose push flags are named
// --dirty and --include-untracked, as potluck's are), when a file a layer
// was built from is not what the sandbox's checkout will hold (the commit ref, or HEAD): the layer is built
// from this directory's files, and the checkout is the commit.
func WarnMismatch(ctx context.Context, w io.Writer, prog string, dir, ref string, dirty, untracked bool, asked []computeapi.BoxDeps) {
	var paths []string
	seen := map[string]bool{}
	for _, a := range asked {
		for _, f := range a.Files {
			if !seen[f.Path] {
				seen[f.Path] = true
				paths = append(paths, f.Path)
			}
		}
	}
	if dir == "" {
		dir = "."
	}
	bad, err := boxpush.Mismatch(ctx, dir, ref, dirty, untracked, paths)
	switch {
	case err != nil: // the push says why this is no checkout
	case len(bad) > 0:
		fmt.Fprintf(w, "%s: warning: the layer is built from %s, which the checkout will not hold as it is here: its dependencies may not be the checkout's (commit them, or --dirty, with --include-untracked for a file git does not track; git-ignored files are never sent)\n", prog, strings.Join(bad, ", "))
	}
}

// shortKey is a layer's key as the line shows it.
func shortKey(k string) string { return k[:min(len(k), 12)] }
