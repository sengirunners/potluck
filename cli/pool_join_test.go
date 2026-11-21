package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

var memberCodeRe = regexp.MustCompile(`potluck join '([^']+)'`)

// A code used once answers the next machine with the pool's own command
// for a new one, in this program's words.
func TestJoinUsedCodeNamesPoolInvite(t *testing.T) {
	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "laptop", "ana@example.com")
	c := startMachine(t, tn, "other", "eve@example.com")
	a.ok(t, "create", "friends")
	m := memberCodeRe.FindStringSubmatch(a.ok(t, "invite", "ana"))
	if m == nil {
		t.Fatal("no member code")
	}
	b.ok(t, "join", m[1])
	_, e, code := c.run(t, "join", m[1])
	if code == 0 || !strings.Contains(e, "invalid, used or expired join code") || !strings.Contains(e, "potluck invite <you>") || strings.Contains(e, "--role") {
		t.Fatalf("join with a used code: exit %d\n%s", code, e)
	}
}

var linkRe = regexp.MustCompile(`(https://potluck\.sh/join#v1\.[A-Za-z0-9_-]+)`)

// A member invite prints its link first (the potluck.sh page) and the code
// second; the link joins as the code does, once; --json carries the link.
func TestJoinByInviteLink(t *testing.T) {
	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "laptop", "ana@example.com")
	c := startMachine(t, tn, "other", "eve@example.com")
	a.ok(t, "create", "friends")
	out := a.ok(t, "invite", "ana")
	l, m := linkRe.FindStringSubmatch(out), memberCodeRe.FindStringSubmatch(out)
	if l == nil || m == nil || strings.Index(out, l[1]) > strings.Index(out, m[0]) {
		t.Fatalf("want the link, then the code:\n%s", out)
	}
	if got, err := project.JoinCodeOf(l[1]); err != nil || got != m[1] {
		t.Fatalf("the link holds %q (%v), the code printed is %q", got, err, m[1])
	}
	if out := b.ok(t, "join", l[1]); !strings.Contains(out, "joined pool friends") {
		t.Fatalf("join by link:\n%s", out)
	}
	if _, e, code := c.run(t, "join", l[1]); code == 0 || !strings.Contains(e, "invalid, used or expired join code") {
		t.Fatalf("a used link: exit %d\n%s", code, e)
	}
	if _, e, code := c.run(t, "join", "https://potluck.sh/join#v1.not*base64"); code == 0 || !strings.Contains(e, "invite link is incomplete") {
		t.Fatalf("a damaged link: exit %d\n%s", code, e)
	}
	var j struct {
		Code string `json:"code"`
		Link string `json:"link"`
	}
	if err := json.Unmarshal([]byte(a.ok(t, "--json", "invite", "ben")), &j); err != nil || j.Link != project.JoinLink("https://potluck.sh/join", j.Code) {
		t.Fatalf("invite --json: %+v %v", j, err)
	}
}

// join --github with an invite, at a machine that runs no GitHub logins:
// the sign-in is optional, so the join goes on with what the invite gave
// and says why it did not sign in. --github without an invite is refused.
func TestJoinGitHubIsOptional(t *testing.T) {
	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "laptop", "ana@example.com")
	a.ok(t, "create", "friends")
	l := linkRe.FindStringSubmatch(a.ok(t, "invite", "ana"))
	if l == nil {
		t.Fatal("no link")
	}
	out := b.ok(t, "join", l[1], "--github")
	if !strings.Contains(out, "joined pool friends") || !strings.Contains(out, "GitHub: GitHub sign-in did not start") || !strings.Contains(out, "no GitHub OAuth client id") {
		t.Fatalf("join --github with no GitHub at the home:\n%s", out)
	}
	if _, e, code := b.run(t, "join", "friends", "--github"); code != 2 || !strings.Contains(e, "--github goes with an invite") {
		t.Fatalf("--github by name: exit %d\n%s", code, e)
	}
}
