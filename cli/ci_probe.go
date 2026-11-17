package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/internal/workflowlint"
	"github.com/sengirunners/potluck/runners"
)

// probeCheck is check 18 of potluck ci doctor.
const probeCheck = 18

// probeWaitSeconds is how long the probe script waits for its job to finish before it gives up.
const probeWaitSeconds = 20 * 60

// probeLabels is what potluck ci doctor --probe asks for, as potluck ci pr's --labels, --size and --os.
type probeLabels struct{ labels, size, osName string }

// flags is the --labels, --size and --os that ask for these labels again, quoted for a shell; with labels false
// it leaves --labels out, for a fix that asks for other labels.
func (p probeLabels) flags(labels bool) string {
	var out string
	if labels && p.labels != "" {
		out += " --labels " + shellQuote(p.labels)
	}
	if p.size != "" {
		out += " --size " + shellQuote(p.size)
	}
	if p.osName != "" && p.osName != "macos" {
		out += " --os " + shellQuote(p.osName)
	}
	return out
}

// ciProbe prepares check 18: the probe script for repo, written but not run, or why it was not written. The
// GitHub App cannot write to a repository, so the script pushes the probe with the person's own gh sign-in, as
// potluck ci pr's does. A public repository needs public, the owner's yes, given on the command line.
//
// notPrepared, when not empty, is why the probe must not be prepared now: the checks before it failed, or did not
// answer at all.
func (a *app) ciProbe(ctx context.Context, repo string, want probeLabels, public bool, notPrepared string) (runners.DoctorCheck, string) {
	check := func(status, message, fix string) runners.DoctorCheck {
		return runners.DoctorCheck{Number: probeCheck, Status: status, Message: message, Fix: fix}
	}
	rerun := a.cmd("ci doctor") + " " + shellQuote(repo) + " --probe" + want.flags(true)
	// The placeholder is quoted: pasted bare, its < and > would be shell redirects.
	relabel := a.cmd("ci doctor") + " " + shellQuote(repo) + " --probe" + want.flags(false) + " --labels " + shellQuote("<labels>")
	if public {
		rerun += " --public"
		relabel += " --public"
	}
	if notPrepared != "" {
		return check("warn", "Probe not prepared: "+notPrepared, "fix that, then "+rerun), ""
	}
	var info struct {
		Private     bool   `json:"private"`
		Visibility  string `json:"visibility"`
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := ghGet(ctx, "repos/"+repo, &info); err != nil {
		if errors.Is(err, errGHNotFound) {
			return check("fail", "Your gh sign-in cannot see "+repo+" (GitHub answers not found when the repository does not exist, or is private and not visible to that sign-in), so it cannot push the probe's branch.", "sign in to gh as someone with write access (gh auth status)"), ""
		}
		return check("warn", "unavailable: the probe needs your gh sign-in to read the repository: "+err.Error(), "gh auth login, then "+rerun), ""
	}
	// The broker's rule (ghapp.Public): an internal repository reads as private, but every member of its
	// enterprise can see the branch and the job.
	if ghapp.Public(info.Private, info.Visibility) && !public {
		kind, who := ghapp.Audience(info.Private, info.Visibility)
		return check("fail", repo+" is "+kind+": the probe pushes a branch and runs a job there that "+who+" can see, so it needs the repository owner's yes first.",
			"ask the owner; with their yes: "+rerun+" --public"), ""
	}
	if !info.Permissions.Push {
		return check("fail", "Your gh sign-in cannot push to "+repo+", so it cannot push the probe's branch.", "sign in to gh as someone with write access (gh auth status)"), ""
	}
	labels, err := a.ciPRLabels(ctx, want.labels, want.size, want.osName)
	if err != nil {
		return check("fail", "The probe has no labels to ask for: "+err.Error(), relabel), ""
	}
	// Labels the person gave must still name this machine's pool: otherwise a GitHub-hosted runner, or another
	// fleet, takes the job and the probe proves nothing about the lent Mac.
	if want.labels != "" {
		st, err := a.poolSetup(ctx)
		if err != nil {
			return check("warn", "unavailable: this machine's pool could not be read: "+err.Error(), rerun), ""
		}
		if ask := runners.ParseLabels(labels, st); !ask.Ours || ask.Why != "" {
			why := cmp.Or(ask.Why, "none of them is the pool's label "+shellQuote(st.Label))
			return check("fail", "The pool would not take a job asking for "+strings.Join(labels, ", ")+": "+why+". A machine outside the pool could take the probe's job and prove nothing about the lent Mac.", relabel), ""
		}
	}
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return check("warn", "unavailable: "+err.Error(), rerun), ""
	}
	branch := "potluck/probe-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:])
	text, err := runners.ProbeWorkflow(branch, labels)
	if err != nil {
		return check("fail", "The probe workflow could not be written: "+err.Error(), relabel), ""
	}
	if p := workflowlint.Lint(string(text)); len(p) > 0 {
		return check("fail", "The fork-safe lint refuses the generated probe workflow: "+strings.Join(p, "; "), "report this: the generator and the lint disagree"), ""
	}
	script, err := writeProbeScript(repo, branch, labels, text, prefix(a.host.Prefix))
	if err != nil {
		return check("warn", "unavailable: the probe script could not be written: "+err.Error(), rerun), ""
	}
	return check("warn", fmt.Sprintf("Probe ready, not run: it pushes branch %s holding only a generated workflow, waits up to %d minutes for its one job on %s, prints the machine's OS, Xcode and free disk, and deletes the branch.",
		branch, probeWaitSeconds/60, strings.Join(labels, ", ")), "sh "+shellQuote(script)), script
}

// writeProbeScript writes, in a new temporary directory, the probe's workflow and git tree and a script that
// pushes them as branch (one commit with no parent, holding only the workflow, so the push starts no other
// workflow), waits for the run, prints what the job reported and deletes the branch, also when it fails or is
// stopped. It returns the script's path.
func writeProbeScript(repo, branch string, labels []string, workflow []byte, p prefix) (string, error) {
	tree, err := json.Marshal(map[string]any{"tree": []map[string]string{{
		"path": runners.ProbeWorkflowPath, "mode": "100644", "type": "blob", "content": string(workflow),
	}}})
	if err != nil {
		return "", err
	}
	marker := strings.TrimSpace(runners.ProbeMarker)
	return writeGHScript("potluck-ci-probe-", "probe.sh", map[string][]byte{"tree.json": tree, "potluck-probe.yml": workflow}, func(q func(string) string) string {
		return fmt.Sprintf(`#!/bin/sh
# The probe %[11]s --probe prepared (written %[1]s) for %[2]s: it pushes branch
# %[3]s holding only the workflow %[4]s, waits for its job on %[5]s,
# prints what the machine reported, and deletes the branch, also when it fails or is stopped.
# Needs gh signed in with the workflow scope (gh auth refresh -s workflow).
set -eu
repo=%[2]s
branch=%[3]s
wait_s=${POTLUCK_PROBE_WAIT:-%[6]d}
poll=${POTLUCK_PROBE_POLL:-10}
# A wait that is no whole number (or too long for sh's arithmetic) never ends; a poll under a second hammers GitHub.
case $wait_s in ''|*[!0-9]*|??????????*) echo "fail: POTLUCK_PROBE_WAIT must be a whole number of seconds (at most 9 digits), not '$wait_s'." >&2; exit 2 ;; esac
case $poll in ''|*[!0-9]*|??????????*) echo "fail: POTLUCK_PROBE_POLL must be a whole number of seconds (at most 9 digits), not '$poll'." >&2; exit 2 ;; esac
if [ "$poll" -lt 1 ]; then echo "fail: POTLUCK_PROBE_POLL must be at least 1 second." >&2; exit 2; fi
run=""
pushed=""
refused=""
finished=""
cleanup() {
  status=$?
  # One failing command here must not skip the branch's delete.
  set +e
  trap - EXIT
  trap '' INT TERM HUP
  # GitHub may make the run a few seconds after the push: look for it a few times before giving up on it.
  tries=0
  while [ -n "$pushed" ] && [ -z "$refused" ] && [ -z "$finished" ] && [ -z "$run" ] && [ "$tries" -lt 3 ]; do
    run=$(gh run list -R "$repo" --branch "$branch" --event push --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null) || run=""
    tries=$((tries + 1))
    [ -n "$run" ] || [ "$tries" -ge 3 ] || sleep "$poll"
  done
  if [ -n "$run" ] && [ -z "$finished" ] && ! gh run cancel "$run" -R "$repo" >/dev/null 2>&1; then
    # A run that finished meanwhile cannot be cancelled and needs no cancel.
    case $(gh run view "$run" -R "$repo" --json status --jq .status 2>/dev/null || true) in
    completed) ;;
    *) echo "Could not cancel run $run; it may still be waiting for a machine: gh run cancel $run -R $repo" >&2 || true; status=1 ;;
    esac
  fi
  if [ -n "$pushed" ]; then
    if gh api --method DELETE "repos/$repo/git/refs/heads/$branch" >/dev/null 2>&1; then
      echo "Deleted branch $branch." || true
    else
      # Only GitHub's 404 says the branch is gone (it was never made); any other answer may leave it behind.
      case $(gh api "repos/$repo/git/ref/heads/$branch" 2>&1 >/dev/null) in
      *"HTTP 404"*) ;;
      *) echo "Could not delete branch $branch: gh api --method DELETE repos/$repo/git/refs/heads/$branch" >&2 || true; status=1 ;;
      esac
    fi
  fi
  rm -rf %[9]s
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
tree=$(gh api --method POST "repos/$repo/git/trees" --input %[7]s --jq .sha)
# The body is JSON on stdin, so an empty parents list needs no recent gh (-f 'parents[]' does).
commit=$(printf '{"message":"%[11]s --probe","tree":"%%s","parents":[]}' "$tree" | gh api --method POST "repos/$repo/git/commits" --input - --jq .sha)
# Set first: a stop while the branch is being made still deletes it (a branch that was never made is no error).
pushed=1
if ! why=$(gh api --method POST "repos/$repo/git/refs" -f ref="refs/heads/$branch" -f sha="$commit" 2>&1 >/dev/null); then
  # gh's own error says why. Only GitHub's 404 on the branch says it was never made, and a request that timed out
  # may still be saving it: the branch is read three times, a poll apart, and only three 404s say it was not made.
  # If it was made, cleanup still finds the run and deletes it.
  gone=1
  for try in 1 2 3; do
    case $(gh api "repos/$repo/git/ref/heads/$branch" 2>&1 >/dev/null) in
    *"HTTP 404"*) ;;
    *) gone=""; break ;;
    esac
    [ "$try" -eq 3 ] || sleep "$poll"
  done
  # Only now: a stop during the reads still has cleanup look for the run.
  refused=$gone
  echo "fail: GitHub did not accept the probe's branch: $why" >&2
  echo "A token without gh's workflow scope is refused like this (gh auth refresh -s workflow); so is a ruleset that blocks the branch name or a GitHub outage. This script removes its files when it ends: run %[11]s %[10]s --probe again for a new one." >&2
  exit 1
fi
echo "Pushed branch $branch; waiting for its job."
start=$(date +%%s)
state=""
while :; do
  if [ -z "$run" ]; then
    # A failed read (a network error, say) is tried again next time round.
    run=$(gh run list -R "$repo" --branch "$branch" --event push --json databaseId --jq '.[0].databaseId // empty') || run=""
  fi
  if [ -n "$run" ]; then
    state=$(gh run view "$run" -R "$repo" --json status,conclusion --jq '.status + " " + .conclusion') || state=""
    case $state in completed*) break ;; esac
  fi
  if [ $(( $(date +%%s) - start )) -ge "$wait_s" ]; then
    if [ -z "$run" ]; then
      echo "fail: GitHub started no run for the probe in $wait_s seconds; check that Actions is on for $repo." >&2
    else
      echo "fail: the probe job waited $wait_s seconds and no machine finished it ($state); run %[11]s $repo and %[12]s." >&2
    fi
    exit 1
  fi
  sleep "$poll"
done
finished=1
conclusion=${state#completed }
# The log can lag the run's end by a few seconds.
report=""
for try in 1 2 3 4 5; do
  report=$(gh run view "$run" -R "$repo" --log 2>/dev/null | sed -n 's/.*%[8]s /  /p') || report=""
  [ -z "$report" ] || break
  [ "$try" -eq 5 ] || sleep "$poll"
done
if [ -n "$report" ]; then
  printf '%%s\n' "$report"
else
  echo "The job's log did not come back, so what the machine reported is not shown: gh run view $run -R $repo --log"
fi
if [ "$conclusion" != success ]; then
  echo "fail: the probe job ended $conclusion: gh run view $run -R $repo --log" >&2
  exit 1
fi
echo "pass: a machine of this pool took the probe job and finished it."
`, time.Now().UTC().Format(time.RFC3339), shellQuote(repo), shellQuote(branch), q("potluck-probe.yml"), strings.Join(labels, ", "),
			probeWaitSeconds, q("tree.json"), marker, q(""), shellQuote(repo), p.cmd("ci doctor"), p.cmd("runners jobs"))
	})
}
