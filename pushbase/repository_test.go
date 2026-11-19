package pushbase_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/pushbase"
)

func TestRepositoryIdentityAndDefaultBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return string(out)
	}
	git("init", "-q")
	commit := func(value string) *boxpush.Source {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", ".")
		git("commit", "-qm", value)
		src, err := boxpush.Resolve(context.Background(), boxpush.MakeOptions{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		return src
	}
	first := commit("first")
	initial, err := pushbase.Identify(context.Background(), first)
	if err != nil || initial == nil || initial.Commit != first.Commit {
		t.Fatalf("initial: %+v %v", initial, err)
	}
	git("update-ref", "refs/remotes/origin/main", first.Commit)
	git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	second := commit("second")
	next, err := pushbase.Identify(context.Background(), second)
	if err != nil || next == nil || next.Key != initial.Key || next.Commit != first.Commit {
		t.Fatalf("default branch base: %+v %v", next, err)
	}
	// A moving branch must not change the source we already resolved.
	git("checkout", "--orphan", "unrelated")
	other := commit("unrelated")
	unrelated, err := pushbase.Identify(context.Background(), other)
	if err != nil || unrelated == nil || unrelated.Key == initial.Key || unrelated.Commit != other.Commit {
		t.Fatalf("unrelated: %+v %v", unrelated, err)
	}
	pinned, err := pushbase.Identify(context.Background(), second)
	if err != nil || pinned == nil || *pinned != *next {
		t.Fatalf("source moved with HEAD: %+v %v", pinned, err)
	}
	// A shallow boundary is not a repository root suitable for sharing.
	gitDir := filepath.Join(dir, ".git")
	if err := os.WriteFile(filepath.Join(gitDir, "shallow"), []byte(other.Commit+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	shallow, err := pushbase.Identify(context.Background(), other)
	if err != nil || shallow != nil {
		t.Fatalf("shallow checkout offered a base: %+v %v", shallow, err)
	}
}
