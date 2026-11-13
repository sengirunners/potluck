package runtarget_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/runtarget/rt"
)

const keyTagDropped = "the pool's key tag here (volumes made under it stay, out of reach)"

// A forget of a project id's authority key that drops host state takes the
// id out of every binding's share list and its handles out of the ledger;
// another project's shares and handles stay.
func TestDropForgottenHostState(t *testing.T) {
	home, drv := hostFixture(t)
	acme, other := ids.New(), ids.New()
	h := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK), joined(other, "other", api.RemoteOK))
	bindings := filepath.Join(home, runtarget.HostFile)
	if err := os.WriteFile(bindings, []byte(rt.EncodeHostRunTargets([]computeapi.HostRunTarget{{RunTarget: sandboxBox, Share: []string{acme, other}}})), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(home, "host_handles.json")
	now := time.Now()
	led, _ := json.Marshal(map[string]any{"version": 2, "handles": map[string]any{
		"ref:a1": map[string]any{"project": acme, "at": now}, "ref:o1": map[string]any{"project": other, "at": now}}})
	if err := os.WriteFile(ledger, led, 0o600); err != nil {
		t.Fatal(err)
	}
	dropped, err := h.svc.DropForgottenForTest(context.Background(), acme)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dropped, []string{keyTagDropped, "share of host binding " + sandboxBox.Name, "handle ref:a1"}) {
		t.Fatalf("dropped %q", dropped)
	}
	b, _ := os.ReadFile(bindings)
	ts, _, err := rt.ParseHostRunTargets(string(b))
	if err != nil || len(ts) != 1 || !slices.Equal(ts[0].Share, []string{other}) {
		t.Fatalf("bindings after the drop: %+v %v", ts, err)
	}
	if owners := ledgerOwners(t, ledger); len(owners) != 1 || owners["ref:o1"] != other {
		t.Fatalf("ledger after the drop: %v", owners)
	}
	// Nothing left for the id but its key tag: nothing else dropped or written.
	if dropped, err := h.svc.DropForgottenForTest(context.Background(), acme); err != nil || !slices.Equal(dropped, []string{keyTagDropped}) {
		t.Fatalf("a second drop: %q %v", dropped, err)
	}
}

// zm-eakh.12.5.10.26: after a drop, a binding shared with the id again meets
// none of the id's old volumes or request keys: the driver is sent a fresh
// request key, volume owner and volume tag, for a project whose tag is its
// id and for one migrated with its old slug's tag alike. Another project's
// keys do not change, a list by an old request id adopts none of the old
// boxes, and a second drop changes the id's again.
func TestDropForgottenNewKeyTag(t *testing.T) {
	home, drv := hostFixture(t)
	log := filepath.Join(filepath.Dir(drv), "requests.log")
	writeOldHost(t, home, "acme", "acme") // acme's tag: its old slug
	acme, fresh, other := ids.New(), ids.New(), ids.New()
	h := startHost(t, home, drv, joined(acme, "acme", api.RemoteOK), joined(fresh, "fresh", api.RemoteOK), joined(other, "other", api.RemoteOK))
	ctx := context.Background()
	share := func() {
		t.Helper()
		if _, err := computeclient.BindRunTarget(ctx, h.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{acme, fresh, other}, Target: sandboxBox}); err != nil {
			t.Fatal(err)
		}
	}
	keys := func(project string) map[string]any {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"request_id": "r1", "actor": "x", "volume_owner": "alice", "ttl_s": 60})
		res, err := h.svc.HostCall(ctx, project, "sandbox", "acquire", "", args)
		var ans struct {
			Handle string `json:"handle"`
		}
		if err != nil || json.Unmarshal(res, &ans) != nil || ans.Handle == "" {
			t.Fatalf("acquire for %s: %s %v", project, res, err)
		}
		_, _ = h.svc.HostCall(ctx, project, "sandbox", "release", ans.Handle, nil)
		got := acquires(t, log)
		last := got[len(got)-1]
		return map[string]any{"request_id": last["request_id"], "volume_owner": last["volume_owner"], "actor": last["actor"], "volume_project": last["volume_project"]}
	}
	share()
	before := map[string]map[string]any{acme: keys(acme), fresh: keys(fresh), other: keys(other)}
	if before[acme]["request_id"] != oldHostKey("acme", "r1") {
		t.Fatalf("acme's tag before the drop is not its slug's: %v", before[acme])
	}
	// A box of fresh's left running: after the drop, a list by its request
	// id must not adopt it under the new tag.
	args, _ := json.Marshal(map[string]any{"request_id": "r2", "actor": "x", "ttl_s": 60})
	res, err := h.svc.HostCall(ctx, fresh, "sandbox", "acquire", "", args)
	var kept struct {
		Handle string `json:"handle"`
	}
	if err != nil || json.Unmarshal(res, &kept) != nil || kept.Handle == "" {
		t.Fatalf("acquire r2: %s %v", res, err)
	}
	seen := map[any]bool{}
	for _, p := range []string{acme, fresh} {
		for _, k := range before[p] {
			seen[k] = true
		}
		if _, err := h.svc.DropForgottenForTest(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	share()
	for _, p := range []string{acme, fresh} {
		for k, v := range keys(p) {
			if seen[v] {
				t.Errorf("%s's %s after the drop and a re-share is an old one: %v", p, k, v)
			}
			seen[v] = true
		}
	}
	r2, _ := json.Marshal(map[string]string{"request_id": "r2"})
	if res, err := h.svc.HostCall(ctx, fresh, "sandbox", "list", "", r2); err != nil || strings.Contains(string(res), kept.Handle) {
		t.Errorf("a list by the old request id after the drop: %s %v; want no %s", res, err, kept.Handle)
	}
	if after := keys(other); !maps.Equal(after, before[other]) {
		t.Errorf("another project's keys changed: %v, were %v", after, before[other])
	}
	// A second drop: a tag again unlike every one before.
	if _, err := h.svc.DropForgottenForTest(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	share()
	for k, v := range keys(fresh) {
		if seen[v] {
			t.Errorf("fresh's %s after a second drop is an old one: %v", k, v)
		}
	}
}
