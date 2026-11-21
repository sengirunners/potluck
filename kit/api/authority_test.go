package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/hometerm"
)

func ptr[T any](v T) *T { return &v }

// marshalIs fails unless v marshals to exactly want: the HQ screen (Z3,
// TypeScript) reads these keys, so a renamed or dropped key must fail here.
func marshalIs(t *testing.T, v any, want string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("marshalled\n%s\nwant\n%s", b, want)
	}
}

var at = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)

// Every code a reader switches on, as it is on the wire. The screen
// matches these strings; changing one is a wire change.
func TestAuthorityCodes(t *testing.T) {
	for got, want := range map[string]string{
		HQServing: "serving", HQFrozen: "frozen", HQFenced: "fenced", HQStandby: "standby",
		SetupAsking: "asking", SetupCopying: "copying", SetupChecking: "checking", SetupReady: "ready",
		MoveKindSwap: "swap", MoveKindPromote: "promote", MoveKindRetire: "retire",
		MoveRunning: "running", MoveFailed: "failed", MoveHalfFlipped: "half_flipped", MoveAborted: "aborted",
		MoveRolledBack: "rolled_back", MoveDone: "done",
		StepPreflight: "preflight", StepFreeze: "freeze", StepFinalSet: "final_set", StepRestore: "restore",
		StepVerify: "verify", StepFlip: "flip", StepFollow: "follow", StepFenceOld: "fence_old",
		ProbeAnswers: "answers", ProbeSilent: "silent",
		ReasonNotTrusted: "not_trusted", ReasonNeedsUpdate: "needs_update", ReasonOffline: "offline",
		ReasonNoDisk: "no_disk", ReasonIsHQ: "is_hq", ReasonIsStandby: "is_standby",
		ReasonBehind: "behind", ReasonNoEngine: "no_engine", ReasonStale: "stale", ReasonNotOwner: "not_owner",
		ReasonHQAlive:  "hq_alive",
		CheckHQServing: "hq_serving", CheckStandbyFollowing: "standby_following", CheckStandbyBuild: "standby_build",
		CheckStandbyDisk: "standby_disk", CheckMemberBuilds: "member_builds", CheckSecretsAcked: "secrets_acked",
		CheckNoRestoreMarker: "no_restore_marker",
	} {
		if got != want {
			t.Errorf("code %q, want %q", got, want)
		}
	}
	// Codes that share a string with another set (a map key holds one).
	for _, c := range []struct{ got, want string }{
		{StandbyNone, "none"}, {StandbyCopying, "copying"}, {StandbyReady, "ready"},
		{StandbyUnreachable, "unreachable"}, {StandbyUnknown, "unknown"},
		{StepPending, "pending"}, {StepRunning, "running"}, {StepDone, "done"}, {StepFailed, "failed"}, {StepSkipped, "skipped"},
		{ProbeUnknown, "unknown"}, {ReasonPublicAddress, "public_address"}, {CheckPublicAddress, "public_address"},
	} {
		if c.got != c.want {
			t.Errorf("code %q, want %q", c.got, c.want)
		}
	}
}

func TestAuthorityStatusWire(t *testing.T) {
	end := at.Add(40 * time.Second)
	full := AuthorityStatus{
		ServedBy: ServedBy{Machine: "mac", State: HQServing},
		Machine:  "echo", State: HQFrozen, Epoch: 2, CanManage: true,
		Counts:  &HQCounts{Projects: 4, Machines: 7, Connected: 5},
		Standby: StandbyStatus{Machine: "delta", State: StandbyReady, StandbyCopy: StandbyCopy{Set: &StandbySet{Stamp: at, LagS: 40}, HasSecrets: ptr(true)}},
		Setup:   &StandbySetup{Machine: "charlie", Step: SetupCopying, Failure: &Reason{Code: ReasonNoDisk, Text: "full"}},
		Move: &HomeMove{ID: "mv1", Kind: MoveKindSwap, State: MoveRunning, From: "echo", To: "delta",
			Steps: []MoveStep{
				{ID: StepFinalSet, State: StepDone, Start: &at, End: &end},
				{ID: StepFollow, State: StepRunning, Start: &end},
				{ID: StepFlip, State: StepPending},
			},
			Follow:     &FollowProgress{Reconnected: 5, Total: 7, Stragglers: []Straggler{{Machine: "clara-laptop", Reason: Reason{Code: ReasonOffline, Text: "offline"}}}},
			RemainingS: ptr(int64(0)),
			By:         MoveActor{Identity: "pat", Machine: "mac", At: at}, Started: at},
		LastMove: &HomeMove{ID: "mv0", Kind: MoveKindPromote, State: MoveHalfFlipped, From: "delta", To: "echo",
			By: MoveActor{Identity: "lee", Machine: "phone", At: at}, Started: at, Ended: &end, Failure: "crashed in flip"},
		FormerHQ: &FormerHQ{Machine: "charlie", Fenced: true},
	}
	marshalIs(t, full, `{"served_by":{"machine":"mac","state":"serving"},"machine":"echo","state":"frozen","epoch":2,"can_manage":true,`+
		`"counts":{"projects":4,"machines":7,"connected":5},`+
		`"standby":{"machine":"delta","state":"ready","set":{"stamp":"2026-11-21T03:00:00Z","lag_s":40},"has_secrets":true,"reasons":[]},`+
		`"setup":{"machine":"charlie","step":"copying","failure":{"code":"no_disk","text":"full"}},`+
		`"move":{"id":"mv1","kind":"swap","state":"running","from":"echo","to":"delta","steps":[`+
		`{"id":"final_set","state":"done","start":"2026-11-21T03:00:00Z","end":"2026-11-21T03:00:40Z"},`+
		`{"id":"follow","state":"running","start":"2026-11-21T03:00:40Z"},{"id":"flip","state":"pending"}],`+
		`"follow":{"reconnected":5,"total":7,"stragglers":[{"machine":"clara-laptop","reason":{"code":"offline","text":"offline"}}]},`+
		`"remaining_s":0,"by":{"identity":"pat","machine":"mac","at":"2026-11-21T03:00:00Z"},"started":"2026-11-21T03:00:00Z"},`+
		`"last_move":{"id":"mv0","kind":"promote","state":"half_flipped","from":"delta","to":"echo","steps":[],`+
		`"by":{"identity":"lee","machine":"phone","at":"2026-11-21T03:00:00Z"},"started":"2026-11-21T03:00:00Z",`+
		`"ended":"2026-11-21T03:00:40Z","failure":"crashed in flip"},`+
		`"former_hq":{"machine":"charlie","fenced":true}}`)

	// HQ silent and never seen by this member, the standby not askable: what
	// is unknown is a state or left out, never a zero.
	silent := AuthorityStatus{
		ServedBy: ServedBy{Machine: "charlie", State: HQFenced, SuccessorURL: "https://delta.example"},
		Machine:  "echo", State: HQServing, Epoch: 2,
		Silent:  &HQSilence{StandbyProbe: ProbeUnknown},
		Standby: StandbyStatus{Machine: "delta", State: StandbyUnknown},
		Setup:   &StandbySetup{Machine: "bravo", Step: SetupAsking},
	}
	marshalIs(t, silent, `{"served_by":{"machine":"charlie","state":"fenced","successor_url":"https://delta.example"},`+
		`"machine":"echo","state":"serving","epoch":2,"can_manage":false,`+
		`"silent":{"standby_probe":"unknown"},`+
		`"standby":{"machine":"delta","state":"unknown","reasons":[]},`+
		`"setup":{"machine":"bravo","step":"asking"}}`)
	marshalIs(t, HQSilence{Since: &at, StandbyProbe: ProbeAnswers, LossS: ptr(int64(40))},
		`{"since":"2026-11-21T03:00:00Z","standby_probe":"answers","loss_s":40}`)
	// No standby being set up and no setup that failed: setup is left out,
	// never null (the screen shows a setup panel for any setup it reads).
	marshalIs(t, AuthorityStatus{ServedBy: ServedBy{Machine: "echo", State: HQServing}, Machine: "echo", State: HQServing,
		Standby: StandbyStatus{State: StandbyNone}},
		`{"served_by":{"machine":"echo","state":"serving"},"machine":"echo","state":"serving","epoch":0,"can_manage":false,`+
			`"standby":{"machine":"","state":"none","reasons":[]}}`)
	marshalIs(t, StandbyStatus{State: StandbyNone}, `{"machine":"","state":"none","reasons":[]}`)
	// Known to hold no secrets is false, not left out.
	marshalIs(t, StandbyStatus{Machine: "delta", State: StandbyCopying, StandbyCopy: StandbyCopy{HasSecrets: ptr(false)}},
		`{"machine":"delta","state":"copying","has_secrets":false,"reasons":[]}`)
}

func TestAuthorityTermWire(t *testing.T) {
	// The public term of a standby that has restored nothing yet: no set
	// (not a zero stamp). It never carries the standby's build, free room
	// or secrets hash: StandbyCopy has no field for them.
	marshalIs(t, AuthorityTerm{State: HQStandby, Epoch: 4, Fingerprint: "fp", Standby: &StandbyCopy{HasSecrets: ptr(false)}},
		`{"state":"standby","epoch":4,"fingerprint":"fp","standby":{"has_secrets":false}}`)
	marshalIs(t, AuthorityTerm{State: HQStandby, Epoch: 4, Fingerprint: "fp", Standby: &StandbyCopy{
		Set: &StandbySet{Stamp: at, LagS: 7}, HasSecrets: ptr(true),
	}}, `{"state":"standby","epoch":4,"fingerprint":"fp","standby":{"set":{"stamp":"2026-11-21T03:00:00Z","lag_s":7},"has_secrets":true}}`)
	marshalIs(t, AuthorityTerm{State: HQServing, Fingerprint: "fp"}, `{"state":"serving","epoch":0,"fingerprint":"fp"}`)
	marshalIs(t, AuthorityTerm{State: HQFrozen, Epoch: 2}, `{"state":"frozen","epoch":2}`)
	marshalIs(t, AuthorityTerm{State: HQServing, Epoch: 3, Predecessor: &HomeKey{Via: "tunnel", Fingerprint: "old"}},
		`{"state":"serving","epoch":3,"predecessor":{"via":"tunnel","fingerprint":"old"}}`)
}

func TestCandidatesWire(t *testing.T) {
	marshalIs(t, CandidateList{}, `{"preview":{"projects":0,"secrets":[]},"candidates":[]}`)
	marshalIs(t, CandidateList{
		Preview: StandbyPreview{Projects: 4, Secrets: []string{"runner app key"}, FirstCopyS: ptr(int64(180)), BehindS: ptr(int64(0))},
		Candidates: []Candidate{
			{Machine: "delta", Online: true, LastSeen: &at, SessionsRunning: 2},
			{Machine: "charlie", Reasons: []Reason{{Code: ReasonOffline, Text: "offline since 03:00"}}},
		},
	}, `{"preview":{"projects":4,"secrets":["runner app key"],"first_copy_s":180,"behind_s":0},"candidates":[`+
		`{"machine":"delta","online":true,"last_seen":"2026-11-21T03:00:00Z","reasons":[],"sessions_running":2},`+
		`{"machine":"charlie","online":false,"reasons":[{"code":"offline","text":"offline since 03:00"}],"sessions_running":0}]}`)
}

func TestPreflightWire(t *testing.T) {
	// Estimates HQ cannot make yet are left out, not 0.
	marshalIs(t, MoveAnswer{Preflight: HomePreflight{ID: "p1", From: "echo", To: "delta"}},
		`{"preflight":{"id":"p1","from":"echo","to":"delta","checks":[],"projects":[],"sessions_stopped":0,"unseen":[]}}`)
	marshalIs(t, HomePreflight{ID: "p1", From: "echo", To: "delta",
		Checks: []PreflightCheck{
			{Code: CheckStandbyFollowing, OK: true, Text: "delta follows"},
			{Code: CheckPublicAddress, Text: "2 members use home.example", Fix: "point it at delta"},
		},
		Projects: []string{"oncall", "zm"}, SessionsStopped: 3, DownS: ptr(int64(180)), CopyS: ptr(int64(50)), RestoreS: ptr(int64(0)),
		PublicAddress: &PublicAddress{Address: "home.example", Members: 2},
		Unseen:        []UnseenMember{{Machine: "clara-laptop", LastSeen: &at}, {Machine: "new-box"}},
	}, `{"id":"p1","from":"echo","to":"delta","checks":[`+
		`{"code":"standby_following","ok":true,"text":"delta follows"},`+
		`{"code":"public_address","ok":false,"text":"2 members use home.example","fix":"point it at delta"}],`+
		`"projects":["oncall","zm"],"sessions_stopped":3,"down_s":180,"copy_s":50,"restore_s":0,`+
		`"public_address":{"address":"home.example","members":2},`+
		`"unseen":[{"machine":"clara-laptop","last_seen":"2026-11-21T03:00:00Z"},{"machine":"new-box"}]}`)
}

// Every press names what the screen showed, so HQ can refuse a stale one.
func TestAuthorityRequestsWire(t *testing.T) {
	shown := NewShown("echo", 0)
	marshalIs(t, StandbyRequest{Machine: "delta", Shown: shown}, `{"machine":"delta","from":"echo","epoch":0}`)
	marshalIs(t, MoveRequest{To: "delta", CheckOnly: true}, `{"to":"delta","check_only":true}`)
	marshalIs(t, MoveRequest{To: "delta", PlanID: "p1", Shown: shown}, `{"to":"delta","plan_id":"p1","from":"echo","epoch":0}`)
	marshalIs(t, MoveControl{MoveID: "mv1"}, `{"move_id":"mv1"}`)
	marshalIs(t, RetireRequest{Machine: "echo", Shown: NewShown("delta", 3)}, `{"machine":"echo","from":"delta","epoch":3}`)
	marshalIs(t, PromoteRequest{Machine: "delta", Shown: shown, OldHomeIsDown: true},
		`{"machine":"delta","from":"echo","epoch":0,"old_home_is_down":true}`)
	marshalIs(t, PromoteRequest{Machine: "delta", Shown: shown, WithoutSecrets: true, Force: true},
		`{"machine":"delta","from":"echo","epoch":0,"without_secrets":true,"force":true}`)

	var req PromoteRequest
	if err := json.Unmarshal([]byte(`{"machine":"delta","from":"echo","epoch":0,"old_home_is_down":true,"without_secrets":true,"force":true}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Machine != "delta" || req.From != "echo" || req.Epoch == nil || *req.Epoch != 0 || !req.OldHomeIsDown || !req.WithoutSecrets || !req.Force {
		t.Fatalf("%+v", req)
	}
}

// A press missing what the route needs is a bad_request naming the field;
// epoch 0 is an epoch, a missing epoch is not.
func TestAuthorityRequestsCheck(t *testing.T) {
	shown := NewShown("echo", 0)
	for _, tc := range []struct {
		name    string
		body    string
		decoded interface{ Check() error }
		missing string // "" = valid
	}{
		{"standby", `{"machine":"delta","from":"echo","epoch":0}`, &StandbyRequest{}, ""},
		{"standby no body", `{}`, &StandbyRequest{}, "machine"},
		{"standby no from", `{"machine":"delta","epoch":2}`, &StandbyRequest{}, "from"},
		{"standby no epoch", `{"machine":"delta","from":"echo"}`, &StandbyRequest{}, "epoch"},
		{"move check only", `{"to":"delta","check_only":true}`, &MoveRequest{}, ""},
		{"move no to", `{"check_only":true}`, &MoveRequest{}, "to"},
		{"move no plan", `{"to":"delta","from":"echo","epoch":2}`, &MoveRequest{}, "plan_id"},
		{"move no epoch", `{"to":"delta","plan_id":"p1","from":"echo"}`, &MoveRequest{}, "epoch"},
		{"move", `{"to":"delta","plan_id":"p1","from":"echo","epoch":2}`, &MoveRequest{}, ""},
		{"abort", `{"move_id":"mv1"}`, &MoveControl{}, ""},
		{"abort no move", `{}`, &MoveControl{}, "move_id"},
		{"retire", `{"machine":"echo","from":"delta","epoch":3}`, &RetireRequest{}, ""},
		{"retire no machine", `{"from":"delta","epoch":3}`, &RetireRequest{}, "machine"},
		{"retire no epoch", `{"machine":"echo","from":"delta"}`, &RetireRequest{}, "epoch"},
		{"promote", `{"machine":"delta","from":"echo","epoch":0,"old_home_is_down":true}`, &PromoteRequest{}, ""},
		{"promote no tick", `{"machine":"delta","from":"echo","epoch":0}`, &PromoteRequest{}, "old_home_is_down"},
		{"promote forced, no tick", `{"machine":"delta","from":"echo","epoch":0,"force":true}`, &PromoteRequest{}, ""},
		{"promote forced no epoch", `{"machine":"delta","from":"echo","force":true}`, &PromoteRequest{}, "epoch"},
		{"promote no from", `{"machine":"delta","epoch":0,"old_home_is_down":true}`, &PromoteRequest{}, "from"},
		{"promote no machine", `{"from":"echo","epoch":0,"old_home_is_down":true}`, &PromoteRequest{}, "machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tc.body), tc.decoded); err != nil {
				t.Fatal(err)
			}
			err := tc.decoded.Check()
			if tc.missing == "" {
				if err != nil {
					t.Fatalf("refused a whole request: %v", err)
				}
				return
			}
			e, ok := err.(*Error)
			if !ok || e.Code != CodeBadRequest || e.Message != tc.missing+" is required" {
				t.Fatalf("got %v, want %s is required", err, tc.missing)
			}
		})
	}
	if (StandbyRequest{Machine: "delta", Shown: shown}).Check() != nil {
		t.Fatal("NewShown's epoch 0 counted as missing")
	}
}

// A list is never null on the wire, and null read back is an empty list.
func TestListNeverNull(t *testing.T) {
	marshalIs(t, List[int](nil), `[]`)
	marshalIs(t, List[int]{1, 2}, `[1,2]`)
	var c CandidateList
	if err := json.Unmarshal([]byte(`{"candidates":null,"preview":{"secrets":null}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Candidates == nil || len(c.Candidates) != 0 || c.Preview.Secrets == nil {
		t.Fatalf("null decoded as %#v", c)
	}
	if err := json.Unmarshal([]byte(`{"candidates":[{"machine":"delta","reasons":[{"code":"is_hq","text":"is HQ"}]}]}`), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Candidates) != 1 || c.Candidates[0].Reasons[0].Code != ReasonIsHQ {
		t.Fatalf("%#v", c)
	}
}

func TestHQRefusals(t *testing.T) {
	e := ErrNoEngine("Example")
	if e.Code != CodeUnavailable || e.Status() != 503 || e.Message != "moving HQ needs Example on this machine" ||
		HQRefusalReason(e) != ReasonNoEngine {
		t.Fatalf("%+v", e)
	}
	marshalIs(t, ErrorResponse{Error: e}, `{"error":{"code":"unavailable","message":"moving HQ needs Example on this machine","data":{"reason":"no_engine"}}}`)
	stale := RefuseHQ(CodeConflict, ReasonStale, "HQ is %s now", "delta")
	if stale.Status() != 409 || HQRefusalReason(stale) != ReasonStale {
		t.Fatalf("%+v", stale)
	}
	if HQRefusalReason(Errorf(CodeConflict, "other")) != "" || HQRefusalReason(nil) != "" {
		t.Fatal("a refusal reason read from an error that has none")
	}
}

// The HQ states on the wire are exactly the term file's states.
func TestHQStatesAreTheTermStates(t *testing.T) {
	wire := []string{HQServing, HQFrozen, HQFenced, HQStandby}
	term := map[string]bool{}
	for _, s := range hometerm.States() {
		term[string(s)] = true
	}
	if len(term) != len(wire) {
		t.Errorf("term states %v, wire states %v", hometerm.States(), wire)
	}
	for _, s := range wire {
		if !term[s] {
			t.Errorf("wire state %q is no term state", s)
		}
	}
}
