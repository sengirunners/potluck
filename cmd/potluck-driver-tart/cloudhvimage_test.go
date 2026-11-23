package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// imageBuild is one run of the cloud-hypervisor layer's image script
// (scripts/cloudhv-image.sh) with a stand-in for each program it calls that
// reaches outside the build's own directory or needs more than a shell has:
// curl, sha256sum, qemu-img, genisoimage, timeout, cp, docker and the
// hypervisor. Nothing is downloaded and no VM starts. The stand-ins record
// what they were asked (calls, container.sh, seed/) and a test says what the
// build boot's console holds, so these tests are of what a build does: whether
// it writes an image, what it leaves behind, what it downloads and asks Docker
// for. The script runs from a copy of the tree whose agent Dockerfile has
// other values on its ARG lines (agentArgs), so what the script takes from
// that file shows.
type imageBuild struct {
	t    *testing.T
	dir  string
	sums map[string]string // a file name's pattern → the sha256 the stand-in sha256sum gives such a file
}

var agentArgs = map[string]string{
	"NODE_VERSION": "91.1.1", "NODE_SHA256_AMD64": "sum-node", "GH_VERSION": "92.2.2", "GH_SHA256_AMD64": "sum-gh",
	"GO_VERSION": "93.3.3", "GO_SHA256_AMD64": "sum-go", "CLAUDE_CODE_VERSION": "94.4.4", "CODEX_VERSION": "95.5.5",
	"AGENT_PACKAGES": "pkg-one pkg-two", "AGENT_TOOLS": "tool-one tool-two",
}

var imageStandIns = map[string]string{
	// -o <file> <url> writes a file; a url alone is Ubuntu's list of checksums.
	"curl": `out= url=
while [ $# -gt 0 ]; do
  case $1 in
    -o) out=$2; shift 2 ;;
    --proto|--retry) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
echo "curl $url" >> "$STANDINS/calls"
if [ -n "$out" ]; then echo "$url" > "$out"; else cat "$STANDINS/SHA256SUMS"; fi
`,
	// A downloaded file's sum is the one the test gave its name; any other file is the image.
	"sha256sum": `f=$(basename "$1") s=sum-of-the-image
while read -r pattern sum; do
  case $f in $pattern) s=$sum ;; esac
done < "$STANDINS/sums"
echo "$s  $1"
`,
	"qemu-img": `[ "$1" != convert ] || { for a; do last=$a; done; echo "a disk" > "$last"; }
`,
	// Keeps the seed disk's files and its label for the test to read.
	"genisoimage": `mkdir -p "$STANDINS/seed"
while [ $# -gt 0 ]; do
  case $1 in
    -output) iso=$2; shift 2 ;;
    -volid) echo "$2" > "$STANDINS/seed-label"; shift 2 ;;
    -*) shift ;;
    *) cp "$1" "$STANDINS/seed/" || exit 1; shift ;;
  esac
done
echo "a seed disk" > "$iso"
`,
	"timeout": `echo "timeout $1 $2" >> "$STANDINS/calls"
[ "$1" != --foreground ] || shift
shift
exec "$@"
`,
	"cp": `[ "$1" != --sparse=always ] || shift
[ ! -f "$STANDINS/copy-fails" ] || case $2 in *.part) echo "half a disk" > "$2"; exit 1 ;; esac
exec /bin/cp "$@"
`,
	// run keeps the container's script and writes the two archives the script expects of it.
	"docker": `printf 'docker %s\n' "$(printf '%s' "$*" | tr '\n' ' ')" >> "$STANDINS/calls"
case $1 in
  info) [ ! -f "$STANDINS/no-docker" ] ;;
  import) [ ! -f "$STANDINS/docker-import-fails" ] ;;
  run)
    for a; do
      last=$a
      case $a in *:/out) o=${a%:/out} ;; esac
    done
    printf '%s\n' "$last" > "$STANDINS/container.sh"
    [ ! -f "$STANDINS/docker-run-fails" ] || { echo "E: Unable to locate package"; exit 100; }
    echo debs > "$o/debs.tar"
    echo local > "$o/local.tar" ;;
esac
`,
	// The build boot: the console the test wrote, and the exit it asked for. A build is cancelled by a signal to
	// the script, which is this one's parent, the stand-in timeout having become it.
	"cloud-hypervisor": `echo "cloud-hypervisor" >> "$STANDINS/calls"
for a; do
  case $a in file=*) [ ! -f "$STANDINS/console" ] || cp "$STANDINS/console" "${a#file=}" ;; esac
done
[ ! -f "$STANDINS/hypervisor-fails" ] || { echo "the hypervisor's own complaint" >&2; exit 1; }
[ ! -f "$STANDINS/cancelled" ] || kill -TERM "$PPID"
`,
}

func newImageBuild(t *testing.T) *imageBuild {
	t.Helper()
	b := &imageBuild{t: t, dir: t.TempDir()}
	read := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, d := range []string{"bin", "cache", "tmp", "home", "images"} {
		if err := os.Mkdir(filepath.Join(b.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range imageStandIns {
		b.write("bin/"+name, "#!/bin/sh\n"+body)
		if err := os.Chmod(filepath.Join(b.dir, "bin", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := read("../../scripts/cloudhv-image.sh")
	b.write("repo/scripts/cloudhv-image.sh", script)
	d := read("images/agent/Dockerfile")
	for name, value := range agentArgs {
		line := regexp.MustCompile(`(?m)^ARG ` + name + `=("[^"\n]+"|[^"\s\\]+)$`)
		set := line.FindAllStringSubmatch(d, -1)
		if len(set) != 1 {
			t.Fatalf("the agent Dockerfile does not set %s once, whole on an ARG line of its own", name)
		}
		if strings.HasPrefix(set[0][1], `"`) { // written as the file writes it, in quotes or bare
			value = `"` + value + `"`
		}
		d = line.ReplaceAllString(d, "ARG "+name+"="+value)
	}
	b.write("repo/cmd/potluck-driver-tart/images/agent/Dockerfile", d)
	docker := regexp.MustCompile(`(?m)^docker_sha256=([0-9a-f]{64})$`).FindStringSubmatch(script)
	if docker == nil {
		t.Fatal("the image script pins no checksum for Docker's release")
	}
	b.sums = map[string]string{
		"noble-server-cloudimg-amd64.img": "sum-ubuntu", "noble-server-cloudimg-amd64-root.tar.xz": "sum-ubuntu-root",
		"docker-*.tgz": docker[1], "node-v*": "sum-node", "gh_*": "sum-gh", "go*.linux-amd64.tar.gz": "sum-go",
	}
	b.write("SHA256SUMS", "sum-ubuntu *noble-server-cloudimg-amd64.img\nsum-ubuntu-root *noble-server-cloudimg-amd64-root.tar.xz\n")
	b.write("guest", "a guest\n")
	b.write("firmware", "a firmware\n")
	return b
}

func (b *imageBuild) write(name, content string) {
	b.t.Helper()
	path := filepath.Join(b.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// file is what a file of the build holds, and nothing where there is none.
func (b *imageBuild) file(name string) string {
	data, _ := os.ReadFile(filepath.Join(b.dir, name))
	return string(data)
}

// names lists a directory of the build.
func (b *imageBuild) names(dir string) string {
	entries, _ := os.ReadDir(filepath.Join(b.dir, dir))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// run builds images/box.raw with the given further arguments (the size, agent), and returns the script's exit
// code and what it said.
func (b *imageBuild) run(args ...string) (int, string) {
	b.t.Helper()
	var sums strings.Builder
	for pattern, sum := range b.sums {
		sums.WriteString(pattern + " " + sum + "\n")
	}
	b.write("sums", sums.String())
	cmd := exec.Command("sh", append([]string{filepath.Join(b.dir, "repo/scripts/cloudhv-image.sh"),
		filepath.Join(b.dir, "images/box.raw"), filepath.Join(b.dir, "guest"), filepath.Join(b.dir, "firmware")}, args...)...)
	cmd.Env = []string{"PATH=" + filepath.Join(b.dir, "bin") + ":/usr/bin:/bin", "HOME=" + filepath.Join(b.dir, "home"),
		"TMPDIR=" + filepath.Join(b.dir, "tmp"), "XDG_CACHE_HOME=" + filepath.Join(b.dir, "cache"), "STANDINS=" + b.dir}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		b.t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

// imageConsoleLines returns what the build boot's setup, and the agent tools' script in it, each say on the
// console when they are through: what the last command of each prints, taken from the seed disk of a build.
// So a test's console says what a real boot would, whatever the script's words for it are.
func imageConsoleLines(t *testing.T) (setUp, installed string) {
	t.Helper()
	b := newImageBuild(t)
	b.run("20G", "agent") // its boot says nothing, so it fails, with the seed disk made
	runcmd, tools := imageSetup(t, b)
	serial := func(said string) string { return strings.ReplaceAll(said, "\n", "\r\n") } // as a serial console has it
	return serial(imageSays(t, runcmd[len(runcmd)-1])), serial(imageSays(t, tools[len(tools)-1]))
}

// imageSetup returns the two scripts of a build's seed disk: cloud-init's runcmd, an item a line, and the agent
// tools' script.
func imageSetup(t *testing.T, b *imageBuild) (runcmd, tools []string) {
	t.Helper()
	var conf struct {
		Runcmd []any `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(b.file("seed/user-data")), &conf); err != nil || len(conf.Runcmd) < 3 {
		t.Fatalf("the seed disk's user-data: %v, a runcmd of %d items", err, len(conf.Runcmd))
	}
	for i, item := range conf.Runcmd {
		line, ok := item.(string)
		if !ok {
			t.Fatalf("runcmd item %d is %T %v, not a string: cloud-init then runs none of them", i+1, item, item)
		}
		runcmd = append(runcmd, line)
	}
	tools = strings.Split(strings.TrimRight(b.file("seed/template.sh"), "\n"), "\n")
	if len(tools) < 3 {
		t.Fatalf("the seed disk's script for the agent tools is %q", tools)
	}
	return runcmd, tools
}

// imageSays runs one printf command of a setup script and returns what it puts on the console.
func imageSays(t *testing.T, command string) string {
	t.Helper()
	command = strings.TrimSuffix(command, " > /dev/ttyS0")
	if !strings.HasPrefix(command, "printf ") || strings.ContainsAny(command, ">|;&`$") {
		t.Fatalf("a setup script ends with %q, not with a printf of the line the build looks for", command)
	}
	out, err := exec.Command("sh", "-c", command).Output()
	if err != nil || !strings.HasPrefix(string(out), "\n") || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("%q prints %q (%v), not a line of its own", command, out, err)
	}
	return string(out)
}

// A build writes an image only after a build boot that set it up: one whose setup said its last line, and with
// agent the tools' script too, and that the hypervisor ended well. Any other build fails, writes no image and
// no digest, keeps what the boot's console said beside where the image would be, and says it. Every build, a
// cancelled one too, removes its work directory and the half of an image it had copied, and with agent the
// Docker image and container it made.
func TestCloudHVImageBuild(t *testing.T) {
	setUp, installed := imageConsoleLines(t)
	const boot = "[  OK  ] Started a unit.\r\ncloud-init[901]: + sync\r\n"
	named := map[string]bool{} // what the builds named their containers: each its own, on a Docker they may share
	for _, c := range []struct {
		name    string
		agent   bool
		console string
		broken  string // a stand-in told to fail
		image   bool
		kept    bool // the boot's console is kept
		says    string
	}{
		{name: "a boot that sets the image up", console: boot + setUp, image: true, says: "box@sha256:sum-of-the-image"},
		{name: "a boot whose setup command fails", console: boot + "cloud-init[901]: groupadd: group 'root' already exists\r\n",
			kept: true, says: "groupadd: group 'root' already exists"},
		{name: "a boot that says the line inside another", console: boot + "x" + strings.TrimPrefix(setUp, "\r\n"), kept: true},
		{name: "a build cancelled during its boot", console: boot + setUp, broken: "cancelled"},
		{name: "an image that cannot be copied whole", console: boot + setUp, broken: "copy-fails"},
		{name: "a hypervisor that fails after the line", console: boot + setUp, broken: "hypervisor-fails", kept: true,
			says: "the hypervisor's own complaint"},
		{name: "a boot with no console", broken: "hypervisor-fails", says: "the hypervisor's own complaint"},
		{name: "agent, a boot that sets up the image and the tools", agent: true, console: boot + installed + setUp, image: true,
			says: "box@sha256:sum-of-the-image"},
		{name: "agent, a boot that installs no tools", agent: true, console: boot + setUp, kept: true},
		{name: "agent, a boot that says the tools' line inside another", agent: true,
			console: boot + "x" + strings.TrimPrefix(installed, "\r\n") + setUp, kept: true},
		{name: "agent, a boot that stops after the tools", agent: true, console: boot + installed, kept: true},
		{name: "agent, an import that fails", agent: true, console: boot + installed + setUp, broken: "docker-import-fails"},
		{name: "agent, a container that fails", agent: true, console: boot + installed + setUp, broken: "docker-run-fails",
			says: "E: Unable to locate package"},
		{name: "agent, no Docker", agent: true, console: boot + installed + setUp, broken: "no-docker", says: "Docker"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newImageBuild(t)
			if c.console != "" {
				b.write("console", c.console)
			}
			if c.broken != "" {
				b.write(c.broken, "")
			}
			args := []string{"20G"}
			if c.agent {
				args = append(args, "agent")
			}
			if c.image {
				b.write("images/box.raw.console.log", "the console of an earlier build that failed\n")
			}
			code, said := b.run(args...)
			want := ""
			switch {
			case c.image:
				want = "box.raw box.raw.sha256"
			case c.kept:
				want = "box.raw.console.log"
			}
			if (code == 0) != c.image || code > 1 || b.names("images") != want {
				t.Errorf("the build exited %d and left %q, want %q; it said:\n%s", code, b.names("images"), want, said)
			}
			if c.image && (b.file("images/box.raw") != "a disk\n" || b.file("images/box.raw.sha256") != "sum-of-the-image\n") {
				t.Errorf("the image is %q and its digest %q", b.file("images/box.raw"), b.file("images/box.raw.sha256"))
			}
			if c.kept && b.file("images/box.raw.console.log") != c.console {
				t.Errorf("the console kept is %q, want the boot's", b.file("images/box.raw.console.log"))
			}
			if !strings.Contains(said, c.says) {
				t.Errorf("the build did not say %q; it said:\n%s", c.says, said)
			}
			if left := b.names("tmp"); left != "" {
				t.Errorf("the build left %q in its temporary directory", left)
			}
			calls := b.file("calls")
			booted := regexp.MustCompile(`(?m)^timeout --foreground \d+\ncloud-hypervisor$`).MatchString(calls) // with a time limit a Ctrl-C passes
			if wantBoot := !strings.Contains(c.broken, "docker"); booted != wantBoot {
				t.Errorf("the build boot ran: %v, want %v", booted, wantBoot)
			}
			// The container is removed when it ends and ended by a cancelled build, reads the downloads and cannot
			// write them, writes into a directory of its own in the build's, and stops at its first command that fails.
			ran := regexp.MustCompile(`(?m)^docker run --rm --init --name (potluck-cloudhv-image-\w+) .*/cache:ro -v \S+/tmp/[^/ ]+/out:/out .* (potluck-cloudhv-image:\w+) sh -euc `).FindStringSubmatch(calls)
			if wantRun := c.agent && c.broken != "no-docker" && c.broken != "docker-import-fails"; (ran != nil) != wantRun {
				t.Fatalf("the build ran a container: %v, want %v; Docker was asked:\n%s", ran != nil, wantRun, calls)
			}
			if ran != nil && named[ran[1]] {
				t.Errorf("two builds named their container %s", ran[1])
			} else if ran != nil {
				named[ran[1]] = true
			}
			if ran != nil && !(strings.Contains(calls, "docker rm -f "+ran[1]+"\n") && strings.Contains(calls, "docker rmi -f "+ran[2]+"\n")) {
				t.Errorf("the build did not remove the container %s and the image %s; Docker was asked:\n%s", ran[1], ran[2], calls)
			}
			if !c.agent && regexp.MustCompile(`(?m)^docker `).MatchString(calls) {
				t.Errorf("a build without agent asked Docker:\n%s", calls)
			}
		})
	}
}

// A build is refused, before it downloads or asks Docker anything, when what follows the firmware is not a size
// and then agent or nothing: agent in the size's place would otherwise build a plain image and say it built one.
func TestCloudHVImageArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"agent"}, "the fourth argument is the disk's size (20G), not agent"},
		{[]string{"20G", "browser"}, "the fifth argument is agent or nothing, not browser"},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			b := newImageBuild(t)
			code, said := b.run(c.args...)
			if code != 2 || !strings.Contains(said, c.says) || b.file("calls") != "" || b.names("images") != "" {
				t.Errorf("the build exited %d, left %q and called:\n%s\nit said:\n%s", code, b.names("images"), b.file("calls"), said)
			}
		})
	}
}

// Every file the script downloads is checked before anything uses it, at each build: a build with one file that
// is not what its pinned or published checksum says, or that has no checksum, stops before Docker is given
// anything and before the build boot. That holds for a file an earlier build left in the cache too.
func TestCloudHVImageChecksEveryDownload(t *testing.T) {
	setUp, installed := imageConsoleLines(t)
	stops := func(t *testing.T, b *imageBuild) {
		t.Helper()
		b.write("console", installed+setUp)
		code, said := b.run("20G", "agent")
		calls := b.file("calls")
		if code != 1 || b.names("images") != "" || regexp.MustCompile(`(?m)^(docker (import|run)|cloud-hypervisor)`).MatchString(calls) {
			t.Errorf("the build exited %d, left %q and went on to:\n%s\nit said:\n%s", code, b.names("images"), calls, said)
		}
	}
	for pattern := range newImageBuild(t).sums {
		t.Run(pattern, func(t *testing.T) {
			b := newImageBuild(t)
			b.sums[pattern] = "another-sum"
			stops(t, b)
		})
		t.Run(pattern+", in the cache", func(t *testing.T) {
			b := newImageBuild(t)
			b.write("console", installed+setUp)
			if code, said := b.run("20G", "agent"); code != 0 {
				t.Fatalf("the build that fills the cache exited %d:\n%s", code, said)
			}
			for _, name := range []string{"calls", "images/box.raw", "images/box.raw.sha256"} {
				if err := os.Remove(filepath.Join(b.dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			b.sums[pattern] = "another-sum"
			stops(t, b)
			if calls := b.file("calls"); strings.Count(calls, "curl ") != 1 || !strings.Contains(calls, "/SHA256SUMS\n") {
				t.Errorf("the build downloaded more than Ubuntu's list of checksums, with every file in the cache:\n%s", calls)
			}
		})
	}
	t.Run("a file Ubuntu lists no checksum of", func(t *testing.T) {
		b := newImageBuild(t)
		b.write("SHA256SUMS", "sum-ubuntu *noble-server-cloudimg-amd64.img\n")
		stops(t, b)
	})
}

// The agent image is the agent template as a VM image: the versions it downloads and installs, the Ubuntu
// packages and the tools its build boot checks for are the ones on the Dockerfile's ARG lines, whatever those
// are. How each is installed is written in both files, and this does not compare that.
func TestCloudHVAgentImageIsTheAgentTemplate(t *testing.T) {
	setUp, installed := imageConsoleLines(t)
	b := newImageBuild(t)
	b.write("console", installed+setUp)
	if code, said := b.run("20G", "agent"); code != 0 {
		t.Fatalf("the build exited %d:\n%s", code, said)
	}
	for file, wants := range map[string][]string{
		"calls": {"curl https://nodejs.org/dist/v91.1.1/node-v91.1.1-linux-x64.tar.xz\n",
			"curl https://github.com/cli/cli/releases/download/v92.2.2/gh_92.2.2_linux_amd64.tar.gz\n",
			"curl https://go.dev/dl/go93.3.3.linux-amd64.tar.gz\n"},
		"container.sh": {"--no-install-recommends pkg-one pkg-two\n", "/cache/node-v91.1.1-linux-x64.tar.xz ",
			"/cache/gh_92.2.2_linux_amd64.tar.gz ", "/cache/go93.3.3.linux-amd64.tar.gz ",
			" @anthropic-ai/claude-code@94.4.4 @openai/codex@95.5.5\n"},
		"seed/template.sh": {"\nfor t in git tool-one tool-two; do\n"},
	} {
		for _, want := range wants {
			if !strings.Contains(b.file(file), want) {
				t.Errorf("%s lacks %q, what the Dockerfile's ARG lines ask for; it is:\n%s", file, want, b.file(file))
			}
		}
	}
	if seed := " " + b.names("seed") + " "; !strings.Contains(seed, " debs.tar ") || !strings.Contains(seed, " local.tar ") {
		t.Errorf("the seed disk holds %q, without the container's two archives", seed)
	}
	if label := b.file("seed-label"); label != "cidata\n" {
		t.Errorf("the seed disk is labelled %q, and cloud-init reads only one labelled cidata", label)
	}
	// A Dockerfile that no longer sets one of them stops the build before Docker is given anything, with its name.
	const dockerfile = "repo/cmd/potluck-driver-tart/images/agent/Dockerfile"
	for name := range agentArgs {
		t.Run("without "+name, func(t *testing.T) {
			b := newImageBuild(t)
			b.write("console", installed+setUp)
			b.write(dockerfile, regexp.MustCompile(`(?m)^ARG `+name+`=.*\n`).ReplaceAllString(b.file(dockerfile), ""))
			code, said := b.run("20G", "agent")
			calls := b.file("calls")
			if code != 1 || !strings.Contains(said, "sets no "+name+"\n") || b.names("images") != "" ||
				regexp.MustCompile(`(?m)^(docker (import|run)|cloud-hypervisor)`).MatchString(calls) {
				t.Errorf("the build exited %d, left %q and went on to:\n%s\nit said:\n%s", code, b.names("images"), calls, said)
			}
		})
	}
}

// The build boot's setup is one script that stops at its first command that fails, and so is the agent tools'
// script, which one of its commands runs. Each says its line on the console as its last command and nowhere
// before, so the line means every command before it ran.
func TestCloudHVImageSetupIsOneScript(t *testing.T) {
	b := newImageBuild(t)
	b.run("20G", "agent")
	runcmd, tools := imageSetup(t, b)
	if runcmd[0] != "set -e" {
		t.Errorf("runcmd begins %q, not set -e", runcmd[0])
	}
	if tools[0] != "#!/bin/sh" || tools[1] != "set -eu" {
		t.Errorf("the agent tools' script begins %q, not with set -eu", tools[:2])
	}
	for _, script := range [][]string{runcmd, tools} {
		last := len(script) - 1
		line := strings.TrimSpace(imageSays(t, script[last]))
		if before := strings.Join(script[:last], "\n"); strings.Contains(before, line) {
			t.Errorf("%q is said before the script's last command, in:\n%s", line, before)
		}
	}
	if last := runcmd[len(runcmd)-1]; !strings.HasSuffix(last, " > /dev/ttyS0") {
		t.Errorf("the setup's last command is %q, which does not say its line on the console", last)
	}
	// The tools' script is run from the seed disk while that is mounted, with what it says on the console.
	at := func(command string) int {
		for i, line := range runcmd {
			if strings.Contains(line, command) {
				return i
			}
		}
		return -1
	}
	mount, run, umount := at("mount -o ro /dev/disk/by-label/cidata /mnt/seed"), at("sh /mnt/seed/template.sh > /dev/ttyS0 2>&1"), at("umount /mnt/seed")
	if mount < 0 || run < mount || umount < run {
		t.Errorf("the setup does not mount the seed disk, run the agent tools' script and unmount it, in that order:\n%s", strings.Join(runcmd, "\n"))
	}
}
