package cli

import "fmt"

// What potluck ci pr ended in (ciPRResult.State).
const (
	ciPRDone   = "done"    // the job already has this change: nothing to do
	ciPRMerge  = "merge"   // a pull request with this change is open: merge URL
	ciPRScript = "script"  // the change is prepared: run Script, with your gh, to open the pull request
	ciPRDryRun = "dry_run" // --dry-run: the diff only; Next is the command to run to prepare it
)

// ciPRResult is potluck ci pr's answer with --json: where the command ended, so a caller reads fields and parses no
// text. Which of URL, Script and Next are set follows State.
type ciPRResult struct {
	State    string   `json:"state"`
	Repo     string   `json:"repo"`
	Job      string   `json:"job,omitempty"`      // the job's id, named or picked
	Workflow string   `json:"workflow,omitempty"` // the workflow file that has it
	Labels   []string `json:"labels"`             // what own branches ask for
	URL      string   `json:"url,omitempty"`      // merge: the open pull request
	Script   string   `json:"script,omitempty"`   // script: the file that opens the pull request
	Next     string   `json:"next,omitempty"`     // the command line that takes the next step
	Diff     string   `json:"diff,omitempty"`     // the change to the workflow, unified
	Notes    []string `json:"notes,omitempty"`    // what the command noticed on the way (files skipped, lint problems the file had)
}

// note records a notice, and says it when the caller does not print JSON.
func (r *ciPRResult) note(say func(string, ...any), format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
	say(format, args...)
}
