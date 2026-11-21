package cli

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// disband removes one pool this machine runs and nothing else: the pool's
// members are cut off and told why, its invites stop working, and another
// pool on the same machine keeps its members and its sandboxes.
func TestDisbandRemovesOnePoolAndKeepsTheRest(t *testing.T) {
	tn := tailnettest.New()
	a := soloMachineOn(t, tn, "")
	b := startMachine(t, tn, "lender", "carl@example.com")
	ctx := context.Background()

	for _, pool := range []string{"friends", "keepers"} {
		a.ok(t, "create", pool)
		if out := a.ok(t, "host", pool, "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo-"+pool, "--reclaim-idle-s", "0", "--no-sudo"); !strings.Contains(out, "shared with pool "+pool) {
			t.Fatalf("host %s:\n%s", pool, out)
		}
		m := memberCodeRe.FindStringSubmatch(a.ok(t, "invite", "carl", "-P", pool))
		if m == nil {
			t.Fatalf("no member code for %s", pool)
		}
		b.ok(t, "join", m[1])
	}
	spare := memberCodeRe.FindStringSubmatch(a.ok(t, "invite", "dee", "-P", "friends"))
	if spare == nil {
		t.Fatal("no code for dee")
	}

	// The plan says, in words, what happens; --dry-run changes nothing.
	out := a.ok(t, "disband", "friends", "--dry-run")
	for _, want := range []string{"potluck disband friends will remove pool friends from this machine", "stop sharing this machine's sizes with the pool: solo-friends-c1m2",
		"cut off 1 machine, each told the pool was removed the next time it reaches this machine: carl (lender)", "cancel 1 invite nobody used yet",
		"keeps this machine's identity, its other pools, the pools it joined and its daemon"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan lacks %q:\n%s", want, out)
		}
	}
	a.ok(t, "invite", "eve", "-P", "friends") // still there

	// A sandbox in the pool stops the removal, and the answer names it.
	id, _, _ := strings.Cut(strings.TrimSpace(a.ok(t, "sandbox", "create", "-P", "friends", "--ttl", "5m")), " ")
	if o, e, code := a.run(t, "disband", "friends", "--yes"); code == 0 || !strings.Contains(e, "1 sandbox is still in the pool") || !strings.Contains(e, "nothing was removed") || strings.Contains(o, "removed pool") {
		t.Fatalf("disband with a sandbox running: exit %d\n%s\n%s", code, o, e)
	}
	a.ok(t, "sandbox", "rm", "-P", "friends", id)

	// It asks first: anything but yes removes nothing.
	if _, e, code := a.runIn(t, "no\n", "disband", "friends"); code == 0 || !strings.Contains(e, "nothing was removed: the answer was not yes") {
		t.Fatalf("disband answered no: exit %d\n%s", code, e)
	}
	a.ok(t, "invite", "eve", "-P", "friends")
	// The machine that runs a pool removes it; one that only joined leaves it.
	if _, e, code := b.run(t, "disband", "friends", "--yes"); code == 0 || !strings.Contains(e, "this machine joined pool friends; it does not run it") || !strings.Contains(e, "potluck leave friends") {
		t.Fatalf("disband a joined pool: exit %d\n%s", code, e)
	}
	if _, e, code := a.run(t, "disband", "nothere", "--yes"); code == 0 || !strings.Contains(e, `runs no pool named "nothere"`) || !strings.Contains(e, "it runs: friends, keepers") {
		t.Fatalf("disband a missing pool: exit %d\n%s", code, e)
	}

	out, e, code := a.runIn(t, "yes\n", "disband", "friends")
	if code != 0 || !strings.Contains(out, "removed pool friends from this machine") || !strings.Contains(out, "cut off: lender") {
		t.Fatalf("disband: exit %d\n%s\n%s", code, out, e)
	}

	// The pool is gone from A: no invite, no sandbox, and its code is dead.
	if _, e, code := a.run(t, "invite", "eve", "-P", "friends"); code == 0 || !strings.Contains(e, "friends") {
		t.Fatalf("invite into a removed pool: exit %d\n%s", code, e)
	}
	if _, e, code := a.run(t, "disband", "friends", "--yes"); code == 0 || !strings.Contains(e, `runs no pool named "friends"`) || !strings.Contains(e, "it runs: keepers") {
		t.Fatalf("disband again: exit %d\n%s", code, e)
	}
	c := startMachine(t, tn, "friend", "dee@example.com")
	if _, e, code := c.run(t, "join", spare[1]); code == 0 {
		t.Fatalf("an invite of a removed pool still joined: %s", e)
	}

	// B is told when it next reaches A: friends is over, with the reason;
	// keepers goes on, and B can still make a sandbox in it on A.
	module[*teamlink.Service](t, b).Heartbeat(ctx)
	var rs []api.Remote
	if err := json.Unmarshal([]byte(b.ok(t, "--json", "remotes")), &rs); err != nil || len(rs) != 2 {
		t.Fatalf("remotes at B: %v\n%v", err, rs)
	}
	for _, r := range rs {
		switch r.Project.Slug {
		case "friends":
			if r.State != api.RemotePurged || r.LastError != "the pool's owner removed pool friends" {
				t.Errorf("friends at B: state %q, error %q", r.State, r.LastError)
			}
		case "keepers":
			if r.State != api.RemoteOK {
				t.Errorf("keepers at B: state %q, error %q", r.State, r.LastError)
			}
		}
	}
	if _, e, code := b.run(t, "sandbox", "ls", "-P", "friends"); code == 0 || !strings.Contains(e, "the pool's owner removed pool friends") {
		t.Fatalf("B uses the removed pool: exit %d\n%s", code, e)
	}
	b.ok(t, "leave", "friends")
	kid, _, _ := strings.Cut(strings.TrimSpace(b.ok(t, "sandbox", "create", "-P", "keepers", "--ttl", "5m")), " ")
	b.ok(t, "sandbox", "rm", "-P", "keepers", kid)

	// The name is free again: a new pool of that name starts empty.
	if o := a.ok(t, "create", "friends"); !strings.Contains(o, "pool friends is ready") {
		t.Fatalf("create again:\n%s", o)
	}
	if o := a.ok(t, "status"); !strings.Contains(o, "keepers") {
		t.Fatalf("status at A:\n%s", o)
	}

	// The member that was cut off is invited again and joins the new pool:
	// nothing of the old pool (its token, its grants) stands in the way.
	again := memberCodeRe.FindStringSubmatch(a.ok(t, "invite", "carl", "-P", "friends"))
	if again == nil {
		t.Fatal("no member code for carl in the new pool")
	}
	b.ok(t, "join", again[1])
	module[*teamlink.Service](t, b).Heartbeat(ctx)
	if o := b.ok(t, "sandbox", "ls", "-P", "friends"); strings.Contains(o, "pool removed") {
		t.Fatalf("carl in the new pool:\n%s", o)
	}
}

// A running sandbox stops disband unless --force; the pool GitHub Actions
// runs on is refused either way, saying how to end that first.
func TestDisbandForceAndTheRunnerPool(t *testing.T) {
	m := onePoolMachine(t, refPin) // pool "trial", one size, sandboxes on this machine
	id, _, _ := strings.Cut(strings.TrimSpace(m.ok(t, "sandbox", "create", "--ttl", "5m")), " ")

	// A second pool, with a sandbox of its own: --force removes it anyway.
	m.ok(t, "create", "--via", "tunnel", "scratch")
	m.ok(t, "host", "scratch", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "scratch", "--reclaim-idle-s", "0", "--no-sudo")
	m.ok(t, "sandbox", "create", "-P", "scratch", "--ttl", "5m")
	if _, e, code := m.run(t, "disband", "scratch", "--yes"); code == 0 || !strings.Contains(e, "1 sandbox is still in the pool") {
		t.Fatalf("disband without --force: exit %d\n%s", code, e)
	}
	out, e, code := m.run(t, "disband", "scratch", "--force", "--yes")
	if code != 0 || !strings.Contains(out, "with --force: 1 sandbox is still in the pool; they go on until their lease ends") || !strings.Contains(out, "removed pool scratch from this machine") {
		t.Fatalf("disband --force: exit %d\n%s\n%s", code, out, e)
	}
	if o := m.ok(t, "remotes"); strings.Contains(o, "scratch") {
		t.Fatalf("remotes:\n%s", o)
	}
	if _, e, code := m.run(t, "invite", "x", "-P", "scratch"); code == 0 {
		t.Fatalf("scratch is still a pool: %s", e)
	}

	m.ok(t, "runners", "setup", "trial", "--url", "https://pool.example")
	if _, e, code := m.run(t, "disband", "trial", "--force", "--yes"); code == 0 || !strings.Contains(e, "pool trial is the pool GitHub Actions jobs run on") || !strings.Contains(e, "runners block out of config.json") {
		t.Fatalf("disband the runner pool: exit %d\n%s", code, e)
	}
	if o := m.ok(t, "sandbox", "exec", id, "--", "echo", "still"); o != "still\n" {
		t.Fatalf("a refused disband changed the sandbox: %q", o)
	}
}

// poolOnMachine is the pool slug m runs, as the daemon knows it.
func poolOnMachine(t *testing.T, m *machine, slug string) (*client.Client, api.Project) {
	t.Helper()
	c := client.NewUnix(m.dm.Deps.Config.SocketPath())
	ps, err := c.LocalProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(p api.Project) bool { return p.Slug == slug })
	if i < 0 {
		t.Fatalf("no pool %s on %s", slug, m.name)
	}
	return c, ps[i]
}

// The daemon, not only the command, refuses to remove a pool while sandboxes
// are in it, the stopped one with its disk kept included; a call that
// forces it removes the pool anyway. The pool's sizes are gone with it, and
// a size shared with it from outside (machine-level) leaves its list: one
// shared with another pool too stays for that pool, one shared with it alone
// goes.
func TestRemoveProjectRefusedWhileSandboxesStayAndSharesEnd(t *testing.T) {
	m := onePoolMachine(t, refPin) // pool "trial"
	m.ok(t, "create", "--via", "tunnel", "scratch")
	m.ok(t, "host", "scratch", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "scratch", "--reclaim-idle-s", "0", "--no-sudo")
	ctx := context.Background()
	c, scratch := poolOnMachine(t, m, "scratch")
	_, trial := poolOnMachine(t, m, "trial")

	l, err := computeclient.RunTargets(ctx, inPool(c, "trial"), false)
	if err != nil || len(targetsOf(l)) == 0 {
		t.Fatalf("trial's sizes: %v %v", l, err)
	}
	for _, b := range []struct {
		name  string
		share []string
	}{{"both", []string{scratch.ID, trial.ID}}, {"alone", []string{scratch.ID}}} {
		target := targetsOf(l)[0]
		target.Name, target.Host, target.HostProject = b.name, "", ""
		if _, err := computeclient.BindRunTarget(ctx, c, computeapi.BindRunTargetRequest{Host: true, Share: b.share, Target: target}); err != nil {
			t.Fatalf("bind %s: %v", b.name, err)
		}
	}
	if sl, err := computeclient.RunTargets(ctx, inPool(c, "scratch"), false); err != nil || len(targetsOf(sl)) != 1 {
		t.Fatalf("scratch's own size before: %v %v", sl, err)
	}

	// A sandbox started after any plan: the DELETE itself refuses.
	id, _, _ := strings.Cut(strings.TrimSpace(m.ok(t, "sandbox", "create", "-P", "scratch", "--ttl", "5m", "--persist")), " ")
	if _, err := c.RemoveProject(ctx, scratch.ID, false); err == nil || !strings.Contains(err.Error(), "1 sandbox is still in pool scratch") {
		t.Fatalf("remove with a sandbox running: %v", err)
	}
	// Stopped with its disk kept is still a sandbox in the pool.
	m.ok(t, "sandbox", "stop", "-P", "scratch", id)
	if _, err := c.RemoveProject(ctx, scratch.ID, false); err == nil || !strings.Contains(err.Error(), "1 stopped, with its disk kept") {
		t.Fatalf("remove with a stopped sandbox: %v", err)
	}
	if _, p := poolOnMachine(t, m, "scratch"); p.ID != scratch.ID {
		t.Fatal("a refused removal changed the pool")
	}
	if hl, err := computeclient.HostRunTargets(ctx, c); err != nil || len(hl.Targets) != 2 {
		t.Fatalf("a refused removal changed the shares: %v %v", hl.Targets, err)
	}

	// Forced: the pool goes with its sandbox left to its lease, and the shares go.
	r, err := c.RemoveProject(ctx, scratch.ID, true)
	if err != nil || len(r.Leftovers) != 0 {
		t.Fatalf("forced removal: %+v %v", r, err)
	}
	hl, err := computeclient.HostRunTargets(ctx, c)
	if err != nil || len(hl.Targets) != 1 || hl.Targets[0].Name != "both" || !slices.Equal(hl.Targets[0].Shared, []string{trial.ID}) {
		t.Fatalf("shares after the removal: %+v %v", hl.Targets, err)
	}
	if _, err := computeclient.RunTargets(ctx, inPool(c, "scratch"), false); err == nil {
		t.Fatal("the removed pool still lists its sizes")
	}
	if o := m.ok(t, "host", "stop", "--force"); strings.Contains(o, "scratch") || !strings.Contains(o, "solo-c1m2") {
		t.Fatalf("host stop after the removal lists a size of the removed pool:\n%s", o)
	}
}

// A GitHub account linked to the pool for Actions jobs refuses the removal
// (said how to end it) and removes nothing; what the owners module keeps for
// a pool that does go is deleted, except the names bound to it, which a
// rule keeps bound.
func TestDisbandLinkedGitHubAndOwnersRows(t *testing.T) {
	m := onePoolMachine(t, refPin) // pool "trial"
	m.ok(t, "create", "--via", "tunnel", "scratch")
	_, scratch := poolOnMachine(t, m, "scratch")
	db := m.dm.Deps.Store.Registry()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO runners_links (installation,account,account_type,project,area,linked_by,token_id,created) VALUES (7,'acme','Organization',?,'','judy','test',1)`, scratch.ID)
	if _, e, code := m.run(t, "disband", "scratch", "--yes"); code == 0 || !strings.Contains(e, "GitHub accounts are linked to this pool for Actions jobs: acme (installation 7)") || !strings.Contains(e, "runners unlink <installation>") {
		t.Fatalf("disband with a linked account: exit %d\n%s", code, e)
	}
	if _, p := poolOnMachine(t, m, "scratch"); p.ID != scratch.ID {
		t.Fatal("a refused disband changed the pool")
	}
	exec(`DELETE FROM runners_links WHERE installation = 7`)

	exec(`INSERT INTO github_grants (account, login, kind, project, area, role, created) VALUES ('1', 'carl', 'user', ?, '', 'member', 1)`, scratch.ID)
	exec(`INSERT INTO github_grant_queue (project, area, login, role, identity_id, identity_name, address, machine_owner, token_id, created, retry_at) VALUES (?, '', 'dee', 'member', 'i', 'judy', '', 0, 't', 1, 2)`, scratch.ID)
	exec(`INSERT INTO github_admissions (id, grant_account, grant_kind, project, account, identity, node, token, source, created, checked) VALUES ('a1', '1', 'user', ?, '1', 'i', 'n', 't', 's', 1, 1)`, scratch.ID)
	exec(`INSERT INTO github_org_checks (project, failing_since, reason) VALUES (?, 1, 'x')`, scratch.ID)
	exec(`INSERT INTO owner_names (owner, name, project, bound, current) VALUES ('1', 'scratch', ?, 1, 1)`, scratch.ID)

	m.ok(t, "disband", "scratch", "--yes")
	for _, table := range []string{"github_grants", "github_grant_queue", "github_admissions", "github_org_checks"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE project = ?`, scratch.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s still holds %d rows of the removed pool (%v)", table, n, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM owner_names WHERE project = ?`, scratch.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("a bound name must stay bound to the removed pool: %d rows (%v)", n, err)
	}
}

// The pool is gone once its registry rows are; a folder that cannot be
// removed afterwards is reported as left behind, not as a failed removal
// (the retry would only say this machine runs no such pool).
func TestDisbandReportsWhatIsLeft(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes any folder")
	}
	m := onePoolMachine(t, refPin)
	m.ok(t, "create", "--via", "tunnel", "scratch")
	_, scratch := poolOnMachine(t, m, "scratch")
	dir := m.dm.Deps.Store.ProjectDir(scratch.ID)
	if err := os.Chmod(dir, 0o500); err != nil { // its files cannot be unlinked
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	out, e, code := m.run(t, "disband", "scratch", "--yes")
	if code == 0 || !strings.Contains(out, "removed pool scratch from this machine") ||
		!strings.Contains(e, "pool scratch was removed, but not everything it held") || !strings.Contains(e, "the project's folder "+dir+" could not be removed") {
		t.Fatalf("disband with a folder that stays: exit %d\n%s\n%s", code, out, e)
	}
	if _, e, code := m.run(t, "disband", "scratch", "--yes"); code == 0 || !strings.Contains(e, `runs no pool named "scratch"`) {
		t.Fatalf("disband again: exit %d\n%s", code, e)
	}
}
