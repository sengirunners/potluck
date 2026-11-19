package cli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
)

// shellWords is how a POSIX shell splits a command a person pasted:
// quotes, backslash-newline continuations, and every byte that is not a
// plain space, tab or newline kept inside a word.
func shellWords(t *testing.T, pasted string) []string {
	t.Helper()
	out, err := exec.Command("sh", "-c", `eval "set -- $1"; for w; do printf '%s\0' "$w"; done`, "sh", pasted).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// What a person pastes as host's or join's arguments is checked before the
// daemon (and so before any pull or join): a whole invite, on one line or
// over several with plain spaces, goes on; one cut short, damaged or
// changed by a copy is refused, exit 2, saying what is wrong in the host
// program's words and never repeating the invite. Nothing is corrected.
func TestPastedInvites(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldDetect, oldRunner := detectHost, shareRunner
	t.Cleanup(func() { detectHost, shareRunner = oldDetect, oldRunner })
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Error("a pasted command ran something")
		return hostprobe.Result{}, nil
	})
	const secret, fp = "ABCD-EFGH", "abcdefghijklmnop"
	code := "potluck-mini:" + fp + ":" + secret
	if _, err := project.ParseJoinCode(code); err != nil {
		t.Fatalf("the test's invite: %v", err)
	}

	// run runs words under top (the potluck program's spelling, or a
	// program that mounts the commands under "supper pool"), reporting
	// whether the commands reached the daemon's client, which refuses.
	run := func(t *testing.T, top string, words ...string) (out string, exit int, reached bool) {
		var o, e bytes.Buffer
		h := Host{Prefix: top, Stdout: &o, Stderr: &e, Client: func() (*client.Client, *config.Config, error) {
			reached = true
			return nil, nil, errors.New("reached the daemon's client")
		}}
		root := &cobra.Command{Use: strings.Fields(top)[0], SilenceUsage: true, SilenceErrors: true}
		parent := root
		if g := strings.Fields(top)[1:]; len(g) == 1 {
			parent = &cobra.Command{Use: g[0]}
			root.AddCommand(parent)
			words = append(slices.Clone(g), words...)
		}
		parent.AddCommand(Commands(h)...)
		root.SetArgs(words)
		root.SetOut(&o)
		root.SetErr(&e)
		if err := root.Execute(); err != nil {
			exit = Report(h, err)
		}
		return o.String() + e.String(), exit, reached
	}

	oneLine := "host '" + code + "' --image " + refPin + " --no-sudo"
	tests := []struct {
		name  string
		words []string
		want  []string // nil: the words reach the daemon's client
	}{
		{"whole, one line", shellWords(t, oneLine), nil},
		{"whole, over lines with plain spaces", shellWords(t, "host '"+code+"' \\\n  --image "+refPin+" \\\n  --no-sudo"), nil},
		{"join, whole", []string{"join", code}, nil},
		{"only the secret", []string{"host", secret}, []string{"only the last part of an invite, its secret", "invite <you> --host", "the invite is not spent"}},
		{"join, only the secret", []string{"join", secret}, []string{"only the last part of an invite", "invite <you>"}},
		{"two parts", []string{"host", "potluck-mini:" + fp}, []string{"incomplete: it has 2 of the 3 parts"}},
		{"secret cut short", []string{"host", "potluck-mini:" + fp + ":ABCD-EF"}, []string{"damaged: it has its 3 parts"}},
		{"over lines with no-break spaces",
			shellWords(t, "host '"+code+"' \\\n\u00a0\u00a0--image "+refPin+" \\\n\u00a0\u00a0--no-sudo"),
			[]string{"argument 2 of 4 after host holds a no-break space (U+00A0)", "looks like the flag --image", "one line with its quotes"}},
		{"a no-break space after a flag", shellWords(t, "host '"+code+"' --image\u00a0"+refPin),
			[]string{"the flag --image is followed by a no-break space (U+00A0)"}},
		{"curly quotes", shellWords(t, "host \u2018"+code+"\u2019"), []string{"argument 1 of 1 after host holds a curly quote (U+2018) where a straight ' goes"}},
		{"a long dash", []string{"host", code, "\u2014no-sudo"}, []string{"a long dash (U+2014)", "looks like the flag --no-sudo"}},
		{"unquoted, split", []string{"host", "potluck-mini:" + fp + ":ABCD", "EFGH"},
			[]string{"host takes at most one argument, the invite in single quotes or a pool's name, but got 2 arguments"}},
	}
	for _, top := range []string{"potluck", "supper pool"} {
		for _, tc := range tests {
			t.Run(top+"/"+tc.name, func(t *testing.T) {
				out, exit, reached := run(t, top, tc.words...)
				if tc.want == nil {
					if exit != 1 || !reached || !strings.Contains(out, "reached the daemon's client") {
						t.Fatalf("exit %d (reached the daemon: %v), want the daemon's client:\n%s", exit, reached, out)
					}
					return
				}
				if exit != 2 || reached {
					t.Fatalf("exit %d (reached the daemon: %v), want 2 before it:\n%s", exit, reached, out)
				}
				for _, w := range tc.want {
					if strings.HasPrefix(w, "invite <you>") {
						w = top + " " + w
					}
					if !strings.Contains(out, w) {
						t.Errorf("no %q in:\n%s", w, out)
					}
				}
				for _, part := range []string{"ABCD", "EFGH", fp, refPin} {
					if strings.Contains(out, part) {
						t.Errorf("the refusal repeats %q:\n%s", part, out)
					}
				}
				if top != "potluck" {
					if n := namesCommand(out, "potluck", []string{"invite", "host", "join"}); n != "" {
						t.Errorf("names potluck's %s, not supper pool's:\n%s", n, out)
					}
				}
			})
		}
	}
}
