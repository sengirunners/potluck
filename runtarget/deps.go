package runtarget

// Dependency layers (docs/daemon.md, Dependency layers; zm-eakh.12.65): an
// acquire's deps ask for a holder's sealed volume holding what a kind's
// recipe built from exactly the files sent (npm: node_modules from the
// lockfile, package.json files and .npmrc's resolution keys). The daemon names the volume from the
// sha256 of everything the build depended on, so a name is its content's,
// and a caller never names one (volumes.go refuses deps-*). The box mounts
// it cow, so nothing it writes reaches the layer or another box.
//
// When the holder has no such layer, the daemon builds it: a fresh box of
// the holder's on the same binding (so the same host, where the volume
// lives) and image, with the volume in mode build and nothing else of the
// caller's (no ref or commit, no volumes, no network setting, no secrets).
// The files go in over exec, the recipe runs, and only its exit 0 is
// followed by a release with seal; every other end deletes the volume (the
// driver's rule, capability volumes-seal), so a failed or partial build is
// never there for a reader. Another acquire wanting the same layer while it
// is built waits for it (deps_wait_s) or, with 0, takes a box without it.
//
// Layers are the holder's like any volume (volume_owner, volumes.go): one
// owner's acquire never builds or reads another's. Trusted hosts only, and
// only a binding naming its pinned images: the image digest is part of the
// key, so this daemon must know it before the box boots.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// depsPrefix starts every layer's volume name, and no caller's.
const depsPrefix = "deps-"

// depsBuildPrefix starts every builder's request_id, and no caller's: a
// builder's row is the daemon's alone, never its holder's to exec in,
// release or renew (loadHandle, rowOf), so nothing but the recipe writes the
// layer before it is sealed.
const depsBuildPrefix = "deps-build-"

// builderRequest says whether a caller's request_id is a builder's.
func builderRequest(id string) bool { return strings.HasPrefix(id, depsBuildPrefix) }

// builderRow says whether a row is a builder's (its key is actorKey's
// "<tag>-<request_id>").
func builderRow(rw row) bool {
	_, id, _ := splitActorKey(rw.RequestID)
	return builderRequest(id)
}

// depsActiveMax is how many acquires with deps one caller may have in this
// daemon at once: each holds its files while it builds or waits.
const depsActiveMax = 8

// depsBodyMax bounds an acquire's body: the layers' files, base64 in JSON.
const depsBodyMax = computeapi.DepsBytesMax/3*4 + 4<<20

// depsBuildTTLS is a builder's lease: the files, then one exec of the
// recipe, which may take exec's longest timeout.
const depsBuildTTLS = execMaxS + 300

// forwardedDeps is how many layers an acquire body a member's proxy
// forwards asks for, and its deps_wait_s (relay_routes.go): 0 layers for
// any other call, or a body that is not an acquire's JSON (the authority
// refuses that itself).
func forwardedDeps(method, path string, body []byte) (n, waitS int) {
	if method != "POST" || !strings.HasPrefix(path, "/v0/run_targets/") || !strings.HasSuffix(path, "/acquire") {
		return 0, 0
	}
	var req struct {
		Deps      []json.RawMessage `json:"deps"`
		DepsWaitS *int              `json:"deps_wait_s"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Deps) == 0 {
		return 0, 0
	}
	return min(len(req.Deps), computeapi.DepsMax), min(max(depsWaitS(req.DepsWaitS), 0), computeapi.DepsWaitMaxS)
}

// depsWaitS is an acquire's deps_wait_s, or the default when it names none.
func depsWaitS(p *int) int {
	if p == nil {
		return computeapi.DepsWaitDefaultS
	}
	return *p
}

// DepsAnswerWithin is how much longer than a plain acquire one with n
// layers may take to answer (acquireDeps): its wait for another box's
// build, and for each layer up to two builds of its own (built again once
// when it is evicted before the box starts), each a builder's acquire, its
// files, its recipe, its release and the box asked for again after it,
// every one relayed to the host.
func DepsAnswerWithin(n, waitS int) time.Duration {
	if n == 0 {
		return 0
	}
	build := time.Duration(depsBuildTTLS)*time.Second + 2*(rt.AcquireLimit+rt.RelaySlack) + 2*(rt.VerbLimit+rt.RelaySlack) +
		2*(rt.AcquireLimit+rt.RelaySlack)
	return time.Duration(waitS)*time.Second + time.Duration(2*n)*build
}

// depsKind is one row of the kinds table: which files a layer of the kind
// is built from, and the recipe that builds it in that directory.
type depsKind struct {
	// files refuses a set of files (path -> bytes) that is not exactly the
	// kind's inputs.
	files func(files map[string][]byte) error
	// recipe is the sh script run in the layer's directory; scripts false
	// skips dependencies' install scripts. Its text is part of the key.
	recipe func(scripts bool) string
	// inputs names the files (relative paths) a project holds for the kind,
	// given a reader of its files: what a caller must send (CollectDepsFiles).
	inputs func(read func(path string) ([]byte, error)) ([]string, error)
	// canon is a checked file as the builder gets it and the key holds it,
	// nil to drop the file (npm: .npmrc down to its key=value lines, none
	// when it has none); nil canon sends every file as it came.
	canon func(path string, data []byte) ([]byte, error)
	// keep is all a built layer holds, given its checked files: the paths
	// (relative to the layer's directory) the builder's release keeps when
	// it seals (capability volumes-keep). Nothing else the recipe or an
	// install script wrote (a .git, a config file, the files sent) reaches
	// a later box.
	keep func(files map[string][]byte) []string
}

// depsKinds is the one table of layer kinds. Go is not one: the shelf
// already saves its downloads, and a build cache has weak keys.
var depsKinds = map[string]depsKind{
	"npm": {files: npmDepsFiles, recipe: npmRecipe, inputs: npmDepsInputs, keep: npmKeep, canon: npmCanon},
}

// npmRecipe links what the shelf holds into the builder's npm cache (when
// the shelf is mounted), installs exactly the lockfile without running any
// script, then runs install scripts with npm rebuild. The root package's
// own lifecycle scripts are not run; npm rebuild may run a workspace
// package's own install scripts, which fail here when they need the
// project's sources (only package.json files are sent): scripts off then.
func npmRecipe(scripts bool) string {
	r := "if [ -x /opt/potluck-cache/bin/potluck-deps ]; then /opt/potluck-cache/bin/potluck-deps link . || true; fi\nnpm ci --ignore-scripts --no-audit --no-fund"
	if scripts {
		r += " && npm rebuild"
	}
	return r + "\n"
}

// npmDepsFiles: package-lock.json (lockfile version 2 or 3), package.json,
// the package.json of every package the lockfile keeps in the project (its
// workspaces and local links), and .npmrc if the project has one
// (deps_npmrc.go: resolution keys only), none other.
func npmDepsFiles(files map[string][]byte) error {
	lock, ok := files["package-lock.json"]
	if !ok {
		return fmt.Errorf("an npm layer needs package-lock.json")
	}
	want, err := npmWantedFiles(lock)
	if err != nil {
		return err
	}
	for p, b := range files {
		if p == npmrcFile {
			if _, err := npmrcCanon(b); err != nil {
				return err
			}
			continue
		}
		if !want[p] {
			return fmt.Errorf("file %s is not one an npm layer is built from (package-lock.json, package.json, each workspace's package.json, and .npmrc)", p)
		}
	}
	var missing []string
	for p := range want {
		if _, ok := files[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the lockfile's packages need %s", strings.Join(missing, ", "))
	}
	return nil
}

// npmWantedFiles is the set of paths an npm layer is built from, read off
// its lockfile: the lockfile, package.json, and each local package's.
func npmWantedFiles(lock []byte) (map[string]bool, error) {
	var lf struct {
		LockfileVersion int                        `json:"lockfileVersion"`
		Packages        map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(lock, &lf); err != nil {
		return nil, fmt.Errorf("package-lock.json is not JSON: %v", err)
	}
	if lf.LockfileVersion < 2 || lf.Packages == nil {
		return nil, fmt.Errorf("package-lock.json is lockfile version %d: an npm layer needs version 2 or 3 (npm 7 or later)", lf.LockfileVersion)
	}
	want := map[string]bool{"package-lock.json": true, "package.json": true}
	for k := range lf.Packages {
		if k == "" || k == "node_modules" || strings.HasPrefix(k, "node_modules/") || strings.Contains(k, "/node_modules/") {
			continue // the root, and installed packages
		}
		if !depsPathOK(k) {
			return nil, fmt.Errorf("package-lock.json names package %q outside the project: an npm layer is built from the project's files only", k)
		}
		want[k+"/package.json"] = true
	}
	return want, nil
}

// npmCanon is .npmrc's key=value lines (npmrcCanon), nil when it has none;
// every other file as sent.
func npmCanon(p string, b []byte) ([]byte, error) {
	if p != npmrcFile {
		return b, nil
	}
	c, err := npmrcCanon(b)
	if err != nil || len(c) == 0 {
		return nil, err
	}
	return c, nil
}

// npmKeep is the node_modules trees an npm layer holds: the root's and each
// local package's (its workspaces and local links), where npm ci put them.
// files passed npmDepsFiles.
func npmKeep(files map[string][]byte) []string {
	out := []string{"node_modules"}
	for p := range files {
		if dir, ok := strings.CutSuffix(p, "/package.json"); ok {
			out = append(out, dir+"/node_modules")
		}
	}
	slices.Sort(out)
	return out
}

// npmDepsInputs lists the files npmDepsFiles takes, by reading the lockfile,
// and .npmrc when the project has one.
func npmDepsInputs(read func(string) ([]byte, error)) ([]string, error) {
	lock, err := read("package-lock.json")
	if err != nil {
		return nil, err
	}
	want, err := npmWantedFiles(lock)
	if err != nil {
		return nil, err
	}
	switch _, err := read(npmrcFile); {
	case err == nil:
		want[npmrcFile] = true
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%s: %w", npmrcFile, err)
	}
	return slices.Sorted(maps.Keys(want)), nil
}

// canonFiles is files as kind k's builder gets them (depsKind.canon), in
// their order; a file canon drops is left out.
func canonFiles(k depsKind, files []computeapi.DepsFile) ([]computeapi.DepsFile, error) {
	if k.canon == nil {
		return files, nil
	}
	out := make([]computeapi.DepsFile, 0, len(files))
	for _, f := range files {
		b, err := k.canon(f.Path, f.Data)
		if err != nil {
			return nil, err
		}
		if b != nil {
			out = append(out, computeapi.DepsFile{Path: f.Path, Data: b})
		}
	}
	return out, nil
}

// depsPathOK is the rule for a layer's file path: relative, clean, no ".."
// or empty segment, nothing a shell or a log line would split on.
func depsPathOK(p string) bool {
	return len(p) <= 512 && fs.ValidPath(p) && p != "." && !strings.ContainsAny(p, "\x00\\\r\n\t")
}

// depsLayer is one layer an acquire asks for, checked, with its key.
type depsLayer struct {
	kind, path string
	scripts    bool
	files      []computeapi.DepsFile // sorted by path
	recipe     string
	keep       []string // what its builder's release keeps (depsKind.keep)
	key        string   // sha256 hex
}

// name is the layer's volume: deps-<kind>-<52 hex of the key>.
func (l depsLayer) name() string { return depsPrefix + l.kind + "-" + l.key[:52] }

// depsKey is the sha256 over everything a build depends on: the format,
// kind, recipe, image digest, mount path, scripts setting, and each file's
// path and bytes (as canon left them: an .npmrc's key=value lines), every
// field length-prefixed so no two inputs meet. The format is 4 since an
// npm layer takes the project's .npmrc (deps_npmrc.go): a layer built
// before then, without it, is never found.
func depsKey(kind, recipe, digest, path string, scripts bool, files []computeapi.DepsFile) string {
	h := sha256.New()
	put := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	sc := "on"
	if !scripts {
		sc = computeapi.DepsScriptsOff
	}
	for _, f := range []string{"potluck-deps/4", kind, recipe, digest, path, sc} {
		put([]byte(f))
	}
	for _, f := range files {
		put([]byte(f.Path))
		put(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// depsLayers checks an acquire's deps and keys them by digest, the image
// the box boots.
func depsLayers(req computeapi.AcquireRunTargetRequest, digest string) ([]depsLayer, error) {
	bad := func(f string, a ...any) error { return api.Errorf(api.CodeBadRequest, f, a...) }
	switch {
	case len(req.Deps) == 0:
		return nil, bad("deps_wait_s is for an acquire with deps")
	case len(req.Deps) > computeapi.DepsMax:
		return nil, bad("at most %d dependency layers", computeapi.DepsMax)
	case len(req.Volumes)+len(req.Deps) > computeapi.VolumesMax:
		return nil, bad("at most %d volumes and dependency layers together", computeapi.VolumesMax)
	case req.DepsWaitS != nil && (*req.DepsWaitS < 0 || *req.DepsWaitS > computeapi.DepsWaitMaxS):
		return nil, bad("deps_wait_s is between 0 and %d", computeapi.DepsWaitMaxS)
	case checkoutLayer(req) && len(req.Deps) > 1:
		// The checkout goes into the layer's directory (checkoutLayer), and
		// layers never nest: a second one could not sit inside it.
		return nil, bad("a box with a ref takes one dependency layer, which holds its checkout")
	case checkoutLayer(req) && req.DepsWaitS != nil && *req.DepsWaitS == 0:
		// A box without its layer would hold the checkout elsewhere (the
		// driver's own place): the same acquire, code at another path.
		return nil, bad("a box with a ref waits for its layer: deps_wait_s 0 would put its checkout elsewhere")
	}
	nfiles, nbytes := 0, 0
	var out []depsLayer
	for _, d := range req.Deps {
		k, ok := depsKinds[d.Kind]
		if !ok {
			return nil, bad("no dependency layer kind %q (npm is the one)", d.Kind)
		}
		if !protocol.VolumePathOK(d.Path) {
			return nil, bad("deps %s: path %q is not a volume path (relative to HOME, plain segments)", d.Kind, d.Path)
		}
		for _, v := range req.Volumes {
			if protocol.VolumePathsOverlap(d.Path, v.Path) {
				return nil, bad("deps %s at %s overlaps volume %s at %s", d.Kind, d.Path, v.Name, v.Path)
			}
		}
		for _, o := range out {
			if protocol.VolumePathsOverlap(d.Path, o.path) {
				return nil, bad("deps at %s and %s overlap", d.Path, o.path)
			}
		}
		if d.Scripts != "" && d.Scripts != computeapi.DepsScriptsOff {
			return nil, bad("deps %s: scripts is %q or absent", d.Kind, computeapi.DepsScriptsOff)
		}
		files := map[string][]byte{}
		for _, f := range d.Files {
			if !depsPathOK(f.Path) {
				return nil, bad("deps %s: file path %q is not relative and clean", d.Kind, f.Path)
			}
			if _, dup := files[f.Path]; dup {
				return nil, bad("deps %s: file %s is named twice", d.Kind, f.Path)
			}
			files[f.Path] = f.Data
			nfiles, nbytes = nfiles+1, nbytes+len(f.Data)
		}
		if nfiles > computeapi.DepsFilesMax || nbytes > computeapi.DepsBytesMax {
			return nil, api.Errorf(api.CodeTooLarge, "dependency layers' files: at most %d files and %d MiB in all", computeapi.DepsFilesMax, computeapi.DepsBytesMax>>20)
		}
		if err := k.files(files); err != nil {
			return nil, bad("deps %s at %s: %v", d.Kind, d.Path, err)
		}
		sent, err := canonFiles(k, d.Files)
		if err != nil {
			return nil, bad("deps %s at %s: %v", d.Kind, d.Path, err)
		}
		l := depsLayer{kind: d.Kind, path: d.Path, scripts: d.Scripts != computeapi.DepsScriptsOff, files: slices.Clone(sent)}
		slices.SortFunc(l.files, func(a, b computeapi.DepsFile) int { return strings.Compare(a.Path, b.Path) })
		l.recipe = k.recipe(l.scripts)
		l.keep = k.keep(files)
		if err := protocol.KeepPathsOK(l.keep); err != nil { // the driver's rule for what a release keeps
			return nil, bad("deps %s at %s: a layer keeps its packages' node_modules, and %v (a package directory of plain characters: letters, digits and . _ - + @ /, within %d bytes)", d.Kind, d.Path, err, 256-len("/node_modules"))
		}
		l.key = depsKey(l.kind, l.recipe, digest, l.path, l.scripts, l.files)
		out = append(out, l)
	}
	return out, nil
}

// depsDigest is the digest of the image req's box boots on b, the one its
// layers' keys hold: the binding's pin, or on a binding another machine
// serves, the pin its host advertises for it (HostAdverts). The box and the
// builder must boot exactly that image (daemonVols.digest), so an advert
// gone stale never keys a layer with an image it was not built on.
func (s *Service) depsDigest(ctx context.Context, b computeapi.RunTarget, req computeapi.AcquireRunTargetRequest) (string, error) {
	if b.Host != "" {
		ra, ok := s.advertOf(ctx, b)
		if !ok {
			return "", api.Errorf(api.CodeUnavailable, "%s advertises no run target %s now (is it running and sharing it with this project?)", b.Host, b.HostBinding)
		}
		if req.Image != "" { // as acquireBox's boxPin refuses it: the host's driver picks by template only
			return "", api.Errorf(api.CodeBadRequest, "run target %s names no images: on a binding another machine serves, name a template", b.Name)
		}
		b.Images = ra.Describe.Images // pins only (cleanAdverts)
		if len(b.Images) == 0 {
			return "", api.Errorf(api.CodeBadRequest, "run target %s: %s advertises no pinned images, and a dependency layer's key holds the image digest", b.Name, b.Host).
				WithHint("its potluck is older than dependency layers on shared bindings: update it")
		}
	}
	pin, _, err := boxPin(b, req)
	if err != nil {
		return "", err
	}
	digest := pinDigest(pin)
	if digest == "" {
		return "", api.Errorf(api.CodeBadRequest, "run target %s names no pinned image: a dependency layer's key holds the image digest", b.Name)
	}
	return digest, nil
}

// acquireDeps is an acquire with deps: the box with each layer mounted cow,
// each built first when the holder has none.
func (s *Service) acquireDeps(ctx context.Context, t core.Target, b computeapi.RunTarget, db *sql.DB, req computeapi.AcquireRunTargetRequest) (any, error) {
	switch {
	case b.Kind != computeapi.RunTargetKindBox:
		return nil, api.Errorf(api.CodeBadRequest, "deps are for a box target")
	case req.Persist || req.JITConfig != "":
		return nil, api.Errorf(api.CodeBadRequest, "a box with deps is neither persistent nor a GitHub runner's")
	}
	if err := s.hostTrusted(ctx, b, "dependency layer"); err != nil {
		return nil, err
	}
	digest, err := s.depsDigest(ctx, b, req)
	if err != nil {
		return nil, err
	}
	layers, err := depsLayers(req, digest)
	if err != nil {
		return nil, err
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return nil, err
	}
	for _, c := range []string{protocol.CapVolumes, protocol.CapVolumesSeal, protocol.CapVolumesKeep, computeapi.EncodingBase64} {
		if !slices.Contains(d.Capabilities, c) {
			return nil, api.Errorf(api.CodeBadRequest, "run target %s builds no dependency layers (potluck-driver-%s lists no %s capability)", b.Name, b.Driver, c)
		}
	}
	wait := depsWaitS(req.DepsWaitS)
	deadline := s.now().Add(time.Duration(wait) * time.Second)
	keys := make([]string, len(layers))
	for i, l := range layers {
		keys[i] = l.kind + ":" + l.key
	}
	sum := strings.Join(keys, ",")
	who := core.SubjectOf(ctx).Address + "\x00" + cmp.Or(req.Actor, core.SubjectOf(ctx).Address)
	s.depsMu.Lock()
	if s.depsActive == nil {
		s.depsActive = map[string]int{}
	}
	if s.depsActive[who] >= depsActiveMax {
		s.depsMu.Unlock()
		return nil, api.Errorf(api.CodeUnavailable, "at most %d acquires with deps at once, each", depsActiveMax)
	}
	s.depsActive[who]++
	s.depsMu.Unlock()
	defer func() {
		s.depsMu.Lock()
		if s.depsActive[who]--; s.depsActive[who] == 0 {
			delete(s.depsActive, who)
		}
		s.depsMu.Unlock()
	}()
	box := req
	box.Deps, box.DepsWaitS = nil, nil
	plain := daemonVols{sum: sum + ";without"} // the box without its layers (deps_wait_s 0): another acquire
	built := make([]int, len(layers))          // builds this acquire made, per layer
	sealedAll := false                         // the last round found every layer sealed
	for {
		vols := make([]computeapi.BoxVolume, len(layers))
		for i, l := range layers {
			vols[i] = computeapi.BoxVolume{Name: l.name(), Path: l.path, Mode: computeapi.VolumeCOW, Sealed: true, Checkout: checkoutLayer(req)}
		}
		res, err := s.acquireBox(ctx, t, b, db, box, daemonVols{vols: vols, sum: sum, digest: digest})
		if wait == 0 && api.IsCode(err, api.CodeConflict) {
			// This request_id may have been answered a box without the
			// layers: a repeat answers that box again.
			if res, perr := s.acquireBox(ctx, t, b, db, box, plain); perr == nil {
				return withDeps(res, layers, built, false), nil
			}
			return nil, err
		}
		if !api.IsCode(err, api.CodeNotFound) {
			if err != nil {
				return nil, err
			}
			return withDeps(res, layers, built, true), nil
		}
		// Not found: a layer is not sealed (none yet, being built, or
		// evicted since). Ask for each in mode build: a sealed one refuses
		// with conflict, one being built with unavailable building.
		progress := false
		for i, l := range layers {
			if built[i] == 2 { // built, then built again once it was gone
				return nil, api.Errorf(api.CodeUnavailable, "dependency layer %s was built twice and was gone again before its box started (evicted: are the holder's volumes over their cap?)", l.key)
			}
			ok, busy, berr := s.buildLayer(ctx, t, b, db, req, l, digest)
			switch {
			case berr != nil:
				return nil, berr
			case ok:
				built[i]++
				progress = true
			case busy > 0 && wait == 0: // the box without the layers, now
				res, err := s.acquireBox(ctx, t, b, db, box, plain)
				if err != nil {
					return nil, err
				}
				return withDeps(res, layers, built, false), nil
			case busy > 0:
				if !s.now().Add(busy).Before(deadline) {
					return nil, api.Errorf(api.CodeUnavailable, "dependency layer %s is still being built by another box after deps_wait_s %d", l.key, wait)
				}
				if err := sleepCtx(ctx, busy); err != nil {
					return nil, err
				}
				progress = true
			}
		}
		if !progress {
			if sealedAll { // every build was refused twice: sealed, or a volume of the name that is not
				var e *api.Error
				if errors.As(err, &e) {
					return nil, e.WithHint("a dependency layer's volume that is there but not sealed (left from a build that did not finish) stays until it is evicted")
				}
				return nil, err
			}
			sealedAll = true // sealed between the two asks: ask again once
			continue
		}
		sealedAll = false
	}
}

// checkoutLayer says whether an acquire's layer holds its checkout: with a
// ref (whose commit the daemon resolves, routing.go), the driver checks the
// commit out into the layer's directory (depsLayers allows one layer then),
// so code and node_modules sit at the paths the layer was built at; the
// layer's files must be the commit's root ones. Without one, the caller
// puts code there itself.
func checkoutLayer(req computeapi.AcquireRunTargetRequest) bool { return req.Ref != "" }

// withDeps adds the answer's deps to an acquire's handle.
func withDeps(res any, layers []depsLayer, built []int, mounted bool) any {
	h, ok := res.(computeapi.RunTargetHandle)
	if !ok {
		return res
	}
	for i, l := range layers {
		h.Deps = append(h.Deps, computeapi.BoxDepsAnswer{Kind: l.kind, Key: l.key, Hit: mounted && built[i] == 0, Built: mounted && built[i] > 0})
	}
	return h
}

// buildLayer asks for a builder of l and, given one, builds l in it: ok
// once l is sealed. busy is how long to wait when another box is building
// it. Neither: l is there (its build acquire found it sealed).
func (s *Service) buildLayer(ctx context.Context, t core.Target, b computeapi.RunTarget, db *sql.DB, req computeapi.AcquireRunTargetRequest, l depsLayer, digest string) (ok bool, busy time.Duration, err error) {
	var n [8]byte
	_, _ = rand.Read(n[:])
	// Nothing of the caller's but who they are (the holder, so the
	// layer's owner) and the image: no ref or commit, no volumes, no
	// network setting, no deps; exec below carries no secrets.
	breq := computeapi.AcquireRunTargetRequest{RequestID: depsBuildPrefix + hex.EncodeToString(n[:]), Actor: req.Actor,
		Image: req.Image, Template: req.Template, TTLS: depsBuildTTLS}
	if d, err := s.describeOf(ctx, b); err == nil {
		if c := lifetimeCap(b, d); c > 0 && breq.TTLS > c {
			breq.TTLS = c
		}
	}
	vol := computeapi.BoxVolume{Name: l.name(), Path: l.path, Mode: protocol.VolumeBuild}
	res, err := s.acquireBox(ctx, t, b, db, breq, daemonVols{vols: []computeapi.BoxVolume{vol}, sum: "build:" + l.key, digest: digest})
	var e *api.Error
	switch {
	case err == nil:
	case errors.As(err, &e) && e.Code == api.CodeConflict:
		return false, 0, nil // sealed already
	case errors.As(err, &e) && e.Code == api.CodeUnavailable && len(e.Data) > 0:
		var bz protocol.Busy
		if json.Unmarshal(e.Data, &bz) == nil && bz.Reason == protocol.ReasonBuilding {
			return false, s.depsRetry(bz.RetryS), nil
		}
		return false, 0, err
	default:
		return false, 0, err
	}
	h, okh := res.(computeapi.RunTargetHandle)
	if !okh {
		return false, 0, api.Errorf(api.CodeInternal, "the builder of dependency layer %s answered no handle", l.key)
	}
	// The build runs to its end though the caller goes away: other acquires
	// may be waiting for it, and the builder must be released either way.
	bctx := context.WithoutCancel(ctx)
	why := s.runBuild(bctx, b, h, l)
	var args any
	if why == "" { // only after the recipe exited 0; the driver deletes all but l.keep before it seals
		args = map[string]any{"seal": true, "keep": l.keep}
	}
	if rerr := s.releaseRow(bctx, t.Project.ID, db, b, h.Handle, h.RequestID, args); rerr != nil && why == "" {
		if api.IsCode(rerr, api.CodeBadRequest) { // the driver refused the seal's args: end the builder anyway, which deletes the volume
			_ = s.releaseRow(bctx, t.Project.ID, db, b, h.Handle, h.RequestID, nil)
		}
		return false, 0, api.Errorf(api.CodeUnavailable, "dependency layer %s: the builder %s was not released to seal it: %s", l.key, h.Handle, errText(rerr))
	}
	if why != "" {
		return false, 0, api.Errorf(api.CodeConflict, "dependency layer %s (%s at %s) did not build: %s", l.key, l.kind, l.path, why)
	}
	return true, 0, nil
}

// runBuild writes l's files into the builder's layer directory and runs the
// recipe there: "" when it exited 0, else why not.
func (s *Service) runBuild(ctx context.Context, b computeapi.RunTarget, h computeapi.RunTargetHandle, l depsLayer) string {
	left := depsBuildTTLS
	if exp, err := time.Parse(time.RFC3339Nano, h.ExpiresAt); err == nil {
		left = int(time.Until(exp) / time.Second)
	}
	exec := func(ctx context.Context, x computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		timeout := min(cmp.Or(x.TimeoutS, execDefaultS), execMaxS, max(left-30, 1))
		args := map[string]any{"argv": x.Argv, "timeout_s": timeout}
		if x.Stdin != "" {
			args["stdin"] = x.Stdin
		}
		if x.Encoding != "" {
			args["encoding"] = x.Encoding
		}
		var out computeapi.RunTargetExec
		err := s.callWithin(ctx, b, verbExec, h.Handle, args, &out, time.Duration(timeout+30)*time.Second)
		capExec(&out)
		return out, err
	}
	files := make([]boxcp.TreeFile, len(l.files))
	for i, f := range l.files {
		files[i] = boxcp.TreeFile{Path: f.Path, Data: f.Data}
	}
	if _, err := boxcp.WriteTree(ctx, exec, l.path, files, boxcp.Options{Max: computeapi.DepsBytesMax + 4<<20}); err != nil {
		return "writing its files: " + errText(err)
	}
	x, err := exec(ctx, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "cd \"$HOME/$1\" || exit 1\n" + l.recipe, "sh", l.path}, TimeoutS: execMaxS})
	switch {
	case err != nil:
		return "its recipe: " + errText(err)
	case x.Exit != 0:
		return fmt.Sprintf("its recipe exited %d: %s", x.Exit, tail(x.Stderr, x.Encoding))
	}
	return ""
}

// tail is the end of a recipe's stderr, one line, for a refusal.
func tail(stderr, encoding string) string {
	if encoding == computeapi.EncodingBase64 {
		if b, err := base64.StdEncoding.DecodeString(stderr); err == nil {
			stderr = string(b)
		}
	}
	stderr = strings.TrimSpace(stderr)
	if len(stderr) > 600 {
		stderr = "..." + stderr[len(stderr)-600:]
	}
	return oneLine(string(bytes.ToValidUTF8([]byte(stderr), []byte("?"))))
}

// depsRetry is how long to wait before asking again for a layer another
// box is building: the driver's retry_s, within 1 to 30 seconds.
func (s *Service) depsRetry(retryS int) time.Duration {
	if s.depsPoll > 0 {
		return s.depsPoll
	}
	return time.Duration(min(max(retryS, 1), 30)) * time.Second
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

// busyData is an unavailable driver answer's data when it is a
// protocol.Busy for a volume being built, re-encoded; nil otherwise. Nothing
// else a driver puts in data reaches a caller.
func busyData(raw json.RawMessage) json.RawMessage {
	var bz protocol.Busy
	if len(raw) == 0 || json.Unmarshal(raw, &bz) != nil || bz.Reason != protocol.ReasonBuilding {
		return nil
	}
	out, _ := json.Marshal(protocol.Busy{Reason: bz.Reason, RetryS: min(max(bz.RetryS, 0), 3600)})
	return out
}
