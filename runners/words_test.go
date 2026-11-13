package runners_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// Every hint, warning and job reason that says what to run is built from
// Service.Command: a program that embeds the broker under another name
// shows its own, and nothing the broker emits says "potluck runners".
func TestCommandNameIsOneSetting(t *testing.T) {
	for _, prog := range []string{"zm", "potluck"} {
		t.Run(prog, func(t *testing.T) {
			f := newPoolFixtureStarting(t, func(r *config.Runners) {
				r.Pool.Sizes = map[string]string{"sa": "a"}
				r.RetiredAllowPublicRepos = true
			}, func(s *runners.Service) { s.Command = prog }, box("a", "sa", "linux", "1"))
			if w := f.link(t, inst, "good"); w.Code != 200 {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			f.gh.mu.Lock()
			f.gh.events[8101] = "push"
			f.gh.publicRepo["acme/pub"] = true
			f.gh.mu.Unlock()
			ctx := context.Background()
			var said []string // everything the broker told a person to run

			// A job's reason, the refusals of allow, the old setting's answer.
			f.deliver(t, "workflow_job", jobEvent("queued", 8101, 8101, "acme/new", true, "pool:sa"))
			eventually(t, "job refused", func() bool { return f.jobs(t)[8101].State == "ignored" })
			said = append(said, f.jobs(t)[8101].Why)
			_, err := f.allow("acme/pub", false)
			said = append(said, fmt.Sprint(err))
			_, err = f.allow("bob/tool", false)
			said = append(said, fmt.Sprint(err))
			_, err = f.set("allow_public_repos", "on")
			said = append(said, fmt.Sprint(err))

			// The settings and the doctor, as JSON.
			st, err := f.set("job_ttl_s", "4000")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(st)
			said = append(said, string(b))
			var doc json.RawMessage
			if err := f.owner.Do(ctx, "GET", "/v0/runners/doctor", nil, &doc); err != nil {
				t.Fatal(err)
			}
			said = append(said, string(doc))
			if err := f.owner.Do(ctx, "GET", "/v0/runners/doctor?repo=acme/web", nil, &doc); err != nil {
				t.Fatal(err)
			}
			said = append(said, string(doc))

			// The broker's HTML pages (app.go, links.go, setup.go) go through
			// the same setting: an unknown state answers each one's expired page.
			state := strings.Repeat("a", 48)
			for _, req := range []struct{ method, path string }{
				{"GET", "/v0/runners/app/new?state=" + state},
				{"GET", "/v0/runners/app/created?state=" + state},
				{"GET", "/v0/runners/oauth?state=" + state},
				{"POST", "/v0/runners/oauth"},
				{"GET", "/v0/runners/app/installed?state=" + state + "&installation_id=7"},
			} {
				w := f.public(t, req.method, req.path, nil, nil)
				if w.Code != 404 || !strings.Contains(w.Body.String(), "runners ") {
					t.Errorf("%s %s: %d %s", req.method, req.path, w.Code, w.Body)
				}
				said = append(said, w.Body.String())
			}

			all := strings.Join(said, "\n")
			want := prog + " runners allow acme/new"
			if !strings.Contains(all, want) || !strings.Contains(all, prog+" runners allow <owner/repo> --public") {
				t.Errorf("no %q among what the broker said:\n%s", want, all)
			}
			for _, other := range []string{"potluck", "zm"} {
				if other == prog {
					continue
				}
				for _, cmd := range []string{"runners", "ci doctor", "host", "daemon"} {
					if strings.Contains(all, other+" "+cmd) {
						t.Errorf("the broker for %q names %q:\n%s", prog, other+" "+cmd, all)
					}
				}
			}
		})
	}
}

// With the broker not set up, the error that says so names the setup command
// in the broker's program.
func TestOffHintNamesTheCommand(t *testing.T) {
	f := newPoolFixtureStarting(t, func(r *config.Runners) { r.Pool.Project = "" },
		func(s *runners.Service) { s.Command = "zm" })
	err := f.owner.Do(context.Background(), "GET", "/v0/runners/repos", nil, nil)
	var e *api.Error
	if err == nil || !errors.As(err, &e) || e.Code != api.CodeUnavailable || !strings.Contains(e.Hint, "zm runners setup <pool>") ||
		strings.Contains(e.Hint, "potluck") {
		t.Fatalf("repos of a broker that is off: %v", err)
	}
}

// A config.json that still has the old allow_public_repos says it does
// nothing: in runners ls's settings and the doctor's warnings, naming the
// per-repo command. One without it says nothing.
func TestRetiredAllowPublicReposIsSaid(t *testing.T) {
	f := newPoolFixture(t, func(r *config.Runners) { r.RetiredAllowPublicRepos = true })
	st, err := f.set("job_ttl_s", "4000") // any setting answers the settings
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notices) != 1 || !strings.Contains(st.Notices[0], "allow_public_repos") ||
		!strings.Contains(st.Notices[0], "does nothing") || !strings.Contains(st.Notices[0], "potluck runners allow <owner/repo> --public") {
		t.Errorf("settings notices: %q", st.Notices)
	}
	var got runners.Setup
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/setup", nil, &got); err != nil || len(got.Notices) != 1 {
		t.Errorf("GET setup: %+v, %v", got.Notices, err)
	}
	var doc runners.DoctorResult
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor", nil, &doc); err != nil {
		t.Fatal(err)
	}
	if !slicesAny(doc.Warnings, "allow_public_repos") {
		t.Errorf("doctor warnings: %q", doc.Warnings)
	}

	plain := newPoolFixture(t, func(r *config.Runners) {})
	if st, err := plain.set("job_ttl_s", "4000"); err != nil || len(st.Notices) != 0 {
		t.Errorf("a config without the key: %q, %v", st.Notices, err)
	}
}

func slicesAny(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Deleting the retired key, as the notice says, is not a settings change the
// daemon must restart for: the doctor's check 17 does not fail on it.
func TestDeletingRetiredKeyKeepsCheck17(t *testing.T) {
	f := newPoolFixture(t, func(r *config.Runners) { r.RetiredAllowPublicRepos = true })
	path := filepath.Join(f.home, "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal(b, &whole); err != nil {
		t.Fatal(err)
	}
	var rn map[string]json.RawMessage
	if err := json.Unmarshal(whole["runners"], &rn); err != nil {
		t.Fatal(err)
	}
	if _, ok := rn["allow_public_repos"]; !ok {
		t.Fatalf("the key is not on disk: %s", b)
	}
	delete(rn, "allow_public_repos")
	whole["runners"], _ = json.Marshal(rn)
	b, _ = json.Marshal(whole)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if c := f.doctor(t, "acme/web").Checks[16]; c.Status == "fail" {
		t.Errorf("check 17 after the key was deleted: %+v", c)
	}
}

// The notice is built from config.json on disk: once the owner deletes the
// key, as it says, runners ls and the doctor stop showing it without a
// restart.
func TestRetiredKeyNoticeGoesWhenTheKeyIsDeleted(t *testing.T) {
	f := newPoolFixture(t, func(r *config.Runners) { r.RetiredAllowPublicRepos = true })
	if st, err := f.set("job_ttl_s", "4000"); err != nil || len(st.Notices) != 1 {
		t.Fatalf("before: %q, %v", st.Notices, err)
	}
	path := filepath.Join(f.home, "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var whole map[string]json.RawMessage
	var rn map[string]json.RawMessage
	if err := json.Unmarshal(b, &whole); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(whole["runners"], &rn); err != nil {
		t.Fatal(err)
	}
	delete(rn, "allow_public_repos")
	whole["runners"], _ = json.Marshal(rn)
	b, _ = json.Marshal(whole)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var got runners.Setup
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/setup", nil, &got); err != nil || len(got.Notices) != 0 {
		t.Errorf("GET setup after the key was deleted: %q, %v", got.Notices, err)
	}
	var doc runners.DoctorResult
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor", nil, &doc); err != nil {
		t.Fatal(err)
	}
	if slicesAny(doc.Warnings, "allow_public_repos") {
		t.Errorf("doctor warnings after the key was deleted: %q", doc.Warnings)
	}
}
