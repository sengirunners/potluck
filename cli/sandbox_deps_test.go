package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The flags are on both commands that acquire a box, and a refusal comes
// before any sandbox is made.
func TestDepsFlagsOnCommands(t *testing.T) {
	for _, args := range [][]string{{"sandbox", "create"}, {"run"}} {
		help := runHelp(t, args...)
		for _, f := range []string{"--deps ", "--deps-wait ", "--deps-scripts "} {
			if !strings.Contains(help, f) {
				t.Errorf("potluck %s --help lacks %s", strings.Join(args, " "), f)
			}
		}
	}
}

func runHelp(t *testing.T, args ...string) string {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	var o, e bytes.Buffer
	if code := Run(context.Background(), append(args, "--help"), strings.NewReader(""), &o, &e); code != 0 {
		t.Fatalf("%v --help: %d %s", args, code, e.String())
	}
	return o.String()
}
