// Package ci holds the tests of the CI helper scripts in this directory.
package ci

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	factory  = "example-self-hosted-4"
	fallback = "example-hosted-4"
)

// report is a capacity report with one free factory runner; edit breaks one thing.
func report(t *testing.T, edit func(r, p, free map[string]any)) string {
	free := map[string]any{factory: 1, "example-other-4": 7}
	p := map[string]any{"closed": []any{}, "github_refusing": false}
	r := map[string]any{"schema": "thefactory-capacity-v1", "ready": true,
		"at": time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00"), "free": free, "pool": p}
	if edit != nil {
		edit(r, p, free)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// route runs runner-route.sh on body and returns the label it wrote to GITHUB_OUTPUT.
func route(t *testing.T, body string, env ...string) string {
	t.Helper()
	out := t.TempDir() + "/out"
	cmd := exec.Command("bash", "runner-route.sh")
	cmd.Env = append(os.Environ(), "CAPACITY_JSON="+body, "GITHUB_OUTPUT="+out, "ROUTING=",
		"ROUTE_LABEL="+factory, "ROUTE_FALLBACK="+fallback, "CAPACITY_URL=")
	cmd.Env = append(cmd.Env, env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner-route.sh: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "label=")
}

func TestRunnerRoute(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq here (the route job's runner has it; without it the script picks the fallback)")
	}
	cases := []struct {
		name string
		body string
		env  []string
		want string
	}{
		{"free runner", report(t, nil), nil, factory},
		{"no route label", report(t, nil), []string{"ROUTE_LABEL="}, fallback},
		{"routing off", report(t, nil), []string{"ROUTING=off"}, fallback},
		{"routing on", report(t, nil), []string{"ROUTING=on"}, factory},
		{"no answer", "", nil, fallback},
		{"not json", "<html>", nil, fallback},
		{"other schema", report(t, func(r, _, _ map[string]any) { r["schema"] = "thefactory-capacity-v2" }), nil, fallback},
		{"not ready", report(t, func(r, _, _ map[string]any) { r["ready"] = false }), nil, fallback},
		{"old report", report(t, func(r, _, _ map[string]any) {
			r["at"] = time.Now().Add(-3 * time.Minute).UTC().Format("2006-01-02T15:04:05+00:00")
		}), nil, fallback},
		{"report from the future", report(t, func(r, _, _ map[string]any) {
			r["at"] = time.Now().Add(3 * time.Minute).UTC().Format("2006-01-02T15:04:05+00:00")
		}), nil, fallback},
		{"no time", report(t, func(r, _, _ map[string]any) { delete(r, "at") }), nil, fallback},
		{"a guard closed", report(t, func(_, p, _ map[string]any) { p["closed"] = []any{"psi"} }), nil, fallback},
		{"GitHub refusing runners", report(t, func(_, p, _ map[string]any) { p["github_refusing"] = true }), nil, fallback},
		{"no free runner", report(t, func(_, _, free map[string]any) { free[factory] = 0 }), nil, fallback},
		{"no such scale set", report(t, func(_, _, free map[string]any) { delete(free, factory) }), nil, fallback},
		{"free as text", report(t, func(_, _, free map[string]any) { free[factory] = "3" }), nil, fallback},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := route(t, c.body, c.env...); got != c.want {
				t.Fatalf("label %q, want %q", got, c.want)
			}
		})
	}
}

// With no CAPACITY_URL and no CAPACITY_JSON (the variables unset, as on a fork or a fresh copy of the
// repository), the script picks the fallback without a request and without needing jq.
func TestRunnerRouteUnconfigured(t *testing.T) {
	out := t.TempDir() + "/out"
	cmd := exec.Command("bash", "runner-route.sh")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CAPACITY_JSON=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GITHUB_OUTPUT="+out, "ROUTING=", "ROUTE_LABEL="+factory, "ROUTE_FALLBACK="+fallback, "CAPACITY_URL=")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runner-route.sh: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), "no route configured") {
		t.Fatalf("output %q, want the unconfigured reason", b)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if l := strings.TrimPrefix(strings.TrimSpace(string(got)), "label="); l != fallback {
		t.Fatalf("label %q, want %q", l, fallback)
	}
}

// CAPACITY_URL lists the report's home first and an old copy after it: the first fresh report is read, so a
// home that does not answer or answers stale falls back to the copy, and a fresh home is never second-guessed.
func TestRunnerRouteURLList(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq here")
	}
	dir := t.TempDir()
	// A curl that answers https://x.example/<name> with the file <name>, and fails like curl -f without it.
	fake := "#!/bin/sh\nfor a; do u=$a; done\ncat \"" + dir + "/${u##*/}\" 2>/dev/null || exit 22\n"
	if err := os.WriteFile(dir+"/curl", []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("free", report(t, nil))
	write("full", report(t, func(_, _, free map[string]any) { free[factory] = 0 }))
	write("stale", report(t, func(r, _, _ map[string]any) {
		r["at"] = time.Now().Add(-3 * time.Minute).UTC().Format("2006-01-02T15:04:05+00:00")
	}))
	write("html", "<html>")
	for _, c := range []struct {
		urls, want string
	}{
		{"https://x.example/free", factory},
		{"https://x.example/missing https://x.example/free", factory},
		{"https://x.example/stale https://x.example/free", factory},
		{"https://x.example/html https://x.example/free", factory},
		{"https://x.example/full https://x.example/free", fallback},
		{"https://x.example/missing https://x.example/stale", fallback},
	} {
		out := t.TempDir() + "/out"
		cmd := exec.Command("bash", "runner-route.sh")
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "CAPACITY_JSON=") && !strings.HasPrefix(kv, "PATH=") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(env, "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out, "ROUTING=",
			"ROUTE_LABEL="+factory, "ROUTE_FALLBACK="+fallback, "CAPACITY_URL="+c.urls)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("runner-route.sh: %v\n%s", err, b)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimPrefix(strings.TrimSpace(string(b)), "label="); got != c.want {
			t.Errorf("%s: label %q, want %q", c.urls, got, c.want)
		}
	}
}
