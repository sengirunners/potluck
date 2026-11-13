package runtarget_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// volumeDriver wraps the reference driver as one listing capability volumes:
// it logs every request it gets and answers acquire's volumes as new.
func volumeDriver(t *testing.T, ref, log string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(log), "zm-run-vol")
	script := fmt.Sprintf(`#!/bin/sh
in=$(cat); printf '%%s\n' "$in" >> %q
out=$(printf '%%s' "$in" | %q)
case "$in" in
*'"verb":"describe"'*) printf '%%s\n' "$out" | sed 's/"capabilities":\[/"capabilities":["volumes",/' ;;
*'"verb":"acquire"'*) printf '%%s\n' "$out" | sed 's/"result":{/"result":{"volumes":[{"name":"cache","path":"c","mode":"rw","new":true}],/' ;;
*) printf '%%s\n' "$out" ;;
esac
`, log, ref)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNamedVolumes (zm-eakh.12.32.1): acquire's volumes go to a driver that
// lists capability volumes with the holder's identity as their owner (never
// the caller's word); a driver without the capability, persist, or a bad
// name is refused before the driver; the answer's volumes reach the caller.
func TestNamedVolumes(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
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
	log := filepath.Join(fix, "requests.log")
	vol := volumeDriver(t, ref, log)
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(name string) (string, error) {
				if name == "vol" {
					return vol, nil
				}
				return ref, nil
			})
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	mint := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	ana, bo := mint("ana"), mint("bo")
	for name, driver := range map[string]string{"vols": "vol", "plain": "ref"} {
		var conf map[string]string
		if name == "plain" { // potluck's reference driver mounts volumes since potluck#2: config volumes=0 lists none
			conf = map[string]string{"volumes": "0"}
		}
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: name, Driver: driver, Kind: "box",
			Isolation: "ephemeral", Images: []string{refPin}, Config: conf}}); err != nil {
			t.Fatal(err)
		}
	}
	cache := []computeapi.BoxVolume{{Name: "cache", Path: "c", Mode: computeapi.VolumeRW}}
	for name, c := range map[string]struct {
		target string
		req    computeapi.AcquireRunTargetRequest
	}{
		"a driver without the capability": {"plain", computeapi.AcquireRunTargetRequest{RequestID: "r1", Volumes: cache}},
		"with persist":                    {"vols", computeapi.AcquireRunTargetRequest{RequestID: "r2", Volumes: cache, Persist: true}},
		"a bad name":                      {"vols", computeapi.AcquireRunTargetRequest{RequestID: "r3", Volumes: []computeapi.BoxVolume{{Name: "Bad Name", Path: "c"}}}},
		"a bad mode":                      {"vols", computeapi.AcquireRunTargetRequest{RequestID: "r4", Volumes: []computeapi.BoxVolume{{Name: "c", Path: "c", Mode: "rwx"}}}},
		"new in a request":                {"vols", computeapi.AcquireRunTargetRequest{RequestID: "r5", Volumes: []computeapi.BoxVolume{{Name: "c", Path: "c", New: true}}}},
		"an absolute path":                {"vols", computeapi.AcquireRunTargetRequest{RequestID: "r6", Volumes: []computeapi.BoxVolume{{Name: "c", Path: "/etc/c"}}}},
	} {
		if _, err := computeclient.AcquireRunTarget(ctx, ana, c.target, c.req); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v, want bad_request", name, err)
		}
	}
	owners := map[string]string{}
	for who, c := range map[string]*client.Client{"ana": ana, "bo": bo} {
		h, err := computeclient.AcquireRunTarget(ctx, c, "vols", computeapi.AcquireRunTargetRequest{RequestID: "k-" + who, Volumes: cache})
		if err != nil {
			lb, _ := os.ReadFile(log)
			t.Fatalf("%s: %v\n%s", who, err, lb)
		}
		if len(h.Volumes) != 1 || !h.Volumes[0].New || h.Volumes[0].Name != "cache" {
			t.Errorf("%s's answer volumes: %+v", who, h.Volumes)
		}
		if _, err := computeclient.ReleaseRunTarget(ctx, c, "vols", idOf(h.Handle)); err != nil {
			t.Fatal(err)
		}
		if _, err := computeclient.AcquireRunTarget(ctx, c, "vols", computeapi.AcquireRunTargetRequest{RequestID: "k-" + who,
			Volumes: []computeapi.BoxVolume{{Name: "cache", Path: "other", Mode: computeapi.VolumeRW}}}); code(err) != api.CodeConflict {
			t.Errorf("%s: the same request_id with other volumes: %v, want conflict", who, err)
		}
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var req struct {
			Verb string `json:"verb"`
			Args struct {
				RequestID   string                 `json:"request_id"`
				Actor       string                 `json:"actor"`
				VolumeOwner string                 `json:"volume_owner"`
				Volumes     []computeapi.BoxVolume `json:"volumes"`
			} `json:"args"`
		}
		if json.Unmarshal([]byte(line), &req) != nil || req.Verb != "acquire" {
			continue
		}
		if len(req.Args.Volumes) != 1 || req.Args.Volumes[0] != cache[0] {
			t.Errorf("the driver got volumes %+v", req.Args.Volumes)
		}
		if !strings.HasPrefix(req.Args.VolumeOwner, "identity:") {
			t.Errorf("volume_owner %q: want the holder's identity", req.Args.VolumeOwner)
		}
		owners[req.Args.Actor] = req.Args.VolumeOwner
	}
	if len(owners) != 2 {
		t.Fatalf("acquires seen by the driver, by actor: %v", owners)
	}
	seen := map[string]bool{}
	for _, o := range owners {
		if seen[o] {
			t.Errorf("two holders sent the same volume_owner %s: %v", o, owners)
		}
		seen[o] = true
	}
}

// One host binding shared with two projects (zm-eakh.12.32.1 review): an
// authority's volume_owner and actor reach the driver keyed by its project,
// so project acme's authority naming orchid's holder gets another owner, and
// neither reaches the other's volumes.
func TestHostVolumesKeyedByProject(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
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
	log := filepath.Join(fix, "requests.log")
	vol := volumeDriver(t, ref, log)
	var svc *runtarget.Service
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			svc = s
			s.SetLookPath(func(string) (string, error) { return vol, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	orchid, acme := ids.New(), ids.New() // team projects (by id) this host lends to
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{orchid, acme}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "vol", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
		t.Fatal(err)
	}
	const victim = "identity:01JVICTIM0000000000000000"
	got := map[string]string{}
	for _, project := range []string{orchid, acme} {
		args, _ := json.Marshal(map[string]any{"request_id": "r-" + project, "actor": "machine:host-d", "ttl_s": 60, "volume_owner": victim,
			"volumes": []computeapi.BoxVolume{{Name: "cache", Path: "c", Mode: "rw"}}})
		res, err := svc.HostCall(ctx, project, "sandbox", "acquire", "", args)
		if err != nil {
			t.Fatalf("%s: %v", project, err)
		}
		var ans struct {
			Handle string `json:"handle"`
		}
		_ = json.Unmarshal(res, &ans)
		_, _ = svc.HostCall(ctx, project, "sandbox", "release", ans.Handle, nil)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	actors := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var req struct {
			Verb string `json:"verb"`
			Args struct {
				RequestID   string `json:"request_id"`
				Actor       string `json:"actor"`
				VolumeOwner string `json:"volume_owner"`
			} `json:"args"`
		}
		if json.Unmarshal([]byte(line), &req) != nil || req.Verb != "acquire" {
			continue
		}
		if req.Args.VolumeOwner == victim || req.Args.Actor == "machine:host-d" {
			t.Errorf("the driver got the authority's word unkeyed: owner %q actor %q", req.Args.VolumeOwner, req.Args.Actor)
		}
		got[req.Args.RequestID], actors[req.Args.Actor] = req.Args.VolumeOwner, true
	}
	if len(got) != 2 || len(actors) != 2 {
		t.Fatalf("acquires by request: %v, actors %v", got, actors)
	}
	seen := map[string]bool{}
	for _, o := range got {
		if seen[o] {
			t.Errorf("two projects' acquires named one volume owner: %v", got)
		}
		seen[o] = true
	}
}

// Re-review (zm-eakh.12.32.1): the driver decodes args case-insensitively,
// so a relayed acquire naming VOLUME_OWNER, Volume_Owner, ACTOR or Request_ID
// would pass HostCall's keying; any arg key not in the protocol's lowercase
// is refused before the driver.
func TestHostRefusesCaseVariantKeys(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
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
	log := filepath.Join(fix, "requests.log")
	vol := volumeDriver(t, ref, log)
	var svc *runtarget.Service
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			svc = s
			s.SetLookPath(func(string) (string, error) { return vol, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	orchid, acme := ids.New(), ids.New() // team projects (by id) this host lends to
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{orchid, acme}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "vol", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
		t.Fatal(err)
	}
	for i, extra := range []map[string]any{{"VOLUME_OWNER": "identity:01JVICTIM"}, {"Volume_Owner": "identity:01JVICTIM"}, {"ACTOR": "identity:01JVICTIM"},
		{"Request_ID": "victim-key"}, {"Volume_Project": "victim"},
		{"requeſt_id": "victim-key"}, {"request_ıd": "victim-key"}, {"volume_ownerſ": "identity:01JVICTIM"}, {"max_byteſ": 1 << 30}} {
		a := map[string]any{"request_id": fmt.Sprintf("c%d", i), "actor": "machine:host-d", "ttl_s": 60, "volume_owner": "identity:01JMINE",
			"volumes": []computeapi.BoxVolume{{Name: "cache", Path: "c", Mode: "rw"}}}
		for k, v := range extra {
			a[k] = v
		}
		raw, _ := json.Marshal(a)
		if res, err := svc.HostCall(ctx, acme, "sandbox", "acquire", "", raw); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("a relayed acquire with %v: %s %v, want bad_request", extra, res, err)
		}
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "VICTIM") || strings.Contains(string(b), "victim") {
		t.Errorf("a case-variant key reached the driver:\n%s", b)
	}
	// Any verb: a case-variant key never reaches the driver.
	raw, _ := json.Marshal(map[string]any{"Argv": []string{"true"}})
	if _, err := svc.HostCall(ctx, acme, "sandbox", "exec", "box:sandbox@mini/x", raw); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("exec with Argv: %v, want bad_request", err)
	}
}
