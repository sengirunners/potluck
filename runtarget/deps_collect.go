package runtarget

// The caller's half of the kinds table (deps.go): which files of a project a
// layer's kind takes, read so that a project's symlinks cannot carry a file
// from outside it into an acquire.

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
)

// DepsKindNames lists the layer kinds, for a caller's usage text.
func DepsKindNames() []string {
	var out []string
	for k := range depsKinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// CollectDepsFiles reads the files a layer of kind is built from out of
// root (the project's directory, as an fs.FS that must not leave it: use
// os.Root's FS) and checks them as the daemon will. Only regular files are
// read. The daemon's limits (computeapi.DepsFilesMax and DepsBytesMax) hold
// all of a box's layers together, so the caller passes what its earlier
// layers left. The files come back as the builder gets them (depsKind.canon).
func CollectDepsFiles(kind string, root fs.FS, filesLeft, bytesLeft int) ([]computeapi.DepsFile, error) {
	k, ok := depsKinds[kind]
	if !ok {
		return nil, fmt.Errorf("no dependency layer kind %q (%s)", kind, strings.Join(DepsKindNames(), ", "))
	}
	used := 0
	got := map[string][]byte{}
	tooBig := fmt.Errorf("the layers' files are at most %d MiB in all", computeapi.DepsBytesMax>>20)
	read := func(p string) ([]byte, error) {
		if b, ok := got[p]; ok {
			return b, nil
		}
		if !depsPathOK(p) {
			return nil, fmt.Errorf("%q is not a plain relative path", p)
		}
		fi, err := fs.Stat(root, p)
		switch {
		case err != nil:
			return nil, err
		case !fi.Mode().IsRegular():
			return nil, fmt.Errorf("%s is not a regular file", p)
		case fi.Size() > int64(bytesLeft-used):
			return nil, tooBig
		}
		b, err := fs.ReadFile(root, p)
		switch {
		case err != nil:
			return nil, err
		case len(b) > bytesLeft-used: // grew since the stat
			return nil, tooBig
		}
		used += len(b)
		got[p] = b
		return b, nil
	}
	paths, err := k.inputs(read)
	if err != nil {
		return nil, err
	}
	if len(paths) > filesLeft {
		return nil, fmt.Errorf("%d files: the layers' files are at most %d in all", len(paths), computeapi.DepsFilesMax)
	}
	files := make([]computeapi.DepsFile, 0, len(paths))
	for _, p := range paths {
		b, err := read(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		files = append(files, computeapi.DepsFile{Path: p, Data: b})
	}
	if err := k.files(got); err != nil {
		return nil, err
	}
	return canonFiles(k, files) // an .npmrc's comments never leave this machine
}
