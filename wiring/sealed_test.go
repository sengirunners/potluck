package wiring_test

// Secrets on every leg (zm-eakh.12.48.2, SPEC 17.11 "Sealed secrets"): a
// member's exec, spawn and box secrets (zm-eakh.12.48.3) with values go
// member -> pool's machine -> host,
// each leg sealed to the receiving daemon's key, over the tailnet and over
// the tunnel alike. A tap on every connection (the tailnet's, and the
// stand-in for Cloudflare) sees no secret value or name, no daemon logs one,
// and none is written under either home; the box gets the exact value. A
// host or a pool's machine too old to seal is refused, plainly, and the
// secret never leaves. This is the daemon side's check of the refusal
// (the driver suite in conformance/ checks a driver's own handling of
// secrets; the daemon decides what reaches the driver).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
	"github.com/sengirunners/potluck/runtarget"

	_ "modernc.org/sqlite"
)

// syncBuf is a buffer safe for the daemons' and taps' concurrent writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) has(x string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Contains(s.b.Bytes(), []byte(x))
}

func (s *syncBuf) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

// homeHas reports the first file under home holding x.
func homeHas(t *testing.T, home, x string) string { return homeHasBut(t, home, x, nil) }

// homeHasBut is homeHas skipping the files skip names.
func homeHasBut(t *testing.T, home, x string, skip func(path string) bool) string {
	t.Helper()
	found := ""
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" || (skip != nil && skip(p)) {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(x)) {
			found = p
		}
		return nil
	})
	return found
}

// nameOutsideLedger reads every project database under home and answers
// the first table, other than the grants table (secret_grants) and the
// event log (events, the secrets ledger), holding x; "" when none does.
func nameOutsideLedger(t *testing.T, home, x string) string {
	t.Helper()
	dbs, _ := filepath.Glob(filepath.Join(home, "projects", "*", "project.db"))
	for _, p := range dbs {
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, n)
		}
		rows.Close()
		for _, tbl := range tables {
			if tbl == "secret_grants" || tbl == "events" {
				continue
			}
			r, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(tbl, `"`, `""`) + `"`)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := r.Columns()
			for r.Next() {
				vals := make([]sql.RawBytes, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := r.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				for _, v := range vals {
					if bytes.Contains(v, []byte(x)) {
						r.Close()
						return p + " table " + tbl
					}
				}
			}
			r.Close()
		}
	}
	return ""
}

// inProject is c naming project (as -P does).
func inProject(c *client.Client, project string) *client.Client {
	x := *c
	x.Project, x.ProjectFrom = project, "flag"
	return &x
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestSecretsSealedOnEveryLeg(t *testing.T) {
	for _, via := range []string{api.ViaTailnet, api.ViaTunnel} {
		t.Run(via, func(t *testing.T) { sealedLegs(t, via) })
	}
}

func sealedLegs(t *testing.T, via string) {
	old := teamlink.BeatEvery
	teamlink.BeatEvery = time.Hour // the test beats by hand
	t.Cleanup(func() { teamlink.BeatEvery = old })
	ctx := context.Background()
	wire, logs := &syncBuf{}, &syncBuf{}
	tn := tailnettest.New()
	tn.Tap(wire)
	var tun *config.Tunnel
	if via == api.ViaTunnel {
		// The authority's tunnel listener behind a tapped stand-in for
		// Cloudflare: every byte between the machines crosses it.
		listen := freeLoopback(t)
		p, err := tunneltest.New(listen)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		p.Tap(wire)
		tun = &config.Tunnel{Listen: listen, URL: p.URL()}
	}
	a := startComputeWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, logs, tun)
	b := startComputeWith(t, tn, "box-host", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box-host", Login: "carl@example.com", Host: "box-host"}, logs, nil)
	m := startComputeWith(t, tn, "member-box", tailnettest.Identity{NodeID: "nMEM", NodeKey: "nodekey:member-box", Login: "dave@example.com", Host: "member-box"}, logs, nil)

	if _, err := a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if mode, err := a.owner.ProjectTeam(ctx, "orchid"); err != nil || !mode.Team {
		t.Fatalf("team: %+v %v", mode, err)
	}
	inv, err := a.owner.Invite(ctx, api.InviteRequest{Project: "orchid", As: "carl", Role: api.RoleMember, Via: via})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := b.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil || j.Remote == nil || j.Remote.Via != via {
		t.Fatalf("join over %s: %+v %v", via, j, err)
	}
	inv2, err := a.owner.Invite(ctx, api.InviteRequest{Project: "orchid", As: "dave", Role: api.RoleMember, Via: via})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := m.owner.Join(ctx, api.JoinRequest{Code: inv2.Code}); err != nil || j.Remote == nil || j.Remote.Via != via {
		t.Fatalf("member joins over %s: %+v %v", via, j, err)
	}

	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	module[*runtarget.Service](t, b).SetLookPath(func(string) (string, error) { return drv, nil })
	if _, err := computeclient.BindRunTarget(ctx, b.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"orchid"}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"jit": "1"}}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	link := module[*teamlink.Service](t, b)
	// dave, the member, on his own machine (not the host): every call goes
	// C -> A, and A relays each driver verb to B, the host. An op sealed to
	// anyone's key but B's would not open there.
	dave := client.WithHandler(m.dm.SocketHandler())
	dave.Project, dave.ProjectFrom = "orchid", "flag"
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("B's box was never listed at A")
		}
		link.Heartbeat(ctx)
		l, err := computeclient.RunTargets(ctx, dave, true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(l.Targets) > 0 && l.Targets[0].Name == "sandbox@box-host" && l.Targets[0].HostClass == api.MachineTrusted {
			break
		}
	}
	stop := beat(link)
	defer stop()

	h, err := computeclient.AcquireRunTarget(ctx, dave, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "seal-1", TTLS: 120})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]

	const canary, name = "c4n4ry-v4lue-9b1e-hunter2", "CANARY_SEAL_NAME"
	const jit = "SklUY2FuYXJ5LXZhbHVlLTM3c2VhbA"
	const boxCanary, boxName = "b0x-c4n4ry-v4lue-71d2-hunter3", "BOX_CANARY_SEAL_NAME"
	sum := sha256.Sum256([]byte(canary))
	want := hex.EncodeToString(sum[:])
	// The box answers the value's hash, never the value or its name.
	hash := `f="$POTLUCK_SECRETS/` + name + `"; (sha256sum < "$f" 2>/dev/null || shasum -a 256 < "$f") | cut -d' ' -f1`
	out, err := computeclient.ExecRunTarget(ctx, dave, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{
		Argv: []string{"sh", "-c", hash}, Secrets: map[string]string{name: canary}})
	if err != nil || out.Exit != 0 || strings.TrimSpace(out.Stdout) != want {
		t.Fatalf("exec with a secret: %+v %v (want %s)", out, err, want)
	}
	sp, err := computeclient.SpawnRunTarget(ctx, dave, "sandbox@box-host", id, computeapi.SpawnRunTargetRequest{RequestID: "seal-s1",
		Argv: []string{"sh", "-c", hash}, Secrets: map[string]string{name: canary}})
	if err != nil || sp.PID == "" {
		t.Fatalf("spawn with a secret: %+v %v", sp, err)
	}
	var o computeapi.RunTargetOutput
	for range 20 {
		if o, err = computeclient.RunTargetOutput(ctx, dave, "sandbox@box-host", id, computeapi.OutputRunTargetRequest{PID: sp.PID, WaitS: 2}); err != nil || !o.Running {
			break
		}
	}
	if err != nil || o.Running || strings.TrimSpace(o.Stdout) != want {
		t.Fatalf("spawn's output: %+v %v", o, err)
	}
	// Box secrets (the secrets verb), under a grant at the pool's machine
	// (no default covers box scope). dave, made an owner of the pool, adds
	// it himself from his machine over the tunnel: the grant's body names a
	// secret, so it crosses sealed, and the answers withhold the name. Over
	// the tailnet answers show names (as everything there reads them), so
	// there the pool's owner edits on its own machine and the tap stays
	// strict.
	editor := inProject(a.owner, "orchid")
	if via == api.ViaTunnel {
		if _, err := a.owner.Grant(ctx, api.GrantRequest{Identity: "dave", Role: api.RoleOwner, Project: "orchid"}); err != nil {
			t.Fatal(err)
		}
		editor = dave
	}
	var bg computeapi.SecretGrant
	for try := 0; ; try++ { // the grant moved dave's scope version: his machine refreshes once
		bg, err = computeclient.AddSecretGrant(ctx, editor, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Identity: "dave"},
			Kind: computeapi.SecretKindValue, Name: boxName, Scopes: []string{computeapi.SecretScopeBox}})
		if !api.IsCode(err, api.CodeRescope) || try == 5 {
			break
		}
		module[*teamlink.Service](t, m).Heartbeat(ctx)
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || bg.ID == "" {
		t.Fatalf("dave adds a box-scope grant from his machine: %+v %v", bg, err)
	}
	if via == api.ViaTunnel && bg.Name != "" {
		t.Errorf("the add's answer echoes the name over the tunnel: %+v", bg)
	}
	if via == api.ViaTunnel { // over the tailnet a list shows names, as everything there reads them
		gl, err := computeclient.SecretGrants(ctx, dave)
		if err != nil || len(gl.Grants) != 3 || gl.Grants[2].Name != "" || gl.Note == "" {
			t.Fatalf("dave lists the grants over the tunnel: %+v %v", gl, err)
		}
	}
	var bs map[string]any
	if err := dave.Do(ctx, http.MethodPost, "/v0/run_targets/sandbox@box-host/handles/"+id+"/secrets",
		map[string]any{"secrets": map[string]string{boxName: boxCanary}}, &bs); err != nil || bs["count"] != 1.0 {
		t.Fatalf("box secrets: %v %v", bs, err)
	}
	// The ledger, replayed by dave through his machine over the tunnel:
	// counts, never names.
	if via == api.ViaTunnel {
		var led []api.Event
		if err := dave.Events(ctx, api.EventQuery{Types: "dev.zm.secret", Project: "orchid", Since: "0", NoLive: true},
			func(e api.Event) error { led = append(led, e); return nil }); err != nil {
			t.Fatalf("dave reads the ledger: %v", err)
		}
		if len(led) == 0 {
			t.Fatal("dave read no ledger entry")
		}
		for _, e := range led {
			var d map[string]json.RawMessage
			if json.Unmarshal(e.Data, &d) != nil || d["names"] != nil || d["sizes"] != nil || d["grants"] != nil || string(d["count"]) == "" || string(d["count"]) == "0" {
				t.Errorf("a ledger entry over the tunnel: %s", e.Data)
			}
		}
	}
	boxSum := sha256.Sum256([]byte(boxCanary))
	// A later exec with no secrets of its own reads the box's: by glob, so
	// its clear argv names neither the value nor the name.
	readBox := `f=$(ls "$POTLUCK_SECRETS"/* | head -n 1); (sha256sum < "$f" 2>/dev/null || shasum -a 256 < "$f") | cut -d' ' -f1`
	if out, err := computeclient.ExecRunTarget(ctx, dave, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", readBox}}); err != nil ||
		strings.TrimSpace(out.Stdout) != hex.EncodeToString(boxSum[:]) {
		t.Fatalf("a later exec reads the box secret: %+v %v", out, err)
	}
	// An unset-only call names a secret too: it crosses sealed.
	if err := dave.Do(ctx, http.MethodPost, "/v0/run_targets/sandbox@box-host/handles/"+id+"/secrets",
		map[string]any{"unset": []string{boxName}}, nil); err != nil {
		t.Fatalf("an unset-only call: %v", err)
	}
	if wire.has(boxName) {
		t.Fatal("an unset-only call's name crossed the wire in the clear")
	}
	// The tap reads what is not sealed: a secret-free call's argv crosses
	// as it is (so a secret that crossed in the clear would be seen).
	if _, err := computeclient.ExecRunTarget(ctx, dave, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "visible-marker-3e7"}}); err != nil {
		t.Fatalf("secret-free exec: %v", err)
	}
	if !wire.has("visible-marker-3e7") {
		t.Fatal("the tap did not see a clear argv: it proves nothing")
	}
	check := func(when string) {
		t.Helper()
		for _, x := range []string{canary, name, jit, boxCanary, boxName} {
			if wire.has(x) {
				t.Fatalf("%s: %q crossed the wire in the clear", when, x)
			}
			if logs.has(x) {
				t.Fatalf("%s: %q is in a daemon's log", when, x)
			}
			for _, d := range []*computeDaemon{a, b, m} {
				var ledger func(string) bool
				if d == a && (x == name || x == boxName) {
					// The secrets ledger and the grants table, in the pool's
					// project database at its machine, keep names (never
					// values) by design.
					ledger = func(p string) bool {
						return strings.HasPrefix(p, filepath.Join(a.home, "projects")) && strings.HasPrefix(filepath.Base(p), "project.db")
					}
				}
				if f := homeHasBut(t, d.home, x, ledger); f != "" {
					t.Fatalf("%s: %q is written in %s", when, x, f)
				}
				if ledger != nil {
					if where := nameOutsideLedger(t, a.home, x); where != "" {
						t.Fatalf("%s: %q is in %s", when, x, where)
					}
				}
			}
		}
	}
	// A runner config through every leg: the member's acquire to A, A's op
	// to B (the reference driver takes it and starts no runner).
	hj, err := computeclient.AcquireRunTarget(ctx, dave, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "seal-jit", TTLS: 120, JITConfig: jit})
	if err != nil {
		t.Fatalf("acquire with a jit_config: %v", err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, dave, "sandbox@box-host", hj.Handle[strings.LastIndex(hj.Handle, "/")+1:]); err != nil {
		t.Fatalf("release the runner box: %v", err)
	}
	if wire.has(jit) || logs.has(jit) {
		t.Fatal("the jit_config crossed the wire or reached a log in the clear")
	}
	if rm, err := computeclient.RemoveSecretGrant(ctx, editor, bg.ID); err != nil || (via == api.ViaTunnel && rm.Name != "") {
		t.Fatalf("dave removes the grant: %+v %v", rm, err)
	}
	check("sealed legs")

	// The pool's machine too old to seal (sealing off stands in for it).
	// Over the tunnel the member's machine refuses before the secret leaves,
	// in every mode. (Over the tailnet a member sends to such a machine as
	// before, which has no mode to require more: TestSealingModes.) The
	// answer that stops naming a key may come first, so a try or two may
	// fail to open.
	if via == api.ViaTunnel {
		undo := module[*tunnel.Service](t, a).SealOff()
		refusedUntil(t, "an older pool's machine", "pool's machine", func() error {
			_, err := computeclient.ExecRunTarget(ctx, dave, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{
				Argv: []string{"true"}, Secrets: map[string]string{name: canary}})
			return err
		}, true)
		undo()
		check("older pool's machine")
	} else {
		// Over the tailnet an older host is refused only when the pool
		// requires sealing (the default, preferred, sends to it as before).
		if _, err := computeclient.SetSealing(ctx, inProject(a.owner, "orchid"), computeapi.SetSealingRequest{Mode: computeapi.SealingRequired}); err != nil {
			t.Fatalf("set required: %v", err)
		}
	}

	// A host too old to seal: the pool's machine sends it nothing. Its
	// owner's own call (at A) leaves A only sealed, or not at all.
	tok, err := a.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "orchid", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	ana := client.WithHandler(a.dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "orchid", "flag"
	ha, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "seal-2", TTLS: 120})
	if err != nil {
		t.Fatalf("acquire at the pool's machine: %v", err)
	}
	ida := ha.Handle[strings.LastIndex(ha.Handle, "/")+1:]
	undo := module[*tunnel.Service](t, b).SealOff()
	refusedUntil(t, "an older host", "the host runs", func() error {
		_, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", ida, computeapi.ExecRunTargetRequest{
			Argv: []string{"true"}, Secrets: map[string]string{name: canary}})
		return err
	}, false)
	// Secret-free calls still work with it.
	if out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", ida, computeapi.ExecRunTargetRequest{Argv: []string{"true"}}); err != nil || out.Exit != 0 {
		t.Fatalf("secret-free exec on an older host: %+v %v", out, err)
	}
	undo()
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", ida); err != nil {
		t.Fatalf("release ana's box: %v", err)
	}
	check("older host")

	if rel, err := computeclient.ReleaseRunTarget(ctx, dave, "sandbox@box-host", id); err != nil || !rel.Released {
		t.Fatalf("release: %+v %v", rel, err)
	}
	check("released")
}

// refusedUntil calls call until it is refused naming who as too old (403),
// within 15 s. Before that it may fail to open (400: a key from an answer
// before the change) and nothing else. tunnelHome: an older home over the
// tunnel answers its own refusal naming the home.
func refusedUntil(t *testing.T, what, who string, call func() error, tunnelHome bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err := call()
		var ae *api.Error
		if err == nil {
			t.Fatalf("%s: a call with secrets went through", what)
		}
		if errors.As(err, &ae) && ae.Code == api.CodeForbidden && strings.Contains(ae.Message, "too old") &&
			(strings.Contains(ae.Message, who) || tunnelHome && strings.Contains(ae.Message, "the home")) {
			return
		}
		if !strings.Contains(err.Error(), "could not be opened") || time.Now().After(deadline) {
			t.Fatalf("%s: want a plain too-old refusal naming %q, got %v", what, who, err)
		}
	}
}

// The pool's sealing mode over the tailnet, with machines from before
// sealing in the pool (sealing off stands in for one): A the pool's
// machine, B a new host and member (carl), C an old host and member (dave).
// preferred (the default) sends to and takes from C as before, in the
// clear, warning once; required refuses both ways; the pre-check names C;
// a change takes effect at once.
func TestSealingModes(t *testing.T) {
	old := teamlink.BeatEvery
	teamlink.BeatEvery = time.Hour
	t.Cleanup(func() { teamlink.BeatEvery = old })
	ctx := context.Background()
	wire, logsA := &syncBuf{}, &syncBuf{}
	tn := tailnettest.New()
	tn.Tap(wire)
	a := startComputeWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, logsA, nil)
	logsB := &syncBuf{}
	b := startComputeWith(t, tn, "box-b", tailnettest.Identity{NodeID: "nBOXB", NodeKey: "nodekey:box-b", Login: "carl@example.com", Host: "box-b"}, logsB, nil)
	c := startCompute(t, tn, "box-c", tailnettest.Identity{NodeID: "nBOXC", NodeKey: "nodekey:box-c", Login: "dave@example.com", Host: "box-c"})
	module[*tunnel.Service](t, c).SealOff() // C runs a build from before sealing

	if _, err := a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.owner.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	for _, x := range []struct {
		d  *computeDaemon
		as string
	}{{b, "carl"}, {c, "dave"}} {
		inv, err := a.owner.Invite(ctx, api.InviteRequest{Project: "orchid", As: x.as, Role: api.RoleMember})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := x.d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
			t.Fatalf("%s joins: %v", x.as, err)
		}
		state := filepath.Join(fix, x.as)
		bin := filepath.Join(fix, x.as+"-bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		drv, err := refdriver.Install(bin, state, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { refdriver.KillAll(state) })
		module[*runtarget.Service](t, x.d).SetLookPath(func(string) (string, error) { return drv, nil })
		if _, err := computeclient.BindRunTarget(ctx, x.d.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"orchid"}, Target: computeapi.RunTarget{
			Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
			t.Fatalf("bind: %v", err)
		}
	}
	carl, dave := inProject(client.WithHandler(b.dm.SocketHandler()), "orchid"), inProject(client.WithHandler(c.dm.SocketHandler()), "orchid")
	lb, lc := module[*teamlink.Service](t, b), module[*teamlink.Service](t, c)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hosts were never listed at A")
		}
		lb.Heartbeat(ctx)
		lc.Heartbeat(ctx)
		if l, err := computeclient.RunTargets(ctx, carl, false); err == nil && len(l.Targets) == 2 {
			break
		}
	}
	defer beat(lb)()
	defer beat(lc)()
	admin := inProject(a.owner, "orchid")

	// The pre-check: C cannot seal.
	v, err := computeclient.Sealing(ctx, admin)
	if err != nil || v.Mode != computeapi.SealingPreferred || len(v.Peers) != 2 || strings.Join(v.CannotSeal, ",") != "box-c" {
		t.Fatalf("sealing at the pool's machine: %+v %v", v, err)
	}
	if v, err := computeclient.Sealing(ctx, carl); err != nil || v.Mode != computeapi.SealingPreferred {
		t.Fatalf("sealing from a member's machine: %+v %v", v, err)
	}

	acquire := func(who *client.Client, target, rid string) string {
		t.Helper()
		h, err := computeclient.AcquireRunTarget(ctx, who, target, computeapi.AcquireRunTargetRequest{RequestID: rid, TTLS: 300})
		if err != nil {
			t.Fatalf("acquire %s: %v", target, err)
		}
		return h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	}
	onB, onC, daveOnB := acquire(carl, "sandbox@box-b", "m-1"), acquire(carl, "sandbox@box-c", "m-2"), acquire(dave, "sandbox@box-b", "m-3")
	n := 0
	// run execs with a fresh canary and answers it and the refusal, if any.
	run := func(who *client.Client, target, id string) (string, error) {
		t.Helper()
		n++
		canary := "c4n4ry-m0de-" + strings.Repeat("x", n) + "-v"
		sum := sha256.Sum256([]byte(canary))
		out, err := computeclient.ExecRunTarget(ctx, who, target, id, computeapi.ExecRunTargetRequest{
			Argv: []string{"sh", "-c", `f="$POTLUCK_SECRETS/MODE_SECRET"; (sha256sum < "$f" 2>/dev/null || shasum -a 256 < "$f") | cut -d' ' -f1`}, Secrets: map[string]string{"MODE_SECRET": canary}})
		if err == nil && strings.TrimSpace(out.Stdout) != hex.EncodeToString(sum[:]) {
			t.Fatalf("exec on %s: the box did not get the secret: %+v", target, out)
		}
		return canary, err
	}
	refused := func(err error, want string) {
		t.Helper()
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeForbidden || !strings.Contains(ae.Message, want) {
			t.Fatalf("want a 403 saying %q, got %v", want, err)
		}
	}

	// preferred.
	if s, err := run(carl, "sandbox@box-b", onB); err != nil || wire.has(s) {
		t.Fatalf("new member -> new host: %v (clear on the wire: %v)", err, wire.has(s))
	}
	for range 2 {
		if s, err := run(carl, "sandbox@box-c", onC); err != nil || !wire.has(s) {
			// Clear on the tailnet to the old host, as before this change.
			t.Fatalf("new authority -> old host: %v (clear on the wire: %v)", err, wire.has(s))
		}
	}
	if s, err := run(dave, "sandbox@box-b", daveOnB); err != nil || !wire.has(s) {
		t.Fatalf("old member -> new authority -> new host: %v (clear on the wire: %v)", err, wire.has(s))
	}
	if w := strings.Count(logOf(logsA), "machine=box-c"); w != 1 || !strings.Contains(logOf(logsA), "update it") {
		t.Fatalf("want one Warn naming box-c: %s", logOf(logsA))
	}

	// Only an admin of the whole pool sets the mode; an area's admin may
	// not, and reads the mode without the machines.
	if _, err := a.owner.AddArea(ctx, "orchid", api.AddAreaRequest{Name: "team"}); err != nil {
		t.Fatalf("area: %v", err)
	}
	ztok, err := a.owner.MintToken(ctx, api.MintTokenRequest{As: "zed", Project: "orchid", Area: "team", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	zed := client.WithHandler(a.dm.Handler)
	zed.Token, zed.Project, zed.ProjectFrom = ztok.Secret, "orchid/team", "flag"
	if _, err := computeclient.SetSealing(ctx, zed, computeapi.SetSealingRequest{Mode: computeapi.SealingRequired, Force: true}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an area admin set the pool's mode: %v", err)
	}
	if v, err := computeclient.Sealing(ctx, zed); err != nil || v.Mode != computeapi.SealingPreferred || len(v.Peers) != 0 || len(v.CannotSeal) != 0 {
		t.Fatalf("an area reader's view: %+v %v", v, err)
	}

	// required: refused while C cannot seal, unless forced; then at once.
	if _, err := computeclient.SetSealing(ctx, admin, computeapi.SetSealingRequest{Mode: computeapi.SealingRequired}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "box-c") {
		t.Fatalf("required with box-c unable to seal: %v", err)
	}
	if _, err := computeclient.SetSealing(ctx, carl, computeapi.SetSealingRequest{Mode: computeapi.SealingRequired, Force: true}); !api.IsCode(err, api.CodeForbidden) && !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a member set the mode: %v", err)
	}
	if v, err := computeclient.SetSealing(ctx, admin, computeapi.SetSealingRequest{Mode: computeapi.SealingRequired, Force: true}); err != nil || v.Mode != computeapi.SealingRequired {
		t.Fatalf("required, forced: %+v %v", v, err)
	}
	var changed []string
	_ = a.owner.Events(ctx, api.EventQuery{Types: computeapi.EventSealingChanged, Project: "orchid", Since: "0", NoLive: true}, func(e api.Event) error {
		changed = append(changed, string(e.Data))
		return nil
	})
	if len(changed) != 1 || !strings.Contains(changed[0], `"from":"preferred"`) || !strings.Contains(changed[0], `"to":"required"`) || !strings.Contains(changed[0], `"by":`) {
		t.Fatalf("the mode change's event: %v", changed)
	}
	if s, err := run(carl, "sandbox@box-b", onB); err != nil || wire.has(s) {
		t.Fatalf("required, new -> new: %v (clear: %v)", err, wire.has(s))
	}
	s, err := run(carl, "sandbox@box-c", onC)
	refused(err, "the host runs a Potluck (or zm) too old")
	if wire.has(s) {
		t.Fatal("required: a secret reached the old host's wire")
	}
	_, err = run(dave, "sandbox@box-b", daveOnB)
	refused(err, "this pool requires sealing, and the calling machine")
	// A tailnet login (no token) naming a key and sending secrets in the
	// clear: the key it names is not taken, so required refuses it.
	if _, err := a.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "lou", TailscaleLogin: "lou@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.owner.Grant(ctx, api.GrantRequest{Identity: "lou", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	louNode := tn.NodeFunc(tailnettest.Identity{NodeID: "nLOU", NodeKey: "nodekey:lou", Login: "lou@example.com", Host: "lou"})(tailnet.NodeConfig{Hostname: "lou"})
	if err := louNode.Start(); err != nil {
		t.Fatal(err)
	}
	louHC := louNode.(*tailnettest.Node).HTTPClient()
	bid, bcert, _ := module[*tunnel.Service](t, b).SealIdentity()
	lou := func(path, body string, key bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://potluck-mini."+tailnettest.Suffix+":7433"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
		api.SetHeader(req.Header, api.HeaderProject, "orchid")
		if key {
			api.SetHeader(req.Header, api.HeaderSealID, bid)
			api.SetHeader(req.Header, api.HeaderSealKey, bcert)
		}
		resp, err := louHC.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	code, out := lou("/v0/run_targets/sandbox@box-b/acquire", `{"request_id":"lou-1","ttl_s":120}`, false)
	var lh computeapi.RunTargetHandle
	if code != 200 || json.Unmarshal([]byte(out), &lh) != nil || lh.Handle == "" {
		t.Fatalf("a tailnet login's acquire: %d %s", code, out)
	}
	lid := lh.Handle[strings.LastIndex(lh.Handle, "/")+1:]
	if code, out := lou("/v0/run_targets/sandbox@box-b/handles/"+lid+"/exec", `{"argv":["true"],"secrets":{"A":"lou-c4n4ry"}}`, true); code != 403 || !strings.Contains(out, "requires sealing") {
		t.Fatalf("a tailnet login naming a key, secrets in the clear, under required: %d %s", code, out)
	}
	if out, err := computeclient.ExecRunTarget(ctx, carl, "sandbox@box-c", onC, computeapi.ExecRunTargetRequest{Argv: []string{"true"}}); err != nil || out.Exit != 0 {
		t.Fatalf("required: secret-free exec on the old host: %+v %v", out, err)
	}

	// Back to preferred: at once too.
	if _, err := computeclient.SetSealing(ctx, admin, computeapi.SetSealingRequest{Mode: computeapi.SealingPreferred}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(carl, "sandbox@box-c", onC); err != nil {
		t.Fatalf("preferred again: %v", err)
	}

	// A rollback of the pool's machine (sealing off at A stands in for a
	// build from before sealing) in a pool that last reported preferred: B,
	// as member and as host, notices without a restart and goes clear,
	// warning every call ("it stopped sealing").
	cyclePoll := func() {
		t.Helper()
		// A held-open poll A answered after the change began still names
		// A's key (as one started before a real rollback would): one
		// secret-free op lets it come back.
		if _, err := computeclient.ExecRunTarget(ctx, carl, "sandbox@box-b", onB, computeapi.ExecRunTargetRequest{Argv: []string{"true"}}); err != nil {
			t.Fatalf("secret-free exec: %v", err)
		}
	}
	// B's held-open poll was answered under the mode before: let it come
	// back so B hears preferred.
	cyclePoll()
	undo := module[*tunnel.Service](t, a).SealOff()
	cyclePoll()
	for range 3 {
		if s, err = run(carl, "sandbox@box-b", onB); err == nil {
			break
		}
	}
	if err != nil || !wire.has(s) || !strings.Contains(logOf(logsB), "stopped sealing") {
		t.Fatalf("a preferred pool's machine rolled back: %v (clear: %v; warned: %v)", err, wire.has(s), strings.Contains(logOf(logsB), "stopped sealing"))
	}
	undo()

	// The same in a pool that last reported required: refused, in the clear
	// never, until B's owner forgets that pool's machine.
	if _, err := computeclient.SetSealing(ctx, admin, computeapi.SetSealingRequest{Mode: computeapi.SealingRequired, Force: true}); err != nil {
		t.Fatal(err)
	}
	if s, err := run(carl, "sandbox@box-b", onB); err != nil || wire.has(s) {
		t.Fatalf("sealed again (B learns required): %v (clear: %v)", err, wire.has(s))
	}
	cyclePoll()
	undo = module[*tunnel.Service](t, a).SealOff()
	defer undo()
	cyclePoll()
	for range 3 {
		s, err = run(carl, "sandbox@box-b", onB)
		if err == nil || wire.has(s) {
			t.Fatalf("a required pool's machine rolled back: %v (clear: %v)", err, wire.has(s))
		}
	}
	if v, err := computeclient.ForgetSealedHome(ctx, b.owner, "orchid"); err != nil || !v.Forgot {
		t.Fatalf("forget: %+v %v", v, err)
	}
	if s, err = run(carl, "sandbox@box-b", onB); err != nil || !wire.has(s) {
		t.Fatalf("after the owner forgot it: %v (clear, as before: %v)", err, wire.has(s))
	}
}

func logOf(b *syncBuf) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// The mode a pool's machine reports beside its key does not depend on the
// request that asked: a request cancelled mid-read neither reports required
// for a preferred pool nor leaves that cached.
func TestReportedSealingIgnoresCancel(t *testing.T) {
	ctx := context.Background()
	a := startCompute(t, tailnettest.New(), "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	if _, err := a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.owner.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	rs := module[*runtarget.Service](t, a)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if m := rs.ReportedSealing(gone); m != computeapi.SealingPreferred {
		t.Fatalf("a cancelled request reported %q for a preferred pool", m)
	}
	if m := rs.ReportedSealing(ctx); m != computeapi.SealingPreferred {
		t.Fatalf("then %q", m)
	}
	if _, err := computeclient.SetSealing(ctx, inProject(a.owner, "orchid"), computeapi.SetSealingRequest{Mode: computeapi.SealingRequired, Force: true}); err != nil {
		t.Fatal(err)
	}
	if m := rs.ReportedSealing(gone); m != computeapi.SealingRequired {
		t.Fatalf("after required, a cancelled request reported %q", m)
	}
}
