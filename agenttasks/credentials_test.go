package agenttasks_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/agenttasks"
)

func TestCredentialDescriptor(t *testing.T) {
	for _, name := range agenttasks.Default().Secrets {
		t.Run(name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			const token = "descriptor-sentinel"
			if err := os.WriteFile(filepath.Join(dir, name), []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			a := agenttasks.Default()
			a.Binary = "sh"
			a.Argv = []string{"sh", "-c", `test "$(cat <&3)" = descriptor-sentinel || exit 41
sh -c 'test -z "${CLAUDE_CODE_OAUTH_TOKEN+x}" && test -z "${ANTHROPIC_API_KEY+x}" && test -z "${ANTHROPIC_AUTH_TOKEN+x}" && test -z "${CLAUDE_CODE_REMOTE+x}" && test -z "${CLAUDE_BG_AUTH_SNAPSHOT_PATH+x}"' || exit 42
printenv`}
			argv, err := a.CredentialCommand(name, "", "")
			if err != nil {
				t.Fatal(err)
			}
			run := func() ([]byte, error) {
				cmd := exec.Command(argv[0], argv[1:]...)
				cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "POTLUCK_SECRETS=" + dir,
					"CLAUDE_CODE_OAUTH_TOKEN=" + token, "ANTHROPIC_API_KEY=" + token, "ANTHROPIC_AUTH_TOKEN=" + token,
					"CLAUDE_CODE_REMOTE=1", "CLAUDE_BG_AUTH_SNAPSHOT_PATH=/must-not-read", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=99", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=98"}
				return cmd.CombinedOutput()
			}
			out, err := run()
			if err != nil {
				t.Fatalf("%s: %v", out, err)
			}
			if strings.Contains(string(out), token) || !strings.Contains(string(out), a.CredentialFD[name]+"=3") {
				t.Fatalf("credential environment: %s", out)
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			out, err = run()
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != agenttasks.ExitNotStarted {
				t.Fatalf("missing door file: %s %v", out, err)
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("credential wrote to HOME: %v %v", entries, err)
			}
		})
	}
}

func TestCredentialMappingRefusesUnsupported(t *testing.T) {
	a := agenttasks.Default()
	a.CredentialFD = nil
	if _, err := a.CredentialCommand(a.Secrets[0], "", ""); err == nil {
		t.Fatal("missing descriptor mapping accepted")
	}
	a = agenttasks.Default()
	a.Env = map[string]string{"CLAUDE_CODE_REMOTE": "1"}
	if _, err := a.CredentialCommand(a.Secrets[0], "", ""); err == nil {
		t.Fatal("conflicting adapter environment accepted")
	}
}
