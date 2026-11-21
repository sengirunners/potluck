package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// The HQ screen and the CLI both read these routes: each method must hit its
// route with its body and decode a daemon's literal answer into the fields
// the screen reads. The answers are written out as JSON, so a renamed key on
// either side fails here.
func TestAuthorityRoutes(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	end := at.Add(40 * time.Second)
	moveJSON := `{"id":"mv1","kind":"swap","state":"failed","from":"echo","to":"delta","steps":[` +
		`{"id":"freeze","state":"done","start":"2026-11-21T03:00:00Z","end":"2026-11-21T03:00:40Z"},` +
		`{"id":"verify","state":"failed","start":"2026-11-21T03:00:40Z","detail":"counts differ"}],` +
		`"follow":{"reconnected":5,"total":7,"stragglers":[{"machine":"clara-laptop","reason":{"code":"needs_update","text":"needs an update"}}]},` +
		`"remaining_s":90,"by":{"identity":"pat","machine":"mac","at":"2026-11-21T03:00:00Z"},"started":"2026-11-21T03:00:00Z",` +
		`"ended":"2026-11-21T03:00:40Z","failure":"counts differ"}`
	remaining := int64(90)
	move := api.HomeMove{
		ID: "mv1", Kind: api.MoveKindSwap, State: api.MoveFailed, From: "echo", To: "delta",
		Steps: api.List[api.MoveStep]{
			{ID: api.StepFreeze, State: api.StepDone, Start: &at, End: &end},
			{ID: api.StepVerify, State: api.StepFailed, Start: &end, Detail: "counts differ"},
		},
		Follow: &api.FollowProgress{Reconnected: 5, Total: 7, Stragglers: api.List[api.Straggler]{
			{Machine: "clara-laptop", Reason: api.Reason{Code: api.ReasonNeedsUpdate, Text: "needs an update"}}}},
		RemainingS: &remaining,
		By:         api.MoveActor{Identity: "pat", Machine: "mac", At: at},
		Started:    at, Ended: &end, Failure: "counts differ",
	}
	statusJSON := `{"served_by":{"machine":"charlie","state":"fenced","successor_url":"https://delta.example"},` +
		`"machine":"echo","state":"serving","epoch":2,"can_manage":true,"counts":{"projects":4,"machines":7,"connected":6},` +
		`"silent":{"since":"2026-11-21T03:00:00Z","standby_probe":"silent","loss_s":40},` +
		`"standby":{"machine":"delta","state":"ready","set":{"stamp":"2026-11-21T03:00:00Z","lag_s":40},"has_secrets":false,` +
		`"reasons":[{"code":"behind","text":"three minutes behind"}]},` +
		`"setup":{"machine":"bravo","step":"checking","failure":{"code":"no_disk","text":"not enough disk"}},` +
		`"move":` + moveJSON + `,"last_move":` + moveJSON + `,"former_hq":{"machine":"alpha","fenced":true}}`
	loss := int64(40)
	yes, no := true, false
	status := api.AuthorityStatus{
		ServedBy: api.ServedBy{Machine: "charlie", State: api.HQFenced, SuccessorURL: "https://delta.example"},
		Machine:  "echo", State: api.HQServing, Epoch: 2, CanManage: true,
		Counts: &api.HQCounts{Projects: 4, Machines: 7, Connected: 6},
		Silent: &api.HQSilence{Since: &at, StandbyProbe: api.ProbeSilent, LossS: &loss},
		Standby: api.StandbyStatus{Machine: "delta", State: api.StandbyReady,
			StandbyCopy: api.StandbyCopy{Set: &api.StandbySet{Stamp: at, LagS: 40}, HasSecrets: &no},
			Reasons:     api.List[api.Reason]{{Code: api.ReasonBehind, Text: "three minutes behind"}}},
		Setup:    &api.StandbySetup{Machine: "bravo", Step: api.SetupChecking, Failure: &api.Reason{Code: api.ReasonNoDisk, Text: "not enough disk"}},
		Move:     &move,
		LastMove: &move,
		FormerHQ: &api.FormerHQ{Machine: "alpha", Fenced: true},
	}
	shown := api.NewShown("echo", 2)
	est := func(v int64) *int64 { return &v }

	for _, tc := range []struct {
		route  string
		body   string // "" = none sent
		answer string
		want   any
		call   func(c *Client, ctx context.Context) (any, error)
	}{
		{"GET /v0/authority/term", "",
			`{"state":"standby","epoch":2,"fingerprint":"fp","standby":{"set":{"stamp":"2026-11-21T03:00:00Z","lag_s":5},"has_secrets":true}}`,
			api.AuthorityTerm{State: api.HQStandby, Epoch: 2, Fingerprint: "fp", Standby: &api.StandbyCopy{
				Set: &api.StandbySet{Stamp: at, LagS: 5}, HasSecrets: &yes}},
			func(c *Client, ctx context.Context) (any, error) { return c.AuthorityTerm(ctx) }},
		{"GET /v0/authority", "", statusJSON, status,
			func(c *Client, ctx context.Context) (any, error) { return c.Authority(ctx) }},
		{"GET /v0/authority/candidates", "",
			`{"preview":{"projects":4,"secrets":["runner app key","receipt key"],"first_copy_s":180,"behind_s":60},"candidates":[` +
				`{"machine":"delta","online":false,"last_seen":"2026-11-21T03:00:00Z",` +
				`"reasons":[{"code":"offline","text":"offline since 03:00"}],"sessions_running":2}]}`,
			api.CandidateList{
				Preview: api.StandbyPreview{Projects: 4, Secrets: api.List[string]{"runner app key", "receipt key"}, FirstCopyS: est(180), BehindS: est(60)},
				Candidates: api.List[api.Candidate]{{Machine: "delta", LastSeen: &at,
					Reasons:         api.List[api.Reason]{{Code: api.ReasonOffline, Text: "offline since 03:00"}},
					SessionsRunning: 2}}},
			func(c *Client, ctx context.Context) (any, error) { return c.AuthorityCandidates(ctx) }},
		{"POST /v0/authority/standby", `{"machine":"delta","from":"echo","epoch":2}`, statusJSON, status,
			func(c *Client, ctx context.Context) (any, error) {
				return c.SetStandby(ctx, api.StandbyRequest{Machine: "delta", Shown: shown})
			}},
		{"POST /v0/authority/standby/release", `{"machine":"delta","from":"echo","epoch":2}`, statusJSON, status,
			func(c *Client, ctx context.Context) (any, error) {
				return c.ReleaseStandby(ctx, api.StandbyRequest{Machine: "delta", Shown: shown})
			}},
		{"POST /v0/authority/move", `{"to":"delta","plan_id":"p1","from":"echo","epoch":2}`,
			`{"preflight":{"id":"p1","from":"echo","to":"delta",` +
				`"checks":[{"code":"member_builds","ok":false,"text":"clara-laptop cannot follow","fix":"update clara-laptop"}],` +
				`"projects":["oncall","zm"],"sessions_stopped":3,"down_s":180,"copy_s":50,"restore_s":70,` +
				`"public_address":{"address":"home.example","members":2},"unseen":[{"machine":"clara-laptop","last_seen":"2026-11-21T03:00:00Z"}]},` +
				`"move":` + moveJSON + `}`,
			api.MoveAnswer{Preflight: api.HomePreflight{ID: "p1", From: "echo", To: "delta",
				Checks:          api.List[api.PreflightCheck]{{Code: api.CheckMemberBuilds, Text: "clara-laptop cannot follow", Fix: "update clara-laptop"}},
				Projects:        api.List[string]{"oncall", "zm"},
				SessionsStopped: 3, DownS: est(180), CopyS: est(50), RestoreS: est(70),
				PublicAddress: &api.PublicAddress{Address: "home.example", Members: 2},
				Unseen:        api.List[api.UnseenMember]{{Machine: "clara-laptop", LastSeen: &at}}},
				Move: &move},
			func(c *Client, ctx context.Context) (any, error) {
				return c.MoveHQ(ctx, api.MoveRequest{To: "delta", PlanID: "p1", Shown: shown})
			}},
		{"POST /v0/authority/move/abort", `{"move_id":"mv1"}`, moveJSON, move,
			func(c *Client, ctx context.Context) (any, error) {
				return c.AbortMove(ctx, api.MoveControl{MoveID: "mv1"})
			}},
		{"POST /v0/authority/move/resume", `{"move_id":"mv1"}`, moveJSON, move,
			func(c *Client, ctx context.Context) (any, error) {
				return c.ResumeMove(ctx, api.MoveControl{MoveID: "mv1"})
			}},
		{"POST /v0/authority/promote", `{"machine":"delta","from":"echo","epoch":2,"old_home_is_down":true,"without_secrets":true}`, moveJSON, move,
			func(c *Client, ctx context.Context) (any, error) {
				return c.PromoteStandby(ctx, api.PromoteRequest{Machine: "delta", Shown: shown, OldHomeIsDown: true, WithoutSecrets: true})
			}},
		{"POST /v0/authority/retire", `{"machine":"echo","from":"delta","epoch":3}`, moveJSON, move,
			func(c *Client, ctx context.Context) (any, error) {
				return c.RetireHQ(ctx, api.RetireRequest{Machine: "echo", Shown: api.NewShown("delta", 3)})
			}},
	} {
		t.Run(tc.route, func(t *testing.T) {
			var method, path string
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
				method, path = r.Method, r.URL.Path
				body, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.answer)
			}))
			defer srv.Close()
			got, err := tc.call(NewRemote(srv.URL, nil), context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if method+" "+path != tc.route {
				t.Fatalf("called %s %s", method, path)
			}
			if string(body) != tc.body {
				t.Fatalf("sent body %q, want %q", body, tc.body)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decoded\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A refusal comes back as *api.Error with its status and reason, and a 202
// queued answer (a member queuing a press) is an error, never a success.
func TestAuthorityRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		answer     string
		code       string
		reason     string
		httpStatus int
	}{
		{"not owner", http.StatusForbidden,
			`{"error":{"code":"forbidden","message":"zm is owned by lee","data":{"reason":"not_owner"}}}`,
			api.CodeForbidden, api.ReasonNotOwner, 403},
		{"stale screen", http.StatusConflict,
			`{"error":{"code":"conflict","message":"HQ is delta now","data":{"reason":"stale"}}}`,
			api.CodeConflict, api.ReasonStale, 409},
		{"no engine", http.StatusServiceUnavailable,
			`{"error":{"code":"unavailable","message":"moving HQ needs Example on this machine","data":{"reason":"no_engine"}}}`,
			api.CodeUnavailable, api.ReasonNoEngine, 503},
		{"queued by a member", http.StatusAccepted,
			`{"queued":true,"write":"w1","project":"zm","note":"HQ is away"}`,
			api.CodeQueued, "", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.answer)
			}))
			defer srv.Close()
			_, err := NewRemote(srv.URL, nil).MoveHQ(context.Background(), api.MoveRequest{To: "delta", PlanID: "p1", Shown: api.NewShown("echo", 2)})
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Code != tc.code || ae.HTTPStatus != tc.httpStatus || api.HQRefusalReason(err) != tc.reason {
				t.Fatalf("got %#v", err)
			}
		})
	}
}

// A press missing a required field is refused before it is sent, on every
// method that sends one.
func TestAuthorityIncompletePressNotSent(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent = true }))
	defer srv.Close()
	c := NewRemote(srv.URL, nil)
	ctx := context.Background()
	noEpoch := api.Shown{From: "echo"}
	for name, call := range map[string]func() error{
		"standby": func() error {
			_, err := c.SetStandby(ctx, api.StandbyRequest{Machine: "delta", Shown: noEpoch})
			return err
		},
		"release": func() error {
			_, err := c.ReleaseStandby(ctx, api.StandbyRequest{Shown: api.NewShown("echo", 2)})
			return err
		},
		"move": func() error {
			_, err := c.MoveHQ(ctx, api.MoveRequest{To: "delta", Shown: api.NewShown("echo", 2)})
			return err
		},
		"abort":  func() error { _, err := c.AbortMove(ctx, api.MoveControl{}); return err },
		"resume": func() error { _, err := c.ResumeMove(ctx, api.MoveControl{}); return err },
		"promote": func() error {
			_, err := c.PromoteStandby(ctx, api.PromoteRequest{Machine: "delta", Shown: api.NewShown("echo", 2)})
			return err
		},
		"retire": func() error {
			_, err := c.RetireHQ(ctx, api.RetireRequest{Machine: "echo", Shown: noEpoch})
			return err
		},
	} {
		sent = false
		if err := call(); !api.IsCode(err, api.CodeBadRequest) || sent {
			t.Errorf("%s: got %v, sent %v", name, err, sent)
		}
	}
}
