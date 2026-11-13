package protocol

import (
	"errors"
	"regexp"
	"strings"
)

// A runner box whose job gets secrets from the door (docs/daemon.md, "Job
// grants") is guarded to one run: the job-started hook stops a job unless
// its run, workflow and ref are the run's, then holds it until the door has
// delivered (the ready marker), so no step runs before the secrets are there
// and no other job ever runs a step in a box that holds them.
const (
	// CapJITGuardRun: the driver's job-started hook takes jit_guard.run.
	// A driver that lists it lists box-secrets too.
	CapJITGuardRun = "github-jit-guard-run"
	// SecretNameReady is the door's ready marker, a box secret whose value
	// is the run's id (decimal), written once every secret of the job is in
	// the box. The authority reserves the name: no holder sets it.
	SecretNameReady = "POTLUCK_READY"
	// JITHoldDefault and JITHoldMax bound how long the hook holds a job for
	// its marker (seconds).
	JITHoldDefault = 120
	JITHoldMax     = 600
)

// JITRun narrows a jit_guard to one GitHub Actions run, as GitHub's API
// answered it: the hook compares GITHUB_RUN_ID, GITHUB_WORKFLOW_REF and
// GITHUB_REF with these and holds the job for the ready marker.
type JITRun struct {
	ID          int64  `json:"id"`               // GITHUB_RUN_ID
	WorkflowRef string `json:"workflow_ref"`     // GITHUB_WORKFLOW_REF: owner/name/.github/workflows/<file>@<ref>
	Ref         string `json:"ref"`              // GITHUB_REF: refs/heads/<branch>
	HoldS       int    `json:"hold_s,omitempty"` // 0: JITHoldDefault
}

var (
	jitRefRE         = regexp.MustCompile(`^refs/heads/[A-Za-z0-9._/-]{1,200}$`)
	jitWorkflowRefRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}/[A-Za-z0-9._-]{1,100}/\.github/workflows/[A-Za-z0-9._-]{1,200}\.ya?ml@refs/heads/[A-Za-z0-9._/-]{1,200}$`)
)

// ErrJITRun is every refusal of a jit_guard.run: it names no value.
var ErrJITRun = errors.New("jit_guard.run is a run id, a workflow ref of the guard's repository at the run's ref, a branch ref and a hold of at most 600 s")

// Check refuses r unless it is a run of repo (owner/name): a positive id, a
// branch ref, a workflow ref under repo's .github/workflows/ at that ref, a
// hold in 0..JITHoldMax. The characters allowed are few enough that the
// values go into the hook as they are.
func (r JITRun) Check(repo string) error {
	switch {
	case r.ID <= 0, r.HoldS < 0, r.HoldS > JITHoldMax:
		return ErrJITRun
	case !jitRefRE.MatchString(r.Ref), !jitWorkflowRefRE.MatchString(r.WorkflowRef):
		return ErrJITRun
	case strings.Contains(r.Ref, ".."), strings.Contains(r.WorkflowRef, ".."), strings.Contains(r.Ref, "//"):
		return ErrJITRun
	case !strings.HasSuffix(r.WorkflowRef, "@"+r.Ref):
		return ErrJITRun
	case !strings.HasPrefix(strings.ToLower(r.WorkflowRef), strings.ToLower(repo)+"/.github/workflows/"):
		return ErrJITRun
	}
	return nil
}

// Hold is how long the hook waits for the ready marker, in seconds.
func (r JITRun) Hold() int {
	if r.HoldS == 0 {
		return JITHoldDefault
	}
	return r.HoldS
}
