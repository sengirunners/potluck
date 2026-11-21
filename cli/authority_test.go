package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// fakeHQ answers the HQ routes as a serving HQ "mini" at epoch 4 would (on
// the standby "laptop": ServedBy), and records every press.
type fakeHQ struct {
	mu      sync.Mutex
	presses []string // "path body"
	checks  []api.PreflightCheck
	public  *api.PublicAddress
	// served is ServedBy.State (default standby); lossS is Silent.LossS.
	served string
	lossS  *int64
}

func (f *fakeHQ) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
	answer := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.Method == http.MethodGet && r.URL.Path == api.AuthorityPath {
		st := api.AuthorityStatus{ServedBy: api.ServedBy{Machine: "laptop", State: cmp.Or(f.served, api.HQStandby)}, Machine: "mini", State: api.HQServing, Epoch: 4,
			CanManage: true, Standby: api.StandbyStatus{Machine: "laptop", State: api.StandbyReady}}
		if f.lossS != nil {
			st.Silent = &api.HQSilence{StandbyProbe: api.ProbeSilent, LossS: f.lossS}
		}
		answer(st)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == api.CandidatesPath {
		answer(api.CandidateList{Preview: api.StandbyPreview{Projects: 3, Secrets: []string{"A", "B"}},
			Candidates: []api.Candidate{{Machine: "laptop", SessionsRunning: 2}}})
		return
	}
	f.mu.Lock()
	f.presses = append(f.presses, r.URL.Path+" "+strings.TrimSpace(string(body)))
	f.mu.Unlock()
	switch r.URL.Path {
	case api.MovePath:
		var q api.MoveRequest
		_ = json.Unmarshal(body, &q)
		a := api.MoveAnswer{Preflight: api.HomePreflight{ID: "p9", From: "mini", To: q.To, Checks: f.checks, PublicAddress: f.public}}
		if !q.CheckOnly {
			a.Move = &api.HomeMove{ID: "mv1", Kind: api.MoveKindSwap, State: api.MoveRunning, From: "mini", To: q.To}
		}
		answer(a)
	default:
		answer(api.HomeMove{ID: "mv1", Kind: api.MoveKindPromote, State: api.MoveRunning, From: "mini", To: "laptop"})
	}
}

func (f *fakeHQ) Presses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.presses...)
}

func runAuthority(t *testing.T, f *fakeHQ, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	a := newApp(Host{Prefix: "potluck", Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
		Client: func() (*client.Client, *config.Config, error) { return client.WithHandler(f), &config.Config{}, nil }})
	cmd := a.authorityCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	return out.String() + errOut.String(), err
}

// A move is checked first and starts only on yes, sending the plan and the
// HQ and epoch it showed; a failed check or another answer starts nothing.
// A takeover needs the owner's "it is down" and names this standby.
func TestAuthorityCommandsSendWhatTheyShowed(t *testing.T) {
	f := &fakeHQ{checks: []api.PreflightCheck{{Code: api.CheckStandbyFollowing, OK: true, Text: "laptop follows HQ"}}}
	if _, err := runAuthority(t, f, "no\n", "move", "laptop"); err == nil || !strings.Contains(err.Error(), "nothing was moved") {
		t.Fatalf("a move answered no: %v", err)
	}
	if got := f.Presses(); len(got) != 1 || got[0] != api.MovePath+` {"to":"laptop","check_only":true}` {
		t.Fatalf("a move answered no sent %q", got)
	}
	out, err := runAuthority(t, f, "yes\n", "move", "laptop")
	if err != nil || !strings.Contains(out, "swap mv1: mini to laptop, running") || !strings.Contains(out, "ok laptop follows HQ") {
		t.Fatalf("a move answered yes: %v\n%s", err, out)
	}
	if got := f.Presses(); len(got) != 3 || got[2] != api.MovePath+` {"to":"laptop","plan_id":"p9","from":"mini","epoch":4}` {
		t.Fatalf("a move answered yes sent %q", got)
	}

	f = &fakeHQ{checks: []api.PreflightCheck{{Code: api.CheckStandbyDisk, Text: "laptop has 2 GB free", Fix: "free 10 GB on laptop"}}}
	if _, err := runAuthority(t, f, "yes\n", "move", "laptop"); err == nil || !strings.Contains(err.Error(), api.CheckStandbyDisk) {
		t.Fatalf("a move whose check failed: %v", err)
	}
	if got := f.Presses(); len(got) != 1 {
		t.Fatalf("a move whose check failed sent %q", got)
	}

	f = &fakeHQ{}
	if _, err := runAuthority(t, f, "yes\n", "promote", "--yes"); err == nil {
		t.Fatal("a takeover without --old-home-is-down ran")
	}
	if _, err := runAuthority(t, f, "", "promote", "--old-home-is-down"); err == nil || !strings.Contains(err.Error(), "nothing was taken over") {
		t.Fatalf("a takeover with no answer: %v", err)
	}
	if got := f.Presses(); len(got) != 0 {
		t.Fatalf("an unconfirmed takeover sent %q", got)
	}
	if _, err := runAuthority(t, f, "yes\n", "promote", "--old-home-is-down"); err != nil {
		t.Fatal(err)
	}
	if got := f.Presses(); len(got) != 1 || got[0] != api.PromotePath+` {"machine":"laptop","from":"mini","epoch":4,"old_home_is_down":true}` {
		t.Fatalf("a takeover sent %q", got)
	}
}

// Every press that changes the standby or takes over asks first, saying
// what it costs: the sessions a new standby stops and what it copies, what
// a release deletes, the writes a takeover loses and what taking over
// without the secrets means; a move's plan names the members the public
// address cuts off. On a serving HQ, promote and retire are refused before
// anything is sent.
func TestAuthorityCommandsAskFirst(t *testing.T) {
	f := &fakeHQ{}
	out, err := runAuthority(t, f, "no\n", "standby", "laptop")
	if err == nil || !strings.Contains(out, "laptop becomes HQ's standby: it stops its 2 agent session(s), then holds a copy of 3 team project(s) and 2 secret(s)") {
		t.Fatalf("standby answered no: %v\n%s", err, out)
	}
	if _, err := runAuthority(t, f, "", "standby", "india"); err == nil || !strings.Contains(err.Error(), "india is not one of the machines") {
		t.Fatalf("standby of a machine that is no candidate: %v", err)
	}
	out, err = runAuthority(t, f, "no\n", "release-standby")
	if err == nil || !strings.Contains(out, "laptop stops being the standby: its copy of 3 team project(s) and 2 secret(s) is deleted") {
		t.Fatalf("release answered no: %v\n%s", err, out)
	}
	if got := f.Presses(); len(got) != 0 {
		t.Fatalf("an unconfirmed standby change sent %q", got)
	}
	if _, err := runAuthority(t, f, "yes\n", "standby", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := runAuthority(t, f, "", "release-standby", "--yes"); err != nil {
		t.Fatal(err)
	}
	if got := f.Presses(); len(got) != 2 || got[0] != api.StandbyPath+` {"machine":"laptop","from":"mini","epoch":4}` ||
		got[1] != api.StandbyReleasePath+` {"machine":"laptop","from":"mini","epoch":4}` {
		t.Fatalf("confirmed standby changes sent %q", got)
	}

	loss := int64(90)
	f = &fakeHQ{lossS: &loss}
	out, err = runAuthority(t, f, "no\n", "promote", "--old-home-is-down", "--without-secrets")
	if err == nil || !strings.Contains(out, "about 1m30s of writes") || !strings.Contains(out, "without the secrets") {
		t.Fatalf("a takeover's confirm: %v\n%s", err, out)
	}
	f = &fakeHQ{}
	if out, _ := runAuthority(t, f, "no\n", "promote", "--old-home-is-down"); !strings.Contains(out, "an unknown stretch of writes") {
		t.Fatalf("a takeover's confirm with no loss known:\n%s", out)
	}

	f = &fakeHQ{served: api.HQServing}
	if _, err := runAuthority(t, f, "", "promote", "--force", "--yes"); err == nil || !strings.Contains(err.Error(), "serves HQ now") {
		t.Fatalf("promote on the serving HQ: %v", err)
	}
	if _, err := runAuthority(t, f, "", "retire", "--yes"); err == nil || !strings.Contains(err.Error(), "serves HQ now") {
		t.Fatalf("retire on the serving HQ: %v", err)
	}
	if got := f.Presses(); len(got) != 0 {
		t.Fatalf("a press on the serving HQ sent %q", got)
	}

	f = &fakeHQ{public: &api.PublicAddress{Address: "hq.example.com:443", Members: 2}}
	out, _ = runAuthority(t, f, "no\n", "move", "laptop")
	if !strings.Contains(out, "2 member(s) connect through the public address hq.example.com:443: they are cut off after the move until it points at laptop") {
		t.Fatalf("a move's plan with a public address:\n%s", out)
	}
}
