package api

// Wire types of the HQ routes: where a team's home (its "HQ") lives, the one
// standby that can take over, and a move or takeover in progress. One file
// owns them; the routes are /v0/authority... (a daemon answers them, a
// client calls them). A move is started by the same command the CLI runs and
// read back from that command's log, so these types carry what the log says
// and the numbers the owner's screen shows, so the screen works nothing out.
//
// Conventions on these routes:
//   - A list is always an array (List), never null.
//   - An optional object is a pointer and is left out when there is none.
//   - A number or time that may be unknown is a pointer: left out means
//     unknown, never 0.
//   - A request names every field it needs; its Check says which is
//     missing. A route checks who called first and answers a missing field
//     as bad_request before anything acts on the request.
//   - A daemon refuses a request field it does not know, and anything after
//     the one JSON value (bad_request, api.DecodeOneJSON), so a field added
//     later is optional and left out when unset: a newer client then still
//     reaches an older daemon until it sets the new field.
//   - Free text (Reason.Text, PreflightCheck.Text and Fix, MoveStep.Detail,
//     HomeMove.Failure) is a fixed sentence the engine writes, with at most
//     machine names, times and numbers filled in; never an error's own
//     text, which may carry a URL, a path or a token.
//   - Codes (states, steps, reasons, checks) are the constants below. A
//     reader shows a code it does not know as its plain text: a newer daemon
//     may add one.

import (
	"encoding/json"
	"errors"
	"time"
)

// TermPath is the route of a home's term (AuthorityTerm), which every
// daemon answers to anyone, a parked one too, with no token on any
// transport: the home that named this one its successor reads it to decide
// whether to fence itself.
const TermPath = "/v0/authority/term"

// The HQ routes (kit/teamlink home_control.go). Every POST is an owner's
// press (ReasonNotOwner otherwise). The standby presses answer
// AuthorityStatus, the move MoveAnswer, and abort, resume, promote and
// retire the HomeMove they act on.
const (
	AuthorityPath      = "/v0/authority"                 // GET: AuthorityStatus, for any member
	CandidatesPath     = "/v0/authority/candidates"      // GET: CandidateList (owner)
	StandbyPath        = "/v0/authority/standby"         // POST StandbyRequest: AuthorityStatus
	StandbyReleasePath = "/v0/authority/standby/release" // POST StandbyRequest: AuthorityStatus
	MovePath           = "/v0/authority/move"            // POST MoveRequest: MoveAnswer
	MoveAbortPath      = "/v0/authority/move/abort"      // POST MoveControl: HomeMove
	MoveResumePath     = "/v0/authority/move/resume"     // POST MoveControl: HomeMove
	PromotePath        = "/v0/authority/promote"         // POST PromoteRequest: HomeMove
	RetirePath         = "/v0/authority/retire"          // POST RetireRequest: HomeMove
)

// HQ states: what a home's daemon may do (kit/hometerm's states).
const (
	HQServing = "serving"
	HQFrozen  = "frozen"  // stopped for a move; serves again if the move is called off
	HQFenced  = "fenced"  // another home serves; this one never serves again at this epoch
	HQStandby = "standby" // holds a copy; serves only once promoted
)

// Standby states: StandbyStatus.State.
const (
	StandbyNone        = "none"        // no standby; Machine is ""
	StandbyCopying     = "copying"     // designated, holds no whole copy yet (first copy, or a copy dropped)
	StandbyReady       = "ready"       // holds a copy and follows HQ's sets
	StandbyUnreachable = "unreachable" // HQ cannot reach it now
	StandbyUnknown     = "unknown"     // HQ is silent and the member answering could not ask the standby
)

// Standby setup steps: StandbySetup.Step, in order. HQ designates the machine
// only once SetupReady is reached; a failed setup leaves it as it was.
const (
	SetupAsking   = "asking"   // HQ asked the machine through its machine-ops queue
	SetupCopying  = "copying"  // restoring the first set
	SetupChecking = "checking" // comparing the restored copy with the set
	SetupReady    = "ready"
)

// Move kinds: HomeMove.Kind.
const (
	MoveKindSwap    = "swap"    // a planned move: nothing is lost
	MoveKindPromote = "promote" // a takeover by the standby: writes after its newest set are lost
	MoveKindRetire  = "retire"  // the fenced old HQ drops its copy
)

// Move states: HomeMove.State.
const (
	MoveRunning     = "running"
	MoveFailed      = "failed"       // a step before flip stopped; waits for resume (try again) or abort
	MoveHalfFlipped = "half_flipped" // stopped between the two halves of flip: no home serves until resume
	MoveAborted     = "aborted"      // called off by an owner before flip; the old HQ serves, nothing lost
	MoveRolledBack  = "rolled_back"  // called off by the move itself (a failed verify); the old HQ serves, nothing lost
	MoveDone        = "done"
)

// Move step ids: MoveStep.ID, in the order a swap runs them. A takeover or a
// retire runs only some of them. The move's end is MoveDone, not a step.
const (
	StepPreflight = "preflight"
	StepFreeze    = "freeze"
	StepFinalSet  = "final_set"
	StepRestore   = "restore"
	StepVerify    = "verify"
	StepFlip      = "flip"   // no abort from here on
	StepFollow    = "follow" // members reconnect; HomeMove.Follow counts them

	// StepFenceOld is a forced takeover's first step (PromoteRequest.Force):
	// the old HQ still answers, so the standby has it fence itself before
	// it serves.
	StepFenceOld = "fence_old"
)

// Move step states: MoveStep.State.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// What the standby saw of HQ while HQ does not answer a member:
// HQSilence.StandbyProbe.
const (
	ProbeAnswers = "answers" // HQ answers the standby: it is alive, use a swap, not a takeover
	ProbeSilent  = "silent"  // HQ answers neither the member nor the standby
	ProbeUnknown = "unknown" // the standby could not be asked
)

// Reason codes: Reason.Code, and the "reason" of an HQ route's refusal
// (HQRefusal).
const (
	// Why a machine cannot be the standby or take HQ now (candidates, the
	// standby's reasons, a move's stragglers).
	ReasonNotTrusted    = "not_trusted"    // its operator class is not trusted with secrets
	ReasonNeedsUpdate   = "needs_update"   // its build cannot follow HQ's sets
	ReasonOffline       = "offline"        // no heartbeat; Candidate.LastSeen says since when
	ReasonNoDisk        = "no_disk"        // not enough free room for a restored copy
	ReasonPublicAddress = "public_address" // it connects through the public address; HQ cannot reach it
	ReasonIsHQ          = "is_hq"
	ReasonIsStandby     = "is_standby"
	ReasonBehind        = "behind" // the standby is more than two minutes behind HQ

	// Why an HQ route refused a press.
	ReasonNoEngine = "no_engine" // this daemon's program has no move engine
	ReasonStale    = "stale"     // what the screen showed no longer holds (Shown, plan id, move id, machine)
	ReasonNotOwner = "not_owner" // the caller is not owner of every team project this home holds
	ReasonHQAlive  = "hq_alive"  // a takeover while HQ answers the standby, without Force
)

// Preflight check codes: PreflightCheck.Code.
const (
	CheckHQServing        = "hq_serving"        // HQ serves and no move is running
	CheckStandbyFollowing = "standby_following" // the target is the standby and follows HQ's sets
	CheckStandbyBuild     = "standby_build"     // the target's build can take HQ
	CheckStandbyDisk      = "standby_disk"      // the target has room for the final set
	CheckMemberBuilds     = "member_builds"     // no member active in the last 7 days runs a build that cannot follow
	CheckPublicAddress    = "public_address"    // no member active in the last 7 days connects through the public address
	CheckSecretsAcked     = "secrets_acked"     // the target acknowledged the secrets bundle of HQ's current hash
	CheckNoRestoreMarker  = "no_restore_marker" // the target has no restore half done (its restoring marker is gone)
)

// The vocabulary of each code field above, for a reader that checks a code
// against it (kit/teamlink answers a code an engine sends outside it as
// UnknownCode). A constant added above is added to its list here.
var (
	HQStates      = []string{HQServing, HQFrozen, HQFenced, HQStandby}
	StandbyStates = []string{StandbyNone, StandbyCopying, StandbyReady, StandbyUnreachable, StandbyUnknown}
	SetupSteps    = []string{SetupAsking, SetupCopying, SetupChecking, SetupReady}
	MoveKinds     = []string{MoveKindSwap, MoveKindPromote, MoveKindRetire}
	MoveStates    = []string{MoveRunning, MoveFailed, MoveHalfFlipped, MoveAborted, MoveRolledBack, MoveDone}
	StepIDs       = []string{StepPreflight, StepFreeze, StepFinalSet, StepRestore, StepVerify, StepFlip, StepFollow, StepFenceOld}
	StepStates    = []string{StepPending, StepRunning, StepDone, StepFailed, StepSkipped}
	Probes        = []string{ProbeAnswers, ProbeSilent, ProbeUnknown}
	Reasons       = []string{ReasonNotTrusted, ReasonNeedsUpdate, ReasonOffline, ReasonNoDisk, ReasonPublicAddress, ReasonIsHQ,
		ReasonIsStandby, ReasonBehind, ReasonNoEngine, ReasonStale, ReasonNotOwner, ReasonHQAlive}
	Checks = []string{CheckHQServing, CheckStandbyFollowing, CheckStandbyBuild, CheckStandbyDisk, CheckMemberBuilds,
		CheckPublicAddress, CheckSecretsAcked, CheckNoRestoreMarker}
)

// UnknownCode stands in an HQ answer for a code, id or name the daemon
// would not pass on as the engine sent it (kit/teamlink).
const UnknownCode = "unknown"

// Reason is one plain-sentence refusal or caveat. Code is one of the Reason
// constants, for a program to switch on; Text is for people.
type Reason struct {
	Code string `json:"code"`
	Text string `json:"text"` // a fixed phrase, never an error's text
}

// HQRefusal is the Data of an HQ route's error: the reason code under the
// error's code (CodeUnavailable for ReasonNoEngine, CodeConflict for
// ReasonStale, CodeForbidden for ReasonNotOwner, CodeConflict for
// ReasonHQAlive).
type HQRefusal struct {
	Reason string `json:"reason"`
}

// RefuseHQ is an HQ route's refusal: code is the error code, reason the
// Reason constant a program switches on.
func RefuseHQ(code, reason, format string, a ...any) *Error {
	return Errorf(code, format, a...).WithData(HQRefusal{Reason: reason})
}

// ErrNoEngine is the answer of every HQ action on a daemon whose program has
// no move engine; program names the program that has one.
func ErrNoEngine(program string) *Error {
	return RefuseHQ(CodeUnavailable, ReasonNoEngine, "moving HQ needs %s on this machine", program)
}

// HQRefusalReason is the "reason" in an *Error's data: an HQ route's
// refusal, or another refusal of the same shape (a limited token's,
// kit/core.LimitReason). "" when there is none.
func HQRefusalReason(err error) string {
	var e *Error
	if !errors.As(err, &e) || len(e.Data) == 0 {
		return ""
	}
	var r HQRefusal
	if json.Unmarshal(e.Data, &r) != nil {
		return ""
	}
	return r.Reason
}

// StandbySet is the newest of HQ's sets a standby holds restored.
type StandbySet struct {
	Stamp time.Time `json:"stamp"` // the set's stamp: the standby has everything up to it
	LagS  int64     `json:"lag_s"` // seconds between HQ's newest set and this one
}

// StandbyCopy is what a standby holds, as anyone may see it. Its build,
// free room and secrets hash are not here: those go to HQ in the standby's
// heartbeat, never on a route a stranger can call.
type StandbyCopy struct {
	// Set is left out until the first set is restored, or when the copy is
	// not known (StandbyUnknown).
	Set *StandbySet `json:"set,omitempty"`
	// HasSecrets: it holds HQ's secrets bundle. Left out when not known
	// (StandbyUnknown, StandbyUnreachable: HQ cannot ask it now), so false
	// always means "it holds none".
	HasSecrets *bool `json:"has_secrets,omitempty"`
}

// AuthorityTerm is GET /v0/authority/term, answered by every daemon, parked
// or not, to anyone: its state and epoch, the key the caller reached it by,
// the home it took over from, and the home it named to serve after it.
type AuthorityTerm struct {
	State string `json:"state"` // HQServing, HQFrozen, HQFenced or HQStandby
	Epoch uint64 `json:"epoch"`
	// Fingerprint is the pin of the key the caller reached this daemon by:
	// its tailnet node's or its tunnel key's. Left out on this machine's
	// socket, which no key guards.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Predecessor is the key of the home this one took over from, or
	// follows as a standby; left out when there is none. A home that named
	// this one its successor fences itself only when it is named here
	// (kit/server), so a successor that serves someone else's data at a
	// higher epoch fences nothing.
	Predecessor *HomeKey `json:"predecessor,omitempty"`
	// Successor is the key of the home this one named to serve after it;
	// left out when it names none (a standby never does). A fenced home
	// always names the one it fenced for, so a standby that waits for the
	// old home to fence (a forced takeover) can tell whether the fence is
	// its own; a fenced answer without it is an older Potluck's and says
	// nothing about whom it fenced for. A serving or frozen home names the
	// standby it would move to.
	Successor *HomeKey     `json:"successor,omitempty"`
	Standby   *StandbyCopy `json:"standby,omitempty"` // on a standby only
}

// HomeKey is a home's key as another home pinned it: the transport and the
// key's fingerprint, without the address.
type HomeKey struct {
	Via         string `json:"via"` // tailnet | tunnel
	Fingerprint string `json:"fingerprint"`
}

// StandbyStatus is the pair's standby as HQ sees it.
type StandbyStatus struct {
	Machine string `json:"machine"` // "" when State is StandbyNone
	State   string `json:"state"`   // StandbyNone ... StandbyUnknown
	StandbyCopy
	// Reasons say why it could not take HQ now (ReasonBehind,
	// ReasonNeedsUpdate, ReasonNoDisk): empty when nothing stops it. A swap
	// also needs HQ to answer and a takeover needs it silent; neither is a
	// reason here. Missing secrets are not a reason either: a takeover
	// without them is the owner's second tick (PromoteRequest.WithoutSecrets).
	Reasons List[Reason] `json:"reasons"`
}

// StandbySetup is a standby being set up, or the setup that failed last.
type StandbySetup struct {
	Machine string `json:"machine"`
	Step    string `json:"step"` // SetupAsking ... SetupReady
	// Failure is why it stopped at Step, left out while it runs. The
	// machine is as it was before.
	Failure *Reason `json:"failure,omitempty"`
}

// ServedBy is the daemon that answered: a member's own, HQ, or a parked
// machine (one that is not HQ any more, or not yet).
type ServedBy struct {
	Machine string `json:"machine"`
	State   string `json:"state"` // its own HQ state; parked when not HQServing
	// SuccessorURL is where HQ is now, from a fenced machine's term: the
	// screen's link off a parked machine; the address only, never a join
	// code or a token. "" otherwise.
	SuccessorURL string `json:"successor_url,omitempty"`
}

// HQCounts are HQ's own counts, for the status line.
type HQCounts struct {
	Projects  int `json:"projects"`  // team projects the home holds
	Machines  int `json:"machines"`  // team machines that follow it
	Connected int `json:"connected"` // of Machines, connected now
}

// HQSilence is set when HQ does not answer the member serving the status:
// what a takeover screen says.
type HQSilence struct {
	// Since is HQ's last answer to this member; left out if it never had one.
	Since        *time.Time `json:"since,omitempty"`
	StandbyProbe string     `json:"standby_probe"` // ProbeAnswers, ProbeSilent or ProbeUnknown
	// LossS estimates the seconds of writes a takeover loses: from the
	// standby's newest set to Since. Left out when either is unknown.
	LossS *int64 `json:"loss_s,omitempty"`
}

// FormerHQ is the machine that was HQ before a takeover.
type FormerHQ struct {
	Machine string `json:"machine"`
	// Fenced: it came back and answered HQ's probe parked and fenced, so it
	// can become the standby or drop its copy (retire).
	Fenced bool `json:"fenced"`
}

// MoveActor is who pressed the button, as the intent log records it (plan
// section 2.3: identity, machine, time), so every owner is told who moved HQ.
type MoveActor struct {
	Identity string    `json:"identity"`
	Machine  string    `json:"machine"`
	At       time.Time `json:"at"`
}

// MoveStep is one step of a move, as its log says.
type MoveStep struct {
	ID     string     `json:"id"`    // StepPreflight ... StepFollow
	State  string     `json:"state"` // StepPending ... StepSkipped
	Start  *time.Time `json:"start,omitempty"`
	End    *time.Time `json:"end,omitempty"`
	Detail string     `json:"detail,omitempty"` // a fixed phrase, set when it failed or was skipped
}

// Straggler is a member machine that has not reconnected after a flip.
type Straggler struct {
	Machine string `json:"machine"`
	Reason  Reason `json:"reason"` // ReasonOffline or ReasonNeedsUpdate
}

// FollowProgress counts the members reconnecting to the new HQ.
type FollowProgress struct {
	Reconnected int             `json:"reconnected"`
	Total       int             `json:"total"`
	Stragglers  List[Straggler] `json:"stragglers"`
}

// HomeMove is a swap, takeover or retire in progress, or the last one.
type HomeMove struct {
	ID    string         `json:"id"`
	Kind  string         `json:"kind"`  // MoveKindSwap ...
	State string         `json:"state"` // MoveRunning ...
	From  string         `json:"from"`  // machine that served
	To    string         `json:"to"`    // machine that serves after
	Steps List[MoveStep] `json:"steps"`
	// Follow is set from StepFollow on.
	Follow *FollowProgress `json:"follow,omitempty"`
	// RemainingS estimates the seconds until HQ serves again; left out
	// when unknown or once it serves.
	RemainingS *int64     `json:"remaining_s,omitempty"`
	By         MoveActor  `json:"by"`
	Started    time.Time  `json:"started"`
	Ended      *time.Time `json:"ended,omitempty"`
	// Failure is the fixed phrase of what stopped it (never an error's
	// text): set for MoveFailed, MoveHalfFlipped and MoveRolledBack.
	Failure string `json:"failure,omitempty"`
}

// AuthorityStatus is GET /v0/authority: HQ, its standby and any move, for
// any member. A member whose HQ does not answer fills it in from its stored
// successor and its own probe, and says so in Silent.
type AuthorityStatus struct {
	ServedBy  ServedBy `json:"served_by"`
	Machine   string   `json:"machine"` // the machine serving as HQ (or named by the member's last answer)
	State     string   `json:"state"`   // of that machine
	Epoch     uint64   `json:"epoch"`
	CanManage bool     `json:"can_manage"` // the caller may press the buttons: an owner
	// Counts are left out while Silent: only HQ knows them.
	Counts *HQCounts `json:"counts,omitempty"`
	// Silent: HQ did not answer; Machine, State and Epoch come from the
	// member's stored successor and its own probe, and Move is left out.
	Silent  *HQSilence    `json:"silent,omitempty"`
	Standby StandbyStatus `json:"standby"`
	// Setup is a standby being set up, or the last setup that failed.
	Setup    *StandbySetup `json:"setup,omitempty"`
	Move     *HomeMove     `json:"move,omitempty"` // running, or stopped and waiting
	LastMove *HomeMove     `json:"last_move,omitempty"`
	FormerHQ *FormerHQ     `json:"former_hq,omitempty"` // after a takeover, until it is the standby or retired
}

// StandbyPreview is the numbers of the "make this machine the standby"
// confirm that are the same for every candidate.
type StandbyPreview struct {
	Projects int          `json:"projects"` // team projects the standby will hold a copy of
	Secrets  List[string] `json:"secrets"`  // plain names of the secrets it will hold
	// FirstCopyS estimates the first copy from the last set; BehindS how
	// far behind it stays after that. Each is left out when unknown.
	FirstCopyS *int64 `json:"first_copy_s,omitempty"`
	BehindS    *int64 `json:"behind_s,omitempty"`
}

// Candidate is a team machine that could be the standby. It may be picked
// when Reasons is empty.
type Candidate struct {
	Machine  string       `json:"machine"`
	Online   bool         `json:"online"`
	LastSeen *time.Time   `json:"last_seen,omitempty"` // its last heartbeat; left out if never seen
	Reasons  List[Reason] `json:"reasons"`
	// SessionsRunning are the agent sessions it would stop to become the
	// standby.
	SessionsRunning int `json:"sessions_running"`
}

// CandidateList is GET /v0/authority/candidates (owner).
type CandidateList struct {
	Preview    StandbyPreview  `json:"preview"`
	Candidates List[Candidate] `json:"candidates"`
}

// Shown is what the owner's screen showed when they pressed: HQ's machine
// and epoch, both required (epoch 0 is a real epoch, so a missing one is
// nil). HQ refuses a press whose Shown no longer holds (CodeConflict,
// ReasonStale), so a screen that has gone stale moves nothing.
type Shown struct {
	From  string  `json:"from,omitempty"` // HQ's machine
	Epoch *uint64 `json:"epoch,omitempty"`
}

// NewShown is the Shown of a screen that showed HQ on from at epoch.
func NewShown(from string, epoch uint64) Shown { return Shown{From: from, Epoch: &epoch} }

// field is one required field of a request: its wire name and whether it
// is there.
type field struct {
	name    string
	present bool
}

// required is the bad_request of the first missing field, nil when none is.
func required(fs ...field) error {
	for _, f := range fs {
		if !f.present {
			return Errorf(CodeBadRequest, "%s is required", f.name)
		}
	}
	return nil
}

func (s Shown) fields() []field {
	return []field{{"from", s.From != ""}, {"epoch", s.Epoch != nil}}
}

// StandbyRequest is POST /v0/authority/standby (owner): ask Machine to become
// the standby; and POST /v0/authority/standby/release: stop using Machine as
// the standby (a POST, not a DELETE, so no proxy drops its body). The answer
// is the status after it.
type StandbyRequest struct {
	Machine string `json:"machine"`
	Shown
}

// Check requires Machine, From and Epoch.
func (r StandbyRequest) Check() error {
	return required(append([]field{{"machine", r.Machine != ""}}, r.fields()...)...)
}

// MoveRequest is POST /v0/authority/move (owner). CheckOnly answers the
// preflight and starts nothing; otherwise PlanID names the preflight the
// owner confirmed and HQ refuses (ReasonStale) when it, From or Epoch no
// longer holds.
type MoveRequest struct {
	To        string `json:"to"`
	CheckOnly bool   `json:"check_only,omitempty"`
	PlanID    string `json:"plan_id,omitempty"` // HomePreflight.ID
	Shown
}

// Check requires To; and unless CheckOnly, PlanID, From and Epoch.
func (r MoveRequest) Check() error {
	fs := []field{{"to", r.To != ""}}
	if !r.CheckOnly {
		fs = append(append(fs, field{"plan_id", r.PlanID != ""}), r.fields()...)
	}
	return required(fs...)
}

// MoveControl is POST /v0/authority/move/abort and .../resume (owner, at the
// parked old HQ): MoveID is the move the screen showed.
type MoveControl struct {
	MoveID string `json:"move_id"`
}

// Check requires MoveID.
func (r MoveControl) Check() error { return required(field{"move_id", r.MoveID != ""}) }

// RetireRequest is POST /v0/authority/retire (owner, at the fenced parked
// old HQ): Machine is the machine whose copy the owner chose to drop, and
// must be the one answering; Shown is the HQ that replaced it.
type RetireRequest struct {
	Machine string `json:"machine"`
	Shown
}

// Check requires Machine, From and Epoch.
func (r RetireRequest) Check() error {
	return required(append([]field{{"machine", r.Machine != ""}}, r.fields()...)...)
}

// PreflightCheck is one check of a planned move.
type PreflightCheck struct {
	Code string `json:"code"` // CheckHQServing ...
	OK   bool   `json:"ok"`
	Text string `json:"text"`          // a fixed phrase either way, never an error's text
	Fix  string `json:"fix,omitempty"` // what to do, when it failed; a fixed phrase
}

// PublicAddress is the members cut off after a move until the public
// address points at the new HQ.
type PublicAddress struct {
	Address string `json:"address"`
	Members int    `json:"members"`
}

// UnseenMember is a member machine not seen lately: it follows the new HQ
// when it comes back.
type UnseenMember struct {
	Machine  string     `json:"machine"`
	LastSeen *time.Time `json:"last_seen,omitempty"` // left out if never seen
}

// HomePreflight is what a move would check, and the numbers of its
// confirm. The move may start when every check is OK.
type HomePreflight struct {
	ID     string               `json:"id"` // the plan's id; MoveRequest.PlanID names it
	From   string               `json:"from"`
	To     string               `json:"to"`
	Checks List[PreflightCheck] `json:"checks"`
	// Projects are the names of the team projects that move.
	Projects List[string] `json:"projects"`
	// SessionsStopped are the agent sessions running on From that the
	// move stops.
	SessionsStopped int `json:"sessions_stopped"`
	// DownS estimates the seconds with no HQ from the last set; CopyS and
	// RestoreS are its parts for taking the final set and restoring it on
	// To. Each is left out when unknown.
	DownS    *int64 `json:"down_s,omitempty"`
	CopyS    *int64 `json:"copy_s,omitempty"`
	RestoreS *int64 `json:"restore_s,omitempty"`
	// PublicAddress is left out when no member connects through one.
	PublicAddress *PublicAddress     `json:"public_address,omitempty"`
	Unseen        List[UnseenMember] `json:"unseen"`
}

// MoveAnswer is the 200 answer of POST /v0/authority/move. Preflight is
// always there; Move only once the move has started. A CheckOnly call and a
// press whose preflight failed are 200 too, with Move left out: a failed
// check is an answer, not an error.
type MoveAnswer struct {
	Preflight HomePreflight `json:"preflight"`
	Move      *HomeMove     `json:"move,omitempty"`
}

// PromoteRequest is POST /v0/authority/promote (an owner's own press, on the
// standby).
type PromoteRequest struct {
	Machine string `json:"machine"` // the standby the owner saw: must be the one answering
	Shown
	// OldHomeIsDown is the "I checked it is down" tick: required unless
	// Force.
	OldHomeIsDown bool `json:"old_home_is_down,omitempty"`
	// WithoutSecrets takes over without the secrets bundle, when the
	// standby holds none.
	WithoutSecrets bool `json:"without_secrets,omitempty"`
	// Force takes over while HQ still answers the standby, fencing it
	// first (StepFenceOld). Only the CLI sets it (promote --force, for the
	// runbook); no screen has a button for it. It changes nothing about
	// who may promote: a human owner, as for every promote, never an agent
	// alone.
	Force bool `json:"force,omitempty"`
}

// Check requires Machine, From and Epoch, and without Force the
// OldHomeIsDown tick: a forced takeover is for an HQ that still answers,
// so it never claims HQ is down.
func (r PromoteRequest) Check() error {
	fs := append([]field{{"machine", r.Machine != ""}}, r.fields()...)
	if !r.Force {
		fs = append(fs, field{"old_home_is_down", r.OldHomeIsDown})
	}
	return required(fs...)
}
