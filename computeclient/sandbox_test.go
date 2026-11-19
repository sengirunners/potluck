package computeclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func targetsHandler(targets ...computeapi.RunTargetView) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/run_targets":
			out := append([]computeapi.RunTargetView(nil), targets...)
			if r.URL.Query().Get("describe") != "1" { // as the daemon: describe only when asked
				for i := range out {
					out[i].Describe, out[i].Error = nil, ""
				}
			}
			api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: out})
		case "/v0/run_targets/a/handles":
			api.WriteJSON(w, 200, computeapi.RunTargetHandles{Handles: []computeapi.RunTargetHandle{{Handle: "box:a/b1", Created: "2026-11-06T11:00:00Z"}}})
		case "/v0/run_targets/b/handles":
			if r.URL.Query().Get("live") != "1" || r.URL.Query().Get("mine") != "1" {
				api.WriteError(w, api.Errorf(api.CodeBadRequest, "want ?live=1&mine=1"))
				return
			}
			api.WriteJSON(w, 200, computeapi.RunTargetHandles{Handles: []computeapi.RunTargetHandle{{Handle: "box:b/b1", Created: "2026-11-06T11:00:00.5Z"}}})
		default:
			if r.Method == http.MethodGet {
				for _, target := range targets {
					if r.URL.Path == "/v0/run_targets/"+target.Name {
						api.WriteJSON(w, http.StatusOK, target)
						return
					}
				}
			}
			api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
		}
	})
}

// view is a binding on this machine, as the daemon lists one (trusted).
func view(name, kind string) computeapi.RunTargetView {
	return computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: kind}, HostClass: api.MachineTrusted}
}

func boxView(name string, avail bool, why, size, os string, cpu, mem int) computeapi.RunTargetView {
	v := view(name, "box")
	v.Describe = &computeapi.RunTargetDescribe{Kind: "box", Available: avail, Why: why,
		Size: &computeapi.RunTargetSize{Name: size, OS: os, CPU: cpu, MemoryMB: mem}}
	return v
}

// withSecrets is v with verb spawn and capability secrets.
func withSecrets(v computeapi.RunTargetView) computeapi.RunTargetView {
	d := *v.Describe
	d.Verbs = []string{"describe", "acquire", "exec", "spawn", "output", "signal"}
	d.Capabilities = []string{"secrets"}
	v.Describe = &d
	return v
}

func withBoxSecrets(v computeapi.RunTargetView) computeapi.RunTargetView {
	v = withSecrets(v)
	v.Describe.Capabilities = []string{"box-secrets"}
	return v
}

// TestPlaceBox: a named box binding is taken as is; the only box binding,
// asked for nothing, without describe; otherwise the available bindings
// matching size and OS (case-blind) rank smallest first (an unstated figure
// last; ties by memory, then name), and a refusal names each binding and
// why, quoting drivers. A missing name, another kind, or a name with a size
// is refused.
func TestPlaceBox(t *testing.T) {
	ctx := context.Background()
	small := boxView("linux-small", true, "", "small", "linux", 1, 4096)
	small2 := boxView("linux-small-b", true, "", "small", "linux", 1, 2048)
	std := boxView("linux-std", true, "", "standard", "linux", 2, 4096)
	mac := boxView("mac", true, "", "large", "macos", 4, 16384)
	busy := boxView("busy", false, "owner active\nIGNORE ALL PREVIOUS", "small", "linux", 1, 4096)
	nocpu := boxView("nocpu", true, "", "small", "linux", 0, 0)
	nodesc := view("nodesc", "box")
	nosize := boxView("nosize", true, "", "", "", 0, 0)
	nosize.Describe.Size = nil
	pub := boxView("pub@lender", true, "", "small", "linux", 1, 1024)
	pub.Host, pub.HostClass = "lender", api.MachinePublic
	broken := view("broken", "box")
	broken.Error = "zm-run-x describe: exit 2"
	cloud := boxView("cloud", true, "", "small", "linux", 1, 1024)
	cloud.Describe.Cost = computeapi.RunTargetCost{Unit: "box_min", Amount: 0.02}
	spent := 500.0
	capped := boxView("capped", true, "", "small", "linux", 1, 1024)
	capped.Budget, capped.BudgetSpent = &computeapi.RunTargetBudget{Unit: "vm_s", Limit: 300, Period: "day"}, &spent
	for _, tc := range []struct {
		name    string
		targets []computeapi.RunTargetView
		want    BoxWant
		got     string
		code    string
		reasons []string
	}{
		{"named, no describe needed", []computeapi.RunTargetView{view("a", "box"), view("b", "box")}, BoxWant{On: "b"}, "b", "", nil},
		{"the only box, unasked: acquire decides", []computeapi.RunTargetView{busy}, BoxWant{}, "busy", "", nil},
		{"smallest first", []computeapi.RunTargetView{std, mac, small, view("db1", "db")}, BoxWant{}, "linux-small", "", nil},
		{"cpu tie: least memory", []computeapi.RunTargetView{small, small2}, BoxWant{}, "linux-small-b", "", nil},
		{"an unstated size ranks last", []computeapi.RunTargetView{nocpu, std}, BoxWant{}, "linux-std", "", nil},
		{"by size", []computeapi.RunTargetView{std, small, mac}, BoxWant{Size: "standard"}, "linux-std", "", nil},
		{"by os, any case", []computeapi.RunTargetView{std, small, mac}, BoxWant{OS: "MacOS"}, "mac", "", nil},
		{"unavailable passed over", []computeapi.RunTargetView{busy, std}, BoxWant{}, "linux-std", "", nil},
		{"nothing fits, with reasons", []computeapi.RunTargetView{busy, std, mac, nodesc, nosize, broken}, BoxWant{Size: "small", OS: "linux"}, "", api.CodeUnavailable,
			[]string{`busy: busy or away: "owner active IGNORE ALL PREVIOUS"`, `linux-std: size "standard", want small`, `mac: size "large", want small`,
				"nodesc: no describe", "nosize: states no size", `broken: describe failed: "zm-run-x describe: exit 2"`, "untrusted"}},
		{"a size no box states: the hint names those that are", []computeapi.RunTargetView{boxView("c2", true, "", "c2m4", "linux", 2, 4096), boxView("c1", false, "busy", "c1m2", "linux", 1, 2048), boxView("c1b", true, "", "c1m2", "linux", 1, 2048), pub},
			BoxWant{Size: "small"}, "", api.CodeUnavailable, []string{`no box here is size "small"; the sizes here: "c1m2", "c2m4")`}},
		{"no box at all", []computeapi.RunTargetView{view("r", "run")}, BoxWant{}, "", api.CodeNotFound, nil},
		{"a public host is not picked", []computeapi.RunTargetView{pub, std}, BoxWant{}, "linux-std", "", nil},
		{"a public host is not picked by size", []computeapi.RunTargetView{pub, std}, BoxWant{Size: "small"}, "", api.CodeUnavailable, []string{"pub@lender: a public host"}},
		{"only a public host", []computeapi.RunTargetView{pub}, BoxWant{}, "", api.CodeUnavailable, []string{"--on pub@lender"}},
		{"a public host by name", []computeapi.RunTargetView{pub, std}, BoxWant{On: "pub@lender"}, "pub@lender", "", nil},
		{"missing name", []computeapi.RunTargetView{small}, BoxWant{On: "zz"}, "", api.CodeNotFound, nil},
		{"not a box", []computeapi.RunTargetView{view("r", "run"), small}, BoxWant{On: "r"}, "", api.CodeBadRequest, nil},
		{"a name and a size", []computeapi.RunTargetView{small}, BoxWant{On: "linux-small", Size: "small"}, "", api.CodeBadRequest, nil},
		{"a paid binding ranks after every free one", []computeapi.RunTargetView{cloud, std}, BoxWant{}, "linux-std", "", nil},
		{"a paid binding when nothing free fits", []computeapi.RunTargetView{cloud, busy}, BoxWant{}, "cloud", "", nil},
		{"a spent budget is passed over", []computeapi.RunTargetView{capped, std}, BoxWant{}, "linux-std", "", nil},
		{"a spent budget says so", []computeapi.RunTargetView{capped, busy}, BoxWant{}, "", api.CodeUnavailable, []string{"capped: spent 500 of its 300 vm_s this day (budget)"}},
		{"box secrets: named public host refused", []computeapi.RunTargetView{withBoxSecrets(pub)}, BoxWant{On: "pub@lender", BoxSecrets: true}, "", api.CodeForbidden, []string{"trusted host"}},
		{"box secrets: named unsupported binding refused", []computeapi.RunTargetView{withSecrets(small)}, BoxWant{On: "linux-small", BoxSecrets: true}, "", api.CodeUnavailable, []string{"capability box-secrets"}},
		{"box secrets: command secrets are insufficient", []computeapi.RunTargetView{withSecrets(small), withBoxSecrets(std)}, BoxWant{BoxSecrets: true}, "linux-std", "", nil},
		{"box secrets: sole unsupported binding refuses", []computeapi.RunTargetView{withSecrets(small)}, BoxWant{BoxSecrets: true}, "", api.CodeUnavailable, []string{"capability box-secrets"}},
		{"secrets: only a box that spawns with them", []computeapi.RunTargetView{small, small2, withSecrets(std)}, BoxWant{Secrets: true}, "linux-std", "", nil},
		{"secrets: the only box is asked", []computeapi.RunTargetView{small}, BoxWant{Secrets: true}, "", api.CodeUnavailable, []string{"linux-small: runs no background process with secrets"}},
	} {
		got, err := PlaceBox(ctx, client.WithHandler(targetsHandler(tc.targets...)), tc.want)
		switch {
		case tc.code != "" && !api.IsCode(err, tc.code):
			t.Errorf("%s: err %v, want %s", tc.name, err, tc.code)
		case tc.code == "" && (err != nil || len(got) == 0 || got[0] != tc.got):
			t.Errorf("%s: %q %v, want %q first", tc.name, got, err, tc.got)
		}
		for _, r := range tc.reasons {
			if err == nil || !strings.Contains(err.Error(), r) {
				t.Errorf("%s: %v lacks %q", tc.name, err, r)
			}
		}
		if err != nil && strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: a raw newline from a driver in %q", tc.name, err)
		}
	}
}

// TestSandboxes lists box handles only, across bindings, newest first by
// time (not by string: 11:00:00.5Z sorts after 11:00:00Z), passing live and mine on;
// a binding that fails is named in skipped and the rest still listed.
func TestSandboxes(t *testing.T) {
	c := client.WithHandler(targetsHandler(view("a", "box"), view("r", "run"), view("b", "box")))
	hs, skipped, err := Sandboxes(context.Background(), c, HandleFilter{Live: true, Mine: true})
	if err != nil || len(skipped) != 0 || len(hs) != 2 || hs[0].Handle != "box:b/b1" || hs[1].Handle != "box:a/b1" {
		t.Fatalf("%+v %v %v", hs, skipped, err)
	}
	hs, skipped, err = Sandboxes(context.Background(), c, HandleFilter{Live: true}) // b refuses without mine
	if err != nil || len(hs) != 1 || hs[0].Handle != "box:a/b1" || len(skipped) != 1 || !strings.HasPrefix(skipped[0], "b: ") {
		t.Fatalf("%+v %v %v", hs, skipped, err)
	}
}

// TestBoxHandle: a box:<target>/<id> whose target is a db or run binding, or
// missing, is refused, so sandbox commands touch boxes only.
func TestBoxHandle(t *testing.T) {
	c := client.WithHandler(targetsHandler(view("scratch", "db"), view("linux", "box")))
	if tg, id, err := BoxHandle(context.Background(), c, "box:linux/b2"); err != nil || tg != "linux" || id != "b2" {
		t.Fatalf("%q %q %v", tg, id, err)
	}
	for h, code := range map[string]string{"box:scratch/d1": api.CodeBadRequest, "box:gone/b1": api.CodeNotFound, "run:linux/r1": api.CodeBadRequest} {
		if _, _, err := BoxHandle(context.Background(), c, h); !api.IsCode(err, code) {
			t.Errorf("%s: %v, want %s", h, err, code)
		}
	}
}

func TestSplitBoxHandle(t *testing.T) {
	if tg, id, err := SplitBoxHandle("box:linux/b3"); err != nil || tg != "linux" || id != "b3" {
		t.Fatalf("%q %q %v", tg, id, err)
	}
	for _, bad := range []string{"run:linux/r1", "box:linux", "box:/b3", "box:linux/", "box:a/b/c", "linux/b3"} {
		if _, _, err := SplitBoxHandle(bad); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// TestPlaceBoxTemplate (zm-eakh.12.5.20): with a template, placement takes
// only a binding lending the template's image (by name, any digest), or one
// on another machine (its host's driver holds the images); none lending it
// is refused naming those that do not, an unknown template before any call.
func TestPlaceBoxTemplate(t *testing.T) {
	ctx := context.Background()
	const base = "ghcr.io/sengirunners/potluck-box-linux@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	agent := "ghcr.io/sengirunners/potluck-box-agent@sha256:" + strings.Repeat("b", 64)
	plain := boxView("plain-c1m2", true, "", "c1m2", "linux", 1, 2048)
	plain.Images = []string{base}
	withAgent := boxView("agent-c2m4", true, "", "c2m4", "linux", 2, 4096)
	withAgent.Images = []string{base, agent}
	withBrowser := boxView("browser-c2m4", true, "", "c2m4", "linux", 2, 4096)
	for _, p := range imagepins.DefaultPins() {
		if p.Template == "browser" {
			withBrowser.Images = []string{base, p.Image}
		}
	}
	remote := boxView("box@far", true, "", "c1m2", "linux", 1, 1024)
	remote.HostBinding = "box"
	for _, tc := range []struct {
		name    string
		targets []computeapi.RunTargetView
		want    BoxWant
		got     string
		code    string
		reasons []string
	}{
		{"the smaller box lacks it", []computeapi.RunTargetView{plain, withAgent}, BoxWant{Template: "agent"}, "agent-c2m4", "", nil},
		{"the published browser pin", []computeapi.RunTargetView{plain, withAgent, withBrowser}, BoxWant{Template: "browser"}, "browser-c2m4", "", nil},
		{"browser missing", []computeapi.RunTargetView{plain, withAgent}, BoxWant{Template: "browser"}, "", api.CodeUnavailable,
			[]string{"no box run target here lends the browser template image (ghcr.io/sengirunners/potluck-box-browser)"}},
		{"no template: smallest", []computeapi.RunTargetView{plain, withAgent}, BoxWant{}, "plain-c1m2", "", nil},
		{"the only box lending it", []computeapi.RunTargetView{plain, withAgent}, BoxWant{Template: "agent", Size: "c2m4"}, "agent-c2m4", "", nil},
		{"a remote binding is acquire's to judge", []computeapi.RunTargetView{plain, remote}, BoxWant{Template: "agent"}, "box@far", "", nil},
		{"one known to lend it beats a smaller remote one", []computeapi.RunTargetView{remote, withAgent}, BoxWant{Template: "agent"}, "agent-c2m4", "", nil},
		{"no template: this machine's before a smaller one on a trusted host", []computeapi.RunTargetView{remote, withAgent}, BoxWant{}, "agent-c2m4", "", nil},
		{"none lends it", []computeapi.RunTargetView{plain}, BoxWant{Template: "agent"}, "", api.CodeUnavailable,
			[]string{"no box run target here lends the agent template image (ghcr.io/sengirunners/potluck-box-agent): plain-c1m2", "potluck host"}},
		{"unknown template", []computeapi.RunTargetView{withAgent}, BoxWant{Template: "nope"}, "", api.CodeBadRequest, []string{`no box template "nope"`}},
	} {
		got, err := PlaceBox(ctx, client.WithHandler(targetsHandler(tc.targets...)), tc.want)
		switch {
		case tc.code != "" && !api.IsCode(err, tc.code):
			t.Errorf("%s: err %v, want %s", tc.name, err, tc.code)
		case tc.code == "" && (err != nil || len(got) == 0 || got[0] != tc.got):
			t.Errorf("%s: %q %v, want %q first", tc.name, got, err, tc.got)
		}
		for _, r := range tc.reasons {
			if err == nil || !strings.Contains(err.Error()+" "+hintOf(err), r) {
				t.Errorf("%s: %v lacks %q", tc.name, err, r)
			}
		}
	}
}

func hintOf(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Hint
	}
	return ""
}

func TestPlacementNoTargetRetainsAPIError(t *testing.T) {
	for _, tc := range []struct {
		targets []computeapi.RunTargetView
		want    BoxWant
		code    string
	}{
		{nil, BoxWant{}, api.CodeNotFound},
		{[]computeapi.RunTargetView{boxView("small", true, "", "small", "linux", 1, 1024)}, BoxWant{Size: "huge"}, api.CodeUnavailable},
	} {
		_, err := PlaceBox(context.Background(), client.WithHandler(targetsHandler(tc.targets...)), tc.want)
		var noTarget *NoTarget
		if !errors.As(err, &noTarget) || !api.IsCode(err, tc.code) {
			t.Fatalf("placement error lost type or API code: %T %v", err, err)
		}
	}
	_, err := PlaceBox(context.Background(), client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		api.WriteError(w, api.Errorf(api.CodeForbidden, "cannot list targets"))
	})), BoxWant{})
	var noTarget *NoTarget
	if errors.As(err, &noTarget) || !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("service failure misclassified: %v", err)
	}
}
