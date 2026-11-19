package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// teamProject makes slug a team project on m (no pool areas), its tailnet
// node up.
func teamProject(t *testing.T, m *machine, slug string) {
	t.Helper()
	ctx := context.Background()
	c := client.WithHandler(m.dm.SocketHandler())
	if st, err := c.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("tailnet up: %+v %v", st, err)
	}
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: slug}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProjectTeam(ctx, slug); err != nil {
		t.Fatal(err)
	}
}

// notRedeemed reports whether the invite made as as is missing or not redeemed.
func notRedeemed(is []api.Invite, as string) bool {
	for _, i := range is {
		if i.As == as {
			return i.State != api.InviteRedeemed
		}
	}
	return true
}

var projectCodeRe = regexp.MustCompile(`potluck join '([^']+)'`)

// invite --as admits a machine into a team project as an identity; join
// redeems it with the member's checkouts and says what it joined; invite
// ls and rm list and remove invites; remotes and leave work on it as on a
// pool.
func TestInviteAndJoinATeamProject(t *testing.T) {
	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "laptop", "ana@example.com")
	teamProject(t, a, "crew")

	out := a.ok(t, "invite", "crew", "--as", "ana", "--trust", "public")
	m := projectCodeRe.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "ana as member in crew, its machine public") {
		t.Fatalf("invite --as:\n%s", out)
	}
	checkout, _ := filepath.EvalSymlinks(t.TempDir())
	out = b.ok(t, "join", m[1], "--path", checkout)
	if !strings.Contains(out, "joined project crew on mini as ana (member)") || !strings.Contains(out, "checkouts: "+checkout) ||
		strings.Contains(out, "sandbox create") {
		t.Fatalf("join:\n%s", out)
	}

	var ir inviteAnswer
	if err := json.Unmarshal([]byte(a.ok(t, "--json", "invite", "crew", "--as", "cy")), &ir); err != nil || ir.Code == "" || ir.Invite.As != "cy" {
		t.Fatalf("invite --as --json: %+v %v", ir, err)
	}
	if _, e, code := b.run(t, "join", m[1]); code == 0 || !strings.Contains(e, "potluck invite <project> --as <you> for a team project") {
		t.Fatalf("join with a used project code: exit %d\n%s", code, e)
	}

	var rs []api.Remote
	if err := json.Unmarshal([]byte(b.ok(t, "--json", "remotes")), &rs); err != nil || len(rs) != 1 || rs[0].Project.Slug != "crew" {
		t.Fatalf("remotes --json: %+v %v", rs, err)
	}

	var is []api.Invite
	if err := json.Unmarshal([]byte(a.ok(t, "--json", "invite", "ls")), &is); err != nil || len(is) != 2 || notRedeemed(is, "ana") {
		t.Fatalf("invite ls --json: %+v %v", is, err)
	}
	if out := a.ok(t, "invite", "ls"); !strings.Contains(out, "by laptop") || !strings.Contains(out, "public") {
		t.Fatalf("invite ls:\n%s", out)
	}
	live := projectCodeRe.FindStringSubmatch(a.ok(t, "invite", "crew", "--as", "bo", "--role", "guest"))
	if live == nil {
		t.Fatal("no second code")
	}
	if err := json.Unmarshal([]byte(a.ok(t, "--json", "invite", "ls")), &is); err != nil || len(is) != 3 {
		t.Fatalf("invite ls after a second: %+v %v", is, err)
	}
	var id string
	for _, i := range is {
		if i.As == "bo" {
			id = i.ID
		}
	}
	if out := a.ok(t, "invite", "rm", "--", id); !strings.Contains(out, "removed invite "+id) {
		t.Fatalf("invite rm:\n%s", out)
	}

	if out := b.ok(t, "leave", "crew"); !strings.Contains(out, "left crew") {
		t.Fatalf("leave:\n%s", out)
	}
}

// A pool's invite (no --as) into a team project that is not a pool is
// refused, naming the project form, and makes no pool area there.
func TestPoolInviteRefusesATeamProject(t *testing.T) {
	a := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	teamProject(t, a, "crew")
	_, e, code := a.run(t, "invite", "ana")
	if code != 2 || !strings.Contains(e, "crew is a team project, not a pool") || !strings.Contains(e, "potluck invite crew --as ana") {
		t.Fatalf("pool invite into a team project: exit %d\n%s", code, e)
	}
	p, err := client.WithHandler(a.dm.SocketHandler()).GetProject(context.Background(), "crew")
	if err != nil || len(p.Areas) != 0 {
		t.Fatalf("areas after a refused invite: %+v %v", p.Areas, err)
	}
}

// invite's two forms do not mix their flags, and join <project>/<area>
// needs --from; each is a usage error before the daemon is asked.
func TestInviteAndJoinFlagRules(t *testing.T) {
	a := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"invite", "crew", "--as", "ana", "--host"}, "--host and --image are a pool's"},
		{[]string{"invite", "ana", "--role", "guest"}, "--role goes with --as"},
		{[]string{"invite", "crew", "--as", "ana", "--role", "owner"}, "--role is member or guest"},
		{[]string{"invite", "Crew!", "--as", "ana"}, "takes <project> or <project>/<area>"},
		{[]string{"join", "crew/docs"}, "also say where, --from"},
	} {
		if _, e, code := a.run(t, tc.args...); code != 2 || !strings.Contains(e, tc.want) {
			t.Errorf("%s: exit %d\n%s", strings.Join(tc.args, " "), code, e)
		}
	}
}

// A host's AfterJoin runs after a join into a team project, in the host's
// words, and not under --json; a pool join does not run it.
func TestAfterJoinRunsForATeamProject(t *testing.T) {
	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "laptop", "ana@example.com")
	teamProject(t, a, "crew")
	a.ok(t, "create", "friends")

	run := func(asJSON bool, args ...string) (string, int) {
		t.Helper()
		var o, e bytes.Buffer
		calls := 0
		h := Host{Prefix: "supper", JSON: func() bool { return asJSON },
			Client: func() (*client.Client, *config.Config, error) {
				cfg, err := config.LoadClientFrom(b.home)
				return client.NewUnix(filepath.Join(b.home, "potluck.sock")), cfg, err
			},
			Stdout: &o, Stderr: &e,
			AfterJoin: func(w io.Writer, r api.JoinResponse) {
				calls++
				io.WriteString(w, "supper: next, supper work in "+r.Remote.Project.Slug+"\n")
			}}
		root := &cobra.Command{Use: "supper", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(Commands(h)...)
		root.SetArgs(args)
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("supper %s: %v\n%s", strings.Join(args, " "), err, e.String())
		}
		return o.String(), calls
	}
	code := func(out string) string {
		t.Helper()
		m := projectCodeRe.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no code in:\n%s", out)
		}
		return m[1]
	}
	if out, calls := run(false, "join", code(a.ok(t, "invite", "crew", "--as", "ana"))); calls != 1 || !strings.Contains(out, "supper: next, supper work in crew") {
		t.Fatalf("team project join: %d calls\n%s", calls, out)
	}
	if out, calls := run(false, "join", code(a.ok(t, "--pool", "friends", "invite", "ana"))); calls != 0 || !strings.Contains(out, "supper sandbox create") {
		t.Fatalf("pool join: %d calls\n%s", calls, out)
	}
}

// A sole authority is still shown with its tailnet owner and confirmed on a
// terminal; elsewhere, and with several, --from must name it. Skipped
// machines are listed with why.
func TestPickAuthority(t *testing.T) {
	a := newApp(Host{Prefix: "supper", Stdout: io.Discard, Stderr: io.Discard, Client: func() (*client.Client, *config.Config, error) { return nil, nil, nil }})
	one := api.JoinResponse{Authorities: []api.JoinAuthority{{Machine: "evil", Owner: "mallory@example.com"}},
		Skipped: []api.JoinSkipped{{Machine: "plain", Reason: "runs no GitHub logins (github:false)"}}}
	var msg bytes.Buffer
	if m, err := a.pickAuthority("harbor", one, true, strings.NewReader("y\n"), &msg); err != nil || m != "evil" {
		t.Fatalf("yes: %q %v", m, err)
	}
	if !strings.Contains(msg.String(), "supper: join harbor at evil (tailnet owner mallory@example.com)? [y/N]") ||
		!strings.Contains(msg.String(), "supper: skipped plain: runs no GitHub logins") {
		t.Fatalf("prompt %q", msg.String())
	}
	for what, tc := range map[string]struct {
		r        api.JoinResponse
		terminal bool
		in       string
	}{"no": {one, true, "n\n"}, "enter": {one, true, "\n"}, "no terminal": {one, false, "y\n"},
		"several": {api.JoinResponse{Authorities: []api.JoinAuthority{{Machine: "a"}, {Machine: "b"}}}, true, "y\n"}} {
		m, err := a.pickAuthority("harbor", tc.r, tc.terminal, strings.NewReader(tc.in), &bytes.Buffer{})
		if err == nil {
			t.Errorf("%s: picked %q", what, m)
		} else if what == "several" && !strings.Contains(err.Error(), "supper join harbor --from <machine>") {
			t.Errorf("several: %v", err)
		}
	}
}

// invite ls says a redeemed invite whose machine left (no live token) has
// left, not that it is in use.
func TestInviteLineLeft(t *testing.T) {
	inv := api.Invite{ID: "01ABC", Project: "pool", Area: "team", As: "ana", Role: "member", State: api.InviteRedeemed, Machine: "laptop", Used: true}
	if l := inviteLine(inv); !strings.Contains(l, "by laptop (in use)") {
		t.Fatalf("in use: %q", l)
	}
	for _, used := range []bool{false, true} { // left wins over used
		inv.Revoked, inv.Used = true, used
		if l := inviteLine(inv); !strings.Contains(l, "by laptop (left: no live token now)") || strings.Contains(l, "in use") {
			t.Fatalf("left (used %v): %q", used, l)
		}
	}
	live := api.Invite{ID: "01ABD", Project: "pool", As: "bo", Role: "guest", State: api.InviteLive, Class: "public"}
	if l := inviteLine(live); !strings.HasPrefix(l, "01ABD  live     bo         guest   pool                 until ") || !strings.HasSuffix(l, ", public\n") {
		t.Fatalf("live: %q", l)
	}
}

// remotes shows a remote's copy: its state, its age, the writes it holds,
// and a refused write's command only where this program has one.
func TestRemoteCopy(t *testing.T) {
	synced := time.Now()
	x := api.Remote{Replica: &api.ReplicaStatus{State: "stale", Synced: &synced, AgeSecs: 90, Pending: 2, Conflicts: 1, LastError: "dial tcp: i/o timeout"}}
	got := remoteCopy(x)
	if !strings.Contains(got, "copy stale, synced 1m30s ago, authority unreachable, outbox 2 pending, 1 refused: dial tcp: i/o timeout") {
		t.Fatalf("copy: %q", got)
	}
	if strings.Contains(got, "potluck outbox") {
		t.Fatalf("names a command potluck does not have: %q", got)
	}
	if got := remoteCopy(api.Remote{}); got != "" {
		t.Fatalf("no copy: %q", got)
	}
}
