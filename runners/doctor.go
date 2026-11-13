package runners

// The CI doctor (potluck ci doctor, docs/ci-doctor.md): read-only checks
// that say why a repository's jobs are not running here. Every check asks
// the broker's own rules (admit, trustReason, forkPRs, bindingFits,
// approvalWarning, workflowLimit); none keeps a copy of one.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// DoctorCheck has a stable number from the setup plan and one next action.
// A warn whose message starts with unavailable is missing evidence, never a pass.
type DoctorCheck struct {
	Number  int    `json:"number"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// DoctorReport is one repository's 17 checks, and check 18 (the probe) when
// potluck ci doctor --probe prepares one.
type DoctorReport struct {
	Repo         string        `json:"repo"`
	Installation int64         `json:"installation,omitempty"`
	CheckedAt    time.Time     `json:"checked_at"`
	Checks       []DoctorCheck `json:"checks"`
}

// DoctorResult is GET /v0/runners/doctor's answer: the reports, warnings
// about the run, and the repositories its gh budget did not reach.
type DoctorResult struct {
	Reports  []DoctorReport `json:"reports"`
	Warnings []string       `json:"warnings,omitempty"`
	Skipped  []string       `json:"skipped,omitempty"`
}

// Failed reports whether any check failed.
func (r DoctorReport) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == "fail" {
			return true
		}
	}
	return false
}

// set records check n; a failure already recorded stays.
func (r *DoctorReport) set(n int, status, message, fix string) {
	if r.Checks[n-1].Status == "fail" && status != "fail" {
		return
	}
	r.Checks[n-1] = DoctorCheck{n, status, message, fix}
}

func (r *DoctorReport) unavailable(n int, what, bead string) {
	r.set(n, "warn", "unavailable: "+what+", see "+bead, "follow "+bead)
}

// doctorHTTP is GET /v0/runners/doctor[?repo=owner/name] (local). Without a
// repository it checks the connected ones and those in the job log, in
// order, until the gh call budget is spent.
func (s *Service) doctorHTTP(r *http.Request) (any, error) {
	ctx := doctorContext(r.Context())
	repo := r.URL.Query().Get("repo")
	if repo != "" && !jobrule.ValidRepo(repo) {
		return nil, api.Errorf(api.CodeBadRequest, "repository must be owner/repo")
	}
	repos := []string{repo}
	warnings := s.notices()
	if repo == "" {
		var err error
		if repos, err = s.doctorInventory(ctx); err != nil {
			return nil, err
		}
		if len(repos) == 0 {
			warnings = append(warnings, "no connected repositories: "+s.cmd("runners allow owner/name")+" connects one; "+s.cmd("ci doctor owner/name")+" checks any repository")
		}
	}
	reports := make([]DoctorReport, 0, len(repos))
	var skipped []string
	for i, repo := range repos {
		if doctorBudgetSpent(ctx) {
			skipped = repos[i:]
			warnings = append(warnings, errDoctorBudget.Error()+"; skipped repositories: "+strings.Join(skipped, ", "))
			break
		}
		reports = append(reports, s.Doctor(ctx, repo))
	}
	return DoctorResult{reports, warnings, skipped}, nil
}

// Doctor is read-only. A named repository is checked even if the App does not
// cover it; using only App-visible repositories would hide that failure.
func (s *Service) Doctor(ctx context.Context, repo string) DoctorReport {
	return s.doctor(doctorContext(ctx), repo, true)
}

// doctorRepo is what GitHub says of the repository: through the App when it
// covers it (as the broker reads it), else through the owner's gh sign-in.
type doctorRepo struct {
	ID         int64  `json:"id"`
	FullName   string `json:"full_name"` // GitHub's spelling, as deliveries and runners_repos name it
	Private    bool   `json:"private"`
	Visibility string `json:"visibility"`
	Fork       bool   `json:"fork"`
}

// public reports whether r counts as public (ghapp.Public).
func (r doctorRepo) public() bool { return ghapp.Public(r.Private, r.Visibility) }

// doctor runs the checks; full adds those that read workflows and the
// repository's settings (the self-check runs without them).
func (s *Service) doctor(ctx context.Context, repo string, full bool) DoctorReport {
	out := DoctorReport{Repo: repo, CheckedAt: s.now(), Checks: make([]DoctorCheck, 17)}
	for i := 1; i <= 17; i++ {
		out.set(i, "warn", "unavailable: prerequisite check did not complete", s.cmd("ci doctor "+repo))
	}
	if !jobrule.ValidRepo(repo) {
		out.set(1, "fail", "repository must be owner/repo", s.cmd("ci doctor owner/repo"))
		return out
	}
	c := s.conf()
	installFix := c.GitHub.WebURL + "/apps/" + c.GitHub.Slug + "/installations/new"
	out.unavailable(14, "image Xcode inventory is not reported", "zm-eakh.12.54.27")
	s.doctorBuild(&out)
	var pool *doctorPool
	var cl *ghapp.Client
	var l link
	linked, covered, transport := false, false, false
	cl, err := s.client(ctx)
	if err == nil {
		err = cl.CheckApp(ctx)
	}
	switch {
	case isOff(err):
		out.set(1, "fail", "Your machine has no GitHub connection configured.", s.cmd("runners setup <pool>"))
	case err != nil:
		out.set(1, "warn", "unavailable: the configured GitHub connection could not be checked", "check GitHub connectivity and the configured App key; keep the existing App")
	default:
		out.set(1, "pass", "GitHub accepts the connection key.", "")
		owner, _, _ := strings.Cut(repo, "/")
		id, e := cl.OwnerInstallation(ctx, owner)
		switch {
		case ghapp.NotFound(e):
			out.set(2, "fail", "The app is not installed on "+owner+".", installFix)
		case e != nil:
			out.set(2, "warn", "unavailable: GitHub could not read the owner's installation", installFix)
		default:
			out.Installation = id
			out.set(2, "pass", "The app is installed on "+owner+".", "")
			_, e = cl.RepoInstallation(ctx, repo)
			switch {
			case ghapp.NotFound(e):
				out.set(3, "fail", repo+" is not included: GitHub never tells this machine about its jobs.", "ask the owner to widen the selected repositories at "+c.GitHub.WebURL+"/settings/installations/"+itoa(id)+" (organization owner: "+c.GitHub.WebURL+"/organizations/"+owner+"/settings/installations/"+itoa(id)+")")
			case e != nil:
				out.set(3, "warn", "unavailable: GitHub could not confirm repository coverage", installFix)
			default:
				covered = true
				out.set(3, "pass", "The app covers "+repo+".", "")
			}
			l, e = s.link(ctx, id)
			switch {
			case errors.Is(e, sql.ErrNoRows):
				out.set(4, "fail", "GitHub is connected but nobody confirmed it.", s.cmd("runners link "+itoa(id)))
			case e != nil:
				out.set(4, "warn", "unavailable: the link could not be read", s.cmd("runners ls"))
			default:
				linked = true
				out.set(4, "pass", "The GitHub connection is confirmed.", "")
				pool = s.doctorMachines(ctx, l, &out)
				s.doctorTransport(ctx, id, &out)
				transport = true
			}
		}
	}
	// Check 15 reads only the broker's own record, so GitHub not answering
	// the checks above (no route to it, a key it refuses: the outages check
	// 15 is there to report) does not stop it: the owner's installation is
	// then the one linked here. Not when GitHub said there is no connection
	// or no installation (checks 1 and 2 fail): there is nothing to check;
	// nor when GitHub named the installation (out.Installation) and it is
	// not linked here (check 4): an old link on the same account is not it.
	if !transport && out.Installation == 0 && out.Checks[0].Status != "fail" && out.Checks[1].Status != "fail" {
		owner, _, _ := strings.Cut(repo, "/")
		id, e := s.linkedInstallationOf(ctx, owner)
		switch {
		case errors.Is(e, sql.ErrNoRows):
		case e != nil:
			out.set(15, "warn", "unavailable: the link could not be read", s.cmd("runners ls"))
		default:
			out.Installation = id
			s.doctorTransport(ctx, id, &out)
		}
	}
	if !full {
		s.doctorWaiting(ctx, repo, &out)
		return out
	}
	facts, ok := s.doctorFacts(ctx, cl, out.Installation, covered, repo)
	if !ok {
		out.set(6, "warn", "unavailable: repository metadata needs this machine's gh sign-in", "check gh authentication, repository access and GitHub connectivity")
	} else {
		if facts.Fork {
			out.set(6, "pass", "A fork: its own branches are served by this build.", "")
		} else {
			out.set(6, "pass", "This repository is not a fork.", "")
		}
		if linked {
			var d delivery
			d.Installation.ID = out.Installation
			// GitHub's spelling: a delivery names the repository so, and
			// admission matches it exactly.
			d.Repo.FullName, d.Repo.ID, d.Repo.Private, d.Repo.Visibility = facts.FullName, facts.ID, facts.Private, facts.Visibility
			switch _, why, e := s.admit(ctx, d); {
			case e != nil:
				out.set(5, "warn", "unavailable: the repository's connection could not be read", s.cmd("runners repos"))
			case why != "":
				out.set(5, "fail", "Its jobs are refused: "+why, s.allowHint(repo, facts.public()))
			default:
				out.set(5, "pass", "The repository is connected to this pool.", "")
			}
		}
		s.doctorApproval(ctx, cl, out.Installation, covered, repo, facts, &out)
	}
	var factsOf *doctorRepo
	if ok {
		factsOf = &facts
	}
	s.doctorWorkflows(ctx, repo, &out, factsOf, pool)
	s.doctorWaiting(ctx, repo, &out)
	return out
}

// doctorSettings is check 17's settings part: the broker runs on the
// settings on disk. It reports whether it set check 17.
func (s *Service) doctorSettings(out *DoctorReport) bool {
	fresh, err := config.LoadFrom(s.d.Config.Home)
	if err != nil {
		out.set(17, "warn", "unavailable: settings on disk could not be read", "check config.json")
		return true
	}
	// The retired key is only read to be reported: deleting it, as the notice
	// says, is not a setting the broker has to restart for.
	disk := fresh.File.Runners
	disk.RetiredAllowPublicRepos = false
	s.cfgMu.RLock()
	mem := s.d.Config.File.Runners
	mem.RetiredAllowPublicRepos = false
	same := reflect.DeepEqual(disk, mem)
	s.cfgMu.RUnlock()
	if !same {
		out.set(17, "fail", "settings in memory differ from settings on disk", "restart the "+s.cmd("daemon"))
	}
	return !same
}

// doctorMachines is check 12 (any job) and 13 for the link's pool; it
// answers the pool for the per-job placement of checks 9 to 12. A link that
// can no longer use the pool fails check 4: that is about the link.
func (s *Service) doctorMachines(ctx context.Context, l link, out *DoctorReport) *doctorPool {
	sub, svc, err := s.poolSubject(ctx, l)
	if err != nil {
		out.set(4, "fail", "The confirmed connection can no longer use the machines.", s.cmd("runners link "+itoa(l.Installation)))
		return nil
	}
	pool, err := s.doctorPool(ctx, sub, svc, l)
	if err != nil {
		out.set(12, "warn", "unavailable: machines could not be read", "check that the pool's machines are reachable")
		return nil
	}
	names, why, permanent := pool.fit(want{}, s.jobTTL()+startupAllowance)
	switch {
	case len(names) > 0:
		out.set(12, "pass", "A machine is available; individual jobs may need a different OS or size.", "")
	case permanent:
		out.set(12, "fail", "No machine in the pool can take a job: "+why, "lend a machine to the pool (from that machine)")
	default:
		out.set(12, "warn", "No machine can take a job now: "+why, "wait for a machine to be listed, or lend one to the pool (from that machine)")
	}
	pool.runners(out, s.cmd)
	return pool
}

// doctorFacts reads the repository through the App when it covers it, else
// with the owner's gh sign-in.
func (s *Service) doctorFacts(ctx context.Context, cl *ghapp.Client, installation int64, covered bool, repo string) (doctorRepo, bool) {
	if covered && cl != nil {
		if r, err := cl.Repo(ctx, installation, repo); err == nil {
			return doctorRepo{ID: r.ID, FullName: r.FullName, Private: r.Private, Visibility: r.Visibility, Fork: r.Fork}, true
		}
	}
	var r doctorRepo
	if err := s.personRead(ctx, "/repos/"+repo, &r); err != nil || r.ID == 0 || !jobrule.ValidRepo(r.FullName) {
		return r, false
	}
	return r, true
}

// doctorApproval is check 7: a public repository's approval rule for
// outside contributors' runs, read and judged as the broker does
// (checkRepo, approvalWarning). A visibility this version does not know
// counts as public, but whether GitHub gives it an approval rule is unknown:
// the broker shows no warning for it, the doctor warns.
func (s *Service) doctorApproval(ctx context.Context, cl *ghapp.Client, installation int64, covered bool, repo string, facts doctorRepo, out *DoctorReport) {
	settings := s.conf().GitHub.WebURL + "/" + repo + "/settings/actions"
	kind, _ := ghapp.Audience(facts.Private, facts.Visibility)
	switch {
	case kind == "private" || kind == "internal": // GitHub's approval rule is for public repositories only
		out.set(7, "pass", "Outside-contributor approval does not apply to this "+kind+" repo.", "")
	case kind != "public":
		out.set(7, "warn", "GitHub names this repo's visibility "+strconv.Quote(kind)+", which this version does not know; it counts as public here and its outside-contributor approval rule cannot be judged.", settings)
	case !covered || cl == nil:
		out.set(7, "warn", "unavailable: the App must cover the repository to read its approval rule (check 3)", settings)
	default:
		approval, err := cl.ForkPRApproval(ctx, installation, repo)
		readErr := ""
		if err != nil {
			readErr = oneLine(err.Error())
		}
		switch w := approvalWarning("public", approval, readErr); {
		case w == "":
			out.set(7, "pass", "All outside contributors need approval.", "")
		case err != nil:
			out.set(7, "warn", "unavailable: "+w, settings)
		default:
			out.set(7, "fail", w, settings)
		}
	}
}

// doctorDeliveryGrace is how long a job may wait on GitHub before check 16
// counts it: its webhook delivery may still be on the way, and the broker's
// own check asks GitHub for waiting jobs every catchUpEvery, so a lost
// delivery is only the doctor's to report once that catch-up has had a turn.
func (s *Service) doctorDeliveryGrace() time.Duration { return s.catchUpEvery() + time.Minute }

// doctorWaiting is check 16: GitHub's waiting jobs for the pool against the
// broker's log and its inbox of deliveries not applied yet.
func (s *Service) doctorWaiting(ctx context.Context, repo string, out *DoctorReport) {
	var unseen []int64
	for _, status := range []string{"queued", "in_progress"} {
		for page := 1; ; page++ {
			var runs struct {
				Runs []struct {
					ID int64 `json:"id"`
				} `json:"workflow_runs"`
			}
			if e := s.personRead(ctx, "/repos/"+repo+"/actions/runs?status="+status+"&per_page=100&page="+itoa(int64(page)), &runs); e != nil {
				out.set(16, "warn", "unavailable: waiting jobs need this machine's gh sign-in", "check gh authentication, repository access and GitHub connectivity")
				return
			}
			for _, run := range runs.Runs {
				for jp := 1; ; jp++ {
					var jobs struct {
						Jobs []struct {
							ID        int64     `json:"id"`
							Status    string    `json:"status"`
							Labels    []string  `json:"labels"`
							CreatedAt time.Time `json:"created_at"`
						} `json:"jobs"`
					}
					if e := s.personRead(ctx, "/repos/"+repo+"/actions/runs/"+itoa(run.ID)+"/jobs?per_page=100&page="+itoa(int64(jp)), &jobs); e != nil {
						out.set(16, "warn", "unavailable: waiting jobs could not be read", "check gh authentication, repository access and GitHub connectivity")
						return
					}
					for _, job := range jobs.Jobs {
						if job.Status != "queued" || s.now().Sub(job.CreatedAt) < s.doctorDeliveryGrace() {
							continue
						}
						if _, ours, _ := s.labels(job.Labels); !ours {
							if SelfHosted(job.Labels) && !doctorDefaultLabels(job.Labels) {
								out.set(9, "warn", "A waiting job names another self-hosted fleet's labels", "inspect its runs-on labels if this pool was intended")
							}
							continue
						}
						known, e := s.doctorKnown(ctx, repo, job.ID)
						if e != nil {
							out.set(16, "warn", "unavailable: broker job history could not be read", s.cmd("runners jobs"))
							return
						}
						if !known {
							unseen = append(unseen, job.ID)
						}
					}
					if len(jobs.Jobs) < 100 {
						break
					}
				}
			}
			if len(runs.Runs) < 100 {
				break
			}
		}
	}
	missing := 0
	if len(unseen) > 0 {
		// Read after GitHub's answers: a delivery that came meanwhile counts.
		inbox, err := s.doctorInbox(ctx)
		if err != nil {
			out.set(16, "warn", "unavailable: broker inbox could not be read", s.cmd("runners jobs"))
			return
		}
		for _, id := range unseen {
			if inbox[strings.ToLower(repo)][id] {
				continue
			}
			// A worker may have drained the inbox since the history read.
			known, err := s.doctorKnown(ctx, repo, id)
			if err != nil {
				out.set(16, "warn", "unavailable: broker job history could not be read", s.cmd("runners jobs"))
				return
			}
			if !known {
				missing++
			}
		}
	}
	if missing == 0 {
		out.set(16, "pass", "No waiting jobs for these machines are missing from the broker's log.", "")
		return
	}
	fix := "the broker's own catch-up with GitHub did not find it either: check the daemon log for the runner check, then the App webhook delivery log on GitHub, and redeliver failed workflow_job deliveries"
	cause := "no matching delivery or job record"
	for _, n := range []int{1, 2, 3, 4, 5, 9, 10} {
		if c := out.Checks[n-1]; c.Status == "fail" {
			cause, fix = "check "+itoa(int64(n))+": "+c.Message, c.Fix
			break
		}
	}
	out.set(16, "fail", itoa(int64(missing))+" self-hosted jobs wait on GitHub; this broker was never told: "+cause+".", fix)
}

// doctorKnown reports whether the broker's log has job of repo.
func (s *Service) doctorKnown(ctx context.Context, repo string, job int64) (bool, error) {
	var n int
	err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_jobs WHERE job = ? AND repo = ? COLLATE NOCASE`, job, repo).Scan(&n)
	return n > 0, err
}

// doctorInbox is the workflow_job deliveries not applied yet, by repository
// (lower case) then job.
func (s *Service) doctorInbox(ctx context.Context) (map[string]map[int64]bool, error) {
	seen := map[string]map[int64]bool{}
	rows, err := s.db().QueryContext(ctx, `SELECT body FROM runners_inbox WHERE done = 0 AND event = 'workflow_job'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var d delivery
		if json.Unmarshal(body, &d) != nil {
			continue // apply drops it too
		}
		repo := strings.ToLower(d.Repo.FullName)
		if seen[repo] == nil {
			seen[repo] = map[int64]bool{}
		}
		seen[repo][d.Job.ID] = true
	}
	return seen, rows.Err()
}
