package hostprobe

import (
	"context"
	"github.com/sengirunners/potluck/computeclient"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// withPins swaps zm's default images for the test's.
func withPins(t *testing.T, pins ...DefaultPin) {
	t.Helper()
	old := pinTable
	pinTable = func() []DefaultPin { return pins }
	t.Cleanup(func() { pinTable = old })
}

// withTartImages supplies compatibility facts for the tests' fake VM images.
func withTartImages(t *testing.T, images ...string) {
	t.Helper()
	pins := pinTable()
	for _, image := range images {
		pins = append(pins, DefaultPin{Layer: "tart", OS: "linux", Image: image, Arch: "arm64", MinHostMacOS: 13, MinFreeGB: 60})
	}
	withPins(t, pins...)
}

// The shipped images.pin: every pin a full digest (or "-", unpublished), one
// line for each layer share can bind, and a Mac lends macOS by default.
func TestDefaultPins(t *testing.T) {
	pins, err := ParsePins(imagesPin)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]DefaultPin{}
	for _, p := range pins {
		if p.Image != "" && !Pinned(p.Image) {
			t.Errorf("%s %s: %q is not a full digest", p.Layer, p.OS, p.Image)
		}
		if _, ok := have[p.Layer]; !ok && p.Template == "" && p.Name == "" {
			have[p.Layer] = p
		}
		if p.Template != "" {
			have[p.Layer+" "+p.Template] = p
		}
	}
	for layer, ok := range Supported {
		if ok && have[layer].Layer == "" {
			t.Errorf("images.pin has no line for %s, a layer share binds", layer)
		}
	}
	// Cirrus Labs' macOS base: its Tart config's cpuCountMin 2, memorySizeMin 4 GiB.
	if d := have["tart"]; d.OS != "darwin" || d.Image == "" || d.MinCPU != 2 || d.MinMemoryMB != 4096 {
		t.Errorf("tart's default: %+v, want a pinned macOS image with its 2 vCPU, 4096 MB floor", d)
	}
	if d := have["oci"]; d.OS != "linux" {
		t.Errorf("oci's default: %+v, want Linux", d)
	}
	// Each default carries GitHub's runner where its image keeps it (zm-eakh.12.24.5): Cirrus Labs' in
	// ~admin/actions-runner, zm's Linux image (GitHub's runner image) in /home/runner.
	if have["tart"].RunnerDir != "actions-runner" || have["oci"].RunnerDir != "/home/runner" {
		t.Errorf("runner dirs: tart %q, oci %q", have["tart"].RunnerDir, have["oci"].RunnerDir)
	}
	if p, err := ParsePins("oci linux - runner_dir=/home/runner min_cpu=1"); err != nil || p[0].RunnerDir != "/home/runner" || p[0].MinCPU != 1 {
		t.Errorf("runner_dir key: %+v %v", p, err)
	}
	// zm's coding-agent image (zm-eakh.12.5.20): a template line beside oci's default, built FROM it, so it keeps
	// its runner. A plain line and template lines of one layer and OS live side by side.
	named := func(image, name string) bool {
		return image == "" || strings.HasPrefix(image, name+"@")
	}
	if a := have["oci agent"]; a.OS != "linux" || a.RunnerDir != "/home/runner" || !named(a.Image, "ghcr.io/sengirunners/potluck-box-agent") {
		t.Errorf("oci's agent template: %+v", a)
	}
	if got := computeapi.BoxTemplates(); !slices.Equal(got, []string{"agent", "browser"}) {
		t.Errorf("published templates: %v", got)
	}
	const browserPin = "ghcr.io/sengirunners/potluck-box-browser@sha256:28b009beab77de08afbc02a1205085a4e595191f32198e4c8df3e164583b487e"
	if b := have["oci browser"]; b.OS != "linux" || b.Image != browserPin || b.RunnerDir != "/home/runner" {
		t.Errorf("oci's browser template: %+v", b)
	}
	// Every template line is one zm's API knows (api.BoxTemplateImage: acquire picks a binding's image by
	// that name), and a published one pins that very image.
	for _, p := range pins {
		if p.Template == "" {
			continue
		}
		name, ok := computeapi.BoxTemplateImage(p.Template)
		if !ok || !named(p.Image, name) {
			t.Errorf("template %s: images.pin pins %q, the API names %q (known %v)", p.Template, p.Image, name, ok)
		}
	}
	if have["oci"].Image == "" || !named(have["oci"].Image, "ghcr.io/sengirunners/potluck-box-linux") {
		t.Errorf("oci's default is %q, want potluck-box-linux", have["oci"].Image)
	}
	if p, err := ParsePins("oci linux -\noci linux - template=agent\noci linux - template=a-2"); err != nil || len(p) != 3 || p[1].Template != "agent" || p[2].Template != "a-2" {
		t.Errorf("template key: %+v %v", p, err)
	}
	for name, bad := range map[string]string{
		"tag":        "tart darwin ghcr.io/x/macos:latest",
		"short":      "oci linux x@sha256:abc",
		"upper":      "oci linux x@sha256:" + strings.Repeat("A", 64),
		"bare name":  "oci linux ghcr.io/sengirunners/potluck-box-linux",
		"no image":   "oci linux",
		"layer":      "lxc linux -",
		"os":         "oci windows -",
		"twice":      "oci linux -\noci linux -",
		"floor key":  "tart darwin -  min_disk=5",
		"floor 0":    "tart darwin - min_cpu=0",
		"floor nan":  "tart darwin - min_memory_mb=lots",
		"runner $":   "oci linux - runner_dir=$(id)",
		"runner '":   "oci linux - runner_dir=a'b",
		"runner -":   "oci linux - runner_dir=-x",
		"runner ''":  "oci linux - runner_dir=",
		"tmpl ''":    "oci linux - template=",
		"tmpl case":  "oci linux - template=Agent",
		"tmpl $":     "oci linux - template=$(id)",
		"tmpl -":     "oci linux - template=-x",
		"tmpl twice": "oci linux - template=agent\noci linux - template=agent",
		"tmpl key2":  "oci linux - template=agent template=b",
	} {
		if _, err := ParsePins(bad); err == nil {
			t.Errorf("%s: parsed %q", name, bad)
		}
	}
}

// Without --image, share takes the layer's default; --image still wins.
func TestShareDefaultImagePerLayer(t *testing.T) {
	withPins(t, DefaultPin{Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "darwin", Image: pinB}, DefaultPin{Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "linux", Image: pinA}, DefaultPin{Layer: "oci", OS: "linux", Image: pinA})
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		report Report
		images []string
		want   string
		pull   string
		dflt   bool
	}{
		{"linux oci", bigReport(), nil, pinA, "docker pull -- " + pinA, true},
		{"mac tart", macReport(), nil, pinB, "tart pull -- " + pinB, true},
		{"--image wins", bigReport(), []string{pinB}, pinB, "docker pull -- " + pinB, false},
		{"--image wins on tart", macReport(), []string{pinA}, pinA, "tart pull -- " + pinA, false},
	} {
		var said []string
		run := &fakeRunner{}
		res, err := shareOn(ctx, run, nil, ShareOptions{Report: c.report, Images: c.images, NamePrefix: "h", Sizes: []string{"c2m4"},
			DryRun: true, Progress: func(s string) { said = append(said, s) }})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(res.Images) != 1 || res.Images[0] != c.want || res.DefaultImage != c.dflt || len(res.Pulls) != 1 ||
			strings.Join(res.Pulls[0].Command, " ") != c.pull || len(run.calls) != 0 {
			t.Errorf("%s: images %v default %v pulls %+v", c.name, res.Images, res.DefaultImage, res.Pulls)
		}
		if conf := strings.Join(res.Sizes[0].Conformance, " "); !strings.Contains(conf, "--config images="+c.want+" ") &&
			!strings.HasSuffix(conf, "--config images="+c.want) {
			t.Errorf("%s: conformance %s", c.name, conf)
		}
		if told := strings.Contains(strings.Join(said, "\n"), "zm's default"); told != c.dflt {
			t.Errorf("%s: progress %q", c.name, said)
		}
	}
}

// A layer with no published default is refused in plain words naming
// --image, before anything runs.
func TestShareMissingDefaultSaysPassImage(t *testing.T) {
	ctx := context.Background()
	for name, pins := range map[string][]DefaultPin{
		"unpublished": {{Layer: "oci", OS: "linux", Image: ""}, {Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "darwin", Image: pinB}},
		"no line":     {{Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "darwin", Image: pinB}},
	} {
		withPins(t, pins...)
		run := &fakeRunner{}
		_, err := shareOn(ctx, run, binderFunc(nil), ShareOptions{Report: bigReport(), NamePrefix: "h"})
		if err == nil || !strings.Contains(err.Error(), "pass --image name@sha256:<64 hex>") || !strings.Contains(err.Error(), "oci") {
			t.Errorf("%s: %v", name, err)
		}
		if name == "unpublished" && !strings.Contains(err.Error(), "not published") {
			t.Errorf("%s: %v", name, err)
		}
		if len(run.calls) != 0 {
			t.Errorf("%s: ran %v", name, run.calls)
		}
	}
	// --image still lends when the layer has no default
	withPins(t, DefaultPin{Layer: "oci", OS: "linux", Image: ""})
	if _, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", DryRun: true}); err != nil {
		t.Errorf("--image with no default: %v", err)
	}
}

// A default binds like any pin, and the daemon checks the digest a box
// booted against it at acquire: the reference driver boots digest ...01, so
// a default pinning another digest is refused and released.
func TestShareDefaultImageCheckedAtBoot(t *testing.T) {
	const ( // the reference driver's box image name, booting digest ...01
		good  = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"
		drift = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000002"
	)
	ctx := context.Background()
	owner, _ := refDaemon(t)
	run := &fakeRunner{on: func(string) (Result, bool) { return Result{Stdout: "PASS"}, true }}
	share := func(prefix, pin string) {
		t.Helper()
		withPins(t, DefaultPin{Layer: "oci", OS: "linux", Image: pin})
		res, err := shareOn(ctx, run, computeclient.Owner{C: owner}, ShareOptions{Report: bigReport(), Sizes: []string{"c1m2"}, NamePrefix: prefix, Driver: "ref"})
		if err != nil || res.Failed() || !res.DefaultImage {
			t.Fatalf("share %s: %v %+v", prefix, err, res.Sizes)
		}
	}
	share("good", good)
	share("drift", drift)
	l, err := computeclient.RunTargets(ctx, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range l.Targets {
		want := map[string]string{"good-c1m2": good, "drift-c1m2": drift}[tg.Name]
		if len(tg.Images) != 1 || tg.Images[0] != want {
			t.Errorf("%s bound images %v, want %s", tg.Name, tg.Images, want)
		}
	}
	h, err := computeclient.AcquireRunTarget(ctx, owner, "good-c1m2", computeapi.AcquireRunTargetRequest{RequestID: "g1", TTLS: 60})
	if err != nil || h.ImageDigest != good[strings.LastIndex(good, "@")+1:] {
		t.Fatalf("acquire on the default pin: %v %+v", err, h)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, owner, "good-c1m2", h.Handle[strings.LastIndex(h.Handle, "/")+1:]); err != nil {
		t.Errorf("release: %v", err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "drift-c1m2", computeapi.AcquireRunTargetRequest{RequestID: "d1", TTLS: 60}); err == nil ||
		!strings.Contains(err.Error(), "not the pinned") {
		t.Errorf("a box booted off the default's digest: %v", err)
	}
}

// A size below the image's floor never boots (tart set refuses it), so share
// with no --sizes starts at the smallest size the image allows, and an asked
// size below it is refused, for a default and an explicit --image of a pinned
// image alike; an image images.pin does not know has no floor.
func TestShareSizesRespectImageFloor(t *testing.T) {
	withPins(t, DefaultPin{Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "darwin", Image: pinB, MinCPU: 2, MinMemoryMB: 4096})
	withTartImages(t, pinA)
	ctx := context.Background()
	names := func(res ShareResult) string {
		var n []string
		for _, s := range res.Sizes {
			n = append(n, s.Size.Name)
		}
		return strings.Join(n, ",")
	}
	for _, images := range [][]string{nil, {pinB}, {pinA, pinB}} {
		res, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: macReport(), Images: images, NamePrefix: "m", DryRun: true})
		if err != nil {
			t.Fatalf("%v: %v", images, err)
		}
		if got := names(res); got != "c2m4,c2m8" || !strings.Contains(strings.Join(res.Sizes[0].Conformance, " "), "--only "+QuickGroups) {
			t.Errorf("%v: sizes %s (the security checks on %v)", images, got, res.Sizes[0].Conformance)
		}
		if _, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: macReport(), Images: images, NamePrefix: "m", DryRun: true,
			Sizes: []string{"c1m2"}}); err == nil || !strings.Contains(err.Error(), "too small for the image") {
			t.Errorf("%v: c1m2 asked: %v", images, err)
		}
	}
	res, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: macReport(), Images: []string{pinA}, NamePrefix: "m", DryRun: true})
	if err != nil || names(res) != "c1m2,c2m4,c2m8" {
		t.Errorf("an image with no known floor: %v %s", err, names(res))
	}
	small := macReport()
	small.Cores, small.RAMMB = 4, 8<<10 // only c1m2 fits twice inside half
	if _, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: small, NamePrefix: "m", DryRun: true, Limits: Limits{Share: 0.5}}); err == nil ||
		!strings.Contains(err.Error(), "big enough for the image") {
		t.Errorf("no size big enough: %v", err)
	}
	// Nor at zm's default share: c1m2 fits twice, so it lends no c2m4 one at
	// a time (conformance's two boxes would then take the whole machine).
	if _, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: small, NamePrefix: "m", DryRun: true}); err == nil ||
		!strings.Contains(err.Error(), "twice inside the share is big enough for the image") {
		t.Errorf("default share: %v", err)
	}
}

// The box-images workflow cannot open a PR (the repository lets Actions open
// none): it pushes a branch and prints the pin lines and a compare URL for a
// person, and it rewrites images.pin's oci lines in the form ParsePins reads:
// the plain line gets potluck-box-linux's digest, the agent template line
// (zm-eakh.12.5.20) potluck-box-agent's, each keeping its keys.
func TestBoxImagesWorkflowLeavesThePRToAPerson(t *testing.T) {
	b, err := os.ReadFile("../.github/workflows/box-images.yml")
	if err != nil {
		t.Fatal(err)
	}
	w := string(b)
	for _, bad := range []string{"gh pr create", "pull-requests: write"} {
		if strings.Contains(w, bad) {
			t.Errorf("the workflow has %q: Actions may not open PRs here", bad)
		}
	}
	const plainSed, agentSed = `/ template=/!s#^(oci +linux +)[^ ]+#\1$PIN#`, `/ template=agent( |$)/s#^(oci +linux +)[^ ]+#\1$AGENT_PIN#`
	for _, want := range []string{`compare/main...$branch`, `echo "images.pin line: oci linux $PIN"`, `HEAD:refs/heads/$branch`,
		plainSed, agentSed, `echo "images.pin line: oci linux $AGENT_PIN template=agent"`, `IMAGE: ghcr.io/sengirunners/potluck-box-linux`, `AGENT_IMAGE: ghcr.io/sengirunners/potluck-box-agent`,
		`scripts/images/publish-box-image.sh "$AGENT_IMAGE"`, `BASE: ${{ steps.push.outputs.pin }}`,
		`BASE: ${{ steps.agent.outputs.pin }}`, `scripts/images/publish-box-image.sh "$BROWSER_IMAGE"`, `cmd/potluck-driver-tart/images/agent`, `claude --version`, `codex --version`, `node --version`, `gh --version`, `python3 --version`} {
		if !strings.Contains(w, want) {
			t.Errorf("the workflow lacks %q", want)
		}
	}
	// The same rewrites on the shipped file swap the images only: runner_dir and template stay (zm-eakh.12.24.5).
	// Line by line, as sed runs them: the patterns have no class whose newline handling differs between
	// GNU sed and Go ([^ ] would cross a newline in a whole-file Go match).
	const pinC = "reg.example/agent@sha256:0000000000000000000000000000000000000000000000000000000000000003"
	re, isTmpl, isAgent := regexp.MustCompile(`^(oci +linux +)[^ ]+`), regexp.MustCompile(` template=`), regexp.MustCompile(` template=agent( |$)`)
	lines := strings.Split(imagesPin, "\n")
	for i, l := range lines {
		if !isTmpl.MatchString(l) {
			l = re.ReplaceAllString(l, "${1}"+pinA)
		}
		if isAgent.MatchString(l) {
			l = re.ReplaceAllString(l, "${1}"+pinC)
		}
		lines[i] = l
	}
	pins, err := ParsePins(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatalf("the file the workflow writes: %v", err)
	}
	var plain, agent int
	for _, p := range pins {
		switch {
		case p.Layer != "oci":
		case p.Template == "" && p.Image == pinA && p.RunnerDir == "/home/runner":
			plain++
		case p.Template == "agent" && p.Image == pinC && p.RunnerDir == "/home/runner":
			agent++
		case p.Template != "" && p.Template != "agent":
			if original, ok := PinForImage(p.Image); !ok || p != original {
				t.Errorf("unrelated template changed: %+v", p)
			}
		default:
			t.Errorf("after the workflow's rewrite: %+v", p)
		}
	}
	if plain != 1 || agent != 1 {
		t.Errorf("after the rewrite: %d plain oci lines, %d agent lines; want one each", plain, agent)
	}
}

// A template line (template=NAME; zm-eakh.12.5.20) is never a layer's
// default, whatever its place: share lends the default first and each
// published template after it, so a binding's first image stays the plain
// one; --image lends only what it names; an unpublished template is left out
// and the result says so.
func TestShareLendsTemplates(t *testing.T) {
	const pinC = "reg.example/agent@sha256:0000000000000000000000000000000000000000000000000000000000000003"
	ctx := context.Background()
	withPins(t, DefaultPin{Layer: "oci", OS: "linux", Image: pinC, Template: "agent", RunnerDir: "/home/runner"},
		DefaultPin{Layer: "oci", OS: "linux", Image: pinA, RunnerDir: "/home/runner"})
	if d, err := DefaultImage("oci"); err != nil || d.Image != pinA {
		t.Fatalf("default with a template line first: %+v %v", d, err)
	}
	res, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: bigReport(), NamePrefix: "h", Sizes: []string{"c2m4"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Images, ",") != pinA+","+pinC || !res.DefaultImage || len(res.Pulls) != 2 || ImageRunnerDir(res.Images) != "/home/runner" {
		t.Errorf("default and agent: images %v default %v pulls %+v runner %q", res.Images, res.DefaultImage, res.Pulls, ImageRunnerDir(res.Images))
	}
	if conf := strings.Join(res.Sizes[0].Conformance, " "); !strings.Contains(conf, "--config images="+pinA+","+pinC) {
		t.Errorf("conformance %s", conf)
	}
	if res, err := shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: bigReport(), Images: []string{pinB}, NamePrefix: "h", DryRun: true}); err != nil ||
		strings.Join(res.Images, ",") != pinB {
		t.Errorf("--image lends only what it names: %v %v", res.Images, err)
	}
	withPins(t, DefaultPin{Layer: "oci", OS: "linux", Image: pinA, RunnerDir: "/home/runner"},
		DefaultPin{Layer: "oci", OS: "linux", Template: "agent", RunnerDir: "/home/runner"})
	res, err = shareOn(ctx, &fakeRunner{}, nil, ShareOptions{Report: bigReport(), NamePrefix: "h", Sizes: []string{"c2m4"}, DryRun: true})
	if err != nil || strings.Join(res.Images, ",") != pinA || !strings.Contains(strings.Join(res.Notes, "\n"), "agent template") {
		t.Errorf("unpublished template: images %v notes %q %v", res.Images, res.Notes, err)
	}
	// only a template line: the layer has no default
	withPins(t, DefaultPin{Layer: "oci", OS: "linux", Image: pinC, Template: "agent"})
	if _, err := DefaultImage("oci"); err == nil {
		t.Errorf("a template line alone made a default")
	}
}

// A template image that will not pull (a private package, a full disk, the
// network) is dropped with a note and the default still binds, its
// conformance run on the images actually bound; the default failing to pull
// still stops share (zm-eakh.12.5.20 review).
func TestShareTemplatePullFailureKeepsTheDefault(t *testing.T) {
	const (
		good = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"
		pinC = "reg.example/agent@sha256:0000000000000000000000000000000000000000000000000000000000000003"
	)
	ctx := context.Background()
	owner, _ := refDaemon(t)
	withPins(t, DefaultPin{Layer: "oci", OS: "linux", Image: good}, DefaultPin{Layer: "oci", OS: "linux", Image: pinC, Template: "agent"})
	failing := func(bad string) *fakeRunner {
		return &fakeRunner{on: func(line string) (Result, bool) {
			if strings.Contains(line, " pull ") && strings.HasSuffix(line, bad) {
				return Result{Exit: 1, Stderr: "denied: not authorized"}, true
			}
			if strings.Contains(line, "zm-run-conformance") && strings.Contains(line, pinC) {
				return Result{Exit: 1, Stdout: "FAIL conformance ran with the dropped image"}, true
			}
			return Result{Stdout: "PASS"}, true
		}}
	}
	res, err := shareOn(ctx, failing(pinC), computeclient.Owner{C: owner}, ShareOptions{Report: bigReport(), Sizes: []string{"c1m2"}, NamePrefix: "t", Driver: "ref"})
	if err != nil || res.Failed() || strings.Join(res.Images, ",") != good {
		t.Fatalf("a template pull failed: %v images %v sizes %+v", err, res.Images, res.Sizes)
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "agent template image") || !strings.Contains(n, "denied") {
		t.Errorf("notes %q", res.Notes)
	}
	l, err := computeclient.RunTargets(ctx, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range l.Targets {
		if tg.Name == "t-c1m2" && strings.Join(tg.Images, ",") != good {
			t.Errorf("bound images %v, want the default only", tg.Images)
		}
	}
	if _, err := shareOn(ctx, failing(good), computeclient.Owner{C: owner}, ShareOptions{Report: bigReport(), Sizes: []string{"c1m2"}, NamePrefix: "u", Driver: "ref"}); err == nil ||
		!strings.Contains(err.Error(), "denied") {
		t.Errorf("the default's pull failed: %v", err)
	}
}
