package inventory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/kit/tunnel/cfsetup"
)

// put writes a file under home with the given mode.
func put(t *testing.T, home, rel string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func putConfig(t *testing.T, home, json string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, h Home, tb Table) Report {
	t.Helper()
	h.Dir = orHome(t, h.Dir)
	rep, err := Check(h, tb)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func orHome(t *testing.T, d string) string {
	if d == "" {
		return t.TempDir()
	}
	return d
}

func kinds(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, string(f.Severity)+":"+string(f.Kind)+":"+f.Row)
	}
	slices.Sort(out)
	return out
}

func TestEmptyHomePasses(t *testing.T) {
	rep := check(t, Home{}, Potluck())
	if rep.Failed() || len(rep.Findings) != 0 {
		t.Fatalf("findings on an empty home: %v", kinds(rep.Findings))
	}
}

func TestPotluckTableIsValid(t *testing.T) {
	if err := Potluck().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnKeysPass(t *testing.T) {
	home := t.TempDir()
	for _, rel := range []string{
		"tunnel/key", "tunnel/seal", "remotes/acme.token", "remotes/acme.headers",
		"tunnel/cloudflared/credentials.json", "tunnel/cloudflared/state.json",
		"tsnet/tailscaled.state", "tsnet/certs/zm.example.ts.net.key", "tsnet/certs/acme-account.key.pem",
		"e2b/ca-key.pem", "e2b/token-secret", "e2b/keys",
		"secrets/runners/app-7-client_secret", "secrets/runners/app-7-webhook_secret",
	} {
		put(t, home, rel, 0o600)
	}
	// Files that are not keys, in directories that hold keys.
	put(t, home, "e2b/ca.pem", 0o644)
	put(t, home, "e2b/ca-bundle.pem", 0o644)
	put(t, home, "secrets/.DS_Store", 0o644)
	put(t, home, "tunnel/nonces", 0o644)
	put(t, home, "tunnel/seal_peers.json", 0o644)
	put(t, home, "tsnet/tailscaled.log1.txt", 0o644)
	putConfig(t, home, `{"tunnel":{"key":"keychain:zm-home-key:abc","edge_secret":"s"},"github":{"lookup_token":"file:`+home+`/lookup"}}`)
	put(t, home, "lookup", 0o600)
	rep := check(t, Home{Dir: home, RunsAgentSessions: true}, Potluck())
	if len(rep.Findings) != 0 {
		t.Fatalf("findings: %v", kinds(rep.Findings))
	}
	if len(rep.Held) < 14 {
		t.Fatalf("scan found only %d keys: %+v", len(rep.Held), rep.Held)
	}
}

func TestUnlistedKeyFails(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, home string){
		"file under a root": func(t *testing.T, home string) { put(t, home, "secrets/stray.key", 0o600) },
		"file in remotes":   func(t *testing.T, home string) { put(t, home, "remotes/acme.pem", 0o600) },
		"config reference": func(t *testing.T, home string) {
			putConfig(t, home, `{"newthing":{"credential":"env:NEW_THING_KEY"}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			setup(t, home)
			rep := check(t, Home{Dir: home}, Potluck())
			if !rep.Failed() {
				t.Fatal("an unlisted key passed")
			}
			got := rep.Of(Fail)
			if len(got) != 1 || got[0].Kind != FindUnlisted || got[0].Row != "" || got[0].Fix == "" {
				t.Fatalf("findings: %+v", got)
			}
		})
	}
}

func TestTableRowFixesUnlisted(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/ghlink/proj.token", 0o600)
	zm := []Row{{ID: "ghlink-token", Name: "linker member token", Class: ClassO,
		Locations: []Location{FileAt("secrets/ghlink/*.token")}}}
	if rep := check(t, Home{Dir: home}, Potluck()); !rep.Failed() {
		t.Fatal("unlisted key passed")
	}
	if rep := check(t, Home{Dir: home}, Potluck().With(zm, nil, nil)); rep.Failed() || len(rep.Findings) != 0 {
		t.Fatalf("findings with the row added: %v", kinds(rep.Findings))
	}
}

func TestIgnoredFileIsNotAKey(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/README", 0o644)
	if rep := check(t, Home{Dir: home}, Potluck().With(nil, nil, []string{"secrets/README"})); len(rep.Findings) != 0 {
		t.Fatalf("findings: %v", kinds(rep.Findings))
	}
}

func appKeyHome(t *testing.T) string {
	home := t.TempDir()
	put(t, home, "secrets/runners/app-7-key.pem", 0o600)
	putConfig(t, home, `{"runners":{"github":{"app_id":7,"private_key":"file:`+home+`/secrets/runners/app-7-key.pem"}}}`)
	return home
}

func TestKeyBelowItsClassFailsWithoutException(t *testing.T) {
	tb := Potluck()
	tb.Exceptions = nil
	rep := check(t, Home{Dir: appKeyHome(t)}, tb)
	if !rep.Failed() {
		t.Fatal("a class S key held at O passed")
	}
	for _, f := range rep.Of(Fail) {
		if f.Kind != FindBelowClass || f.Row != "runner-app-key" || f.Fix == "" {
			t.Fatalf("finding: %+v", f)
		}
	}
	if n := len(rep.Of(Fail)); n != 1 { // one per row, not per place
		t.Fatalf("%d failures: %v", n, kinds(rep.Findings))
	}
}

func TestExceptionsListPasses(t *testing.T) {
	rep := check(t, Home{Dir: appKeyHome(t)}, Potluck())
	if rep.Failed() {
		t.Fatalf("failures with the exceptions list: %v", kinds(rep.Findings))
	}
}

func TestAgentWarning(t *testing.T) {
	cases := []struct {
		name   string
		home   func(t *testing.T) string
		agents bool
		want   []string
	}{
		{"app key and agents", appKeyHome, true, []string{"warn:agents-can-read:runner-app-key"}},
		{"app key, no agents", appKeyHome, false, nil},
		{"agents, no app key", func(t *testing.T) string {
			h := t.TempDir()
			put(t, h, "tunnel/key", 0o600)
			return h
		}, true, nil},
		{"issuer key kept elsewhere, and agents", func(t *testing.T) string {
			h := t.TempDir()
			putConfig(t, h, `{"secrets":{"issuers":{"ci":{"kind":"github-app","private_key":"keychain:ci-app"}}}}`)
			return h
		}, true, nil},
		{"issuer key is the runner App key file", func(t *testing.T) string {
			h := t.TempDir()
			put(t, h, "secrets/runners/app-7-key.pem", 0o600)
			putConfig(t, h, `{"secrets":{"issuers":{"ci":{"private_key":"file:`+h+`/secrets/runners/app-7-key.pem"}}}}`)
			return h
		}, true, []string{"warn:agents-can-read:runner-app-key"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := check(t, Home{Dir: c.home(t), RunsAgentSessions: c.agents}, Potluck())
			if rep.Failed() {
				t.Fatalf("a warning must not fail the check: %v", kinds(rep.Findings))
			}
			if got := kinds(rep.Findings); !slices.Equal(got, c.want) {
				t.Fatalf("findings %v, want %v", got, c.want)
			}
			for _, f := range rep.Findings {
				if f.Fix == "" || f.Exception != "zm-eakh.12.54.13" {
					t.Fatalf("warning without a fix or its bead: %+v", f)
				}
			}
		})
	}
}

func TestLooseFileFailsEvenWithException(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/runners/app-7-key.pem", 0o644)
	rep := check(t, Home{Dir: home}, Potluck())
	if got := kinds(rep.Of(Fail)); !slices.Equal(got, []string{"FAIL:loose-mode:runner-app-key"}) {
		t.Fatalf("failures: %v", got)
	}
}

func TestLooseReferencedFileOutsideHomeFails(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "lookup")
	put(t, filepath.Dir(outside), "lookup", 0o640)
	putConfig(t, home, `{"github":{"lookup_token":"file:`+outside+`"}}`)
	rep := check(t, Home{Dir: home}, Potluck())
	if got := kinds(rep.Of(Fail)); !slices.Equal(got, []string{"FAIL:loose-mode:github-lookup-token"}) {
		t.Fatalf("failures: %v", got)
	}
}

func TestLiteralKeyInConfigFails(t *testing.T) {
	home := t.TempDir()
	putConfig(t, home, `{"github":{"lookup_token":"ghp_notareference"}}`)
	rep := check(t, Home{Dir: home}, Potluck())
	if got := kinds(rep.Of(Fail)); !slices.Equal(got, []string{"FAIL:literal:github-lookup-token"}) {
		t.Fatalf("failures: %v", got)
	}
}

func TestKeyBehindSymlinkedDirectoryIsSeen(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	put(t, elsewhere, "app-7-key.pem", 0o644)
	if err := os.MkdirAll(filepath.Join(home, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, "secrets", "runners")); err != nil {
		t.Fatal(err)
	}
	// A loop must not hang the walk.
	if err := os.Symlink(home, filepath.Join(elsewhere, "loop")); err != nil {
		t.Fatal(err)
	}
	rep := check(t, Home{Dir: home}, Potluck())
	if got := kinds(rep.Of(Fail)); !slices.Equal(got, []string{"FAIL:loose-mode:runner-app-key"}) {
		t.Fatalf("failures: %v", got)
	}

	// The root itself a symlink.
	home2 := t.TempDir()
	put(t, elsewhere, "stray.key", 0o600)
	if err := os.Symlink(elsewhere, filepath.Join(home2, "secrets")); err != nil {
		t.Fatal(err)
	}
	if rep := check(t, Home{Dir: home2}, Potluck()); !rep.Failed() {
		t.Fatal("a key behind a symlinked root passed")
	}
}

func TestIgnoreNeverExcusesAListedKey(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/runners/app-7-key.pem", 0o644)
	tb := Potluck().With(nil, nil, []string{"secrets/**"})
	if rep := check(t, Home{Dir: home}, tb); !rep.Failed() {
		t.Fatal("an ignore pattern hid a loose listed key")
	}
}

func TestLooseReferencedFileInsideHomeFails(t *testing.T) {
	home := t.TempDir()
	put(t, home, "keys/lookup", 0o644)
	putConfig(t, home, `{"github":{"lookup_token":"file:`+filepath.Join(home, "keys", "lookup")+`"}}`)
	rep := check(t, Home{Dir: home}, Potluck())
	if got := kinds(rep.Of(Fail)); !slices.Equal(got, []string{"FAIL:loose-mode:github-lookup-token"}) {
		t.Fatalf("failures: %v", got)
	}
}

func TestNonStringWhereAReferenceBelongsFails(t *testing.T) {
	for _, cfg := range []string{
		`{"github":{"lookup_token":{"v":"ghp_x"}}}`,
		`{"github":{"lookup_token":["ghp_x"]}}`,
		`{"github":{"lookup_token":12345}}`,
	} {
		home := t.TempDir()
		putConfig(t, home, cfg)
		rep := check(t, Home{Dir: home}, Potluck())
		if got := kinds(rep.Of(Fail)); !slices.Equal(got, []string{"FAIL:literal:github-lookup-token"}) {
			t.Fatalf("%s: failures: %v", cfg, got)
		}
	}
}

func TestIssuerNameWithADotIsStillAnIssuer(t *testing.T) {
	home := t.TempDir()
	putConfig(t, home, `{"secrets":{"issuers":{"ci.prod":{"private_key":"keychain:ci"}}}}`)
	rep := check(t, Home{Dir: home, RunsAgentSessions: true}, Potluck())
	if got := kinds(rep.Findings); len(got) != 0 {
		t.Fatalf("findings: %v", got)
	}
	if len(rep.Held) != 1 || rep.Held[0].Row != "issuer-key" {
		t.Fatalf("held: %+v", rep.Held)
	}
}

func TestLeftoverTemporaryCopyOfAKeyFails(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/runners/app-7-key.pem.tmp", 0o600)
	if rep := check(t, Home{Dir: home}, Potluck()); !rep.Failed() {
		t.Fatal("a stray copy of a key passed")
	}
}

func TestReferencedFileUnderARootIsThatRowsKey(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/github-lookup", 0o600)
	putConfig(t, home, `{"github":{"lookup_token":"file:`+filepath.Join(home, "secrets", "github-lookup")+`"}}`)
	if rep := check(t, Home{Dir: home}, Potluck()); len(rep.Findings) != 0 {
		t.Fatalf("findings: %v", kinds(rep.Findings))
	}
	// The same file, loose: judged once, as that row's.
	put(t, home, "secrets/github-lookup", 0o644)
	rep := check(t, Home{Dir: home}, Potluck())
	if got := kinds(rep.Of(Fail)); !slices.Equal(got, []string{"FAIL:loose-mode:github-lookup-token"}) {
		t.Fatalf("failures: %v", got)
	}
}

func TestHomeThatIsNotAnExistingAbsoluteDirectoryIsAnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"", "relative/home", filepath.Join(t.TempDir(), "missing"), file} {
		if _, err := Check(Home{Dir: dir}, Potluck()); err == nil {
			t.Errorf("Check passed for home %q", dir)
		}
	}
}

func TestLiteralRowReadsOnlyItsOwnPath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every directory")
	}
	home := t.TempDir()
	locked := filepath.Join(home, "projects", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	put(t, home, "receipt.key", 0o600)
	tb := Potluck().With([]Row{{ID: "receipt-key", Name: "receipt key", Class: ClassO, Locations: []Location{FileAt("receipt.key")}}}, nil, nil)
	rep := check(t, Home{Dir: home}, tb)
	if len(rep.Findings) != 0 || len(rep.Held) != 1 {
		t.Fatalf("held %+v findings %v", rep.Held, kinds(rep.Findings))
	}
}

func TestRowsNameTheDirectoriesTheCodeWrites(t *testing.T) {
	want := map[string]string{
		"tunnel/key":                    tunnel.KeyFile,
		"tunnel/seal":                   tunnel.SealKeyFile,
		"remotes/*.token":               teamlink.DirRemotes + "/*.token",
		"tsnet/tailscaled.state":        tailnet.StateDir + "/tailscaled.state",
		"tunnel/cloudflared/state.json": filepath.ToSlash(strings.TrimPrefix(cfsetup.Dir(""), string(filepath.Separator))) + "/state.json",
	}
	have := map[string]bool{}
	for _, r := range Potluck().Rows {
		for _, l := range r.Locations {
			have[l.Pattern] = true
		}
	}
	for pattern, fromCode := range want {
		if pattern != fromCode {
			t.Errorf("row pattern %q, code writes %q", pattern, fromCode)
		}
		if !have[pattern] {
			t.Errorf("no row lists %q", pattern)
		}
	}
}

func TestLooseConfigFileFailsForKeysWrittenInIt(t *testing.T) {
	home := t.TempDir()
	putConfig(t, home, `{"tunnel":{"edge_secret":"s"},"github":{"lookup_token":"ghp_literal"}}`)
	if err := os.Chmod(filepath.Join(home, "config.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := check(t, Home{Dir: home}, Potluck())
	want := []string{"FAIL:literal:github-lookup-token", "FAIL:loose-mode:github-lookup-token", "FAIL:loose-mode:tunnel-edge-secret"}
	if got := kinds(rep.Of(Fail)); !slices.Equal(got, want) {
		t.Fatalf("failures: %v", got)
	}
}

func TestLooseFindingNamesTheReferencedFile(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	put(t, outside, "lookup", 0o640)
	putConfig(t, home, `{"github":{"lookup_token":"file:`+filepath.Join(outside, "lookup")+`"}}`)
	rep := check(t, Home{Dir: home}, Potluck())
	f := rep.Of(Fail)
	if len(f) != 1 || !strings.Contains(f[0].Fix, filepath.Join(outside, "lookup")) {
		t.Fatalf("findings: %+v", f)
	}
}

func TestSymlinkAliasInsideARootIsNotAnotherPath(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/runners/app-1-key.pem", 0o600)
	if err := os.Symlink("runners", filepath.Join(home, "secrets", "alias")); err != nil {
		t.Fatal(err)
	}
	rep := check(t, Home{Dir: home}, Potluck())
	if rep.Failed() || len(rep.Held) != 1 {
		t.Fatalf("held %+v findings %v", rep.Held, kinds(rep.Findings))
	}
}

func TestKeyBehindALinkOutOfTheRootsIsStillUnlisted(t *testing.T) {
	home := t.TempDir()
	put(t, home, "stash/leaked.pem", 0o600)
	put(t, home, "runnerkeys/app-1-key.pem", 0o600)
	put(t, home, "runnerkeys/app-1-key.pem.bak", 0o600)
	if err := os.MkdirAll(filepath.Join(home, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "stash"), filepath.Join(home, "secrets", "more")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "runnerkeys"), filepath.Join(home, "secrets", "runners")); err != nil {
		t.Fatal(err)
	}
	rep := check(t, Home{Dir: home}, Potluck())
	var unlisted []string
	for _, f := range rep.Of(Fail) {
		if f.Kind == FindUnlisted {
			unlisted = append(unlisted, f.Where)
		}
	}
	slices.Sort(unlisted)
	if want := []string{"secrets/more/leaked.pem", "secrets/runners/app-1-key.pem.bak"}; !slices.Equal(unlisted, want) {
		t.Fatalf("unlisted %v, want %v", unlisted, want)
	}
}

func TestOneLooseFileIsOneFinding(t *testing.T) {
	home := t.TempDir()
	put(t, home, "secrets/runners/app-7-key.pem", 0o644)
	putConfig(t, home, `{"secrets":{"issuers":{"ci":{"private_key":"file:`+filepath.Join(home, "secrets", "runners", "app-7-key.pem")+`"}}}}`)
	rep := check(t, Home{Dir: home}, Potluck())
	if n := len(rep.Of(Fail)); n != 1 {
		t.Fatalf("%d failures: %v", n, kinds(rep.Findings))
	}
}

func TestBadConfigIsAnErrorNotAPass(t *testing.T) {
	home := t.TempDir()
	putConfig(t, home, `{not json`)
	if _, err := Check(Home{Dir: home}, Potluck()); err == nil {
		t.Fatal("an unreadable config.json passed")
	}
}

func TestValidateRefusesBadTables(t *testing.T) {
	ok := Row{ID: "a", Name: "a", Class: ClassO, Locations: []Location{FileAt("x/y")}}
	for name, tb := range map[string]Table{
		"duplicate id":                       {Rows: []Row{ok, ok}},
		"no id":                              {Rows: []Row{{Name: "n", Class: ClassO}}},
		"unknown class":                      {Rows: []Row{{ID: "a", Class: "Z"}}},
		"E with a place":                     {Rows: []Row{{ID: "a", Class: ClassE, Locations: []Location{FileAt("x")}}}},
		"absolute path":                      {Rows: []Row{{ID: "a", Class: ClassO, Locations: []Location{FileAt("/etc/x")}}}},
		"bad pattern":                        {Rows: []Row{{ID: "a", Class: ClassO, Locations: []Location{FileAt("x/[")}}}},
		"exception, no row":                  {Rows: []Row{ok}, Exceptions: map[string]string{"b": "zm-1"}},
		"exception, no bead":                 {Rows: []Row{ok}, Exceptions: map[string]string{"a": ""}},
		"exception on a class O row":         {Rows: []Row{ok}, Exceptions: map[string]string{"a": "zm-1"}},
		"root with a wildcard":               {Roots: []string{"secrets/*"}},
		"root above the home":                {Roots: []string{"../x"}},
		"later segment is a bad pattern":     {Rows: []Row{{ID: "a", Class: ClassO, Locations: []Location{FileAt("a/[ab]/[")}}}},
		"root with a trailing slash":         {Roots: []string{"secrets/"}},
		"root dot":                           {Roots: []string{"."}},
		"file pattern with no literal start": {Rows: []Row{{ID: "a", Class: ClassO, Locations: []Location{FileAt("*/x")}}}},
		"file pattern with an empty segment": {Rows: []Row{{ID: "a", Class: ClassO, Locations: []Location{FileAt("a//x")}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tb.Validate(); err == nil {
				t.Fatal("a bad table validated")
			}
			if _, err := Check(Home{Dir: t.TempDir()}, tb); err == nil {
				t.Fatal("Check ran a bad table")
			}
		})
	}
}

func TestMatchSegments(t *testing.T) {
	for _, c := range []struct {
		pat, name string
		want      bool
	}{
		{"a/*.token", "a/x.token", true},
		{"a/*.token", "a/b/x.token", false},
		{"a/**", "a/b/c", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/b", "a", false},
	} {
		got, err := matchSegments(c.pat, c.name, "/")
		if err != nil || got != c.want {
			t.Errorf("match(%q, %q) = %v, %v; want %v", c.pat, c.name, got, err, c.want)
		}
	}
	if ok, _ := matchSegments("secrets.issuers.*.private_key", "secrets.issuers.ci.private_key", "."); !ok {
		t.Error("dotted pattern did not match")
	}
}
