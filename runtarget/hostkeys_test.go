package runtarget_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/runtarget/rt"
	"github.com/sengirunners/potluck/wiring"
)

// hostDaemon is one run of a host's daemon on home (a restart is a new one
// on the same home): teamlink loads the joins it records, joins adds more
// (as recorded joins), then the run-target module starts.
type hostDaemon struct {
	svc   *runtarget.Service
	owner *client.Client
	close func()
}

func startHost(t *testing.T, home, drv string, joins ...func(*runtarget.Service)) *hostDaemon {
	t.Helper()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	h := &hostDaemon{owner: client.WithHandler(dm.SocketHandler()), close: func() { once.Do(func() { dm.Close() }) }}
	t.Cleanup(h.close)
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			h.svc = s
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // before the close
	if err := h.svc.LoadJoinsForTest(ctx); err != nil {
		t.Fatal(err)
	}
	for _, j := range joins {
		j(h.svc)
	}
	if err := h.svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func joined(id, slug, state string) func(*runtarget.Service) {
	return func(s *runtarget.Service) { s.JoinStateForTest(id, slug, state) }
}

// oldHostKey is the request key a build that keyed host shares by slug sent
// its driver.
func oldHostKey(slug, requestID string) string {
	sum := sha256.Sum256([]byte("project\x00" + slug))
	return hex.EncodeToString(sum[:5]) + "-" + requestID
}

// oldBindings is a run_targets.toml as a build that keyed shares by slug
// wrote it: no format line.
func oldBindings(ts ...computeapi.HostRunTarget) []byte {
	return []byte(strings.Replace(rt.EncodeHostRunTargets(ts), fmt.Sprintf("format = %d\n", rt.HostFileFormat), "", 1))
}

// oldAcquire makes a box as a build that keyed host shares by slug made it
// for slug: straight through the driver, with the request key, actor and
// volume tag of that build. The handle.
func oldAcquire(t *testing.T, drv, slug, requestID string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"request_id": oldHostKey(slug, requestID), "actor": oldHostKey(slug, "x"), "ttl_s": 600,
		"volume_project": oldHostKey(slug, "volumes")})
	req, _ := json.Marshal(protocol.NewRequest("acquire", "", args, map[string]string{"target": sandboxBox.Name, "kind": "box", "images": refPin}))
	cmd := exec.Command(drv)
	cmd.Stdin = bytes.NewReader(req)
	out, err := cmd.Output()
	var ans struct {
		Result struct {
			Handle string `json:"handle"`
		} `json:"result"`
	}
	if err != nil || json.Unmarshal(out, &ans) != nil || ans.Result.Handle == "" {
		t.Fatalf("the old build's acquire: %s %v", out, err)
	}
	return ans.Result.Handle
}

// frozenKeys are host_keys.json's legacy_keys (project id -> old slug); nil:
// there is no such file.
func frozenKeys(t *testing.T, home string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "host_keys.json"))
	if os.IsNotExist(err) {
		return nil
	}
	var k struct {
		LegacyKeys map[string]string `json:"legacy_keys"`
	}
	if err != nil || json.Unmarshal(b, &k) != nil {
		t.Fatalf("host_keys.json: %s %v", b, err)
	}
	return k.LegacyKeys
}

// ledgerOwners are host_handles.json's owners by handle (its version 2).
func ledgerOwners(t *testing.T, ledger string) map[string]string {
	t.Helper()
	b, _ := os.ReadFile(ledger)
	var led struct {
		Version int                                 `json:"version"`
		Handles map[string]struct{ Project string } `json:"handles"`
	}
	if json.Unmarshal(b, &led) != nil || led.Version != 2 {
		t.Fatalf("the ledger is not version 2: %s", b)
	}
	out := map[string]string{}
	for h, e := range led.Handles {
		out[h] = e.Project
	}
	return out
}

// acquires are the driver's acquire args, in order.
func acquires(t *testing.T, log string) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(log)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var req struct {
			Verb string         `json:"verb"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal([]byte(line), &req) == nil && req.Verb == "acquire" {
			out = append(out, req.Args)
		}
	}
	return out
}

// zm-eakh.12.5.10.7: a host's shares and handle ledger belong to a team
// project by its id, not the slug this machine joined it under. A host
// upgraded from a build that keyed them by slug keeps its shares and boxes
// for the project it is in under that slug (and the request keys its boxes
// and volumes were made with); a slug it is no longer in is dropped. Then:
// leave that project and join an unrelated one under the same slug, and no
// share or handle passes to it.
func TestHostSharesKeyedByProjectID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	log := filepath.Join(fix, "requests.log")
	drv := volumeDriver(t, ref, log)
	ctx := context.Background()
	acme, other := ids.New(), ids.New()
	box := computeapi.RunTarget{Name: "sandbox", Driver: "vol", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}

	// A box made through a relay, as the old build made it (its request key
	// and volume tag from the slug), and the files as that build wrote them:
	// shares and ledger by slug, no host_keys.json, nothing in the registry.
	oldHandle := oldAcquire(t, drv, "acme", "r1")
	ans := struct{ Handle string }{oldHandle}
	orphan := box
	orphan.Name = "orphan"
	if err := os.WriteFile(filepath.Join(home, runtarget.HostFile), oldBindings(computeapi.HostRunTarget{RunTarget: box, Share: []string{"acme", "gone"}}, computeapi.HostRunTarget{RunTarget: orphan, Share: []string{"gone"}}), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	v1, _ := json.Marshal(map[string]any{ans.Handle: map[string]any{"project": "acme", "at": now}, "box:sandbox/gone-1": map[string]any{"project": "gone", "at": now}})
	ledger := filepath.Join(home, "host_handles.json")
	if err := os.WriteFile(ledger, v1, 0o600); err != nil {
		t.Fatal(err)
	}

	// The upgraded host, joined to acme under slug acme.
	h1 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
	waitAdverts(t, h1.svc, acme, 1)
	if l, err := computeclient.HostRunTargets(ctx, h1.owner); err != nil || len(l.Targets) != 1 || l.Targets[0].Name != "sandbox" || !slices.Equal(l.Targets[0].Shared, []string{"acme"}) {
		t.Fatalf("host list after the upgrade: %+v %v; want sandbox shared with acme, orphan gone", l, err)
	}
	file, _ := os.ReadFile(filepath.Join(home, runtarget.HostFile))
	if !strings.Contains(string(file), acme) || strings.Contains(string(file), `"acme"`) || strings.Contains(string(file), "gone") {
		t.Errorf("the bindings file after the upgrade:\n%s", file)
	}
	if st, err := h1.svc.HostCall(ctx, acme, "sandbox", "status", ans.Handle, nil); err != nil || !strings.Contains(string(st), `"ready"`) {
		t.Errorf("acme's box after the upgrade: %s %v", st, err)
	}
	if _, err := h1.svc.HostCall(ctx, other, "sandbox", "status", ans.Handle, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("another project's status of acme's box: %v", err)
	}
	// The old box answers acme's request key: an acquire again with its
	// request_id is that box, and a list by it names it.
	args, _ := json.Marshal(map[string]any{"request_id": "r1", "actor": "x", "ttl_s": 600})
	res, err := h1.svc.HostCall(ctx, acme, "sandbox", "acquire", "", args)
	var again struct {
		Handle string `json:"handle"`
	}
	if err != nil || json.Unmarshal(res, &again) != nil || again.Handle != oldHandle {
		t.Errorf("acme's acquire again of the old box's request: %s %v; want %s", res, err, oldHandle)
	}
	list1, _ := json.Marshal(map[string]string{"request_id": "r1"})
	if res, err := h1.svc.HostCall(ctx, acme, "sandbox", "list", "", list1); err != nil || !strings.Contains(string(res), oldHandle) {
		t.Errorf("acme's list for the old box's request: %s %v", res, err)
	}
	if res, err := h1.svc.HostCall(ctx, acme, "sandbox", protocol.VerbProgress, "", list1); err != nil || !strings.Contains(string(res), `"ready"`) {
		t.Errorf("acme's progress of the old box's request: %s %v", res, err)
	}
	// acme keeps the request keys its boxes and volumes were made with.
	args2, _ := json.Marshal(map[string]any{"request_id": "r2", "actor": "x", "ttl_s": 60})
	res, err = h1.svc.HostCall(ctx, acme, "sandbox", "acquire", "", args2)
	var ans2 struct {
		Handle string `json:"handle"`
	}
	if err != nil || json.Unmarshal(res, &ans2) != nil {
		t.Fatalf("acquire after the upgrade: %s %v", res, err)
	}
	got := acquires(t, log)
	if last := got[len(got)-1]; last["request_id"] != oldHostKey("acme", "r2") || last["volume_project"] != oldHostKey("acme", "volumes") {
		t.Errorf("acme's keys after the upgrade: %v; want the slug-keyed ones it had", last)
	}
	list, _ := json.Marshal(map[string]string{"request_id": "r2"})
	if res, err := h1.svc.HostCall(ctx, acme, "sandbox", "list", "", list); err != nil || !strings.Contains(string(res), ans2.Handle) {
		t.Errorf("acme's list for r2: %s %v", res, err)
	}
	if _, err := h1.svc.HostCall(ctx, acme, "sandbox", "release", ans2.Handle, nil); err != nil {
		t.Errorf("release: %v", err)
	}

	// Leave acme; join an unrelated project under the same slug, then restart.
	newAcme := ids.New()
	h1.svc.LeaveForTest(acme)
	h1.svc.JoinForTest(newAcme, "acme")
	if ads := h1.svc.HostAdverts(ctx, newAcme); len(ads) != 0 {
		t.Errorf("the new acme sees the old one's box binding: %+v", ads)
	}
	h1.close()
	h2 := startHost(t, home, drv, joined(newAcme, "acme", api.RemoteOK))
	if ads := h2.svc.HostAdverts(ctx, newAcme); len(ads) != 0 {
		t.Errorf("after a restart, the new acme sees the old one's box binding: %+v", ads)
	}
	for _, h := range []string{ans.Handle, "box:sandbox/gone-1"} {
		if _, err := h2.svc.HostCall(ctx, newAcme, "sandbox", "status", h, nil); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("the new acme reaches %s: %v", h, err)
		}
	}
	// Shared by its slug now, the binding is the new project's, keyed by its id.
	if _, err := computeclient.BindRunTarget(ctx, h2.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{"acme"}, Target: box}); err != nil {
		t.Fatal(err)
	}
	if l, err := computeclient.HostRunTargets(ctx, h2.owner); err != nil || len(l.Targets) != 1 || !slices.Equal(l.Targets[0].Shared, []string{"acme"}) {
		t.Fatalf("host list: %+v %v", l, err)
	}
	waitAdverts(t, h2.svc, newAcme, 1)
	if ads := h2.svc.HostAdverts(ctx, acme); len(ads) != 0 {
		t.Errorf("the left project still sees the binding: %+v", ads)
	}
	res, err = h2.svc.HostCall(ctx, newAcme, "sandbox", "acquire", "", args)
	var ans3 struct {
		Handle string `json:"handle"`
	}
	if err != nil || json.Unmarshal(res, &ans3) != nil || ans3.Handle == "" || ans3.Handle == ans.Handle {
		t.Fatalf("the new acme's acquire with the old one's request_id: %s %v (old box %s)", res, err, ans.Handle)
	}
	got = acquires(t, log)
	if last := got[len(got)-1]; last["request_id"] == oldHostKey("acme", "r1") || last["volume_project"] == oldHostKey("acme", "volumes") {
		t.Errorf("the new acme got the old one's keys: %v", last)
	}
	_, _ = h2.svc.HostCall(ctx, newAcme, "sandbox", "release", ans3.Handle, nil)
	// A slug this machine is not in cannot be shared with.
	if _, err := computeclient.BindRunTarget(ctx, h2.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{"gone"}, Target: box}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("a share with a project this machine is not in: %v", err)
	}
}

// waitAdverts waits for n adverts for project (the describe behind one is
// asked in the background).
func waitAdverts(t *testing.T, svc *runtarget.Service, project string, n int) {
	t.Helper()
	for by := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		ads := svc.HostAdverts(context.Background(), project)
		if len(ads) == n {
			return
		}
		if time.Now().After(by) {
			t.Fatalf("adverts for %s: %+v, want %d", project, ads, n)
		}
	}
}

// The migration's edges: a project whose access
// is revoked for now keeps its shares, handles and key; once migrated, a slug
// resolves only to the project it meant then, never to one joined later
// under it; and a host that left every project before upgrading drops its
// slug shares then, so a project joined afterwards under one gets nothing.
func TestHostKeyMigrationEdges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	log := filepath.Join(fix, "requests.log")
	drv := volumeDriver(t, ref, log)
	ctx := context.Background()
	box := computeapi.RunTarget{Name: "sandbox", Driver: "vol", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}
	bindings := filepath.Join(home, runtarget.HostFile)
	ledger := filepath.Join(home, "host_handles.json")
	writeOld := func(share ...string) {
		t.Helper()
		if err := os.WriteFile(bindings, oldBindings(computeapi.HostRunTarget{RunTarget: box, Share: share}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeOld("acme", "beta")
	v1, _ := json.Marshal(map[string]any{"box:sandbox/h1": map[string]any{"project": "acme", "at": time.Now().UTC()}})
	if err := os.WriteFile(ledger, v1, 0o600); err != nil {
		t.Fatal(err)
	}

	// acme's access is revoked for now: it is still acme's.
	acme := ids.New()
	h1 := startHost(t, home, drv, joined(acme, "acme", api.RemoteRevoked))
	if l, err := computeclient.HostRunTargets(ctx, h1.owner); err != nil || len(l.Targets) != 1 || !slices.Equal(l.Targets[0].Shared, []string{"acme"}) {
		t.Fatalf("host list with acme revoked: %+v %v; want sandbox shared with acme (beta dropped)", l, err)
	}
	if o, k := ledgerOwners(t, ledger), frozenKeys(t, home); o["box:sandbox/h1"] != acme || k[acme] != "acme" || len(k) != 1 {
		t.Errorf("after the migration: owners %v, frozen keys %v", o, k)
	}
	h1.close()

	// acme left; another project joined under its slug; a share by that
	// slug comes back into the file (a restored backup): it is the old acme's.
	newAcme := ids.New()
	writeOld("acme")
	h2 := startHost(t, home, drv, joined(newAcme, "acme", api.RemoteOK))
	if ads := h2.svc.HostAdverts(ctx, newAcme); len(ads) != 0 {
		t.Errorf("the new acme sees a share the old one had: %+v", ads)
	}
	if b, _ := os.ReadFile(bindings); !strings.Contains(string(b), acme) || strings.Contains(string(b), newAcme) {
		t.Errorf("the restored slug share resolved to the project joined now:\n%s", b)
	}
	if _, err := computeclient.BindRunTarget(ctx, h2.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{"acme"}, Target: box}); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"request_id": "r1", "actor": "x", "ttl_s": 60})
	res, err := h2.svc.HostCall(ctx, newAcme, "sandbox", "acquire", "", args)
	if err != nil {
		t.Fatalf("the new acme's acquire: %s %v", res, err)
	}
	var ans struct {
		Handle string `json:"handle"`
	}
	_ = json.Unmarshal(res, &ans)
	_, _ = h2.svc.HostCall(ctx, newAcme, "sandbox", "release", ans.Handle, nil)
	got := acquires(t, log)
	if len(got) == 0 || got[len(got)-1]["volume_project"] == oldHostKey("acme", "volumes") {
		t.Errorf("the new acme got the old one's volume tag: %v", got)
	}
}

// A host that left every project, then upgraded, then joined an unrelated
// project under an old slug: the slug shares and handles were dropped when
// the daemon started, so the new project gets none of them, nor the old key tag.
func TestHostKeyMigrationWithNoJoin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	drv := volumeDriver(t, ref, filepath.Join(fix, "requests.log"))
	ctx := context.Background()
	box := computeapi.RunTarget{Name: "sandbox", Driver: "vol", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}
	if err := os.WriteFile(filepath.Join(home, runtarget.HostFile), oldBindings(computeapi.HostRunTarget{RunTarget: box, Share: []string{"acme"}}), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(home, "host_handles.json")
	v1, _ := json.Marshal(map[string]any{"box:sandbox/h1": map[string]any{"project": "acme", "at": time.Now().UTC()}})
	if err := os.WriteFile(ledger, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	// The daemon starts with no join; the first host call comes only after
	// the new project's join (its heartbeat).
	h := startHost(t, home, drv)
	newAcme := ids.New()
	h.svc.JoinForTest(newAcme, "acme")
	if ads := h.svc.HostAdverts(ctx, newAcme); len(ads) != 0 {
		t.Errorf("the new acme sees the old share: %+v", ads)
	}
	if _, err := h.svc.HostCall(ctx, newAcme, "sandbox", "status", "box:sandbox/h1", nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("the new acme reaches the old handle: %v", err)
	}
	if l, err := computeclient.HostRunTargets(ctx, h.owner); err != nil || len(l.Targets) != 0 {
		t.Errorf("host list after the upgrade with no join: %+v %v; want none", l, err)
	}
	if o, k := ledgerOwners(t, ledger), frozenKeys(t, home); o["box:sandbox/h1"] != "slug:acme" || k == nil || len(k) != 0 {
		t.Errorf("after the upgrade with no join: owners %v, frozen keys %v; want h1 left to no project, nothing frozen", o, k)
	}
}

// bareHost is a host daemon whose joins are not loaded and whose module is
// not started (teamlink failed, or has not run yet).
func bareHost(t *testing.T, home string) *hostDaemon {
	t.Helper()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	drv := volumeDriver(t, ref, filepath.Join(fix, "requests.log"))
	h := &hostDaemon{owner: client.WithHandler(dm.SocketHandler()), close: func() {}}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			h.svc = s
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	return h
}

var sandboxBox = computeapi.RunTarget{Name: "sandbox", Driver: "vol", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}

// writeOldHost writes run_targets.toml and host_handles.json as a build that
// keyed them by slug did: sandbox shared with share, handle h1 owned by owner.
func writeOldHost(t *testing.T, home, owner string, share ...string) (bindings, ledger string) {
	t.Helper()
	bindings, ledger = filepath.Join(home, runtarget.HostFile), filepath.Join(home, "host_handles.json")
	if err := os.WriteFile(bindings, oldBindings(computeapi.HostRunTarget{RunTarget: sandboxBox, Share: share}), 0o600); err != nil {
		t.Fatal(err)
	}
	v1, _ := json.Marshal(map[string]any{"box:sandbox/h1": map[string]any{"project": owner, "at": time.Now().UTC()}})
	if err := os.WriteFile(ledger, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	return bindings, ledger
}

// A ledger in the old format that names no slug (empty) needs no joins to
// move on: a daemon whose joins are not loaded still binds and lends by id.
func TestHostOldEmptyLedgerNeedsNoJoins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ledger := filepath.Join(home, "host_handles.json")
	if err := os.WriteFile(ledger, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := bareHost(t, home)
	if _, err := computeclient.BindRunTarget(context.Background(), h.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{ids.New()}, Target: sandboxBox}); err != nil {
		t.Fatalf("bind with an old empty ledger and no joins loaded: %v", err)
	}
	if b, _ := os.ReadFile(ledger); !strings.Contains(string(b), `"version":2`) {
		t.Errorf("the ledger was not moved on: %s", b)
	}
}

// Slug shares with the joins not loaded: nothing is migrated (that would drop
// every share) and the host refuses binds and relayed calls until it can.
func TestHostKeyMigrationWaitsForJoins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bindings, ledger := writeOldHost(t, home, "acme", "acme")
	before := map[string][]byte{}
	for _, f := range []string{bindings, ledger} {
		before[f], _ = os.ReadFile(f)
	}
	h := bareHost(t, home)
	ctx := context.Background()
	if _, err := computeclient.BindRunTarget(ctx, h.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{ids.New()}, Target: sandboxBox}); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("bind before the joins loaded: %v, want unavailable", err)
	}
	if _, err := h.svc.HostCall(ctx, ids.New(), "sandbox", "status", "box:sandbox/h1", nil); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("a relayed call before the joins loaded: %v, want unavailable", err)
	}
	for f, b := range before {
		if now, _ := os.ReadFile(f); string(now) != string(b) {
			t.Errorf("%s changed before the joins loaded:\n%s\nwas\n%s", f, now, b)
		}
	}
}

// Two recorded joins under one slug (acme revoked for now, then an unrelated
// project joined under acme): the old slug names neither, so its share is
// dropped and its handle answers no project.
func TestHostKeyMigrationSlugTwice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, ledger := writeOldHost(t, home, "acme", "acme")
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	drv := volumeDriver(t, ref, filepath.Join(fix, "requests.log"))
	a, b := ids.New(), ids.New()
	h := startHost(t, home, drv, joined(a, "acme", api.RemoteRevoked), joined(b, "acme", api.RemoteOK))
	ctx := context.Background()
	if l, err := computeclient.HostRunTargets(ctx, h.owner); err != nil || len(l.Targets) != 0 {
		t.Errorf("host list: %+v %v; want the slug share dropped", l, err)
	}
	for _, p := range []string{a, b} {
		if _, err := h.svc.HostCall(ctx, p, "sandbox", "status", "box:sandbox/h1", nil); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("project %s reaches the old handle: %v", p, err)
		}
	}
	if o, k := ledgerOwners(t, ledger), frozenKeys(t, home); o["box:sandbox/h1"] != "slug:acme" || len(k) != 0 {
		t.Errorf("the slug went to a project: owners %v, frozen keys %v", o, k)
	}
}

// The migration fails at start (the ledger cannot be written); before it is
// retried, acme is left and an unrelated project joins under its slug. The
// retry still means the acme of the start: the new one gets neither the
// share, the handle nor the old key tag.
func TestHostKeyMigrationRetryKeepsStartJoins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bindings, ledger := writeOldHost(t, home, "acme", "acme")
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	drv := volumeDriver(t, ref, filepath.Join(fix, "requests.log"))
	acme := ids.New()
	readOnly := func(*runtarget.Service) {
		if err := os.Chmod(home, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	h := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK), readOnly)
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ledger); strings.Contains(string(b), `"version"`) || frozenKeys(t, home) != nil {
		t.Fatalf("migrated with its directory read-only: %s", b)
	}
	h.svc.LeaveForTest(acme)
	newAcme := ids.New()
	h.svc.JoinForTest(newAcme, "acme")
	ctx := context.Background()
	if _, err := h.svc.HostCall(ctx, newAcme, "sandbox", "status", "box:sandbox/h1", nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("the new acme reaches the old handle: %v", err)
	}
	if o, k := ledgerOwners(t, ledger), frozenKeys(t, home); o["box:sandbox/h1"] != acme || k[acme] != "acme" || len(k) != 1 {
		t.Errorf("after the retry: owners %v, frozen keys %v; want h1 and the old tag the acme of the start's", o, k)
	}
	if f, _ := os.ReadFile(bindings); strings.Contains(string(f), newAcme) || !strings.Contains(string(f), acme) {
		t.Errorf("the bindings after the retry: %s", f)
	}
}

// A migrated host, acme left and an unrelated project joined under its slug,
// then the old build's ledger restored from a backup (with the bindings as
// they are, or after every binding was removed): its slugs meant the old
// acme, so the handle stays the old acme's and the new one gets neither it
// nor the old key tag.
func TestHostRestoredOldLedgerResolvesNoSlug(t *testing.T) {
	for _, unbound := range []bool{false, true} {
		t.Run(fmt.Sprintf("unbound=%v", unbound), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			_, ledger := writeOldHost(t, home, "acme", "acme")
			backup, _ := os.ReadFile(ledger)
			fix, _ := filepath.EvalSymlinks(t.TempDir())
			state := filepath.Join(fix, "state")
			ref, err := refdriver.Install(fix, state, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { refdriver.KillAll(state) })
			drv := volumeDriver(t, ref, filepath.Join(fix, "requests.log"))
			acme := ids.New()
			h1 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
			ctx := context.Background()
			if l, err := computeclient.HostRunTargets(ctx, h1.owner); err != nil || len(l.Targets) != 1 {
				t.Fatalf("host list after the upgrade: %+v %v", l, err)
			}
			if unbound {
				if err := computeclient.UnbindHostRunTarget(ctx, h1.owner, "sandbox"); err != nil {
					t.Fatal(err)
				}
			}
			h1.close()

			// acme left; an unrelated project joined under acme, and shared with.
			newAcme := ids.New()
			if err := os.WriteFile(ledger, backup, 0o600); err != nil {
				t.Fatal(err)
			}
			h2 := startHost(t, home, drv, joined(newAcme, "acme", api.RemoteOK))
			if _, err := computeclient.BindRunTarget(ctx, h2.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{newAcme}, Target: sandboxBox}); err != nil {
				t.Fatal(err)
			}
			if _, err := h2.svc.HostCall(ctx, newAcme, "sandbox", "status", "box:sandbox/h1", nil); !api.IsCode(err, api.CodeNotFound) {
				t.Errorf("the new acme reaches the old handle: %v", err)
			}
			if o, k := ledgerOwners(t, ledger), frozenKeys(t, home); o["box:sandbox/h1"] != acme || k[acme] != "acme" || len(k) != 1 {
				t.Errorf("after the restore: owners %v, frozen keys %v; want both the old acme's", o, k)
			}
		})
	}
}

// hostFixture is a home and a volume driver for a host test.
func hostFixture(t *testing.T) (home, drv string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	return home, volumeDriver(t, ref, filepath.Join(fix, "requests.log"))
}

// A host with no old files freezes an empty mapping at its first start, so
// an old build's bindings restored later name no project, not one joined
// under the slug since.
func TestHostFreshHostFreezesNothing(t *testing.T) {
	home, drv := hostFixture(t)
	acme := ids.New()
	h1 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
	h1.close()
	if k := frozenKeys(t, home); k == nil || len(k) != 0 {
		t.Fatalf("frozen keys of a fresh host: %v; want an empty file", k)
	}
	if err := os.WriteFile(filepath.Join(home, runtarget.HostFile), oldBindings(computeapi.HostRunTarget{RunTarget: sandboxBox, Share: []string{"acme"}}), 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := startHost(t, home, drv, joined(ids.New(), "acme", api.RemoteOK))
	if l, err := computeclient.HostRunTargets(context.Background(), h2.owner); err != nil || len(l.Targets) != 0 {
		t.Errorf("host list after restoring old bindings: %+v %v; want the slug share dropped", l, err)
	}
}

// host_keys.json removed beside migrated files: the host refuses (made again
// empty, the migrated projects' old key tags would be gone), whether or not
// registry.db still holds the migration's mark.
func TestHostMissingKeyFileRefuses(t *testing.T) {
	for _, forget := range []bool{false, true} {
		t.Run(fmt.Sprintf("mark forgotten=%v", forget), func(t *testing.T) {
			home, drv := hostFixture(t)
			writeOldHost(t, home, "acme", "acme")
			acme := ids.New()
			h1 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
			if forget {
				if err := h1.svc.ForgetHostMarkForTest(); err != nil {
					t.Fatal(err)
				}
			}
			h1.close()
			if err := os.Remove(filepath.Join(home, "host_keys.json")); err != nil {
				t.Fatal(err)
			}
			h2 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
			_, err := computeclient.BindRunTarget(context.Background(), h2.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{acme}, Target: sandboxBox})
			if _, _, hint := teamlink.ErrParts(err); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(hint, "restore host_keys.json") {
				t.Errorf("bind with host_keys.json gone: %v (hint %q), want unavailable, saying to restore it", err, hint)
			}
		})
	}
}

// A full restore of the files from before the
// upgrade (run_targets.toml and host_handles.json in the old format, no
// host_keys.json), after acme was left and an unrelated project joined under
// its slug. Migrated again, the slug would give the new project the old
// one's box, handle and key tag; registry.db's mark (which a restore of the
// files does not undo) makes the host refuse instead, and change nothing.
func TestHostRestoredOldFilesRefuse(t *testing.T) {
	home, drv := hostFixture(t)
	bindings, ledger := writeOldHost(t, home, "acme", "acme")
	backup := map[string][]byte{}
	for _, f := range []string{bindings, ledger} {
		backup[f], _ = os.ReadFile(f)
	}
	acme := ids.New()
	h1 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
	h1.close()

	for f, b := range backup {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(home, "host_keys.json")); err != nil {
		t.Fatal(err)
	}
	newAcme := ids.New()
	h2 := startHost(t, home, drv, joined(newAcme, "acme", api.RemoteOK))
	ctx := context.Background()
	if ads := h2.svc.HostAdverts(ctx, newAcme); len(ads) != 0 {
		t.Errorf("the new acme sees the old one's binding: %+v", ads)
	}
	if _, err := h2.svc.HostCall(ctx, newAcme, "sandbox", "status", "box:sandbox/h1", nil); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("the new acme's call on the old handle: %v, want unavailable", err)
	}
	args, _ := json.Marshal(map[string]any{"request_id": "r1", "actor": "x", "ttl_s": 60})
	if _, err := h2.svc.HostCall(ctx, newAcme, "sandbox", "acquire", "", args); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("the new acme's acquire: %v, want unavailable", err)
	}
	for f, b := range backup {
		if now, _ := os.ReadFile(f); !bytes.Equal(now, b) {
			t.Errorf("%s changed:\n%s", f, now)
		}
	}
	if k := frozenKeys(t, home); k != nil {
		t.Errorf("host_keys.json made again: %v", k)
	}
}

// registry.db's mark set and no file left at all (the owner removed both
// files, as the refusal says): the host starts again with nothing frozen,
// and an old file restored after that names no project.
func TestHostMarkWithNoFilesStartsEmpty(t *testing.T) {
	home, drv := hostFixture(t)
	bindings, ledger := writeOldHost(t, home, "acme", "acme")
	acme := ids.New()
	startHost(t, home, drv, joined(acme, "acme", api.RemoteOK)).close()
	for _, f := range []string{bindings, ledger, filepath.Join(home, "host_keys.json")} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	newAcme := ids.New()
	h := startHost(t, home, drv, joined(newAcme, "acme", api.RemoteOK))
	ctx := context.Background()
	if _, err := computeclient.BindRunTarget(ctx, h.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"acme"}, Target: sandboxBox}); err != nil {
		t.Fatalf("bind after removing both files: %v", err)
	}
	if k := frozenKeys(t, home); k == nil || len(k) != 0 {
		t.Errorf("frozen keys: %v; want an empty file", k)
	}
	waitAdverts(t, h.svc, newAcme, 1)
}

// registry.db restored from before the upgrade (no mark) beside the files
// this build wrote: host_keys.json is the word; the mark is set again.
func TestHostKeyFileWithoutMark(t *testing.T) {
	home, drv := hostFixture(t)
	_, ledger := writeOldHost(t, home, "acme", "acme")
	acme := ids.New()
	h1 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
	if err := h1.svc.ForgetHostMarkForTest(); err != nil {
		t.Fatal(err)
	}
	h1.close()
	h2 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
	waitAdverts(t, h2.svc, acme, 1)
	if o, k := ledgerOwners(t, ledger), frozenKeys(t, home); o["box:sandbox/h1"] != acme || k[acme] != "acme" {
		t.Errorf("owners %v, frozen keys %v", o, k)
	}
	h2.close()
	// The mark is back: the key file removed now is refused even with no
	// file in the new format left.
	if err := os.Remove(filepath.Join(home, "host_keys.json")); err != nil {
		t.Fatal(err)
	}
	writeOldHost(t, home, "acme", "acme")
	h3 := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
	if _, err := h3.svc.HostCall(context.Background(), acme, "sandbox", "status", "box:sandbox/h1", nil); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("a call with the key file gone and old files back: %v, want unavailable", err)
	}
}

// A host_keys.json that gives one slug to two projects (both would get the
// same key tag, and so each other's boxes and volumes) is refused, as is one
// that is not ids to slugs.
func TestHostKeyFileRefusals(t *testing.T) {
	a, b := ids.New(), ids.New()
	for name, body := range map[string]string{
		"a slug twice": fmt.Sprintf(`{"legacy_keys":{%q:"acme",%q:"acme"}}`, a, b),
		"not an id":    `{"legacy_keys":{"acme":"acme"}}`,
		"not a slug":   fmt.Sprintf(`{"legacy_keys":{%q:"Not A Slug"}}`, a),
	} {
		t.Run(name, func(t *testing.T) {
			home, drv := hostFixture(t)
			if err := os.WriteFile(filepath.Join(home, "host_keys.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			h := startHost(t, home, drv, joined(a, "acme", api.RemoteOK))
			if _, err := computeclient.BindRunTarget(context.Background(), h.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{a}, Target: sandboxBox}); !api.IsCode(err, api.CodeUnavailable) {
				t.Errorf("bind: %v, want unavailable", err)
			}
		})
	}
}

// An old file's share is a slug by the file's format, whatever its shape: a
// slug of 26 digits (which looks like an id) is migrated as the slug it is,
// keeps its old key tag, and is shared with by that slug afterwards.
func TestHostDigitSlug(t *testing.T) {
	home, drv := hostFixture(t)
	digits := strings.Repeat("1", 26)
	if !ids.Valid(digits) {
		t.Fatal("want a slug that looks like an id")
	}
	_, ledger := writeOldHost(t, home, digits, digits)
	p := ids.New()
	h := startHost(t, home, drv, joined(p, digits, api.RemoteOK))
	waitAdverts(t, h.svc, p, 1)
	if o, k := ledgerOwners(t, ledger), frozenKeys(t, home); o["box:sandbox/h1"] != p || k[p] != digits {
		t.Errorf("owners %v, frozen keys %v; want both project %s's", o, k, p)
	}
	ctx := context.Background()
	if _, err := computeclient.BindRunTarget(ctx, h.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{digits}, Target: sandboxBox}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, runtarget.HostFile)); !strings.Contains(string(b), p) || strings.Contains(string(b), digits) {
		t.Errorf("shared by the digit slug, the file holds:\n%s", b)
	}
}

// Every file the host writes is the owner's alone.
func TestHostFilesPrivate(t *testing.T) {
	home, drv := hostFixture(t)
	writeOldHost(t, home, "acme", "acme")
	startHost(t, home, drv, joined(ids.New(), "acme", api.RemoteOK)).close()
	for _, f := range []string{"host_keys.json", "host_handles.json", runtarget.HostFile} {
		if st, err := os.Stat(filepath.Join(home, f)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v; want 0600", f, st.Mode(), err)
		}
	}
}

// The old build's ledger restored while the daemon runs: it resolves through
// what is frozen, and relayed acquires keep working.
func TestHostLedgerRestoredWhileRunning(t *testing.T) {
	home, drv := hostFixture(t)
	_, ledger := writeOldHost(t, home, "acme", "acme")
	backup, _ := os.ReadFile(ledger)
	acme := ids.New()
	h := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK))
	ctx := context.Background()
	if _, err := computeclient.HostRunTargets(ctx, h.owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"request_id": "r1", "actor": "x", "ttl_s": 600})
	if res, err := h.svc.HostCall(ctx, acme, "sandbox", "acquire", "", args); err != nil {
		t.Fatalf("acquire after the ledger was restored: %s %v", res, err)
	}
	if o := ledgerOwners(t, ledger); o["box:sandbox/h1"] != acme || len(o) != 2 {
		t.Errorf("owners after the restore and an acquire: %v; want h1 acme's and the new box", o)
	}
}
