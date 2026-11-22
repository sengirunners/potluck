package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// syncRepos makes a private clone that carries this checkout's public-sync.sh, the repository it calls origin,
// and a public repository holding the first commit. The address the script publishes to is read from the
// script and rewritten to the local public repository, so no test reaches the real one.
func syncRepos(t *testing.T) (private, public string) {
	t.Helper()
	script, err := os.ReadFile("public-sync.sh")
	if err != nil {
		t.Fatal(err)
	}
	address := regexp.MustCompile(`(?m)^PUBLIC="([^"]+)"$`).FindSubmatch(script)
	if address == nil {
		t.Fatal("public-sync.sh no longer names its public address as PUBLIC=\"...\"; this test cannot redirect it")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// The stand-in for gitleaks lists the folder it was asked to scan, so a test can see what was scanned.
	leaks := "#!/bin/sh\nfor a; do last=$a; done\nls \"$last\" > \"$HOME/scanned\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gitleaks"), []byte(leaks), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	public = filepath.Join(dir, "public.git")
	private = filepath.Join(dir, "private")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git(t, dir, "config", "--global", "user.name", "Test")
	git(t, dir, "config", "--global", "user.email", "test@example.com")
	git(t, dir, "config", "--global", "init.defaultBranch", "main")
	git(t, dir, "config", "--global", "url."+public+".insteadOf", string(address[1]))
	git(t, dir, "init", "--quiet", "--bare", public)
	git(t, dir, "init", "--quiet", "--bare", filepath.Join(dir, "origin.git"))
	git(t, dir, "init", "--quiet", private)
	git(t, private, "remote", "add", "origin", filepath.Join(dir, "origin.git"))
	commit(t, private, "scripts/public-sync.sh", string(script))
	git(t, private, "push", "--quiet", public, "main")
	return private, public
}

// gitEnv is the environment without the variables git sets for a hook (GIT_DIR and the like), which would point
// these commands at the repository the test was started from. The two this test sets itself stay.
func gitEnv() []string {
	var env []string
	for _, v := range os.Environ() {
		if strings.HasPrefix(v, "GIT_") && !strings.HasPrefix(v, "GIT_CONFIG_GLOBAL=") && !strings.HasPrefix(v, "GIT_CONFIG_NOSYSTEM=") {
			continue
		}
		env = append(env, v)
	}
	return env
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes one file (or removes it when body is empty), commits it and pushes it to origin's main.
func commit(t *testing.T, repo, name, body string) string {
	t.Helper()
	path := filepath.Join(repo, name)
	if body == "" {
		git(t, repo, "rm", "--quiet", name)
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", name)
	}
	git(t, repo, "commit", "--quiet", "-m", name)
	git(t, repo, "push", "--quiet", "origin", "HEAD:main")
	git(t, repo, "fetch", "--quiet", "origin")
	return git(t, repo, "rev-parse", "HEAD")
}

// sync runs the private repository's own copy of the script from dir, a path inside that repository.
func sync(t *testing.T, private, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(private, "scripts", "public-sync.sh")}, args...)...)
	cmd.Dir, cmd.Env = filepath.Join(private, dir), gitEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func samePublicTree(t *testing.T, private, public string) {
	t.Helper()
	if got, want := git(t, public, "rev-parse", "main^{tree}"), git(t, private, "rev-parse", "HEAD^{tree}"); got != want {
		t.Fatalf("public tree %s, want the private tree %s", got, want)
	}
}

// Started from a folder inside the repository, the script still scans and publishes the whole tree.
func TestPublicSyncPublishesTheTree(t *testing.T) {
	private, public := syncRepos(t)
	commit(t, private, "new.txt", "two\n")
	if out, err := sync(t, private, "scripts", "origin/main", "--push"); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	samePublicTree(t, private, public)
	scanned, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), "scanned"))
	if err != nil || !strings.Contains(string(scanned), "new.txt") || !strings.Contains(string(scanned), "scripts") {
		t.Fatalf("the scan did not cover the whole tree: %v: %q", err, scanned)
	}
}

func TestPublicSyncRefusesToPushWhileAHoldIsOn(t *testing.T) {
	private, public := syncRepos(t)
	before := git(t, public, "rev-parse", "main")
	commit(t, private, "fix.txt", "the fix\n")
	commit(t, private, "public-hold/fence", "the fence fix is not on the hosted machines yet\n")
	commit(t, private, "later.txt", "work merged after the fix\n")

	// From the top of the repository and from a folder inside it.
	for _, dir := range []string{".", "scripts"} {
		out, err := sync(t, private, dir, "origin/main", "--push")
		if err == nil {
			t.Fatalf("from %s: a held commit was pushed: %s", dir, out)
		}
		if !strings.Contains(out, "held by public-hold/fence") {
			t.Fatalf("from %s: the refusal does not name the hold: %s", dir, out)
		}
		if got := git(t, public, "rev-parse", "main"); got != before {
			t.Fatalf("from %s: public main moved from %s to %s under a hold", dir, before, got)
		}
	}

	// Without --push the script still shows what would go over, pushes nothing, and its last line names the hold.
	out, err := sync(t, private, ".", "origin/main")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; err != nil || !strings.Contains(last, "--push would refuse: held by public-hold/fence") {
		t.Fatalf("dry run under a hold: %v: last line %q of: %s", err, last, out)
	}
	if got := git(t, public, "rev-parse", "main"); got != before {
		t.Fatalf("a dry run moved public main to %s", got)
	}

	// The hold comes off on the private main; the same sync then publishes everything, and no hold file goes over.
	commit(t, private, "public-hold/fence", "")
	if out, err := sync(t, private, ".", "origin/main", "--push"); err != nil {
		t.Fatalf("sync after the hold came off: %v: %s", err, out)
	}
	samePublicTree(t, private, public)
	if files := git(t, public, "ls-tree", "-r", "--name-only", "main"); strings.Contains(files, "public-hold") {
		t.Fatalf("a hold file reached the public repository: %s", files)
	}
}

// A fix and its hold can land as two commits (a merge that is not a squash). The commit with the fix and no hold
// is refused while main carries the hold, and a commit that carries a hold is refused after main took it off.
func TestPublicSyncRefusesACommitBesideItsHold(t *testing.T) {
	private, public := syncRepos(t)
	before := git(t, public, "rev-parse", "main")
	fix := commit(t, private, "fix.txt", "the fix\n")
	held := commit(t, private, "public-hold/agent", "the guest agent fix is not on the hosted machines yet\n")

	out, err := sync(t, private, ".", fix, "--push")
	if err == nil || !strings.Contains(out, "held by public-hold/agent") {
		t.Fatalf("the fix's own commit, which has no hold file, under a hold on main: %v: %s", err, out)
	}
	commit(t, private, "public-hold/agent", "")
	out, err = sync(t, private, ".", held, "--push")
	if err == nil || !strings.Contains(out, "held by public-hold/agent") {
		t.Fatalf("a commit that carries a hold, after main took it off: %v: %s", err, out)
	}
	if got := git(t, public, "rev-parse", "main"); got != before {
		t.Fatalf("public main moved to %s", got)
	}
}

func TestPublicSyncTakesAMisnamedHoldAsAHold(t *testing.T) {
	for _, name := range []string{"Public-Hold/fence", "public-holds/fence", "docs/public-hold/fence", "public-hold",
		"public_hold/fence", "publichold/fence", "public-hold.md", ".public-hold/fence", "public hold/fence",
		`public-hold/a "quoted" name`} {
		private, public := syncRepos(t)
		before := git(t, public, "rev-parse", "main")
		commit(t, private, name, "a hold under the wrong name\n")
		out, err := sync(t, private, ".", "origin/main", "--push")
		if err == nil || !strings.Contains(out, "held by "+name) {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		if got := git(t, public, "rev-parse", "main"); got != before {
			t.Fatalf("%s: public main moved to %s", name, got)
		}
	}
}

// A checkout that is behind the commit it publishes runs an older script. The script refuses to run unless it is
// the one in that commit, whatever the checkout's own commit says.
func TestPublicSyncRefusesAScriptThatIsNotTheCommits(t *testing.T) {
	private, public := syncRepos(t)
	before := git(t, public, "rev-parse", "main")
	script, err := os.ReadFile(filepath.Join(private, "scripts", "public-sync.sh"))
	if err != nil {
		t.Fatal(err)
	}
	commit(t, private, "scripts/public-sync.sh", string(script)+"# a newer rule\n")
	git(t, private, "switch", "--quiet", "--detach", "HEAD~1")
	out, err := sync(t, private, ".", "origin/main", "--push")
	if err == nil || !strings.Contains(out, "is not the scripts/public-sync.sh of") {
		t.Fatalf("a checkout behind the commit it publishes: %v: %s", err, out)
	}
	if got := git(t, public, "rev-parse", "main"); got != before {
		t.Fatalf("public main moved to %s", got)
	}
}

// Another clone merged the hold after this clone last fetched. The script reads the private main as it is now.
func TestPublicSyncSeesAHoldThisCloneHasNotFetched(t *testing.T) {
	private, public := syncRepos(t)
	before := git(t, public, "rev-parse", "main")
	fix := commit(t, private, "fix.txt", "the fix\n")
	commit(t, private, "public-hold/fence", "the fence fix is not on the hosted machines yet\n")
	git(t, private, "reset", "--quiet", "--hard", fix)
	git(t, private, "update-ref", "refs/remotes/origin/main", fix)

	out, err := sync(t, private, ".", "origin/main", "--push")
	if err == nil || !strings.Contains(out, "held by public-hold/fence") {
		t.Fatalf("a hold merged since this clone's last fetch: %v: %s", err, out)
	}
	if got := git(t, public, "rev-parse", "main"); got != before {
		t.Fatalf("public main moved to %s", got)
	}
}
